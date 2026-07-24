//go:build linux

package disk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// DataPartitionSupported reports whether this platform can add the optional
// exFAT data partition after writing. Only Linux can: its sgdisk/mkfs.exfat
// reliably extend the image's GPT, whereas macOS's tools refuse the isohybrid
// images' hybrid MBR.
func DataPartitionSupported() bool { return true }

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
// whose failure is swallowed by "|| echo FERRY-DATA error", so a partitioning
// problem can neither abort the parent's "set -e" nor skip the eject; it is
// reported as a warning, never a failed write.
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
		command -v sgdisk >/dev/null 2>&1 || { echo "FERRY-DATA error sgdisk is not installed"; exit 1; }
		if command -v mkfs.exfat >/dev/null 2>&1; then mkexfat="mkfs.exfat"
		elif command -v mkexfatfs >/dev/null 2>&1; then mkexfat="mkexfatfs"
		else echo "FERRY-DATA error no exFAT formatter found (install exfatprogs)"; exit 1; fi

		# Move the backup GPT to the true end of the device; a dd'd isohybrid
		# leaves it mid-device, which blocks adding any partition past it.
		sgdisk -e "$dev" >/dev/null 2>&1

		# Next free partition number = highest existing + 1.
		num=$(sgdisk -p "$dev" 2>/dev/null | awk '/^[[:space:]]*[0-9]+/{n=$1} END{print n+0}')
		num=$((num + 1))

		# New partition: first aligned free sector to end of disk, type 0700
		# (Microsoft basic data, which exFAT lives under). Start 0 = next free
		# aligned sector, so it can never overlap the image region.
		sgdisk -a 2048 -n ${num}:0:0 -t ${num}:0700 -c ${num}:"$label" "$dev" >/dev/null 2>&1

		# Re-read the table so the kernel creates the new node.
		partprobe "$dev" >/dev/null 2>&1 || blockdev --rereadpt "$dev" >/dev/null 2>&1 || partx -u "$dev" >/dev/null 2>&1 || true
		udevadm settle >/dev/null 2>&1 || true

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
