// Package cache stores downloaded FyshOS images on disk and downloads new ones.
package cache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fyne.io/fyne/v2"

	"github.com/fyshos/ferry/internal/releases"
)

// Cache manages a directory of downloaded ISO images.
type Cache struct {
	dir  string
	HTTP *http.Client
}

// New returns a Cache backed by the application's Fyne cache storage (added in
// Fyne 2.8).
//
// Fyne's Cache API is stream-based, but writing an image needs a real path to
// hand to the privileged `dd` helper, so we use the Cache().RootURI() and manage ourselves.
func New(app fyne.App) (*Cache, error) {
	root := app.Cache().RootURI()
	if root == nil {
		return nil, fmt.Errorf("no cache storage available")
	}
	dir := root.Path()
	if dir == "" {
		return nil, fmt.Errorf("cache storage %q is not a local directory", root)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Cache{dir: dir}, nil
}

// NewAt returns a Cache rooted at an explicit directory (used by tests).
func NewAt(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Cache{dir: dir}, nil
}

// Dir returns the cache directory.
func (c *Cache) Dir() string { return c.dir }

func (c *Cache) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 0} // no timeout: downloads are large
}

// Entry is a cached ISO file.
type Entry struct {
	Name    string    // filename
	Path    string    // absolute path on disk
	Size    int64     // size in bytes
	ModTime time.Time // last modified
}

// List returns the cached ISO files, newest (by mtime) first.
func (c *Cache) List() ([]Entry, error) {
	des, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(strings.ToLower(de.Name()), ".iso") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		entries = append(entries, Entry{
			Name:    de.Name(),
			Path:    filepath.Join(c.dir, de.Name()),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].ModTime.After(entries[j].ModTime)
	})
	return entries, nil
}

// PathFor returns the on-disk path an image would occupy in the cache.
func (c *Cache) PathFor(img releases.Image) string {
	return filepath.Join(c.dir, img.Name)
}

// Has reports whether the image is already fully downloaded.
func (c *Cache) Has(img releases.Image) bool {
	info, err := os.Stat(c.PathFor(img))
	if err != nil {
		return false
	}
	if img.Size > 0 && info.Size() != img.Size {
		return false
	}
	return true
}

// Progress reports download progress.
type Progress struct {
	Downloaded int64
	Total      int64
}

// Fraction returns progress in the range [0,1], or 0 when the total is unknown.
func (p Progress) Fraction() float64 {
	if p.Total <= 0 {
		return 0
	}
	return float64(p.Downloaded) / float64(p.Total)
}

// Download fetches the image into the cache, reporting progress via the optional callback.
// It returns the final on-disk path after completion and move into place.
func (c *Cache) Download(ctx context.Context, img releases.Image, onProgress func(Progress)) (string, error) {
	dest := c.PathFor(img)
	tmp := dest + ".part"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, img.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", img.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: unexpected status %s", img.Name, resp.Status)
	}

	total := img.Size
	if total <= 0 {
		total = resp.ContentLength
	}

	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}

	pw := &progressWriter{total: total, onProgress: onProgress}
	_, err = io.Copy(f, io.TeeReader(resp.Body, pw))
	closeErr := f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("downloading %s: %w", img.Name, err)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return "", closeErr
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dest, nil
}

// Remove deletes a cached file by name.
func (c *Cache) Remove(name string) error {
	// Guard against path traversal — only operate within the cache dir.
	if name != filepath.Base(name) {
		return fmt.Errorf("invalid cache entry %q", name)
	}
	return os.Remove(filepath.Join(c.dir, name))
}

type progressWriter struct {
	total      int64
	downloaded int64
	onProgress func(Progress)
	lastReport time.Time
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.downloaded += int64(n)
	if w.onProgress != nil {
		// Throttle callbacks so the UI is not swamped on fast links.
		now := time.Now()
		if now.Sub(w.lastReport) >= 100*time.Millisecond || w.downloaded == w.total {
			w.lastReport = now
			w.onProgress(Progress{Downloaded: w.downloaded, Total: w.total})
		}
	}
	return n, nil
}
