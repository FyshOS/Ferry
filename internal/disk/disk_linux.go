//go:build linux

package disk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DataPartitionSupported reports whether this platform can add the optional
// exFAT data partition after writing. Only Linux/amd64 combinations supported.
func DataPartitionSupported() bool { return true }

// privilegedToolDirs are the directories the up-front tool check searches in
// addition to Ferry's own PATH. The write runs under pkexec, which resets the
// environment to a root PATH that includes the sbin directories.
var privilegedToolDirs = []string{
	"/usr/local/sbin", "/usr/local/bin",
	"/usr/sbin", "/usr/bin",
	"/sbin", "/bin",
}

// haveTool reports whether any of the named executables is available, looking
// first on Ferry's PATH and then in the standard system directories the
// privileged write uses.
func haveTool(names ...string) bool {
	for _, n := range names {
		if _, err := exec.LookPath(n); err == nil {
			return true
		}
		for _, dir := range privilegedToolDirs {
			if fi, err := os.Stat(filepath.Join(dir, n)); err == nil &&
				!fi.IsDir() && fi.Mode()&0o111 != 0 {
				return true
			}
		}
	}
	return false
}

// MissingDataTools reports which external tools the data-partition step needs
// but cannot be found, as human-readable names.
func MissingDataTools() []string {
	var missing []string

	if !haveTool("sfdisk") {
		missing = append(missing, "sfdisk (from util-linux)")
	}
	if !haveTool("mkfs.exfat", "mkexfatfs") {
		missing = append(missing, "mkfs.exfat (from exfatprogs)")
	}
	return missing
}

// Enumerate returns the removable USB disks currently attached.
func Enumerate() ([]Disk, error) {
	cmd := exec.Command("lsblk", "-b", "-J", "-o",
		"NAME,PATH,SIZE,TYPE,MOUNTPOINT,RM,HOTPLUG,TRAN,MODEL,VENDOR")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running lsblk: %w", err)
	}
	return parseLsblk(out)
}

// writeScript runs as root via pkexec. It unmounts any partitions on the
// target, writes the image, flushes, reads the written bytes back to checksum
// them, optionally adds an exFAT data partition in the free space, then ejects.
// Arguments: iso, device, byte-size, data-label, make-data ("1"/"0").
//
// blockdev --flushbufs drops the device's buffer cache before the read-back.
// Without it the verify could be served from the pages we just wrote.
//
// The data-partition section runs only after the SHA256 line is already on
// stdout, so the write is verified-good before it starts. It runs in a subshell
// whose failure is ignored as this step is optional.
const writeScript = `
set -e
iso="$1"; dev="$2"; size="$3"; label="$4"; mkdata="$5"
for p in "$dev"*; do umount "$p" 2>/dev/null || true; done
echo "FERRY-PHASE writing" >&2
dd if="$iso" of="$dev" bs=4M oflag=direct status=progress ||
	dd if="$iso" of="$dev" bs=4M status=progress
echo "FERRY-PHASE syncing" >&2
sync
blockdev --flushbufs "$dev" 2>/dev/null || true
echo "FERRY-PHASE verifying" >&2
sum=$(dd if="$dev" bs=4M count="$size" iflag=count_bytes status=progress | sha256sum | cut -d' ' -f1)
echo "SHA256 $sum"
if [ "$mkdata" = "1" ]; then
	echo "FERRY-PHASE partitioning" >&2
	(
		set -e
		if command -v mkfs.exfat >/dev/null 2>&1; then mkexfat="mkfs.exfat"
		elif command -v mkexfatfs >/dev/null 2>&1; then mkexfat="mkexfatfs"
		else echo "FERRY-DATA error no exFAT formatter found (install exfatprogs)"; exit 1; fi
		command -v sfdisk >/dev/null 2>&1 || { echo "FERRY-DATA error sfdisk is not installed"; exit 1; }

		# These images are a hybrid MBR ("dos") disklabel carrying a decorative GPT
		# whose backup copy is invalid once the image is dd'd onto a larger stick.
		# Append one primary partition, empty start = first free 1 MiB-aligned sector,
		# empty size = to the end of the device, type 07 (Microsoft basic data, where
		# exFAT lives). --append keeps the ISO and ESP entries untouched.
		if ! err=$(printf ',,7\n' | sfdisk --append --force "$dev" 2>&1 >/dev/null); then
			echo "FERRY-DATA error sfdisk could not add the partition: $(echo "$err" | tr '\n' ' ')"
			exit 1
		fi

		# Re-read the table so the kernel creates the new node.
		partprobe "$dev" >/dev/null 2>&1 || blockdev --rereadpt "$dev" >/dev/null 2>&1 || partx -u "$dev" >/dev/null 2>&1 || true
		udevadm settle >/dev/null 2>&1 || true

		# New partition = the highest-numbered entry now in the table.
		num=$(sfdisk -d "$dev" 2>/dev/null | sed -n "s#^${dev}p\{0,1\}\([0-9]\{1,\}\) .*#\1#p" | sort -n | tail -1)
		[ -n "$num" ] || { echo "FERRY-DATA error could not determine the new partition number"; exit 1; }

		case "$dev" in
			*[0-9]) part="${dev}p${num}" ;;
			*)      part="${dev}${num}" ;;
		esac
		ok=""
		for i in 1 2 3 4 5 6 7 8 9 10; do
			[ -b "$part" ] && { ok=1; break; }
			sleep 0.3
		done
		[ -n "$ok" ] || { echo "FERRY-DATA error data partition $part did not appear"; exit 1; }

		echo "FERRY-PHASE formatting" >&2
		if "$mkexfat" --version 2>&1 | grep -qi exfatprogs; then
			"$mkexfat" -L "$label" "$part" >/dev/null 2>&1
		else
			"$mkexfat" -n "$label" "$part" >/dev/null 2>&1
		fi
		echo "FERRY-DATA created $part $label"
	) || echo "FERRY-DATA error the data partition could not be created"
fi
echo "FERRY-PHASE ejecting" >&2
eject "$dev" 2>/dev/null || udisksctl power-off -b "$dev" 2>/dev/null || true
`

// Write copies isoPath onto the disk, verifies the result and ejects the media.
// It requires administrator rights and prompts for them via pkexec. Progress is
// reported through the optional callback.
//
// When opts.DataPartition is set and the device has room to spare, an exFAT data
// partition is added in the free space after the image. That step runs only once
// the image is verified good and its failure is reported in the returned
// WriteResult, never as an error: a verified image always succeeds.
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

	// Only attempt the data partition when asked and there is genuinely room.
	wantData := opts.DataPartition && DataFreeBytes(d.Size, size) > 0
	mkdata := "0"
	if wantData {
		mkdata = "1"
	}

	report := func(p WriteProgress) {
		if onProgress != nil {
			onProgress(p)
		}
	}

	// Checksum the source in the background so it overlaps the write.
	type hashResult struct {
		sum string
		err error
	}
	hashCh := make(chan hashResult, 1)
	go func() {
		sum, herr := sha256File(isoPath)
		hashCh <- hashResult{sum, herr}
	}()

	if _, err := exec.LookPath("pkexec"); err != nil {
		return WriteResult{}, errors.New("pkexec is required to write to the device but was not found")
	}

	cmd := exec.CommandContext(ctx, "pkexec", "/bin/sh", "-c", writeScript,
		"sh", isoPath, d.Path, strconv.FormatInt(size, 10), opts.label(), mkdata)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return WriteResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return WriteResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return WriteResult{}, fmt.Errorf("starting write: %w", err)
	}

	// The script announces each stage on stderr, interleaved with dd's
	// carriage-return separated progress updates.
	var diags []string
	ddDone := make(chan struct{})
	go func() {
		defer close(ddDone)
		diags = parseWriteProgress(stderr, size, report)
	}()

	// The checksum line and the optional data-partition result arrive on stdout.
	var gotSum string
	var gotData bool
	var data WriteResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if m := shaLine.FindStringSubmatch(line); m != nil {
				gotSum = m[1]
				continue
			}
			// Keep only the first data line: on failure the script may emit a
			// specific reason followed by a generic catch-all.
			if status, node, detail, ok := parseDataResult(line); ok && !gotData {
				gotData = true
				data.Data = status
				switch status {
				case DataCreated:
					data.DataNode, data.DataLabel = node, detail
				case DataFailed:
					data.DataWarn = detail
				}
			}
		}
	}()

	<-ddDone
	<-done

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return WriteResult{}, ctx.Err()
		}
		if detail := scriptError(diags); detail != "" {
			return WriteResult{}, fmt.Errorf("writing image: %s", detail)
		}
		return WriteResult{}, fmt.Errorf("writing image: %w", err)
	}

	if gotSum == "" {
		return WriteResult{}, errors.New("write finished but no verification checksum was produced")
	}

	src := <-hashCh
	if src.err != nil {
		return WriteResult{}, fmt.Errorf("checksumming image: %w", src.err)
	}
	if gotSum != src.sum {
		return WriteResult{}, ErrVerifyMismatch
	}

	// The image is written and verified. Settle the data-partition outcome: if
	// we asked for one but the script reported nothing, treat that as a failure
	// (a warning only — the image is still good).
	if wantData && !gotData {
		data = WriteResult{Data: DataFailed, DataWarn: "the data partition step produced no result"}
	}

	report(WriteProgress{Phase: PhaseDone, Bytes: size, Total: size})
	return data, nil
}
