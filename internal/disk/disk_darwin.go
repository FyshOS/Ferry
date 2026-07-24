//go:build darwin

package disk

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Enumerate returns the removable disks currently attached.
//
// `diskutil list external physical` already excludes the machine's own drives
// and synthesised/APFS containers, so anything it names is a candidate; we then
// ask diskutil for the details of each. Disk images report as external too, so
// parseDiskutilInfo drops anything flagged virtual.
func Enumerate() ([]Disk, error) {
	out, err := exec.Command("diskutil", "list", "external", "physical").Output()
	if err != nil {
		return nil, fmt.Errorf("running diskutil list: %w", err)
	}

	mounts := diskMountpoints()

	var disks []Disk
	for _, id := range parseDiskutilList(out) {
		info, err := exec.Command("diskutil", "info", id).Output()
		if err != nil {
			// A device unplugged mid-enumeration should not fail the whole list.
			continue
		}
		d, ok := parseDiskutilInfo(info)
		if !ok {
			continue
		}
		d.Mountpoints = mounts[d.Name]
		disks = append(disks, d)
	}
	return disks, nil
}

// diskutilID matches the whole-disk identifiers in `diskutil list` output,
// whose device lines look like "/dev/disk4 (external, physical):".
var diskutilID = regexp.MustCompile(`^/dev/(disk\d+)\b`)

// parseDiskutilList extracts the whole-disk identifiers from `diskutil list`.
func parseDiskutilList(data []byte) []string {
	var ids []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		if m := diskutilID.FindStringSubmatch(sc.Text()); m != nil {
			ids = append(ids, m[1])
		}
	}
	return ids
}

// diskutilBytes pulls the exact byte count out of a diskutil size field, which
// reads "8.6 GB (8640300544 Bytes) (exactly 16875587 512-Byte-Units)".
var diskutilBytes = regexp.MustCompile(`\((\d+) Bytes\)`)

// parseDiskutilInfo converts `diskutil info <id>` output into a Disk. It
// reports false for anything Ferry must not offer as a target: partitions
// rather than whole disks, virtual devices (disk images), and zero-size media.
func parseDiskutilInfo(data []byte) (Disk, bool) {
	fields := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		key, val, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}

	// Writing to a partition rather than the whole disk would corrupt the media.
	if !strings.EqualFold(fields["Whole"], "Yes") {
		return Disk{}, false
	}
	// Disk images mount as external removable media; they are not write targets.
	if strings.EqualFold(fields["Virtual"], "Yes") {
		return Disk{}, false
	}
	if strings.EqualFold(fields["Read-Only Media"], "Yes") {
		return Disk{}, false
	}

	path := fields["Device Node"]
	if path == "" {
		return Disk{}, false
	}

	var size int64
	if m := diskutilBytes.FindStringSubmatch(fields["Disk Size"]); m != nil {
		size, _ = strconv.ParseInt(m[1], 10, 64)
	}
	if size <= 0 {
		return Disk{}, false
	}

	protocol := fields["Protocol"]
	return Disk{
		Name: strings.TrimPrefix(path, "/dev/"),
		Path: path,
		Size: size,
		// macOS reports vendor and model as one string, so Vendor stays empty.
		Model: fields["Device / Media Name"],
		Removable: strings.EqualFold(fields["Removable Media"], "Removable") ||
			strings.EqualFold(fields["Ejectable"], "Yes"),
		Transport: strings.ToLower(protocol),
	}, true
}

// wholeDisk captures the whole-disk identifier from a device node, mapping
// "/dev/disk4s1" to "disk4".
var wholeDisk = regexp.MustCompile(`^/dev/(disk\d+)`)

// diskMountpoints maps a whole-disk identifier ("disk4") to the mountpoints of
// it and any of its partitions.
func diskMountpoints() map[string][]string {
	out, err := exec.Command("mount").Output()
	if err != nil {
		return nil
	}
	return parseMountOutput(out)
}

// parseMountOutput groups mountpoints by whole-disk identifier from `mount`
// output, whose lines look like "/dev/disk4s1 on /Volumes/USB (msdos, local)".
func parseMountOutput(out []byte) map[string][]string {
	mounts := map[string][]string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		dev, rest, found := strings.Cut(sc.Text(), " on ")
		if !found || !strings.HasPrefix(dev, "/dev/disk") {
			continue
		}
		point, _, found := strings.Cut(rest, " (")
		if !found || point == "" {
			continue
		}
		// Trim the partition suffix: /dev/disk4s1 belongs to disk4. Matching the
		// leading "diskN" rather than cutting at the first "s" matters, since
		// "disk" itself contains one.
		m := wholeDisk.FindStringSubmatch(dev)
		if m == nil {
			continue
		}
		mounts[m[1]] = append(mounts[m[1]], point)
	}
	return mounts
}

// errAuthCancelled is returned when the user dismisses the authorization dialog.
var errAuthCancelled = errors.New("authorization was declined")

// devBlockAlign is the boundary raw device I/O must land on. Real media use 512
// or 4096 byte blocks; 4096 satisfies both, so we align to it rather than
// interrogating each device.
const devBlockAlign = 4096

// copyChunk is the unit of I/O, a multiple of devBlockAlign, and also how often
// progress is reported.
const copyChunk = 4 << 20

// rawDevicePath maps a device node to its raw counterpart: /dev/disk4 becomes
// /dev/rdisk4. The raw node is dramatically faster and bypasses the buffer
// cache, so the read-back verify sees the media rather than pages we just wrote.
func rawDevicePath(path string) string {
	return strings.Replace(path, "/dev/disk", "/dev/rdisk", 1)
}

// authopenDevice opens a device for reading and writing with administrator
// authorization and returns the open file.
//
// The privileged step is only the open. /usr/libexec/authopen is setuid root and
// carries the TCC entitlement for removable volumes, which is what actually
// permits raw disk access - being root is not sufficient and not required, so
// Ferry never runs as root and needs no sudo. authopen presents the system
// authorization dialog, then hands the open descriptor back over a socketpair as
// an SCM_RIGHTS control message.
func authopenDevice(ctx context.Context, path string) (*os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("preparing authorization channel: %w", err)
	}
	parent := os.NewFile(uintptr(fds[0]), "authopen-parent")
	child := os.NewFile(uintptr(fds[1]), "authopen-child")
	defer parent.Close()

	cmd := exec.CommandContext(ctx, "/usr/libexec/authopen",
		"-stdoutpipe", "-o", strconv.Itoa(syscall.O_RDWR), path)
	cmd.Stdout = child
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		child.Close()
		return nil, fmt.Errorf("starting authopen: %w", err)
	}
	// The child holds its own copy; ours must go or the read below never ends.
	child.Close()

	buf := make([]byte, 32)
	oob := make([]byte, syscall.CmsgSpace(4))
	_, oobn, _, _, rerr := syscall.Recvmsg(int(parent.Fd()), buf, oob, 0)
	waitErr := cmd.Wait()

	if rerr != nil || oobn == 0 {
		// No descriptor came back: the dialog was declined, or authorization
		// was refused outright.
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("%w: %s", errAuthCancelled, detail)
		}
		if waitErr != nil {
			return nil, errAuthCancelled
		}
		return nil, fmt.Errorf("authopen returned no descriptor for %s", path)
	}

	scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(scms) == 0 {
		return nil, fmt.Errorf("reading authorization reply: %w", err)
	}
	got, err := syscall.ParseUnixRights(&scms[0])
	if err != nil || len(got) == 0 {
		return nil, fmt.Errorf("reading authorization reply: %w", err)
	}
	return os.NewFile(uintptr(got[0]), path), nil
}

// Write copies isoPath onto the disk, verifies the result and ejects the media.
// It asks for authorization through the system dialog; Ferry itself stays
// unprivileged. Progress is reported through the optional callback.
//
// opts.DataPartition is accepted for signature parity but ignored: macOS cannot
// add the data partition (see DataPartitionSupported), and the UI never offers
// it here, so the returned WriteResult always reports DataSkipped.
func Write(ctx context.Context, isoPath string, d Disk, opts WriteOptions, onProgress func(WriteProgress)) (WriteResult, error) {
	info, err := os.Stat(isoPath)
	if err != nil {
		return WriteResult{}, fmt.Errorf("reading image: %w", err)
	}
	size := info.Size()
	if size <= 0 {
		return WriteResult{}, fmt.Errorf("image %q is empty", isoPath)
	}
	if d.Size < size {
		return WriteResult{}, fmt.Errorf("device %s (%s) is too small for the image (%s)",
			d.Path, FormatSize(d.Size), FormatSize(size))
	}

	report := func(p WriteProgress) {
		if onProgress != nil {
			onProgress(p)
		}
	}

	src, err := os.Open(isoPath)
	if err != nil {
		return WriteResult{}, fmt.Errorf("reading image: %w", err)
	}
	defer src.Close()

	// Writing to a disk with mounted volumes fails with a bare "Resource busy".
	// This needs no privileges for an external disk.
	if out, err := exec.CommandContext(ctx, "diskutil", "unmountDisk", "force", d.Path).
		CombinedOutput(); err != nil {
		return WriteResult{}, fmt.Errorf("could not unmount %s: %s", d.Path, strings.TrimSpace(string(out)))
	}

	dev, err := authopenDevice(ctx, rawDevicePath(d.Path))
	if err != nil {
		return WriteResult{}, err
	}
	defer dev.Close()

	// Hash the image as it streams past, so verification costs no extra read.
	srcHash := sha256.New()

	report(WriteProgress{Phase: PhaseWriting, Bytes: 0, Total: size})
	buf := make([]byte, copyChunk)
	var written int64
	for written < size {
		n, rerr := io.ReadFull(src, buf)
		if n == 0 {
			if rerr == io.EOF {
				break
			}
			return WriteResult{}, fmt.Errorf("reading image: %w", rerr)
		}
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return WriteResult{}, fmt.Errorf("reading image: %w", rerr)
		}
		srcHash.Write(buf[:n])

		// Raw devices reject writes that are not a whole number of blocks, so
		// the final short chunk is zero-padded up to the boundary. The extra
		// bytes land past the image and are excluded from verification.
		out := n
		if rem := out % devBlockAlign; rem != 0 {
			pad := devBlockAlign - rem
			for i := out; i < out+pad; i++ {
				buf[i] = 0
			}
			out += pad
		}
		if _, werr := dev.Write(buf[:out]); werr != nil {
			return WriteResult{}, fmt.Errorf("writing to %s: %w", d.Path, writeHint(werr))
		}

		written += int64(n)
		report(WriteProgress{Phase: PhaseWriting, Bytes: written, Total: size})

		if ctx.Err() != nil {
			return WriteResult{}, ctx.Err()
		}
	}
	if written < size {
		return WriteResult{}, fmt.Errorf("image ended after %s of %s", FormatSize(written), FormatSize(size))
	}

	report(WriteProgress{Phase: PhaseSyncing, Bytes: 0, Total: size})
	if err := dev.Sync(); err != nil && !benignSyncError(err) {
		return WriteResult{}, fmt.Errorf("flushing %s: %w", d.Path, err)
	}

	// Read the image back off the media and compare. The raw device is
	// uncached, so this reads what was actually stored.
	report(WriteProgress{Phase: PhaseVerifying, Bytes: 0, Total: size})
	if _, err := dev.Seek(0, io.SeekStart); err != nil {
		return WriteResult{}, fmt.Errorf("rewinding %s: %w", d.Path, err)
	}
	devHash := sha256.New()
	var read int64
	for read < size {
		n, rerr := dev.Read(buf)
		if n > 0 {
			use := int64(n)
			if remaining := size - read; use > remaining {
				use = remaining
			}
			devHash.Write(buf[:use])
			read += use
			report(WriteProgress{Phase: PhaseVerifying, Bytes: read, Total: size})
		}
		if rerr != nil {
			return WriteResult{}, fmt.Errorf("reading back from %s: %w", d.Path, rerr)
		}
		if n == 0 {
			return WriteResult{}, fmt.Errorf("reading back from %s stopped at %s of %s",
				d.Path, FormatSize(read), FormatSize(size))
		}
		if ctx.Err() != nil {
			return WriteResult{}, ctx.Err()
		}
	}
	if hex.EncodeToString(devHash.Sum(nil)) != hex.EncodeToString(srcHash.Sum(nil)) {
		return WriteResult{}, ErrVerifyMismatch
	}

	// The image is verified. macOS cannot add the data partition (see
	// DataPartitionSupported), so opts.DataPartition is never set here; the write
	// simply finishes and ejects.
	dev.Close()

	report(WriteProgress{Phase: PhaseEjecting, Bytes: 0, Total: size})
	// A failure to eject does not make the write any less valid.
	_ = exec.CommandContext(ctx, "diskutil", "eject", d.Path).Run()

	report(WriteProgress{Phase: PhaseDone, Bytes: size, Total: size})
	return WriteResult{}, nil
}

// DataPartitionSupported reports whether this platform can add the optional
// exFAT data partition. macOS cannot: the isohybrid images present a hybrid MBR
// (amd64) or no partition table at all (arm64), and macOS's gpt/diskutil refuse
// to add a partition to either. The feature is offered only on Linux.
func DataPartitionSupported() bool { return false }

// benignSyncError reports whether a failed flush can be ignored. Writes to a
// raw device go straight to the media rather than through the buffer cache, so
// there is nothing to flush and fsync is simply not implemented for it: macOS
// answers ENOTTY. Failing the write over that would reject data already safely
// on the disk.
func benignSyncError(err error) bool {
	return errors.Is(err, syscall.ENOTTY) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EINVAL)
}

// writeHint annotates the error macOS returns when its privacy controls, rather
// than the permission bits, are refusing access.
func writeHint(err error) error {
	if errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("%w (macOS is blocking raw disk access; grant Ferry "+
			"Removable Volumes or Full Disk Access in System Settings › Privacy & Security)", err)
	}
	return err
}
