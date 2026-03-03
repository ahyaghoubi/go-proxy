package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestPathToModule(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"", ""},
		{"/@v/list", ""},
		{"github.com/user/repo/@v/list", "github.com/user/repo"},
		{"github.com/user/repo/@v/v1.0.0.info", "github.com/user/repo"},
		{"github.com/user/repo/@v/v1.0.0.mod", "github.com/user/repo"},
		{"github.com/user/repo/@v/v1.0.0.zip", "github.com/user/repo"},
		{"example.com/pkg/@v/v2.3.4.info", "example.com/pkg"},
		{"no-at-v/here", "no-at-v/here"},
	}
	for _, tt := range tests {
		got := pathToModule(tt.path)
		if got != tt.want {
			t.Errorf("pathToModule(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestNewUsageStore(t *testing.T) {
	dir := t.TempDir()
	store, err := NewUsageStore(dir)
	if err != nil {
		t.Fatalf("NewUsageStore failed: %v", err)
	}
	defer store.Close()

	dbPath := filepath.Join(dir, ".usage.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("database file was not created: %v", err)
	}
}

func TestRecordUsage(t *testing.T) {
	dir := t.TempDir()
	store, err := NewUsageStore(dir)
	if err != nil {
		t.Fatalf("NewUsageStore failed: %v", err)
	}
	defer store.Close()

	store.RecordUsage("github.com/user/repo/@v/list")
	store.RecordUsage("github.com/user/repo/@v/v1.0.0.info")
	store.RecordUsage("example.com/other/@v/v2.0.0.mod")

	// Recently used modules should not be stale
	stale, err := store.LoadStaleModules(1 * time.Hour)
	if err != nil {
		t.Fatalf("LoadStaleModules failed: %v", err)
	}
	for _, s := range stale {
		if s == "github.com/user/repo" || s == "example.com/other" {
			t.Errorf("recently used module %q should not be stale", s)
		}
	}
}

func TestRecordUsageEmptyPath(t *testing.T) {
	dir := t.TempDir()
	store, err := NewUsageStore(dir)
	if err != nil {
		t.Fatalf("NewUsageStore failed: %v", err)
	}
	defer store.Close()

	// Should not panic
	store.RecordUsage("github.com/user/repo/@v/list")
}

func TestLoadStaleModules(t *testing.T) {
	dir := t.TempDir()
	store, err := NewUsageStore(dir)
	if err != nil {
		t.Fatalf("NewUsageStore failed: %v", err)
	}
	defer store.Close()

	store.RecordUsage("github.com/stale/module/@v/list")

	// With very short maxAge (e.g., 1ms), the module should not be stale yet due to timing
	// Use a negative maxAge to simulate "all time ago" - modules recorded now should not be stale
	// Actually: LoadStaleModules(maxAge) returns modules where last_used_at < now - maxAge
	// So if maxAge is 1 nanosecond, last_used_at (just now) might be >= cutoff. Let me try 1 hour -
	// the module we just recorded should NOT be in stale (it was just used).
	stale, err := store.LoadStaleModules(1 * time.Hour)
	if err != nil {
		t.Fatalf("LoadStaleModules failed: %v", err)
	}
	for _, s := range stale {
		if s == "github.com/stale/module" {
			t.Errorf("just-recorded module should not be stale yet")
		}
	}

	// With very long maxAge (100 years), nothing should be stale
	stale, err = store.LoadStaleModules(100 * 365 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("LoadStaleModules failed: %v", err)
	}
	for _, s := range stale {
		if s == "github.com/stale/module" {
			t.Errorf("with 100y maxAge, recently used module should not be stale")
		}
	}
}

func TestDeleteModule(t *testing.T) {
	dir := t.TempDir()
	store, err := NewUsageStore(dir)
	if err != nil {
		t.Fatalf("NewUsageStore failed: %v", err)
	}
	defer store.Close()

	// Create module dir
	moduleDir := filepath.Join(dir, "github.com", "user", "repo")
	if err := os.MkdirAll(moduleDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "file.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	store.RecordUsage("github.com/user/repo/@v/list")

	if err := store.DeleteModule("github.com/user/repo"); err != nil {
		t.Fatalf("DeleteModule failed: %v", err)
	}

	if _, err := os.Stat(moduleDir); err == nil {
		t.Error("module directory should be deleted")
	}

	// Should not appear in stale (row was deleted)
	stale, err := store.LoadStaleModules(0)
	if err != nil {
		t.Fatalf("LoadStaleModules failed: %v", err)
	}
	for _, s := range stale {
		if s == "github.com/user/repo" {
			t.Error("deleted module should not appear in stale list")
		}
	}
}

func TestUsageStoreStopAndClose(t *testing.T) {
	dir := t.TempDir()
	store, err := NewUsageStore(dir)
	if err != nil {
		t.Fatalf("NewUsageStore failed: %v", err)
	}

	// Start cleanup goroutine (will block on done)
	go store.RunCleanup(24*time.Hour, 24*time.Hour)

	store.Stop()
	if err := store.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}
}

func TestCleanupDeletesStaleModules(t *testing.T) {
	dir := t.TempDir()
	store, err := NewUsageStore(dir)
	if err != nil {
		t.Fatalf("NewUsageStore failed: %v", err)
	}
	defer store.Close()

	// Create module dir and record usage
	moduleDir := filepath.Join(dir, "github.com", "stale", "module")
	if err := os.MkdirAll(moduleDir, 0755); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage("github.com/stale/module/@v/list")

	// Manually set last_used_at to 2 hours ago in the DB
	dbPath := filepath.Join(dir, ".usage.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cutoff := time.Now().Add(-2 * time.Hour).Unix()
	_, err = db.Exec("UPDATE cache_usage SET last_used_at = ? WHERE module_path = ?", cutoff, "github.com/stale/module")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Run cleanup with maxAge 1 hour - module should be deleted
	go store.RunCleanup(1*time.Hour, 10*time.Millisecond)
	defer store.Stop()

	time.Sleep(50 * time.Millisecond) // Wait for first tick

	// Module dir should be deleted
	if _, err := os.Stat(moduleDir); err == nil {
		t.Error("stale module directory should be deleted by cleanup")
	}
}

func TestRecordUsageEmptyModule(t *testing.T) {
	dir := t.TempDir()
	store, err := NewUsageStore(dir)
	if err != nil {
		t.Fatalf("NewUsageStore failed: %v", err)
	}
	defer store.Close()

	// Path that yields empty module - should not panic
	store.RecordUsage("/@v/list")
}

func TestDeleteModuleNonexistentDir(t *testing.T) {
	dir := t.TempDir()
	store, err := NewUsageStore(dir)
	if err != nil {
		t.Fatalf("NewUsageStore failed: %v", err)
	}
	defer store.Close()

	store.RecordUsage("github.com/nonexistent/module/@v/list")

	// Delete module that has no dir on disk - DeleteModule should still remove DB row
	err = store.DeleteModule("github.com/nonexistent/module")
	if err != nil {
		t.Errorf("DeleteModule with nonexistent dir should succeed: %v", err)
	}

	stale, err := store.LoadStaleModules(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stale {
		if s == "github.com/nonexistent/module" {
			t.Error("deleted module should not appear in stale list")
		}
	}
}
