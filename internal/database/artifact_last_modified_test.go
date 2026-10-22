package database

import (
	"database/sql"
	"testing"
	"time"
)

// last_modified belongs to the stored file: a refetch replaces it, and a
// refetch whose upstream sent none clears it rather than keeping a date that
// described other bytes.
func TestArtifactLastModifiedFollowsUpsert(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		const pkgPURL, versionPURL, filename = "pkg:maven/org.grails/grails-core", "pkg:maven/org.grails/grails-core@6.0.0", "grails-core-6.0.0.pom"
		if err := db.UpsertPackage(&Package{PURL: pkgPURL, Ecosystem: "maven", Name: "org.grails:grails-core"}); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
		if err := db.UpsertVersion(&Version{PURL: versionPURL, PackagePURL: pkgPURL}); err != nil {
			t.Fatalf("UpsertVersion: %v", err)
		}
		published := time.Date(2024, 3, 5, 10, 11, 12, 0, time.UTC)
		upsert := func(path string, lastModified sql.NullTime) {
			t.Helper()
			err := db.UpsertArtifact(&Artifact{
				VersionPURL:  versionPURL,
				Filename:     filename,
				UpstreamURL:  "https://repo.example/" + filename,
				StoragePath:  sql.NullString{String: path, Valid: true},
				ContentHash:  sql.NullString{String: "abc", Valid: true},
				Size:         sql.NullInt64{Int64: 10, Valid: true},
				FetchedAt:    sql.NullTime{Time: time.Now(), Valid: true},
				LastModified: lastModified,
			})
			if err != nil {
				t.Fatalf("UpsertArtifact: %v", err)
			}
		}

		upsert("maven/a", sql.NullTime{Time: published, Valid: true})
		got, err := db.GetCachedArtifact(pkgPURL, versionPURL, filename)
		if err != nil || got == nil || !got.LastModified.Valid || !got.LastModified.Time.Equal(published) {
			t.Fatalf("after fetch: %+v, %v; want last_modified %v", got, err, published)
		}

		upsert("maven/b", sql.NullTime{})
		got, err = db.GetCachedArtifact(pkgPURL, versionPURL, filename)
		if err != nil || got == nil || got.LastModified.Valid {
			t.Errorf("after undated refetch: %+v, %v; want no last_modified", got, err)
		}

		// Clearing the bytes clears their date with them.
		upsert("maven/c", sql.NullTime{Time: published, Valid: true})
		if cleared, err := db.ClearArtifactCache(versionPURL, filename, "maven/c"); err != nil || !cleared {
			t.Fatalf("ClearArtifactCache: %v, %v", cleared, err)
		}
		row, err := db.GetArtifact(versionPURL, filename)
		if err != nil || row == nil || row.LastModified.Valid {
			t.Errorf("after clear: %+v, %v; want no last_modified", row, err)
		}
	})
}

// A database from before the column gets it on upgrade, with existing rows
// left undated: they are served without Last-Modified, as before.
func TestMigrationAddsArtifactLastModified(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		if _, err := db.Exec(`ALTER TABLE artifacts DROP COLUMN last_modified`); err != nil {
			t.Fatalf("drop column: %v", err)
		}
		if _, err := db.Exec(`DELETE FROM migrations WHERE name = '027_add_artifact_last_modified'`); err != nil {
			t.Fatalf("forget migration: %v", err)
		}
		if _, err := db.Exec(db.Rebind(`INSERT INTO artifacts (version_purl, filename, upstream_url, storage_path, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`),
			"pkg:maven/g/a@1", "a-1.jar", "https://repo.example/a-1.jar", "maven/a-1.jar", time.Now(), time.Now()); err != nil {
			t.Fatalf("insert existing row: %v", err)
		}

		if err := db.MigrateSchema(); err != nil {
			t.Fatalf("MigrateSchema: %v", err)
		}
		got, err := db.GetArtifact("pkg:maven/g/a@1", "a-1.jar")
		if err != nil || got == nil || got.LastModified.Valid {
			t.Errorf("existing row after migration: %+v, %v; want it kept, undated", got, err)
		}
	})
}
