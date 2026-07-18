package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fyshos/ferry/internal/releases"
)

func newTestCache(t *testing.T) *Cache {
	t.Helper()
	c, err := NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewAt: %v", err)
	}
	return c
}

func TestDownloadAndHas(t *testing.T) {
	payload := []byte("PRETEND-ISO-CONTENTS-1234567890")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "31")
		w.Write(payload)
	}))
	defer srv.Close()

	c := newTestCache(t)
	img := releases.Image{
		Name: "FyshOS-2026-07-15_12.25-amd64.hybrid.iso",
		URL:  srv.URL,
		Size: int64(len(payload)),
	}

	if c.Has(img) {
		t.Fatal("cache should be empty before download")
	}

	var lastFrac float64
	path, err := c.Download(context.Background(), img, func(p Progress) {
		lastFrac = p.Fraction()
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if filepath.Base(path) != img.Name {
		t.Errorf("downloaded to %q, want name %q", path, img.Name)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("downloaded contents wrong: %q err=%v", got, err)
	}
	if lastFrac != 1.0 {
		t.Errorf("final progress fraction = %v, want 1.0", lastFrac)
	}
	if !c.Has(img) {
		t.Error("Has should be true after a complete download")
	}

	// No leftover .part file.
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Errorf(".part file should not remain")
	}
}

func TestHasRejectsWrongSize(t *testing.T) {
	c := newTestCache(t)
	img := releases.Image{Name: "FyshOS-2026-07-15_12.25-amd64.hybrid.iso", Size: 100}
	// Write a file of the wrong size.
	if err := os.WriteFile(c.PathFor(img), []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c.Has(img) {
		t.Error("Has should be false when the cached file size does not match")
	}
}

func TestListSortsAndFiltersISO(t *testing.T) {
	c := newTestCache(t)
	dir := c.Dir()
	writeFile(t, dir, "a.iso", "aaa")
	time.Sleep(10 * time.Millisecond)
	writeFile(t, dir, "b.iso", "bbbb")
	writeFile(t, dir, "notes.txt", "ignore me")

	entries, err := c.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 iso files: %+v", len(entries), entries)
	}
	// Newest (b.iso) first.
	if entries[0].Name != "b.iso" {
		t.Errorf("expected newest first, got %s", entries[0].Name)
	}
}

func TestRemoveRejectsTraversal(t *testing.T) {
	c := newTestCache(t)
	if err := c.Remove("../escape.iso"); err == nil {
		t.Error("Remove should reject names containing path separators")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
