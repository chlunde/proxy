package handler

import (
	"crypto/md5"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"strings"
)

// Maven resolvers verify a download against X-Checksum-SHA1/MD5 and then
// skip the GET of the .sha1 beside it. The proxy hashes each Maven file as
// it stores it and sends the digests with those bytes:
//
//	store ─► bodyDigests ─► fetchedArtifact.sums ─► artifacts.sha1, md5
//	              │ (miss)                              │ (hit, same query)
//	              └──────────► X-Checksum-* ◄───────────┘

// checksums are hex digests a response carries as X-Checksum-SHA1 and
// X-Checksum-MD5. An empty field is not sent.
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

// bodyDigests computes the SHA-1 and MD5 of a body read through tee.
type bodyDigests struct {
	sha1 hash.Hash
	md5  hash.Hash
}

// digestsFor returns the digests to compute while storing filename: for a
// Maven file other than a checksum or signature, nil otherwise. Other
// ecosystems' clients don't read them, so only Maven pays for the hashing.
func digestsFor(ecosystem, filename string) *bodyDigests {
	if ecosystem != "maven" || isMavenChecksumFile(filename) {
		return nil
	}
	return newBodyDigests()
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

// hashOnRead returns r hashing what is read through it. When r reaches EOF
// after exactly size bytes, done gets the digests. A seek, for a range,
// stops the hashing. It fills rows stored before digests were kept.
func hashOnRead(r io.ReadCloser, size int64, done func(checksums)) io.ReadCloser {
	h := &hashingReader{ReadCloser: r, digests: newBodyDigests(), left: size, done: done}
	if s, ok := r.(io.Seeker); ok {
		return &seekableHashingReader{hashingReader: h, seeker: s}
	}
	return h
}

type hashingReader struct {
	io.ReadCloser
	digests *bodyDigests // nil once done or seeked
	left    int64
	done    func(checksums)
}

func (r *hashingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if r.digests == nil {
		return n, err
	}
	_, _ = r.digests.Write(p[:n])
	r.left -= int64(n)
	if errors.Is(err, io.EOF) {
		if r.left == 0 {
			r.done(r.digests.result())
		}
		r.digests = nil
	}
	return n, err
}

type seekableHashingReader struct {
	*hashingReader
	seeker io.Seeker
}

func (r *seekableHashingReader) Seek(offset int64, whence int) (int64, error) {
	r.digests = nil
	return r.seeker.Seek(offset, whence)
}

// isMavenChecksumFile reports whether filename is a checksum or signature
// file, such as guava-33.4.0-jre.jar.sha1 or guava-33.4.0-jre.jar.asc.
func isMavenChecksumFile(filename string) bool {
	for _, ext := range []string{".sha1", ".sha256", ".sha512", ".md5", ".asc"} {
		if strings.HasSuffix(filename, ext) {
			return true
		}
	}
	return false
}

// nullString is s as a nullable column, NULL when empty.
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
