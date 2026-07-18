//go:build linux

package disk

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
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

// ddProgress matches the leading byte count of a `dd status=progress` line.
var ddProgress = regexp.MustCompile(`^(\d+) bytes`)

// shaLine matches the checksum line our privileged script prints on stdout.
var shaLine = regexp.MustCompile(`^SHA256 ([0-9a-f]{64})`)

// debugWrite turns on verbose logging of the privileged script's output.
// Enable with FERRY_DEBUG_WRITE=1.
var debugWrite = os.Getenv("FERRY_DEBUG_WRITE") != ""

// phaseMarker prefixes the phase announcements the script writes to stderr.
const phaseMarker = "FERRY-PHASE "

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
	ddDone := make(chan struct{})
	go func() {
		defer close(ddDone)
		parseWriteStderr(stderr, size, report)
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

// parseWriteStderr consumes the privileged script's stderr, turning its phase
// markers and dd's progress updates into progress reports. Each stage restarts
// at zero.
func parseWriteStderr(r io.Reader, size int64, report func(WriteProgress)) {
	phase := PhaseWriting
	start := time.Now()

	sc := bufio.NewScanner(r)
	sc.Split(scanCROrLF)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if debugWrite && line != "" {
			log.Printf("ferry: +%6.1fs raw %q", time.Since(start).Seconds(), line)
		}
		if line == "" {
			continue
		}

		if name, ok := strings.CutPrefix(line, phaseMarker); ok {
			switch name {
			case "writing":
				phase = PhaseWriting
			case "syncing":
				phase = PhaseSyncing
			case "verifying":
				phase = PhaseVerifying
			case "ejecting":
				phase = PhaseEjecting
			default:
				continue
			}
			// Announce the new stage with a reset count so the bar restarts.
			report(WriteProgress{Phase: phase, Bytes: 0, Total: size})
			continue
		}

		if m := ddProgress.FindStringSubmatch(line); m != nil {
			if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				// dd counts past the image on the read-back's final block;
				// clamp so the bar never overshoots.
				if n > size {
					n = size
				}
				if debugWrite {
					log.Printf("ferry: +%6.1fs report %s %d/%d (%.1f%%)",
						time.Since(start).Seconds(), phase, n, size,
						float64(n)/float64(size)*100)
				}
				report(WriteProgress{Phase: phase, Bytes: n, Total: size})
			}
		}
	}
}

// sha256File returns the hex sha256 of a file's contents.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// scanCROrLF is a bufio.SplitFunc that breaks on either CR or LF, so we can read
// dd's carriage-return separated progress updates as individual tokens.
func scanCROrLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
