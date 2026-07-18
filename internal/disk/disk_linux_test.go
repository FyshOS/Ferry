//go:build linux

package disk

import (
	"bufio"
	"strings"
	"testing"
)

// TestDDProgressParsing feeds a realistic dd status=progress stream (updates
// separated by carriage returns, then a newline summary) through the same
// scanner the writer uses, and checks we recover the final byte count.
func TestDDProgressParsing(t *testing.T) {
	stream := "1048576 bytes (1.0 MB, 1.0 MiB) copied, 0.1 s, 10 MB/s\r" +
		"524288000 bytes (524 MB, 500 MiB) copied, 5 s, 105 MB/s\r" +
		"1048576000 bytes (1.0 GB, 1000 MiB) copied, 10 s, 105 MB/s\r" +
		"1200+0 records in\n1200+0 records out\n" +
		"1258291200 bytes (1.3 GB, 1.2 GiB) copied, 12 s, 105 MB/s\n"

	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Split(scanCROrLF)

	var last int64 = -1
	var samples int
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := ddProgress.FindStringSubmatch(line); m != nil {
			samples++
			var n int64
			for _, r := range m[1] {
				n = n*10 + int64(r-'0')
			}
			last = n
		}
	}
	if samples < 3 {
		t.Errorf("expected several progress samples, got %d", samples)
	}
	if last != 1258291200 {
		t.Errorf("final parsed bytes = %d, want 1258291200", last)
	}
}

// TestParseWriteStderrPhases feeds a full script stderr stream - phase markers
// interleaved with dd's progress for both the write and the read-back - and
// checks each stage restarts the progress bar rather than leaving it pinned at
// 100% from the previous stage.
func TestParseWriteStderrPhases(t *testing.T) {
	const size = 1000
	stream := "FERRY-PHASE writing\n" +
		"\r250 bytes (250 B) copied, 1 s, 250 B/s" +
		"\r1000 bytes (1000 B) copied, 4 s, 250 B/s\n" +
		"1+0 records in\n1+0 records out\n" +
		"1000 bytes (1000 B) copied, 4 s, 250 B/s\n" +
		"FERRY-PHASE syncing\n" +
		"FERRY-PHASE verifying\n" +
		"\r500 bytes (500 B) copied, 1 s, 500 B/s" +
		"\r1000 bytes (1000 B) copied, 2 s, 500 B/s\n" +
		"FERRY-PHASE ejecting\n"

	var got []WriteProgress
	parseWriteStderr(strings.NewReader(stream), size, func(p WriteProgress) {
		got = append(got, p)
	})

	// Every phase must be announced with a zeroed count so the bar restarts.
	for _, phase := range []string{PhaseWriting, PhaseSyncing, PhaseVerifying, PhaseEjecting} {
		found := false
		for _, p := range got {
			if p.Phase == phase && p.Bytes == 0 {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("phase %q never reported with a reset (Bytes==0)", phase)
		}
	}

	// The verify stage must report real byte progress of its own, and at least
	// one of those reports must be a partial (not just the final 100%).
	var verifyPartial bool
	for _, p := range got {
		if p.Phase == PhaseVerifying && p.Bytes > 0 && p.Bytes < size {
			verifyPartial = true
		}
	}
	if !verifyPartial {
		t.Error("verify stage reported no intermediate progress - bar would sit still")
	}

	// Progress must never exceed the total.
	for _, p := range got {
		if p.Bytes > p.Total {
			t.Errorf("progress overshoot: %d > %d in phase %q", p.Bytes, p.Total, p.Phase)
		}
	}

	// Writing must not be the phase still in effect once verifying started.
	last := got[len(got)-1]
	if last.Phase != PhaseEjecting {
		t.Errorf("last phase = %q, want %q", last.Phase, PhaseEjecting)
	}
}

// TestParseWriteStderrClampsOvershoot covers dd's read-back reporting a final
// block that runs past the image size.
func TestParseWriteStderrClampsOvershoot(t *testing.T) {
	const size = 1000
	stream := "FERRY-PHASE verifying\n\r1048576 bytes (1.0 MB) copied, 1 s, 1 MB/s\n"

	var max int64
	parseWriteStderr(strings.NewReader(stream), size, func(p WriteProgress) {
		if p.Bytes > max {
			max = p.Bytes
		}
	})
	if max != size {
		t.Errorf("clamped progress = %d, want %d", max, size)
	}
}

func TestSHALineRegex(t *testing.T) {
	line := "SHA256 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	m := shaLine.FindStringSubmatch(line)
	if m == nil {
		t.Fatal("shaLine did not match a valid checksum line")
	}
	if len(m[1]) != 64 {
		t.Errorf("captured checksum length = %d, want 64", len(m[1]))
	}
	if shaLine.MatchString("SHA256 nothex") {
		t.Error("shaLine should not match non-hex content")
	}
}
