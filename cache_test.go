package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCachePath(t *testing.T) {
	tests := []struct {
		baseDir string
		urlPath string
		want    string
	}{
		{"/cache", "github.com/user/repo/@v/list", filepath.Join("/cache", "github.com", "user", "repo", "@v", "list")},
		{"/cache", "github.com/user/repo/@v/v1.0.0.info", filepath.Join("/cache", "github.com", "user", "repo", "@v", "v1.0.0.info")},
		{"./cache", "example.com/module/@v/v2.0.0.zip", filepath.Join(".", "cache", "example.com", "module", "@v", "v2.0.0.zip")},
	}
	for _, tt := range tests {
		got := cachePath(tt.baseDir, tt.urlPath)
		if got != tt.want {
			t.Errorf("cachePath(%q, %q) = %q, want %q", tt.baseDir, tt.urlPath, got, tt.want)
		}
	}
}

func TestWriteCacheAndReadCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "file.txt")
	data := []byte("hello world")

	if err := writeCache(path, data); err != nil {
		t.Fatalf("writeCache failed: %v", err)
	}

	got, err := readCache(path)
	if err != nil {
		t.Fatalf("readCache failed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("readCache = %q, want %q", got, data)
	}
}

func TestWriteCacheCreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "dir", "file.txt")
	data := []byte("data")

	if err := writeCache(path, data); err != nil {
		t.Fatalf("writeCache failed: %v", err)
	}

	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Errorf("directory was not created: %v", err)
	}
}

func TestWriteCacheAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atomic.txt")
	data := []byte("atomic write")

	if err := writeCache(path, data); err != nil {
		t.Fatalf("writeCache failed: %v", err)
	}

	// Temp file should not exist after rename
	tmpPath := path + ".tmp"
	if _, err := os.Stat(tmpPath); err == nil {
		t.Error("temp file should not exist after successful write")
	}

	got, err := readCache(path)
	if err != nil {
		t.Fatalf("readCache failed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("readCache = %q, want %q", got, data)
	}
}

func TestReadCacheNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.txt")

	_, err := readCache(path)
	if err == nil {
		t.Error("readCache should fail for nonexistent file")
	}
}

func TestWriteCacheEmptyData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")

	if err := writeCache(path, nil); err != nil {
		t.Fatalf("writeCache failed: %v", err)
	}
	got, err := readCache(path)
	if err != nil {
		t.Fatalf("readCache failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("readCache empty file = %q (len %d), want empty", got, len(got))
	}
}

func TestCacheExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exists.txt")
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	if !cacheExists(path) {
		t.Error("cacheExists should be true for existing file")
	}
	if cacheExists(filepath.Join(dir, "nonexistent.txt")) {
		t.Error("cacheExists should be false for nonexistent file")
	}
}
