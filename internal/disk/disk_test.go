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
// bulk, verifying most of the rest, the optional data-partition steps rest at
// their floor, and only a finished job reads 100%.
func TestWriteProgressFraction(t *testing.T) {
	const total = 1000
	cases := []struct {
		name string
		p    WriteProgress
		want float64
	}{
		{"write start", WriteProgress{Phase: PhaseWriting, Bytes: 0, Total: total}, 0},
		{"write half", WriteProgress{Phase: PhaseWriting, Bytes: 500, Total: total}, writeShare / 2},
		{"write done", WriteProgress{Phase: PhaseWriting, Bytes: total, Total: total}, writeShare},
		{"syncing", WriteProgress{Phase: PhaseSyncing, Bytes: 0, Total: total}, writeShare},
		{"verify start", WriteProgress{Phase: PhaseVerifying, Bytes: 0, Total: total}, writeShare},
		{"verify half", WriteProgress{Phase: PhaseVerifying, Bytes: 500, Total: total}, writeShare + verifyShare/2},
		{"verify done", WriteProgress{Phase: PhaseVerifying, Bytes: total, Total: total}, writeShare + verifyShare},
		{"partitioning", WriteProgress{Phase: PhasePartitioning, Total: total}, writeShare + verifyShare},
		{"formatting", WriteProgress{Phase: PhaseFormatting, Total: total}, writeShare + verifyShare + partShare},
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
		{Phase: PhasePartitioning, Bytes: 0, Total: total},
		{Phase: PhaseFormatting, Bytes: 0, Total: total},
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

func TestDataFreeBytes(t *testing.T) {
	const GiB = 1 << 30
	const MiB = 1 << 20
	cases := []struct {
		name             string
		dev, image, want int64
	}{
		{"ample aligned image", 16 * GiB, 2 * GiB, 16*GiB - 2*GiB - gptTailReserve},
		{"unaligned image rounds up", 16 * GiB, 2*GiB + 100, 16*GiB - (2*GiB + MiB) - gptTailReserve},
		{"just under threshold", 4 * GiB, 3*GiB + 900*MiB, 0},
		{"image fills device", 8 * GiB, 8 * GiB, 0},
		{"zero image", 16 * GiB, 0, 0},
		{"zero device", 0, 2 * GiB, 0},
	}
	for _, tc := range cases {
		if got := DataFreeBytes(tc.dev, tc.image); got != tc.want {
			t.Errorf("%s: DataFreeBytes(%d, %d) = %d, want %d", tc.name, tc.dev, tc.image, got, tc.want)
		}
	}
}

func TestDefaultDataLabel(t *testing.T) {
	if DefaultDataLabel != "Data" {
		t.Errorf("DefaultDataLabel = %q, want %q", DefaultDataLabel, "Data")
	}
}

func TestWriteOptionsLabel(t *testing.T) {
	if got := (WriteOptions{}).label(); got != DefaultDataLabel {
		t.Errorf("empty label = %q, want default %q", got, DefaultDataLabel)
	}
	if got := (WriteOptions{DataLabel: "Payload"}).label(); got != "Payload" {
		t.Errorf("label = %q, want %q", got, "Payload")
	}
}

func TestParseDataResult(t *testing.T) {
	cases := []struct {
		line       string
		wantOK     bool
		wantStatus DataStatus
		wantNode   string
		wantDetail string
	}{
		{"FERRY-DATA created /dev/sdb3 Data", true, DataCreated, "/dev/sdb3", "Data"},
		{"FERRY-DATA created /dev/disk4s3 My Files", true, DataCreated, "/dev/disk4s3", "My Files"},
		{"FERRY-DATA skipped not enough room", true, DataSkipped, "", "not enough room"},
		{"FERRY-DATA error sgdisk is not installed", true, DataFailed, "", "sgdisk is not installed"},
		{"SHA256 deadbeef", false, 0, "", ""},
		{"just some dd output", false, 0, "", ""},
	}
	for _, tc := range cases {
		status, node, detail, ok := parseDataResult(tc.line)
		if ok != tc.wantOK {
			t.Errorf("%q: ok = %v, want %v", tc.line, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if status != tc.wantStatus || node != tc.wantNode || detail != tc.wantDetail {
			t.Errorf("%q: got (%v, %q, %q), want (%v, %q, %q)",
				tc.line, status, node, detail, tc.wantStatus, tc.wantNode, tc.wantDetail)
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
