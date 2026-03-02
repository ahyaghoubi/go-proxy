package main

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS cache_usage (
	module_path TEXT PRIMARY KEY,
	last_used_at INTEGER NOT NULL
);`

// UsageStore tracks module access times and runs periodic cleanup.
type UsageStore struct {
	db       *sql.DB
	cacheDir string
	done     chan struct{}
	mu       sync.Mutex
}

// NewUsageStore creates a UsageStore and initializes the database.
func NewUsageStore(cacheDir string) (*UsageStore, error) {
	dbPath := filepath.Join(cacheDir, ".usage.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	// Configure SQLite for better concurrency: set a busy timeout and use WAL journal mode.
	if _, err = db.Exec(`PRAGMA busy_timeout = 5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		db.Close()
		return nil, err
	}

	_, err = db.Exec(schema)
	if err != nil {
		db.Close()
		return nil, err
	}

	return &UsageStore{
		db:       db,
		cacheDir: cacheDir,
		done:     make(chan struct{}),
	}, nil
}

// pathToModule extracts the module path from a request path.
// e.g. github.com/user/repo/@v/list -> github.com/user/repo
//      github.com/user/repo/@v/v1.0.0.info -> github.com/user/repo
func pathToModule(path string) string {
	if idx := strings.Index(path, "/@v/"); idx >= 0 {
		return path[:idx]
	}
	return path
}

// pathToModuleAndVersion extracts module and version from a path like github.com/foo/bar/@v/v1.0.0.zip
func pathToModuleAndVersion(path string) (module, version string) {
	module = pathToModule(path)
	if idx := strings.Index(path, "/@v/"); idx >= 0 {
		rest := path[idx+4:]
		if dot := strings.LastIndex(rest, "."); dot >= 0 {
			version = rest[:dot]
		}
	}
	return module, version
}

// RecordUsage updates the last-used timestamp for the module in the given path.
func (u *UsageStore) RecordUsage(path string) {
	module := pathToModule(path)
	if module == "" {
		return
	}

	now := time.Now().Unix()
	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		_, err := u.db.Exec(
			"INSERT OR REPLACE INTO cache_usage (module_path, last_used_at) VALUES (?, ?)",
			module, now,
		)
		if err == nil {
			return
		}
		// Best-effort handling of SQLITE_BUSY / "database is locked" errors.
		// We match on the error string to avoid depending on driver-specific error types.
		if !strings.Contains(err.Error(), "database is locked") {
			log.Printf("[WARN] Failed to record usage for %s: %v", module, err)
			return
		}
		if attempt == maxAttempts {
			log.Printf("[WARN] Failed to record usage for %s after %d attempts (database is locked): %v", module, maxAttempts, err)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// LoadStaleModules returns module paths not used within maxAge.
func (u *UsageStore) LoadStaleModules(maxAge time.Duration) ([]string, error) {
	cutoff := time.Now().Add(-maxAge).Unix()
	rows, err := u.db.Query(
		"SELECT module_path FROM cache_usage WHERE last_used_at < ?",
		cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var modules []string
	for rows.Next() {
		var module string
		if err := rows.Scan(&module); err != nil {
			return nil, err
		}
		modules = append(modules, module)
	}
	return modules, rows.Err()
}

// DeleteModule removes the module directory from cache and its row from the DB.
func (u *UsageStore) DeleteModule(modulePath string) error {
	dirPath := filepath.Join(u.cacheDir, modulePath)
	if err := os.RemoveAll(dirPath); err != nil {
		return err
	}
	_, err := u.db.Exec("DELETE FROM cache_usage WHERE module_path = ?", modulePath)
	return err
}

// RunCleanup runs a periodic cleanup job. It deletes modules unused for longer
// than maxAge, every interval. It stops when the context is cancelled or done is closed.
func (u *UsageStore) RunCleanup(maxAge, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-u.done:
			return
		case <-ticker.C:
			u.cleanupOnce(maxAge)
		}
	}
}

func (u *UsageStore) cleanupOnce(maxAge time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()

	modules, err := u.LoadStaleModules(maxAge)
	if err != nil {
		log.Printf("[WARN] Failed to load stale modules: %v", err)
		return
	}

	if len(modules) == 0 {
		return
	}

	var deleted int
	for _, module := range modules {
		if err := u.DeleteModule(module); err != nil {
			log.Printf("[WARN] Failed to delete module %s: %v", module, err)
			continue
		}
		deleted++
		log.Printf("[CLEANUP] Removed unused module %s (not used in %v)", module, maxAge)
	}
	log.Printf("[CLEANUP] Removed %d/%d stale modules", deleted, len(modules))
}

// Stop stops the cleanup goroutine.
func (u *UsageStore) Stop() {
	close(u.done)
}

// Close closes the database connection.
func (u *UsageStore) Close() error {
	return u.db.Close()
}
