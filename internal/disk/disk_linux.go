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
// them, then ejects. Arguments: iso, device, byte-size.
//
// blockdev --flushbufs drops the device's buffer cache before the read-back.
// Without it the verify could be served from the pages we just wrote.
const writeScript = `
set -e
iso="$1"; dev="$2"; size="$3"
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
echo "FERRY-PHASE ejecting" >&2
eject "$dev" 2>/dev/null || udisksctl power-off -b "$dev" 2>/dev/null || true
`

// Write copies isoPath onto the disk, verifies the result and ejects the media.
// It requires administrator rights and prompts for them via pkexec. Progress is
// reported through the optional callback.
func Write(ctx context.Context, isoPath string, d Disk, onProgress func(WriteProgress)) error {
	info, err := os.Stat(isoPath)
	if err != nil {
		return fmt.Errorf("reading image: %w", err)
	}
	size := info.Size()
	if size <= 0 {
		return fmt.Errorf("image %q is empty", isoPath)
	}
	if d.Size < size {
		return fmt.Errorf("device %s (%s) is too small for the image (%s)",
			d.Path, FormatSize(d.Size), FormatSize(size))
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
		return errors.New("pkexec is required to write to the device but was not found")
	}

	cmd := exec.CommandContext(ctx, "pkexec", "/bin/sh", "-c", writeScript,
		"sh", isoPath, d.Path, strconv.FormatInt(size, 10))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting write: %w", err)
	}

	// The script announces each stage on stderr, interleaved with dd's
	// carriage-return separated progress updates.
	var diags []string
	ddDone := make(chan struct{})
	go func() {
		defer close(ddDone)
		diags = parseWriteProgress(stderr, size, report)
	}()

	// The checksum line is the only thing we expect on stdout.
	var gotSum string
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if m := shaLine.FindStringSubmatch(line); m != nil {
				gotSum = m[1]
			}
		}
	}()

	<-ddDone
	<-done

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if detail := scriptError(diags); detail != "" {
			return fmt.Errorf("writing image: %s", detail)
		}
		return fmt.Errorf("writing image: %w", err)
	}

	if gotSum == "" {
		return errors.New("write finished but no verification checksum was produced")
	}

	src := <-hashCh
	if src.err != nil {
		return fmt.Errorf("checksumming image: %w", src.err)
	}
	if gotSum != src.sum {
		return ErrVerifyMismatch
	}

	report(WriteProgress{Phase: PhaseDone, Bytes: size, Total: size})
	return nil
}
