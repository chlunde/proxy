package handler

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

const (
	checksumTestJar  = "jar bytes"
	checksumTestSHA1 = "01c56e3ae46c962debe4976038d5ba38d1e61ef7" // sha1("jar bytes")
	checksumTestMD5  = "b0136f9ebbab9987c4580ebb0e34bb64"         // md5("jar bytes")
)

// checksumUpstream serves files from a path->body map, with headers from
// a path->headers map, and counts requests per path.
type checksumUpstream struct {
	mu       sync.Mutex
	files    map[string]string
	headers  map[string]http.Header
	requests map[string]int
}

func (u *checksumUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests[r.URL.Path]++
	body, ok := u.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	for name, values := range u.headers[r.URL.Path] {
		w.Header()[name] = values
	}
	_, _ = io.WriteString(w, body)
}

func (u *checksumUpstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[path]
}

// newChecksumHandler serves the default repository from files.
func newChecksumHandler(t *testing.T, files map[string]string) (http.Handler, *checksumUpstream, *Proxy, *mockStorage) {
	t.Helper()
	upstream := &checksumUpstream{files: files, requests: map[string]int{}}
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	proxy.HTTPClient = srv.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	return NewMavenHandler(proxy, "http://localhost", srv.URL, srv.URL+"/portal").Routes(), upstream, proxy, store
}

func requestMaven(h http.Handler, method, path string, header ...string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	h.ServeHTTP(w, r)
	return w
}

func assertChecksumHeaders(t *testing.T, step string, w *httptest.ResponseRecorder, wantSHA1, wantMD5 string) {
	t.Helper()
	if got := w.Header().Get("X-Checksum-SHA1"); got != wantSHA1 {
		t.Errorf("%s: X-Checksum-SHA1 = %q, want %q", step, got, wantSHA1)
	}
	if got := w.Header().Get("X-Checksum-MD5"); got != wantMD5 {
		t.Errorf("%s: X-Checksum-MD5 = %q, want %q", step, got, wantMD5)
	}
}

// With stream_artifacts nothing is read from the cache, so no header is
// sent, and the streamed response, which has no digest, is served intact.
func TestMavenChecksumHeaderStreamArtifacts(t *testing.T) {
	h, upstream, proxy, _ := newChecksumHandler(t, map[string]string{
		mavenJarPath:           checksumTestJar,
		mavenJarPath + ".sha1": checksumTestSHA1,
	})
	upstream.headers = map[string]http.Header{mavenJarPath: centralHeaders(checksumTestSHA1, checksumTestMD5)}
	requestMaven(h, http.MethodGet, mavenJarPath+".sha1")
	proxy.StreamArtifacts = true
	w := requestMaven(h, http.MethodGet, mavenJarPath)
	if w.Code != http.StatusOK || w.Body.String() != checksumTestJar {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body)
	}
	assertChecksumHeaders(t, "stream", w, "", "")
	if n := upstream.count(mavenJarPath + ".sha1"); n != 1 {
		t.Errorf("upstream .sha1 requests = %d, want 1", n)
	}
}

// failingSeeker fails every seek, as a broken storage reader would.
type failingSeeker struct{ io.Reader }

func (failingSeeker) Seek(int64, int) (int64, error) { return 0, io.ErrUnexpectedEOF }
func (failingSeeker) Close() error                   { return nil }

// A range response that fails to seek drops the checksum headers with the
// other headers that describe the body.
func TestServeArtifactSeekErrorDropsChecksums(t *testing.T) {
	result := &CacheResult{
		Reader:    failingSeeker{strings.NewReader(checksumTestJar)},
		Artifact:  testArtifact(checksumTestJar, "pkg:maven/org.grails/grails-core@6.0.0", "grails-core-6.0.0.jar", "application/java-archive"),
		checksums: checksums{sha1: checksumTestSHA1, md5: checksumTestMD5},
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Range", "bytes=0-2")
	ServeArtifactRequest(w, r, result)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	assertChecksumHeaders(t, "seek error", w, "", "")
}

// centralHeaders are the digest headers Maven Central sends with a file,
// lowercase as over HTTP/2.
func centralHeaders(sha1, md5 string) http.Header {
	return http.Header{"x-checksum-sha1": {sha1}, "x-checksum-md5": {md5}}
}

// Concurrent cold requests sharing one upstream fetch all get the digests.
func TestMavenChecksumHeadersCoalescedMisses(t *testing.T) {
	h, upstream, _, _ := newChecksumHandler(t, map[string]string{mavenJarPath: checksumTestJar})
	upstream.headers = map[string]http.Header{mavenJarPath: centralHeaders(checksumTestSHA1, checksumTestMD5)}
	responses := make([]*httptest.ResponseRecorder, 5)
	var wg sync.WaitGroup
	for i := range responses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses[i] = requestMaven(h, http.MethodGet, mavenJarPath)
		}()
	}
	wg.Wait()
	for _, w := range responses {
		assertChecksumHeaders(t, "client", w, checksumTestSHA1, checksumTestMD5)
	}
}

// Only Maven files are hashed; an npm tarball is stored without digests.
func TestChecksumsOnlyForMaven(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Checksum-Sha1", checksumTestSHA1)
		_, _ = io.WriteString(w, checksumTestJar)
	}))
	t.Cleanup(srv.Close)
	proxy, db, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })

	for ecosystem, want := range map[string]bool{"npm": false, "maven": true} {
		result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), ecosystem, "lib", "1.0.0", "lib-1.0.0.tgz", srv.URL+"/"+ecosystem)
		if err != nil {
			t.Fatalf("%s: %v", ecosystem, err)
		}
		_ = result.Reader.Close()
		var sha1 sql.NullString
		row := db.QueryRow(db.Rebind(`SELECT sha1 FROM artifacts WHERE upstream_url = ?`), srv.URL+"/"+ecosystem)
		if err := row.Scan(&sha1); err != nil {
			t.Fatalf("%s: %v", ecosystem, err)
		}
		if sha1.Valid != want {
			t.Errorf("%s: sha1 = %v, want recorded = %v", ecosystem, sha1, want)
		}
	}
}

// With stream_artifacts digests recorded earlier are not used either: the
// streamed file gets no headers and its .sha1 is fetched, as every cached
// entry is ignored in that mode.
func TestMavenChecksumRecordedIgnoredWhenStreaming(t *testing.T) {
	h, upstream, proxy, _ := newChecksumHandler(t, map[string]string{
		mavenJarPath:           checksumTestJar,
		mavenJarPath + ".sha1": checksumTestSHA1,
	})
	upstream.headers = map[string]http.Header{mavenJarPath: centralHeaders(checksumTestSHA1, checksumTestMD5)}
	requestMaven(h, http.MethodGet, mavenJarPath)
	proxy.StreamArtifacts = true
	assertChecksumHeaders(t, "stream", requestMaven(h, http.MethodGet, mavenJarPath), "", "")
	requestMaven(h, http.MethodGet, mavenJarPath+".sha1")
	if n := upstream.count(mavenJarPath + ".sha1"); n != 1 {
		t.Errorf("upstream .sha1 requests = %d, want 1", n)
	}
}

// rowChecksums reads the digest columns of a cached Maven file directly.
func rowChecksums(t *testing.T, proxy *Proxy, filename string) (sha1, md5 sql.NullString) {
	t.Helper()
	row := proxy.DB.QueryRow(proxy.DB.Rebind(`SELECT sha1, md5 FROM artifacts WHERE filename = ?`), filename)
	if err := row.Scan(&sha1, &md5); err != nil {
		t.Fatal(err)
	}
	return sha1, md5
}

// clearRowChecksums makes a row look cached before migration 019.
func clearRowChecksums(t *testing.T, proxy *Proxy) {
	t.Helper()
	if _, err := proxy.DB.Exec(`UPDATE artifacts SET sha1 = NULL, md5 = NULL`); err != nil {
		t.Fatal(err)
	}
}

// opens is how often the handler opened storage so far.
func (s *mockStorage) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

// Without upstream headers the digests are computed as the file is stored,
// and sent on GET, HEAD and a range; a 416 carries none.
func TestMavenChecksumComputedWithoutHeaders(t *testing.T) {
	h, upstream, _, _ := newChecksumHandler(t, map[string]string{mavenJarPath: checksumTestJar})
	for _, step := range []struct {
		name, method string
		header       []string
		status       int
	}{
		{"cold", http.MethodGet, nil, http.StatusOK},
		{"hot", http.MethodGet, nil, http.StatusOK},
		{"head", http.MethodHead, nil, http.StatusOK},
		{"range", http.MethodGet, []string{"Range", "bytes=0-2"}, http.StatusPartialContent},
	} {
		w := requestMaven(h, step.method, mavenJarPath, step.header...)
		if w.Code != step.status {
			t.Fatalf("%s: status = %d, want %d", step.name, w.Code, step.status)
		}
		assertChecksumHeaders(t, step.name, w, checksumTestSHA1, checksumTestMD5)
	}
	w := requestMaven(h, http.MethodGet, mavenJarPath, "Range", "bytes=100-")
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unsatisfiable range: status = %d", w.Code)
	}
	assertChecksumHeaders(t, "unsatisfiable range", w, "", "")
	if n := upstream.count(mavenJarPath); n != 1 {
		t.Errorf("upstream jar requests = %d, want 1", n)
	}
}

// A file only the Gradle Plugin Portal has is stored with digests too.
func TestMavenChecksumPortalFallback(t *testing.T) {
	h, upstream, proxy, _ := newChecksumHandler(t, map[string]string{"/portal" + mavenJarPath: checksumTestJar})
	assertChecksumHeaders(t, "cold", requestMaven(h, http.MethodGet, mavenJarPath), checksumTestSHA1, checksumTestMD5)
	if sha1, md5 := rowChecksums(t, proxy, "grails-core-6.0.0.jar"); sha1.String != checksumTestSHA1 || md5.String != checksumTestMD5 {
		t.Errorf("row = %v, %v", sha1, md5)
	}
	assertChecksumHeaders(t, "hot", requestMaven(h, http.MethodGet, mavenJarPath), checksumTestSHA1, checksumTestMD5)
	if n := upstream.count("/portal" + mavenJarPath); n != 1 {
		t.Errorf("portal jar requests = %d, want 1", n)
	}
}

// A named repository's files, cached under its prefix, carry digests too.
func TestMavenChecksumNamedRepository(t *testing.T) {
	upstream := &checksumUpstream{files: map[string]string{"/corp" + mavenJarPath: checksumTestJar}, requests: map[string]int{}}
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewMavenHandler(proxy, "http://localhost", srv.URL, srv.URL+"/portal").
		WithRepositories(map[string]string{"corp": srv.URL + "/corp"}).Routes()

	assertChecksumHeaders(t, "cold", requestMaven(h, http.MethodGet, "/corp"+mavenJarPath), checksumTestSHA1, checksumTestMD5)
	assertChecksumHeaders(t, "hot", requestMaven(h, http.MethodGet, "/corp"+mavenJarPath), checksumTestSHA1, checksumTestMD5)
	if n := upstream.count("/corp" + mavenJarPath); n != 1 {
		t.Errorf("named jar requests = %d, want 1", n)
	}
}

// A row without digests, cached before migration 019, is served as before:
// no headers, and the file is not read again to hash it.
func TestMavenChecksumOldRowSendsNone(t *testing.T) {
	h, _, proxy, store := newChecksumHandler(t, map[string]string{mavenJarPath: checksumTestJar})
	requestMaven(h, http.MethodGet, mavenJarPath)
	clearRowChecksums(t, proxy)
	before := store.openCount()
	w := requestMaven(h, http.MethodGet, mavenJarPath)
	if w.Code != http.StatusOK || w.Body.String() != checksumTestJar {
		t.Fatalf("hit: status = %d, body = %q", w.Code, w.Body)
	}
	assertChecksumHeaders(t, "hit", w, "", "")
	if n := store.openCount() - before; n != 1 {
		t.Errorf("storage opens on hit = %d, want 1", n)
	}
}

// A HEAD hit, answered from the row without opening storage, carries the
// row's digests.
func TestMavenChecksumServingHead(t *testing.T) {
	h, _, proxy, store := newChecksumHandler(t, map[string]string{mavenJarPath: checksumTestJar})
	requestMaven(h, http.MethodGet, mavenJarPath)
	head := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodHead, mavenJarPath, nil)
		h.ServeHTTP(w, r.WithContext(ServingHead(r.Context())))
		return w
	}
	before := store.openCount()
	assertChecksumHeaders(t, "head", head(), checksumTestSHA1, checksumTestMD5)
	clearRowChecksums(t, proxy)
	assertChecksumHeaders(t, "head, no digests", head(), "", "")
	if n := store.openCount() - before; n != 0 {
		t.Errorf("storage opens on HEAD = %d, want 0", n)
	}
}

// Checksum files themselves get no digest columns.
func TestMavenChecksumNotForChecksumFiles(t *testing.T) {
	h, _, proxy, _ := newChecksumHandler(t, map[string]string{mavenJarPath + ".sha1": checksumTestSHA1})
	requestMaven(h, http.MethodGet, mavenJarPath+".sha1")
	if sha1, md5 := rowChecksums(t, proxy, "grails-core-6.0.0.jar.sha1"); sha1.Valid || md5.Valid {
		t.Errorf("row = %v, %v; want NULL", sha1, md5)
	}
}

// requestMavenStreamed marks the request as the server marks a GET, so a
// cache miss is sent while it is stored.
func requestMavenStreamed(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	ctx := DetachUpstreamFetch(StreamCacheMiss(r.Context()))
	h.ServeHTTP(w, r.WithContext(ctx))
	return w
}

// A streamed miss sends its headers before the proxy has hashed the bytes,
// so it passes on the digests upstream sent; hits send the stored ones.
// Either way the client skips the .sha1.
func TestMavenChecksumStreamedMissPassesUpstream(t *testing.T) {
	h, upstream, _, _ := newChecksumHandler(t, map[string]string{mavenJarPath: checksumTestJar})
	upstream.headers = map[string]http.Header{mavenJarPath: centralHeaders(checksumTestSHA1, checksumTestMD5)}
	w := requestMavenStreamed(h, mavenJarPath)
	if w.Code != http.StatusOK || w.Body.String() != checksumTestJar {
		t.Fatalf("miss: status = %d, body = %q", w.Code, w.Body)
	}
	assertChecksumHeaders(t, "miss", w, checksumTestSHA1, checksumTestMD5)
	waitChecksumRow(t, h)
	assertChecksumHeaders(t, "hit", requestMavenStreamed(h, mavenJarPath), checksumTestSHA1, checksumTestMD5)
	if n := upstream.count(mavenJarPath); n != 1 {
		t.Errorf("upstream jar requests = %d, want 1", n)
	}
}

// Without upstream digests a streamed miss has none to send; the hit
// sends those computed when it was stored.
func TestMavenChecksumStreamedMissWithoutHeaders(t *testing.T) {
	h, _, _, _ := newChecksumHandler(t, map[string]string{mavenJarPath: checksumTestJar})
	assertChecksumHeaders(t, "miss", requestMavenStreamed(h, mavenJarPath), "", "")
	waitChecksumRow(t, h)
	assertChecksumHeaders(t, "hit", requestMavenStreamed(h, mavenJarPath), checksumTestSHA1, checksumTestMD5)
}

// A body that doesn't match upstream's SHA-1 is aborted before its last
// bytes and not cached.
func TestMavenChecksumStreamedMissMismatch(t *testing.T) {
	h, upstream, _, _ := newChecksumHandler(t, map[string]string{mavenJarPath: checksumTestJar})
	upstream.headers = map[string]http.Header{mavenJarPath: centralHeaders(strings.Repeat("0", 40), checksumTestMD5)}
	w, aborted := serveRecovering(h, mavenJarPath)
	if !aborted && w.Body.String() == checksumTestJar {
		t.Fatal("client got every byte of a file that doesn't match upstream's SHA-1")
	}
	time.Sleep(100 * time.Millisecond)
	serveRecovering(h, mavenJarPath)
	if n := upstream.count(mavenJarPath); n != 2 {
		t.Errorf("upstream jar requests = %d, want 2 (mismatch not cached)", n)
	}
}

// A buffered miss whose bytes don't match upstream's SHA-1 fails and is
// not cached.
func TestMavenChecksumMismatchRejected(t *testing.T) {
	h, upstream, _, _ := newChecksumHandler(t, map[string]string{mavenJarPath: checksumTestJar})
	upstream.headers = map[string]http.Header{mavenJarPath: centralHeaders(strings.Repeat("0", 40), checksumTestMD5)}
	if w := requestMaven(h, http.MethodGet, mavenJarPath); w.Code == http.StatusOK {
		t.Fatalf("status = %d, want an error", w.Code)
	}
	requestMaven(h, http.MethodGet, mavenJarPath)
	if n := upstream.count(mavenJarPath); n != 2 {
		t.Errorf("upstream jar requests = %d, want 2 (mismatch not cached)", n)
	}
}

// A malformed upstream digest is neither passed on nor checked.
func TestMavenChecksumMalformedUpstream(t *testing.T) {
	h, upstream, _, _ := newChecksumHandler(t, map[string]string{mavenJarPath: checksumTestJar})
	upstream.headers = map[string]http.Header{mavenJarPath: centralHeaders("not-a-digest", "xyz")}
	w := requestMavenStreamed(h, mavenJarPath)
	if w.Code != http.StatusOK || w.Body.String() != checksumTestJar {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body)
	}
	assertChecksumHeaders(t, "miss", w, "", "")
}

// serveRecovering serves a streamed GET, reporting whether the handler
// aborted the response as net/http would.
func serveRecovering(h http.Handler, path string) (w *httptest.ResponseRecorder, aborted bool) {
	defer func() {
		if r := recover(); r != nil {
			if r != http.ErrAbortHandler {
				panic(r)
			}
			aborted = true
		}
	}()
	return requestMavenStreamed(h, path), false
}

// waitChecksumRow waits until the streamed miss is stored, which happens
// after the client's response ends.
func waitChecksumRow(t *testing.T, h http.Handler) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w := requestMaven(h, http.MethodHead, mavenJarPath)
		if w.Header().Get("X-Checksum-SHA1") != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
