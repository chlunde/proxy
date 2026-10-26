package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	preparedPackagePURL = "pkg:npm/lodash"
	preparedVersionPURL = "pkg:npm/lodash@4.17.21"
	preparedFilename    = "lodash-4.17.21.tgz"
	preparedProxyURL    = "http://proxy.example"

	// preparedLookups is how many lookups race to prepare one query.
	preparedLookups = 16
	// slowPrepare holds each prepare long enough that all racing lookups
	// miss the statement cache and prepare their own.
	slowPrepare = 10 * time.Millisecond
)

// countingDriver wraps the SQLite driver and counts driver-level prepares
// and the statements still open. Its connections hide the driver's direct
// query path, so database/sql prepares every query it runs, as the
// unprepared path does too.
type countingDriver struct {
	sqlite   driver.Driver
	dsn      string
	delay    time.Duration
	prepares atomic.Int64
	open     atomic.Int64
}

func (d *countingDriver) Connect(context.Context) (driver.Conn, error) {
	c, err := d.sqlite.Open(d.dsn)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: c, d: d}, nil
}

func (d *countingDriver) Driver() driver.Driver { return d.sqlite }

type countingConn struct {
	driver.Conn
	d *countingDriver
}

func (c *countingConn) Prepare(query string) (driver.Stmt, error) {
	c.d.prepares.Add(1)
	time.Sleep(c.d.delay)
	s, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	c.d.open.Add(1)
	return &countingStmt{Stmt: s, d: c.d}, nil
}

type countingStmt struct {
	driver.Stmt
	d *countingDriver
}

func (s *countingStmt) Close() error {
	s.d.open.Add(-1)
	return s.Stmt.Close()
}

// seedHits writes the rows every lookup in hitLookups finds.
func seedHits(t *testing.T, db *DB) {
	t.Helper()
	seedCachedArtifactTestData(t, db, preparedPackagePURL, preparedVersionPURL, preparedFilename)
	if err := db.MarkArtifactCached(preparedVersionPURL, preparedFilename, "npm/"+preparedFilename,
		testContentHash, 1, "application/gzip"); err != nil {
		t.Fatalf("MarkArtifactCached failed: %v", err)
	}
	if err := db.UpsertMetadataCache(&MetadataCacheEntry{
		Ecosystem: "npm", Name: "lodash", StoragePath: "_metadata/npm/lodash",
	}); err != nil {
		t.Fatalf("UpsertMetadataCache failed: %v", err)
	}
	date := MetadataDate{ETag: `"etag"`, LastModified: time.Now()}
	if _, err := db.InsertMetadataDate("npm", preparedProxyURL, "lodash", date); err != nil {
		t.Fatalf("InsertMetadataDate failed: %v", err)
	}
}

// hitLookups are the request path lookups SQLite runs prepared. Each
// fails unless it finds the row seedHits wrote.
var hitLookups = map[string]func(*DB) error{
	"GetCachedArtifact": func(db *DB) error {
		return found(db.GetCachedArtifact(preparedPackagePURL, preparedVersionPURL, preparedFilename))
	},
	"GetArtifact": func(db *DB) error {
		return found(db.GetArtifact(preparedVersionPURL, preparedFilename))
	},
	"GetMetadataCache": func(db *DB) error {
		return found(db.GetMetadataCache("npm", "lodash"))
	},
	"GetMetadataDate": func(db *DB) error {
		return found(db.GetMetadataDate("npm", preparedProxyURL, "lodash"))
	},
	"GetVersionByPURL": func(db *DB) error {
		return found(db.GetVersionByPURL(preparedVersionPURL))
	},
}

// found turns a lookup's miss into an error.
func found[T any](row *T, err error) error {
	if err != nil {
		return err
	}
	if row == nil {
		return errors.New("row not found")
	}
	return nil
}

// openCounting seeds a cached artifact, then reopens the database as
// dialect with its reader pool on a countingDriver of one connection.
func openCounting(t *testing.T, dialect Dialect, delay time.Duration) (*DB, *countingDriver) {
	t.Helper()
	seed := createTestDB(t)
	seedHits(t, seed)
	path := seed.Path()
	if err := seed.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	sqlite := sqlx.MustOpen("sqlite", path)
	d := &countingDriver{sqlite: sqlite.Driver(), dsn: path + sqliteReaderParams, delay: delay}
	_ = sqlite.Close()
	sqlDB := sql.OpenDB(d)
	sqlDB.SetMaxOpenConns(1)
	writer := sqlx.MustOpen("sqlite", path+sqliteWriterParams)
	writer.SetMaxOpenConns(1)
	db := &DB{DB: writer, reader: sqlx.NewDb(sqlDB, "sqlite"), dialect: dialect, path: path}
	t.Cleanup(func() { _ = db.Close() })
	return db, d
}

func lookupPrepared(t *testing.T, db *DB) {
	t.Helper()
	cached, err := db.GetCachedArtifact(preparedPackagePURL, preparedVersionPURL, preparedFilename)
	if err != nil {
		t.Errorf("GetCachedArtifact failed: %v", err)
		return
	}
	if cached == nil {
		t.Error("GetCachedArtifact missed a cached artifact")
	}
}

func TestPreparedLookupKeepsOneStatement(t *testing.T) {
	db, d := openCounting(t, DialectSQLite, slowPrepare)

	// Lookups racing to prepare the query keep one statement and close the
	// others.
	var wg sync.WaitGroup
	for range preparedLookups {
		wg.Go(func() { lookupPrepared(t, db) })
	}
	wg.Wait()
	if n := d.open.Load(); n != 1 {
		t.Fatalf("open statements after racing lookups = %d, want 1", n)
	}

	// Later lookups reuse it.
	prepares := d.prepares.Load()
	for range preparedLookups {
		lookupPrepared(t, db)
	}
	if n := d.prepares.Load() - prepares; n != 0 {
		t.Errorf("prepares for repeated lookups = %d, want 0", n)
	}

	db.closeStmts()
	if n := d.open.Load(); n != 0 {
		t.Errorf("open statements after closeStmts = %d, want 0", n)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if _, err := db.GetCachedArtifact(preparedPackagePURL, preparedVersionPURL, preparedFilename); err == nil {
		t.Error("GetCachedArtifact after Close succeeded, want an error")
	}
}

// TestPreparedHitsKeepOneStatement checks that each hit lookup prepares
// its query once on SQLite and keeps it.
func TestPreparedHitsKeepOneStatement(t *testing.T) {
	for name, lookup := range hitLookups {
		t.Run(name, func(t *testing.T) {
			db, d := openCounting(t, DialectSQLite, 0)
			for range preparedLookups {
				if err := lookup(db); err != nil {
					t.Fatalf("%s failed: %v", name, err)
				}
			}
			if n := d.prepares.Load(); n != 1 {
				t.Errorf("prepares = %d, want 1", n)
			}
			if n := d.open.Load(); n != 1 {
				t.Errorf("open statements = %d, want 1", n)
			}
		})
	}
}

// TestPostgresHitsNotPrepared checks that each hit lookup runs as text on
// Postgres: one prepare per lookup, none kept.
func TestPostgresHitsNotPrepared(t *testing.T) {
	for name, lookup := range hitLookups {
		t.Run(name, func(t *testing.T) {
			db, d := openCounting(t, DialectPostgres, 0)
			for range preparedLookups {
				if err := lookup(db); err != nil {
					t.Fatalf("%s failed: %v", name, err)
				}
			}
			if n := d.prepares.Load(); n != preparedLookups {
				t.Errorf("prepares = %d, want %d", n, preparedLookups)
			}
			if n := d.open.Load(); n != 0 {
				t.Errorf("open statements = %d, want 0", n)
			}
		})
	}
}

// TestPostgresKeepsNoPreparedStatement checks on a real server that the
// lookup leaves no named statement in the session: lib/pq's unnamed
// statements are not listed in pg_prepared_statements.
func TestPostgresKeepsNoPreparedStatement(t *testing.T) {
	db := createTestPostgresDB(t)
	if db == nil {
		t.Skip("PROXY_DATABASE_URL not set, skipping postgres test")
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	seedHits(t, db)

	for name, lookup := range hitLookups {
		for range preparedLookups {
			if err := lookup(db); err != nil {
				t.Fatalf("%s failed: %v", name, err)
			}
		}
	}
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM pg_prepared_statements"); err != nil {
		t.Fatalf("counting prepared statements: %v", err)
	}
	if n != 0 {
		t.Errorf("prepared statements in session = %d, want 0", n)
	}
}

// checkpointBusy writes a row, so the WAL has frames to checkpoint, and
// reports whether a TRUNCATE checkpoint was blocked by a reader.
func checkpointBusy(t *testing.T, db *DB) bool {
	t.Helper()
	if err := db.UpsertMetadataCache(&MetadataCacheEntry{
		Ecosystem: "npm", Name: "left-pad", StoragePath: "_metadata/npm/left-pad",
	}); err != nil {
		t.Fatalf("UpsertMetadataCache failed: %v", err)
	}
	var busy, log, done int
	if err := db.DB.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &done); err != nil {
		t.Fatalf("wal_checkpoint failed: %v", err)
	}
	return busy != 0
}

// A kept statement holds no read transaction between lookups, so it
// doesn't hold back WAL checkpoints. An open read does, which shows the
// check would notice.
func TestPreparedLookupAllowsCheckpoint(t *testing.T) {
	db := createTestDB(t)
	seedHits(t, db)
	for name, lookup := range hitLookups {
		if err := lookup(db); err != nil {
			t.Fatalf("%s failed: %v", name, err)
		}
	}
	if checkpointBusy(t, db) {
		t.Fatal("checkpoint blocked after prepared lookups")
	}

	// Fail the blocked checkpoint at once rather than after busy_timeout.
	if _, err := db.Exec("PRAGMA busy_timeout = 0"); err != nil {
		t.Fatalf("busy_timeout failed: %v", err)
	}
	rows, err := db.reader.Query("SELECT id FROM artifacts")
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if !rows.Next() {
		t.Fatal("no artifact row")
	}
	if !checkpointBusy(t, db) {
		t.Error("checkpoint not blocked by an open read; the check proves nothing")
	}
	_ = rows.Close()
}
