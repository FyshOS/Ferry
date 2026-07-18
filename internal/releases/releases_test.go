package releases

import "testing"

// sample mirrors the shape of the real GitHub releases payload, mixing the
// current 2026 image names with legacy names and non-ISO assets.
const sample = `[
  {
    "tag_name": "testing",
    "assets": [
      {"name": "FyshOS-2026-07-15_12.25-amd64.hybrid.iso", "browser_download_url": "https://x/amd64-new.iso", "size": 1206779904},
      {"name": "FyshOS-2026-07-15_12.09-arm64.hybrid.iso", "browser_download_url": "https://x/arm64.iso", "size": 967229440},
      {"name": "FyshOS-2026-07-11_15.09-amd64.hybrid.iso", "browser_download_url": "https://x/amd64-old.iso", "size": 1206779904},
      {"name": "FyshOS-2026-07-15_12.25-i386.hybrid.iso", "browser_download_url": "https://x/i386.iso", "size": 929038336},
      {"name": "FyshOS-amd64-2025-10-27_21.40.hybrid.iso", "browser_download_url": "https://x/legacy.iso", "size": 1322811392},
      {"name": "checksums.txt", "browser_download_url": "https://x/checksums.txt", "size": 123}
    ]
  }
]`

func TestParseReleasesExcludesLegacyAndNonISO(t *testing.T) {
	imgs, err := parseReleases([]byte(sample))
	if err != nil {
		t.Fatalf("parseReleases: %v", err)
	}
	// 4 valid new-format ISOs; the legacy name and checksums.txt are dropped.
	if len(imgs) != 4 {
		t.Fatalf("got %d images, want 4: %+v", len(imgs), imgs)
	}
	for _, img := range imgs {
		if img.Name == "FyshOS-amd64-2025-10-27_21.40.hybrid.iso" {
			t.Errorf("legacy-format image should have been excluded")
		}
		if img.Name == "checksums.txt" {
			t.Errorf("non-iso asset should have been excluded")
		}
	}
}

func TestParseReleasesSortedNewestFirst(t *testing.T) {
	imgs, _ := parseReleases([]byte(sample))
	for i := 1; i < len(imgs); i++ {
		if imgs[i-1].Built.Before(imgs[i].Built) {
			t.Fatalf("images not sorted newest-first: %s before %s",
				imgs[i-1].Name, imgs[i].Name)
		}
	}
}

func TestLatestPerArch(t *testing.T) {
	imgs, _ := parseReleases([]byte(sample))

	amd, ok := Latest(imgs, ArchAMD64)
	if !ok {
		t.Fatal("expected an amd64 image")
	}
	if amd.Name != "FyshOS-2026-07-15_12.25-amd64.hybrid.iso" {
		t.Errorf("latest amd64 = %s, want the 07-15 build", amd.Name)
	}
	if amd.Arch != ArchAMD64 {
		t.Errorf("arch = %s, want amd64", amd.Arch)
	}

	arm, ok := Latest(imgs, ArchARM64)
	if !ok || arm.Arch != ArchARM64 {
		t.Errorf("expected an arm64 latest, got %+v ok=%v", arm, ok)
	}

	i386, ok := Latest(imgs, ArchI386)
	if !ok || i386.Arch != ArchI386 {
		t.Errorf("expected an i386 latest, got %+v ok=%v", i386, ok)
	}
}

func TestFilterArch(t *testing.T) {
	imgs, _ := parseReleases([]byte(sample))
	amd := FilterArch(imgs, ArchAMD64)
	if len(amd) != 2 {
		t.Fatalf("got %d amd64 images, want 2", len(amd))
	}
	for _, img := range amd {
		if img.Arch != ArchAMD64 {
			t.Errorf("FilterArch returned %s image", img.Arch)
		}
	}
}

func TestParseImageRejectsBadNames(t *testing.T) {
	bad := []string{
		"FyshOS-amd64-2023-01-06_14.10.hybrid.iso", // legacy arch-first layout
		"FyshOS-2026-07-15_12.25-riscv.hybrid.iso", // unsupported arch
		"FyshOS-2026-07-15-amd64.hybrid.iso",       // missing time component
		"ubuntu-24.04-amd64.iso",                   // unrelated
		"FyshOS-2026-07-15_12.25-amd64.iso",        // not .hybrid.iso
	}
	for _, name := range bad {
		if _, ok := parseImage(ghAsset{Name: name}, "testing"); ok {
			t.Errorf("parseImage accepted invalid name %q", name)
		}
	}
}

func TestHostArch(t *testing.T) {
	// Whatever the host is, it must map to one of the published arches.
	got := HostArch()
	found := false
	for _, a := range Arches {
		if a == got {
			found = true
		}
	}
	if !found {
		t.Errorf("HostArch returned unsupported arch %q", got)
	}
}
