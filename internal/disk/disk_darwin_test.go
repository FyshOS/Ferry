//go:build darwin

package disk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// usbInfo is real `diskutil info` output for a USB stick, trimmed to the
// fields Enumerate consumes.
const usbInfo = `
   Device Identifier:         disk4
   Device Node:               /dev/disk4
   Whole:                     Yes
   Part of Whole:             disk4
   Device / Media Name:       SanDisk Ultra USB 3.0

   Volume Name:               Not applicable (no file system)
   Protocol:                  USB
   Disk Size:                 30.8 GB (30765219840 Bytes) (exactly 60088320 512-Byte-Units)
   Removable Media:           Removable
   Read-Only Media:           No
   Ejectable:                 Yes
`

func TestParseDiskutilInfoUSB(t *testing.T) {
	d, ok := parseDiskutilInfo([]byte(usbInfo))
	if !ok {
		t.Fatal("a removable USB disk should be an eligible target")
	}
	if d.Path != "/dev/disk4" {
		t.Errorf("Path = %q, want /dev/disk4", d.Path)
	}
	if d.Name != "disk4" {
		t.Errorf("Name = %q, want disk4", d.Name)
	}
	if d.Size != 30765219840 {
		t.Errorf("Size = %d, want 30765219840", d.Size)
	}
	if d.Model != "SanDisk Ultra USB 3.0" {
		t.Errorf("Model = %q", d.Model)
	}
	if d.Transport != "usb" {
		t.Errorf("Transport = %q, want usb", d.Transport)
	}
	if !d.Removable {
		t.Error("Removable = false, want true")
	}
}

// A disk image presents as external removable media, so the virtual flag is
// the only thing keeping Ferry from offering to overwrite one.
func TestParseDiskutilInfoRejectsVirtual(t *testing.T) {
	const img = `
   Device Node:               /dev/disk6
   Whole:                     Yes
   Device / Media Name:       Disk Image
   Protocol:                  Disk Image
   Disk Size:                 8.6 GB (8640300544 Bytes) (exactly 16875587 512-Byte-Units)
   Removable Media:           Removable
   Virtual:                   Yes
`
	if _, ok := parseDiskutilInfo([]byte(img)); ok {
		t.Error("a disk image must not be offered as a write target")
	}
}

// Writing to a partition rather than the whole disk would corrupt the media.
func TestParseDiskutilInfoRejectsPartition(t *testing.T) {
	const part = `
   Device Node:               /dev/disk4s1
   Whole:                     No
   Part of Whole:             disk4
   Disk Size:                 30.7 GB (30700000000 Bytes)
   Removable Media:           Removable
`
	if _, ok := parseDiskutilInfo([]byte(part)); ok {
		t.Error("a partition must not be offered as a write target")
	}
}

func TestParseDiskutilInfoRejectsReadOnly(t *testing.T) {
	const ro = `
   Device Node:               /dev/disk4
   Whole:                     Yes
   Disk Size:                 700.0 MB (700000000 Bytes)
   Read-Only Media:           Yes
   Removable Media:           Removable
`
	if _, ok := parseDiskutilInfo([]byte(ro)); ok {
		t.Error("read-only media must not be offered as a write target")
	}
}

func TestParseDiskutilList(t *testing.T) {
	const out = `/dev/disk4 (external, physical):
   #:                       TYPE NAME                    SIZE       IDENTIFIER
   0:     FDisk_partition_scheme                        *30.8 GB    disk4
   1:                 DOS_FAT_32 FYSHOS                  30.8 GB    disk4s1

/dev/disk9 (external, physical):
   #:                       TYPE NAME                    SIZE       IDENTIFIER
   0:      GUID_partition_scheme                        *1.0 TB     disk9
`
	got := parseDiskutilList([]byte(out))
	want := []string{"disk4", "disk9"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("id[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The identifiers in the indented body of `diskutil list` must not be mistaken
// for device lines.
func TestParseDiskutilListIgnoresBody(t *testing.T) {
	const out = `                                 Physical Store disk4s1
   1:                APFS Volume iOS Simulator          8.4 GB     disk5s1
`
	if got := parseDiskutilList([]byte(out)); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

func TestDiskMountpointsGrouping(t *testing.T) {
	// Exercised through the parsing helper shape rather than a live `mount`.
	const out = `/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)
devfs on /dev (devfs, local, nobrowse)
/dev/disk4s1 on /Volumes/FYSHOS (msdos, local, nodev, nosuid, noowners)
/dev/disk4s2 on /Volumes/DATA (msdos, local, nodev, nosuid, noowners)
map auto_home on /System/Volumes/Data/home (autofs, automounted, nobrowse)
`
	got := parseMountOutput([]byte(out))
	if len(got["disk4"]) != 2 {
		t.Fatalf("disk4 mountpoints = %v, want 2", got["disk4"])
	}
	if got["disk4"][0] != "/Volumes/FYSHOS" || got["disk4"][1] != "/Volumes/DATA" {
		t.Errorf("disk4 mountpoints = %v", got["disk4"])
	}
	if len(got["disk3"]) != 1 || got["disk3"][0] != "/" {
		t.Errorf("disk3 mountpoints = %v, want [/]", got["disk3"])
	}
	if _, ok := got["devfs"]; ok {
		t.Error("non-/dev/disk mounts must be ignored")
	}
}

func TestRawDevicePath(t *testing.T) {
	cases := map[string]string{
		"/dev/disk4":   "/dev/rdisk4",
		"/dev/disk20":  "/dev/rdisk20",
		"/dev/rdisk20": "/dev/rdisk20", // already raw
	}
	for in, want := range cases {
		if got := rawDevicePath(in); got != want {
			t.Errorf("rawDevicePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Raw devices reject writes that are not a whole number of blocks, so the
// padding arithmetic in Write has to round every short chunk up.
func TestBlockAlignmentPadding(t *testing.T) {
	for _, n := range []int{1, 511, 512, 4095, 4096, 4097, copyChunk - 1, copyChunk} {
		out := n
		if rem := out % devBlockAlign; rem != 0 {
			out += devBlockAlign - rem
		}
		if out%devBlockAlign != 0 {
			t.Errorf("n=%d padded to %d, not block aligned", n, out)
		}
		if out < n {
			t.Errorf("n=%d padded down to %d", n, out)
		}
		if out-n >= devBlockAlign {
			t.Errorf("n=%d over-padded to %d", n, out)
		}
		if out > copyChunk {
			t.Errorf("n=%d padded to %d, past the buffer", n, out)
		}
	}
}

// copyChunk is used as both the I/O size and the padding headroom, so it must
// itself be a whole number of blocks.
func TestCopyChunkIsBlockAligned(t *testing.T) {
	if copyChunk%devBlockAlign != 0 {
		t.Errorf("copyChunk %d is not a multiple of devBlockAlign %d", copyChunk, devBlockAlign)
	}
}

func TestWriteRejectsOversizedImage(t *testing.T) {
	dir := t.TempDir()
	iso := filepath.Join(dir, "big.iso")
	if err := os.WriteFile(iso, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Write(context.Background(), iso, Disk{Path: "/dev/disk99", Size: 1024}, nil)
	if err == nil || !strings.Contains(err.Error(), "too small") {
		t.Errorf("expected a too-small error, got %v", err)
	}
}

func TestWriteRejectsEmptyImage(t *testing.T) {
	dir := t.TempDir()
	iso := filepath.Join(dir, "empty.iso")
	if err := os.WriteFile(iso, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := Write(context.Background(), iso, Disk{Path: "/dev/disk99", Size: 1 << 20}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected an empty-image error, got %v", err)
	}
}

// A raw device is uncached and has no fsync, so macOS answers ENOTTY
// ("inappropriate ioctl for device"). Treating that as a failure would reject a
// write whose data already reached the media.
func TestBenignSyncError(t *testing.T) {
	benign := []error{
		&os.PathError{Op: "sync", Path: "/dev/rdisk20", Err: syscall.ENOTTY},
		&os.PathError{Op: "sync", Path: "/dev/rdisk20", Err: syscall.ENOTSUP},
		&os.PathError{Op: "sync", Path: "/dev/rdisk20", Err: syscall.EINVAL},
		syscall.ENOTTY,
	}
	for _, err := range benign {
		if !benignSyncError(err) {
			t.Errorf("benignSyncError(%v) = false, want true", err)
		}
	}

	// A real I/O failure must still surface.
	fatal := []error{
		&os.PathError{Op: "sync", Path: "/dev/rdisk20", Err: syscall.EIO},
		&os.PathError{Op: "sync", Path: "/dev/rdisk20", Err: syscall.ENOSPC},
		errors.New("something else"),
	}
	for _, err := range fatal {
		if benignSyncError(err) {
			t.Errorf("benignSyncError(%v) = true, want false", err)
		}
	}
}
