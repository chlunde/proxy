package database

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// benchCachedPackages is how many cached artifacts the hit lookup benchmark
// seeds, so the lookup runs against indexes of a realistic depth.
const benchCachedPackages = 10000

// BenchmarkGetCachedArtifact measures the cache hit lookup every artifact
// request makes before it opens the stored object. SQLite serves requests on
// a few reader connections, so this time is shared across all clients.
func BenchmarkGetCachedArtifact(b *testing.B) {
	db, err := Create(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	for i := range benchCachedPackages {
		name := fmt.Sprintf("pkg%d", i)
		packagePURL := "pkg:npm/" + name
		versionPURL := packagePURL + "@1.0.0"
		filename := name + "-1.0.0.tgz"
		if err := db.UpsertPackage(&Package{PURL: packagePURL, Ecosystem: "npm", Name: name}); err != nil {
			b.Fatalf("UpsertPackage failed: %v", err)
		}
		if err := db.UpsertVersion(&Version{
			PURL:        versionPURL,
			PackagePURL: packagePURL,
			Integrity:   sql.NullString{String: testIntegrity, Valid: true},
		}); err != nil {
			b.Fatalf("UpsertVersion failed: %v", err)
		}
		if err := db.UpsertArtifact(&Artifact{VersionPURL: versionPURL, Filename: filename}); err != nil {
			b.Fatalf("UpsertArtifact failed: %v", err)
		}
		if err := db.MarkArtifactCached(versionPURL, filename, "npm/"+filename,
			testContentHash, 1, "application/gzip"); err != nil {
			b.Fatalf("MarkArtifactCached failed: %v", err)
		}
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		name := fmt.Sprintf("pkg%d", i%benchCachedPackages)
		i++
		packagePURL := "pkg:npm/" + name
		cached, err := db.GetCachedArtifact(packagePURL, packagePURL+"@1.0.0", name+"-1.0.0.tgz")
		if err != nil || cached == nil {
			b.Fatalf("GetCachedArtifact() = %v, %v; want a hit", cached, err)
		}
	}
}

// BenchmarkGetMetadataCache measures the metadata cache row lookup every
// cached metadata request makes (npm packuments, Go .info/.mod/@v/list,
// PyPI simple pages, ...) when cache_metadata is on.
func BenchmarkGetMetadataCache(b *testing.B) {
	db, err := Create(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	for i := range benchCachedPackages {
		name := fmt.Sprintf("pkg%d", i)
		if err := db.UpsertMetadataCache(&MetadataCacheEntry{
			Ecosystem:   "npm",
			Name:        name,
			StoragePath: "_metadata/npm/" + name,
			FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
		}); err != nil {
			b.Fatalf("UpsertMetadataCache failed: %v", err)
		}
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		name := fmt.Sprintf("pkg%d", i%benchCachedPackages)
		i++
		entry, err := db.GetMetadataCache("npm", name)
		if err != nil || entry == nil {
			b.Fatalf("GetMetadataCache() = %v, %v; want a hit", entry, err)
		}
	}
}

// BenchmarkGetMetadataDate measures the date lookup a cached rewrite of
// metadata (npm, PyPI, ...) makes to set its Last-Modified.
func BenchmarkGetMetadataDate(b *testing.B) {
	db, err := Create(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	const proxyURL = "http://proxy.example"
	date := MetadataDate{ETag: `"etag"`, LastModified: time.Now()}
	for i := range benchCachedPackages {
		if _, err := db.InsertMetadataDate("npm", proxyURL, fmt.Sprintf("pkg%d", i), date); err != nil {
			b.Fatalf("InsertMetadataDate failed: %v", err)
		}
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		name := fmt.Sprintf("pkg%d", i%benchCachedPackages)
		i++
		got, err := db.GetMetadataDate("npm", proxyURL, name)
		if err != nil || got == nil {
			b.Fatalf("GetMetadataDate() = %v, %v; want a hit", got, err)
		}
	}
}

// BenchmarkGetVersionByPURL measures the publish time lookup an npm
// tarball request makes when cooldown is on.
func BenchmarkGetVersionByPURL(b *testing.B) {
	db, err := Create(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()

	for i := range benchCachedPackages {
		name := fmt.Sprintf("pkg%d", i)
		packagePURL := "pkg:npm/" + name
		if err := db.UpsertPackage(&Package{PURL: packagePURL, Ecosystem: "npm", Name: name}); err != nil {
			b.Fatalf("UpsertPackage failed: %v", err)
		}
		if err := db.UpsertVersion(&Version{
			PURL:        packagePURL + "@1.0.0",
			PackagePURL: packagePURL,
			PublishedAt: sql.NullTime{Time: time.Now(), Valid: true},
		}); err != nil {
			b.Fatalf("UpsertVersion failed: %v", err)
		}
	}

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		purl := fmt.Sprintf("pkg:npm/pkg%d@1.0.0", i%benchCachedPackages)
		i++
		v, err := db.GetVersionByPURL(purl)
		if err != nil || v == nil {
			b.Fatalf("GetVersionByPURL() = %v, %v; want a hit", v, err)
		}
	}
}
