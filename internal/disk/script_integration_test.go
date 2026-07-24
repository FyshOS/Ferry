//go:build linux

package disk

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestWriteScriptEndToEnd runs the real privileged script (minus pkexec) against
// ordinary files standing in for the image and the device, checking that it
// reports every phase, streams progress for both the write and the read-back,
// and produces a checksum matching the source.
func TestWriteScriptEndToEnd(t *testing.T) {
	if testing.Short() {
		// Runs real dd plus a full `sync`, and the eject fallback stalls when
		// the stand-in "device" is not a real block device.
		t.Skip("slow: exercises the real script")
	}
	dir := t.TempDir()
	iso := filepath.Join(dir, "test.iso")
	dev := filepath.Join(dir, "device.img")

	// A few MB so dd does real work across more than one block.
	data := make([]byte, 5<<20)
	rng := rand.New(rand.NewSource(1))
	rng.Read(data)
	if err := os.WriteFile(iso, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dev, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	size := int64(len(data))

	// mkdata "0": exercise the data-partition argument contract without asking
	// the script to partition a plain file standing in for a device.
	cmd := exec.Command("/bin/sh", "-c", writeScript,
		"sh", iso, dev, strconv.FormatInt(size, 10), "Data", "0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		parseWriteProgress(stderr, size, func(p WriteProgress) { seen[p.Phase] = true })
	}()

	var got string
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if m := shaLine.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil {
			got = m[1]
		}
	}
	<-done
	if err := cmd.Wait(); err != nil {
		t.Fatalf("script failed: %v", err)
	}

	if got != want {
		t.Errorf("script checksum = %q, want %q", got, want)
	}
	for _, phase := range []string{PhaseWriting, PhaseSyncing, PhaseVerifying, PhaseEjecting} {
		if !seen[phase] {
			t.Errorf("phase %q was never reported", phase)
		}
	}
}
