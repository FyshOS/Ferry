package disk

import "testing"

// lsblkSample has an internal NVMe system disk (must be excluded), an unmounted
// USB stick, and a mounted USB stick with two partitions.
const lsblkSample = `{
  "blockdevices": [
    {
      "name": "nvme0n1", "path": "/dev/nvme0n1", "size": 1024209543168, "type": "disk",
      "mountpoint": null, "rm": false, "hotplug": false, "tran": "nvme",
      "model": "Samsung SSD", "vendor": null,
      "children": [
        {"name": "nvme0n1p1", "path": "/dev/nvme0n1p1", "size": 272629760, "type": "part", "mountpoint": "/boot/efi", "rm": false, "hotplug": false, "tran": "nvme"}
      ]
    },
    {
      "name": "sdb", "path": "/dev/sdb", "size": 15718187008, "type": "disk",
      "mountpoint": null, "rm": true, "hotplug": true, "tran": "usb",
      "model": "Ultra Fit", "vendor": "SanDisk"
    },
    {
      "name": "sdc", "path": "/dev/sdc", "size": 8004304896, "type": "disk",
      "mountpoint": null, "rm": true, "hotplug": true, "tran": "usb",
      "model": "DataTraveler", "vendor": "Kingston",
      "children": [
        {"name": "sdc1", "path": "/dev/sdc1", "size": 4000000000, "type": "part", "mountpoint": "/run/media/andy/DATA"},
        {"name": "sdc2", "path": "/dev/sdc2", "size": 4000000000, "type": "part", "mountpoint": null}
      ]
    }
  ]
}`

func TestParseLsblkExcludesSystemDisk(t *testing.T) {
	disks, err := parseLsblk([]byte(lsblkSample))
	if err != nil {
		t.Fatalf("parseLsblk: %v", err)
	}
	if len(disks) != 2 {
		t.Fatalf("got %d disks, want 2 (usb only): %+v", len(disks), disks)
	}
	for _, d := range disks {
		if d.Name == "nvme0n1" {
			t.Errorf("system NVMe disk must not be offered as a target")
		}
	}
}

func TestParseLsblkMountState(t *testing.T) {
	disks, _ := parseLsblk([]byte(lsblkSample))
	byName := map[string]Disk{}
	for _, d := range disks {
		byName[d.Name] = d
	}

	sdb, ok := byName["sdb"]
	if !ok {
		t.Fatal("missing sdb")
	}
	if sdb.Mounted() {
		t.Errorf("sdb has no mounted partitions, want not mounted")
	}
	if sdb.Size != 15718187008 {
		t.Errorf("sdb size = %d", sdb.Size)
	}

	sdc, ok := byName["sdc"]
	if !ok {
		t.Fatal("missing sdc")
	}
	if !sdc.Mounted() {
		t.Errorf("sdc has a mounted partition, want mounted")
	}
	if len(sdc.Mountpoints) != 1 || sdc.Mountpoints[0] != "/run/media/andy/DATA" {
		t.Errorf("sdc mountpoints = %v, want one entry", sdc.Mountpoints)
	}
}

func TestDiskLabel(t *testing.T) {
	d := Disk{Path: "/dev/sdb", Model: "Ultra Fit", Vendor: "SanDisk", Size: 15718187008}
	label := d.Label()
	if label == "" {
		t.Fatal("empty label")
	}
	// Should carry the path, vendor/model and an availability marker.
	for _, want := range []string{"/dev/sdb", "SanDisk", "Ultra Fit", "available"} {
		if !contains(label, want) {
			t.Errorf("label %q missing %q", label, want)
		}
	}
}

// TestWriteProgressFraction pins the single-bar behaviour: writing fills the
// first two thirds, verifying the last third, and only a finished job reads
// 100%.
func TestWriteProgressFraction(t *testing.T) {
	const total = 1000
	cases := []struct {
		name string
		p    WriteProgress
		want float64
	}{
		{"write start", WriteProgress{Phase: PhaseWriting, Bytes: 0, Total: total}, 0},
		{"write half", WriteProgress{Phase: PhaseWriting, Bytes: 500, Total: total}, 1.0 / 3.0},
		{"write done", WriteProgress{Phase: PhaseWriting, Bytes: total, Total: total}, 2.0 / 3.0},
		{"syncing", WriteProgress{Phase: PhaseSyncing, Bytes: 0, Total: total}, 2.0 / 3.0},
		{"verify start", WriteProgress{Phase: PhaseVerifying, Bytes: 0, Total: total}, 2.0 / 3.0},
		{"verify half", WriteProgress{Phase: PhaseVerifying, Bytes: 500, Total: total}, 5.0 / 6.0},
		{"verify done", WriteProgress{Phase: PhaseVerifying, Bytes: total, Total: total}, 1},
		{"ejecting", WriteProgress{Phase: PhaseEjecting, Total: total}, 1},
		{"done", WriteProgress{Phase: PhaseDone, Bytes: total, Total: total}, 1},
	}
	for _, tc := range cases {
		got := tc.p.Fraction()
		if diff := got - tc.want; diff > 0.0001 || diff < -0.0001 {
			t.Errorf("%s: Fraction() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWriteProgressFractionNeverGoesBackwards walks a whole run and checks the
// bar only ever advances - the previous behaviour reset to 0% at each phase.
func TestWriteProgressFractionMonotonic(t *testing.T) {
	const total = 1000
	run := []WriteProgress{
		{Phase: PhaseWriting, Bytes: 0, Total: total},
		{Phase: PhaseWriting, Bytes: 400, Total: total},
		{Phase: PhaseWriting, Bytes: total, Total: total},
		{Phase: PhaseSyncing, Bytes: 0, Total: total},
		{Phase: PhaseVerifying, Bytes: 0, Total: total},
		{Phase: PhaseVerifying, Bytes: 600, Total: total},
		{Phase: PhaseVerifying, Bytes: total, Total: total},
		{Phase: PhaseEjecting, Bytes: 0, Total: total},
		{Phase: PhaseDone, Bytes: total, Total: total},
	}
	prev := -1.0
	for _, p := range run {
		f := p.Fraction()
		if f < prev {
			t.Errorf("progress went backwards at %q: %v after %v", p.Phase, f, prev)
		}
		if f < 0 || f > 1 {
			t.Errorf("fraction out of range at %q: %v", p.Phase, f)
		}
		prev = f
	}
	if prev != 1 {
		t.Errorf("final fraction = %v, want 1", prev)
	}
}

// Writing alone must never reach 100%, or the bar would claim to be finished
// before anything has been verified.
func TestWritingNeverReachesFull(t *testing.T) {
	p := WriteProgress{Phase: PhaseWriting, Bytes: 999999, Total: 1000}
	if f := p.Fraction(); f >= 1 {
		t.Errorf("writing reported %v; must stay below 1 until verified", f)
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[int64]string{
		512:         "512 B",
		1024:        "1.0 KiB",
		15718187008: "14.6 GiB",
		1206779904:  "1.1 GiB",
	}
	for in, want := range cases {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
