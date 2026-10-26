package handler

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"hash"
	"io"
	"net/http"

	"github.com/git-pkgs/registries/fetch"
	"github.com/opencontainers/go-digest"
)

// checksums are hex digests a response carries as X-Checksum-SHA1 and
// X-Checksum-MD5. Maven resolvers verify a download against them and then
// skip fetching the .sha1 beside it. An empty field is not sent.
type checksums struct {
	sha1 string
	md5  string
}

const (
	headerChecksumSHA1 = "X-Checksum-SHA1"
	headerChecksumMD5  = "X-Checksum-MD5"
)

// setHeaders sets the X-Checksum-* headers for the known digests.
func (c checksums) setHeaders(h http.Header) {
	if c.sha1 != "" {
		h.Set(headerChecksumSHA1, c.sha1)
	}
	if c.md5 != "" {
		h.Set(headerChecksumMD5, c.md5)
	}
}

// deleteChecksumHeaders removes the X-Checksum-* headers from a response
// that turned into an error, which has no body for them to describe.
func deleteChecksumHeaders(h http.Header) {
	h.Del(headerChecksumSHA1)
	h.Del(headerChecksumMD5)
}

// fetchHashed fetches url through fetchUpstream. For a Maven file other
// than a checksum or signature it also returns digests that hash the body
// as it is stored. Other ecosystems' clients don't read them, so only Maven
// pays for the hashing.
func (p *Proxy) fetchHashed(ctx context.Context, ecosystem, filename, url string, headers http.Header) (*fetch.Artifact, *bodyDigests, error) {
	artifact, err := p.fetchUpstream(ctx, url, headers)
	if err != nil || ecosystem != "maven" || isMavenChecksumFile(filename) {
		return artifact, nil, err
	}
	return artifact, newBodyDigests(), nil
}

// bodyDigests computes the SHA-1 and MD5 of a body read through tee.
type bodyDigests struct {
	sha1 hash.Hash
	md5  hash.Hash
}

func newBodyDigests() *bodyDigests {
	return &bodyDigests{sha1: sha1.New(), md5: md5.New()}
}

func (d *bodyDigests) Write(b []byte) (int, error) {
	d.sha1.Write(b)
	d.md5.Write(b)
	return len(b), nil
}

// tee hashes body into d as the store reads it, in the same pass; a nil d
// leaves body as is.
func (d *bodyDigests) tee(body io.Reader) io.Reader {
	if d == nil {
		return body
	}
	return io.TeeReader(body, d)
}

// result returns the computed digests, none when d is nil.
func (d *bodyDigests) result() checksums {
	if d == nil {
		return checksums{}
	}
	return checksums{sha1: hex.EncodeToString(d.sha1.Sum(nil)), md5: hex.EncodeToString(d.md5.Sum(nil))}
}

// nullString is s as a nullable column, NULL when empty.
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// recordedChecksums returns the digests recorded with the artifact
// just fetched, keyed by its content so a row another fetch rewrote is not
// used.
func (p *Proxy) recordedChecksums(ecosystem, name, version, filename string, stored digest.Digest) checksums {
	_, versionPURL, err := packagePURLStrings(ecosystem, name, version)
	if err != nil || stored == "" {
		return checksums{}
	}
	sha1, md5, err := p.DB.GetArtifactChecksums(versionPURL, filename, stored.Encoded())
	if err != nil {
		p.Logger.Warn("failed to read artifact checksums", "purl", versionPURL, "filename", filename, "error", err)
		return checksums{}
	}
	return checksums{sha1: sha1, md5: md5}
}
