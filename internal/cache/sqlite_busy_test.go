package cache

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// AUD-REL-004: every persistent cache store must open its SQLite file
// with WAL + a busy timeout, on every pooled connection. Without the
// busy timeout a concurrent write fails immediately with SQLITE_BUSY
// ("database is locked") instead of waiting; without WAL a reader
// cannot run while a writer holds the database. The pragmas are set
// in the DSN so every connection gets them, not just whichever one a
// one-shot `db.Exec("PRAGMA ...")` happened to land on.
func TestCacheSQLiteStoresApplyBusyTimeoutAndWAL(t *testing.T) {
	opens := []struct {
		name string
		open func(path string) (*sql.DB, func(), error)
	}{
		{"exact", func(path string) (*sql.DB, func(), error) {
			p, err := OpenPersistentCache(path, 100)
			if err != nil {
				return nil, nil, err
			}
			return p.db, func() { _ = p.Close() }, nil
		}},
		{"semantic", func(path string) (*sql.DB, func(), error) {
			p, err := OpenPersistentSemanticCache(path, LocalSemanticCacheConfig{})
			if err != nil {
				return nil, nil, err
			}
			return p.db, func() { _ = p.Close() }, nil
		}},
		{"reasoning", func(path string) (*sql.DB, func(), error) {
			c, err := OpenPersistentReasoningCache(path, ReasoningCacheConfig{})
			if err != nil {
				return nil, nil, err
			}
			return c.persist.db, func() { _ = c.Close() }, nil
		}},
	}

	for _, tc := range opens {
		t.Run(tc.name, func(t *testing.T) {
			db, closeFn, err := tc.open(filepath.Join(t.TempDir(), "cache.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer closeFn()

			var bt int
			if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&bt); err != nil {
				t.Fatalf("PRAGMA busy_timeout: %v", err)
			}
			if bt <= 0 {
				t.Fatalf("busy_timeout = %d, want > 0 (AUD-REL-004)", bt)
			}

			var mode string
			if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
				t.Fatalf("PRAGMA journal_mode: %v", err)
			}
			if mode != "wal" {
				t.Fatalf("journal_mode = %q, want wal (AUD-REL-004)", mode)
			}
		})
	}
}

// AUD-REL-004 behavioural regression: with busy_timeout set, a write
// that finds another writer holding the database WAITS for the lock
// instead of failing. One handle holds a write transaction; a second
// handle's write must still be pending after 300ms (it is waiting),
// then succeed once the lock is released. Pre-fix (no busy_timeout)
// the second write returns immediately with SQLITE_BUSY.
func TestCacheSQLiteConcurrentWriteWaitsNotBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	p1, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	defer p1.Close()
	p2, err := OpenPersistentCache(path, 100)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer p2.Close()

	const insert = `INSERT INTO cache_entries(key, tenant, model, response, expires_unix)
		VALUES('waiter','','','{}', 9999999999)`

	// Hold the write lock on the first handle.
	tx, err := p1.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO cache_entries(key, tenant, model, response, expires_unix)
		VALUES('held','','','{}', 9999999999)`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("hold write: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, e := p2.db.Exec(insert)
		done <- e
	}()

	select {
	case e := <-done:
		_ = tx.Rollback()
		t.Fatalf("concurrent cache write returned while the lock was held (busy_timeout not applied): %v", e)
	case <-time.After(300 * time.Millisecond):
		// Still waiting on the lock: the busy timeout is doing its job.
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("concurrent cache write failed after the lock was released: %v", e)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("concurrent cache write did not complete after the lock was released")
	}
}
