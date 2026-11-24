// Shared SQLite connection settings for the cache's persistent
// stores (AUD-REL-004).
//
// The cache opens its SQLite files with a pooled *sql.DB, so more
// than one connection can exist at once (a background flush goroutine
// and a synchronous write-through, two handles to the same file, or a
// reader during a write). Opened with no pragmas, a write that finds
// another writer holding the database returns SQLITE_BUSY
// ("database is locked") immediately instead of waiting. Every cache
// store therefore opens through cacheSQLiteDSN, which turns on WAL
// (a reader runs while a writer works; the writer does not block
// readers) and a 5s busy timeout (a contended writer waits for the
// lock instead of failing).
package cache

// cacheSQLiteDSN builds the SQLite connection string for the cache
// file at path with the pragmas every connection needs. They are
// supplied in the DSN rather than via a one-shot
// `db.Exec("PRAGMA ...")` because a PRAGMA executed through a pooled
// *sql.DB configures only whichever single connection served that
// call — every other connection the pool opens would run with the
// default journal mode and a zero busy timeout. In the DSN, every
// connection the driver opens starts in WAL mode with the busy
// timeout.
func cacheSQLiteDSN(path string) string {
	return path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}
