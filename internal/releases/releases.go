// Package releases talks to the FyshOS GitHub releases and turns the published
// assets into the ISO images that Ferry knows how to write.
package releases

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ReleasesURL is the GitHub API endpoint listing FyshOS releases.
const ReleasesURL = "https://api.github.com/repos/FyshOS/fyshos/releases"

// Arch identifies a target CPU architecture that FyshOS ships images for.
type Arch string

const (
	ArchAMD64 Arch = "amd64" // 64-bit Intel/AMD
	ArchARM64 Arch = "arm64" // 64-bit ARM
	ArchI386  Arch = "i386"  // 32-bit Intel/AMD
)

// Arches lists the architectures Ferry offers, in display order.
var Arches = []Arch{ArchAMD64, ArchARM64, ArchI386}

// Label returns a human friendly name for the architecture.
func (a Arch) Label() string {
	switch a {
	case ArchAMD64:
		return "64-bit (amd64)"
	case ArchARM64:
		return "ARM 64-bit (arm64)"
	case ArchI386:
		return "32-bit (i386)"
	}
	return string(a)
}

// HostArch maps the running machine to a FyshOS architecture, defaulting to
// amd64 when the host is something we do not publish images for.
func HostArch() Arch {
	switch runtime.GOARCH {
	case "arm64":
		return ArchARM64
	case "386":
		return ArchI386
	default:
		return ArchAMD64
	}
}

// isoName matches only the current (2026 onwards) FyshOS image naming scheme:
//
//	FyshOS-2026-07-15_12.25-amd64.hybrid.iso
var isoName = regexp.MustCompile(`^FyshOS-(\d{4}-\d{2}-\d{2}_\d{2}\.\d{2})-(amd64|arm64|i386)\.hybrid\.iso$`)

// dateLayout matches the timestamp captured by isoName's first group.
const dateLayout = "2006-01-02_15.04"

// Image is a single downloadable FyshOS ISO.
type Image struct {
	Name    string    // asset filename, e.g. FyshOS-2026-07-15_12.25-amd64.hybrid.iso
	URL     string    // browser download URL
	Size    int64     // size in bytes
	Arch    Arch      // architecture the image targets
	Built   time.Time // build timestamp parsed from the filename
	Release string    // release tag the asset belongs to
}

// ghRelease and ghAsset mirror the subset of the GitHub releases API we use.
type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// parseImage returns the Image described by an asset, or ok=false when the
// asset is not a supported FyshOS ISO.
func parseImage(a ghAsset, release string) (Image, bool) {
	m := isoName.FindStringSubmatch(a.Name)
	if m == nil {
		return Image{}, false
	}
	built, err := time.Parse(dateLayout, m[1])
	if err != nil {
		return Image{}, false
	}
	return Image{
		Name:    a.Name,
		URL:     a.URL,
		Size:    a.Size,
		Arch:    Arch(m[2]),
		Built:   built,
		Release: release,
	}, true
}

// parseReleases turns the raw API payload into the supported images, newest
// first.
func parseReleases(data []byte) ([]Image, error) {
	var rels []ghRelease
	if err := json.Unmarshal(data, &rels); err != nil {
		return nil, fmt.Errorf("decoding releases: %w", err)
	}
	var images []Image
	for _, r := range rels {
		for _, a := range r.Assets {
			if img, ok := parseImage(a, r.TagName); ok {
				images = append(images, img)
			}
		}
	}
	sort.SliceStable(images, func(i, j int) bool {
		return images[i].Built.After(images[j].Built)
	})
	return images, nil
}

// Client fetches release metadata. The zero value is usable and talks to the
// public GitHub API.
type Client struct {
	HTTP *http.Client
	URL  string
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) url() string {
	if c.URL != "" {
		return c.URL
	}
	return ReleasesURL
}

// List returns every supported FyshOS image across all releases, newest first.
func (c *Client) List() ([]Image, error) {
	req, err := http.NewRequest(http.MethodGet, c.url(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching releases: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("reading releases: %w", err)
	}
	return parseReleases(body)
}

// FilterArch returns only the images for the given architecture, preserving
// order (newest first).
func FilterArch(images []Image, arch Arch) []Image {
	var out []Image
	for _, img := range images {
		if img.Arch == arch {
			out = append(out, img)
		}
	}
	return out
}

// Latest returns the newest image for the given architecture, or ok=false when
// none is published.
func Latest(images []Image, arch Arch) (Image, bool) {
	for _, img := range images {
		if img.Arch == arch {
			return img, true
		}
	}
	return Image{}, false
}

// DisplayName returns a friendly one-line description of an image.
func (i Image) DisplayName() string {
	return fmt.Sprintf("%s — %s", i.Built.Format("2 Jan 2006 15:04"), strings.ToUpper(string(i.Arch)))
}
