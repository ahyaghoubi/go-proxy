package main

import (
	"os"
	"path/filepath"
	"strings"
)

// cachePath converts a URL path to a safe filesystem path
func cachePath(baseDir, urlPath string) string {
	// Sanitize path for filesystem - replace / with OS-specific separator
	safePath := strings.ReplaceAll(urlPath, "/", string(filepath.Separator))
	return filepath.Join(baseDir, safePath)
}

// readCache reads data from the cache file
func readCache(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// writeCache writes data to cache atomically using temp file + rename
func writeCache(path string, data []byte) error {
	// Create directory if needed
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	// Write atomically using temp file
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// cacheExists checks if a cache file exists
func cacheExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

