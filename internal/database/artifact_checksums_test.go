package database

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

const (
	checksumPackagePURL = "pkg:maven/org.example/lib"
	checksumVersionPURL = "pkg:maven/org.example/lib@1.0"
	checksumFilename    = "lib-1.0.jar"
	checksumSHA1        = "da39a3ee5e6b4b0d3255bfef95601890afd80709"
	checksumMD5         = "d41d8cd98f00b204e9800998ecf8427e"
)

// otherContentHash is the SHA-256 of bytes other than those testContentHash
// describes.
var otherContentHash = strings.Repeat("ab", 32)

// upsertChecksumArtifact records a cached Maven jar holding testContentHash
// with the given upstream digests.
func upsertChecksumArtifact(t *testing.T, db *DB, storagePath, sha1, md5 string) {
	t.Helper()
	if err := db.UpsertPackage(&Package{PURL: checksumPackagePURL, Ecosystem: "maven", Name: "org.example:lib"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertVersion(&Version{PURL: checksumVersionPURL, PackagePURL: checksumPackagePURL}); err != nil {
		t.Fatal(err)
	}
	err := db.UpsertArtifact(&Artifact{
		VersionPURL: checksumVersionPURL,
		Filename:    checksumFilename,
		UpstreamURL: "https://repo1.maven.org/maven2/org/example/lib/1.0/" + checksumFilename,
		StoragePath: sql.NullString{String: storagePath, Valid: true},
		ContentHash: sql.NullString{String: testContentHash, Valid: true},
		Size:        sql.NullInt64{Int64: 0, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true},
		SHA1:        sql.NullString{String: sha1, Valid: sha1 != ""},
		MD5:         sql.NullString{String: md5, Valid: md5 != ""},
	})
	if err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}
}

// cachedChecksums returns the digests a cache hit would send, or "-" for
// both when the artifact is not cached.
func cachedChecksums(t *testing.T, db *DB) (sha1, md5 string) {
	t.Helper()
	cached, err := db.GetCachedArtifact(checksumPackagePURL, checksumVersionPURL, checksumFilename)
	if err != nil {
		t.Fatalf("GetCachedArtifact: %v", err)
	}
	if cached == nil {
		return "-", "-"
	}
	return cached.SHA1, cached.MD5
}

// A hit reads the recorded digests, and a refetch replaces them with what
// its response carried, none included, rather than keeping old ones.
func TestArtifactChecksumsRoundTrip(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertChecksumArtifact(t, db, "maven/a", checksumSHA1, checksumMD5)
		if sha1, md5 := cachedChecksums(t, db); sha1 != checksumSHA1 || md5 != checksumMD5 {
			t.Errorf("checksums = %q, %q; want %q, %q", sha1, md5, checksumSHA1, checksumMD5)
		}
		upsertChecksumArtifact(t, db, "maven/b", "", "")
		if sha1, md5 := cachedChecksums(t, db); sha1 != "" || md5 != "" {
			t.Errorf("checksums after refetch = %q, %q; want empty", sha1, md5)
		}
	})
}

// storedChecksums reads the digest columns directly, whether or not the
// artifact is cached.
func storedChecksums(t *testing.T, db *DB) (sha1, md5 sql.NullString) {
	t.Helper()
	row := db.QueryRow(db.Rebind(`SELECT sha1, md5 FROM artifacts WHERE version_purl = ? AND filename = ?`), checksumVersionPURL, checksumFilename)
	if err := row.Scan(&sha1, &md5); err != nil {
		t.Fatal(err)
	}
	return sha1, md5
}

// Every writer that changes or clears an artifact's bytes clears its
// digests, so they never outlive the bytes they describe.
func TestArtifactChecksumsClearedWithBytes(t *testing.T) {
	writers := map[string]func(t *testing.T, db *DB){
		"clear": func(t *testing.T, db *DB) {
			if cleared, err := db.ClearArtifactCache(checksumVersionPURL, checksumFilename, "maven/a"); err != nil || !cleared {
				t.Fatalf("ClearArtifactCache = %v, %v", cleared, err)
			}
		},
		"expire": func(t *testing.T, db *DB) {
			if cleared, err := db.ClearExpiredArtifact(checksumVersionPURL, checksumFilename, "maven/a", time.Now().Add(time.Hour)); err != nil || !cleared {
				t.Fatalf("ClearExpiredArtifact = %v, %v", cleared, err)
			}
		},
		"mark cached": func(t *testing.T, db *DB) {
			if err := db.MarkArtifactCached(checksumVersionPURL, checksumFilename, "maven/b", otherContentHash, 0, ""); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			runWithBothDatabases(t, func(t *testing.T, db *DB) {
				upsertChecksumArtifact(t, db, "maven/a", checksumSHA1, checksumMD5)
				write(t, db)
				if sha1, md5 := storedChecksums(t, db); sha1.Valid || md5.Valid {
					t.Errorf("checksums = %v, %v; want NULL", sha1, md5)
				}
			})
		})
	}
}

// TestArtifactChecksumsMigration upgrades a database from before migration
// 019 holding a cached artifact: the row is still served, with no digests,
// and new rows record them.
func TestArtifactChecksumsMigration(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertChecksumArtifact(t, db, "maven/a", "", "")
		for _, q := range []string{
			"ALTER TABLE artifacts DROP COLUMN sha1",
			"ALTER TABLE artifacts DROP COLUMN md5",
			"DELETE FROM migrations WHERE name = '025_add_artifact_checksums'",
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}

		if err := db.MigrateSchema(); err != nil {
			t.Fatalf("MigrateSchema: %v", err)
		}
		cached, err := db.GetCachedArtifact(checksumPackagePURL, checksumVersionPURL, checksumFilename)
		if err != nil || cached == nil {
			t.Fatalf("GetCachedArtifact after migration = %+v, %v", cached, err)
		}
		if cached.StoragePath != "maven/a" || cached.Artifact.Digest.Encoded() != testContentHash {
			t.Errorf("row after migration = %+v", cached)
		}
		if cached.SHA1 != "" || cached.MD5 != "" {
			t.Errorf("checksums after migration = %q, %q; want empty", cached.SHA1, cached.MD5)
		}

		upsertChecksumArtifact(t, db, "maven/b", checksumSHA1, checksumMD5)
		if sha1, md5 := cachedChecksums(t, db); sha1 != checksumSHA1 || md5 != checksumMD5 {
			t.Errorf("checksums after refetch = %q, %q; want %q, %q", sha1, md5, checksumSHA1, checksumMD5)
		}
	})
}

// SetArtifactChecksums fills a row without digests, but only for the bytes
// they were computed from, and never replaces digests a fetch recorded.
func TestSetArtifactChecksums(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		upsertChecksumArtifact(t, db, "maven/a", "", "")
		if err := db.SetArtifactChecksums(checksumVersionPURL, checksumFilename, otherContentHash, "bad", "bad"); err != nil {
			t.Fatal(err)
		}
		if sha1, md5 := cachedChecksums(t, db); sha1 != "" || md5 != "" {
			t.Errorf("other bytes: %q, %q; want none", sha1, md5)
		}
		if err := db.SetArtifactChecksums(checksumVersionPURL, checksumFilename, testContentHash, checksumSHA1, checksumMD5); err != nil {
			t.Fatal(err)
		}
		if sha1, md5 := cachedChecksums(t, db); sha1 != checksumSHA1 || md5 != checksumMD5 {
			t.Errorf("same bytes: %q, %q", sha1, md5)
		}
		if err := db.SetArtifactChecksums(checksumVersionPURL, checksumFilename, testContentHash, "bad", "bad"); err != nil {
			t.Fatal(err)
		}
		if sha1, _ := cachedChecksums(t, db); sha1 != checksumSHA1 {
			t.Errorf("recorded digests replaced with %q", sha1)
		}
	})
}
