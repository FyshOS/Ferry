//go:build linux

package disk

import (
	"bufio"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Both the Linux and macOS backends drive the write from a privileged shell
// script that reports progress in a common line protocol: phase announcements
// prefixed with phaseMarker, dd's own byte counts, and a final checksum line.
// The parsing below is shared; only the scripts and the way they are elevated
// differ per platform.

// ddProgress matches the leading byte count of a dd progress line. GNU dd
// (status=progress) writes "N bytes (...) copied"; BSD dd on SIGINFO writes
// "N bytes transferred in ...". The leading count is common to both.
var ddProgress = regexp.MustCompile(`^(\d+) bytes`)

// shaLine matches the checksum line our privileged script reports.
var shaLine = regexp.MustCompile(`^SHA256 ([0-9a-f]{64})`)

// debugWrite turns on verbose logging of the privileged script's output.
// Enable with FERRY_DEBUG_WRITE=1.
var debugWrite = os.Getenv("FERRY_DEBUG_WRITE") != ""

// phaseMarker prefixes the phase announcements the script writes.
const phaseMarker = "FERRY-PHASE "

// errorMarker prefixes a stage failure the script reports explicitly.
const errorMarker = "FERRY-ERROR "

// noteMarker prefixes non-fatal context the script reports for debugging.
const noteMarker = "FERRY-NOTE "

// diagLimit caps how many diagnostic lines we retain from a failing script.
const diagLimit = 20

// ddRecords matches dd's "12+0 records in" summary lines, which are noise
// rather than diagnostics.
var ddRecords = regexp.MustCompile(`^\d+\+\d+ records (in|out)$`)

// parseWriteProgress consumes the privileged script's progress stream, turning
// its phase markers and dd's byte counts into progress reports. Each stage
// restarts at zero.
//
// It returns whatever the script said that was not progress: the elevation
// helpers give us no usable stderr of their own, so these lines are the only
// explanation available when a write fails.
func parseWriteProgress(r io.Reader, size int64, report func(WriteProgress)) []string {
	phase := PhaseWriting
	start := time.Now()
	var diags []string

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
			continue
		}

		// Anything else is the script or dd explaining itself, most usefully
		// when something has gone wrong.
		if ddRecords.MatchString(line) || shaLine.MatchString(line) {
			continue
		}
		if len(diags) < diagLimit {
			diags = append(diags, line)
		}
	}
	return diags
}

// scriptError turns the script's diagnostic lines into an error message. The
// script's own FERRY-ERROR line says which stage failed, but the useful detail
// is whatever the failing tool printed, so both go into the message.
func scriptError(diags []string) string {
	var stage string
	var detail []string
	for _, l := range diags {
		switch {
		case strings.HasPrefix(l, errorMarker):
			if stage == "" {
				stage = strings.TrimPrefix(l, errorMarker)
			}
		case strings.HasPrefix(l, noteMarker):
			// Notes are context for debugging, not failures in themselves.
		default:
			detail = append(detail, l)
		}
	}
	switch {
	case stage != "" && len(detail) > 0:
		return stage + ": " + strings.Join(detail, "; ")
	case stage != "":
		return stage
	default:
		return strings.Join(detail, "; ")
	}
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
