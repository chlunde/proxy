package handler

// Extra tests for the .pom Last-Modified change. Not for the upstream PR:
// they pin edges the PR's five tests leave open. Run with
//   go test ./internal/handler -run 'TestMavenPomX' -race -count=3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// datedUpstream serves every path with the Last-Modified in dates[path],
// 404 for paths ending in a key of missing, and counts HEADs and GETs.
type datedUpstream struct {
	srv         *httptest.Server
	mu          sync.Mutex
	dates       map[string]string
	missing     []string
	heads, gets atomic.Int32
}

func newDatedUpstream(t *testing.T) *datedUpstream {
	t.Helper()
	u := &datedUpstream{dates: map[string]string{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			u.heads.Add(1)
		} else {
			u.gets.Add(1)
		}
		u.mu.Lock()
		date, missing := u.dates[r.URL.Path], u.isMissing(r.URL.Path)
		u.mu.Unlock()
		if missing {
			http.NotFound(w, r)
			return
		}
		if date != "" {
			w.Header().Set("Last-Modified", date)
		}
		_, _ = io.WriteString(w, "<project>"+strings.Repeat("x", 64)+"</project>")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *datedUpstream) isMissing(p string) bool {
	for _, m := range u.missing {
		if strings.HasSuffix(p, m) {
			return true
		}
	}
	return false
}

func (u *datedUpstream) set(p, date string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.dates[p] = date
}

// do sends a request the way internal/server delivers it.
func do(h http.Handler, method, p string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, p, nil)
	if method == http.MethodHead {
		// As the server's middleware does for every HEAD.
		r = r.WithContext(ServingHead(r.Context()))
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func lm(w *httptest.ResponseRecorder) string { return w.Header().Get("Last-Modified") }

const (
	xDir    = "/org/grails/grails-core/6.0.0/"
	xPom    = xDir + "grails-core-6.0.0.pom"
	xModule = xDir + "grails-core-6.0.0.module"
	xJar    = xDir + "grails-core-6.0.0.jar"
)

// A .module carries its own time with cooldown off, like the .pom.
func TestMavenPomXModuleCooldownOff(t *testing.T) {
	u := newDatedUpstream(t)
	u.set(xModule, pomDate)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, u.srv)
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL, u.srv.URL).Routes()

	for _, step := range []string{"miss", "hit"} {
		if w := do(h, http.MethodGet, xModule); lm(w) != pomDate {
			t.Errorf("%s: Last-Modified %q, want %q", step, lm(w), pomDate)
		}
	}
}

// Whichever of .pom and .module comes first sets the version's time; the
// other file then carries it too (one time per version, by design).
func TestMavenPomXModuleFirstWins(t *testing.T) {
	u := newDatedUpstream(t)
	u.set(xModule, "Tue, 05 Mar 2024 10:11:10 GMT")
	u.set(xPom, pomDate)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, u.srv)
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL, u.srv.URL).Routes()

	do(h, http.MethodGet, xModule)
	if w := do(h, http.MethodHead, xPom); lm(w) != "Tue, 05 Mar 2024 10:11:10 GMT" {
		t.Errorf("pom after module: Last-Modified %q, want the module's", lm(w))
	}
}

// The proxy doesn't evaluate If-Modified-Since on artifacts: a client
// sending one gets the full file, not a 304 it didn't get before.
func TestMavenPomXIfModifiedSinceStill200(t *testing.T) {
	u := newDatedUpstream(t)
	u.set(xPom, pomDate)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, u.srv)
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL, u.srv.URL).Routes()

	do(h, http.MethodGet, xPom)
	w := do(h, http.MethodGet, xPom, "If-Modified-Since", "Fri, 01 Jan 2100 00:00:00 GMT")
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Errorf("status %d, %d bytes; want 200 with body", w.Code, w.Body.Len())
	}
}

// A Range GET on a cached .pom still answers 206, with the date.
func TestMavenPomXRange(t *testing.T) {
	u := newDatedUpstream(t)
	u.set(xPom, pomDate)
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	useRealFetcher(t, proxy, u.srv)
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL, u.srv.URL).Routes()

	do(h, http.MethodGet, xPom)
	w := do(h, http.MethodGet, xPom, "Range", "bytes=0-3")
	if w.Code != http.StatusPartialContent || lm(w) != pomDate {
		t.Errorf("status %d, Last-Modified %q; want 206, %q", w.Code, lm(w), pomDate)
	}
}

// With direct serve, a cached GET redirects (the date on the 302 is
// harmless) and HEAD, which Dependabot sends, is answered with the date.
func TestMavenPomXDirectServe(t *testing.T) {
	u := newDatedUpstream(t)
	u.set(xPom, pomDate)
	proxy, _, store, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, u.srv)
	proxy.DirectServe = true
	store.signedURL = "https://bucket.example/signed"
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL, u.srv.URL).Routes()

	do(h, http.MethodGet, xPom)
	if w := do(h, http.MethodGet, xPom); w.Code != http.StatusFound {
		t.Errorf("cached GET: status %d, want 302", w.Code)
	}
	if w := do(h, http.MethodHead, xPom); w.Code != http.StatusOK || lm(w) != pomDate {
		t.Errorf("cached HEAD: status %d, Last-Modified %q; want 200, %q", w.Code, lm(w), pomDate)
	}
}

// An unparsable upstream date is dropped, not sent on or stored as zero.
func TestMavenPomXBadUpstreamDate(t *testing.T) {
	u := newDatedUpstream(t)
	u.set(xPom, "yesterday")
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, u.srv)
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL, u.srv.URL).Routes()

	for _, step := range []string{"miss", "hit"} {
		if w := do(h, http.MethodHead, xPom); w.Code != http.StatusOK || lm(w) != "" {
			t.Errorf("%s: status %d, Last-Modified %q; want 200 and none", step, w.Code, lm(w))
		}
	}
}

// Two named repositories with the same coordinates keep their own times.
func TestMavenPomXNamedRepositories(t *testing.T) {
	u := newDatedUpstream(t)
	u.set("/a"+xPom, "Mon, 01 Jan 2024 00:00:00 GMT")
	u.set("/b"+xPom, "Tue, 02 Jan 2024 00:00:00 GMT")
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, u.srv)
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL, u.srv.URL).
		WithRepositories(map[string]string{"a": u.srv.URL + "/a", "b": u.srv.URL + "/b"}).Routes()

	for range 2 {
		if w := do(h, http.MethodHead, "/a"+xPom); lm(w) != "Mon, 01 Jan 2024 00:00:00 GMT" {
			t.Errorf("a: Last-Modified %q", lm(w))
		}
		if w := do(h, http.MethodHead, "/b"+xPom); lm(w) != "Tue, 02 Jan 2024 00:00:00 GMT" {
			t.Errorf("b: Last-Modified %q", lm(w))
		}
	}
}

// Concurrent cold HEADs all get the date; upstream sees a bounded number
// of requests. Run with -race.
func TestMavenPomXConcurrentCold(t *testing.T) {
	u := newDatedUpstream(t)
	u.set(xPom, pomDate)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, u.srv)
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL, u.srv.URL).Routes()

	const n = 16
	var wg sync.WaitGroup
	got := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = lm(do(h, http.MethodHead, xPom))
		}()
	}
	wg.Wait()
	for i, g := range got {
		if g != pomDate {
			t.Errorf("request %d: Last-Modified %q, want %q", i, g, pomDate)
		}
	}
	if total := u.heads.Load() + u.gets.Load(); total > 2*n {
		t.Errorf("upstream requests = %d, want at most %d", total, 2*n)
	}
}

// A cached .pom whose upstream since lost it is still served, undated,
// and the miss is not retried as a date on a GET.
func TestMavenPomXBackfillUpstreamGone(t *testing.T) {
	u := newDatedUpstream(t)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, u.srv)
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL, u.srv.URL).Routes()

	do(h, http.MethodGet, xPom)
	u.mu.Lock()
	u.missing = []string{".pom"}
	u.mu.Unlock()
	if w := do(h, http.MethodHead, xPom); w.Code != http.StatusOK || lm(w) != "" {
		t.Errorf("HEAD: status %d, Last-Modified %q; want 200 and none", w.Code, lm(w))
	}
	before := u.heads.Load() + u.gets.Load()
	do(h, http.MethodGet, xPom)
	if after := u.heads.Load() + u.gets.Load(); after != before {
		t.Errorf("cached GET went upstream (%d → %d)", before, after)
	}
}

// Turning cooldown on after times were stored with it off: the stored
// time is used, a young version is refused, an old one gets its date.
func TestMavenPomXCooldownAfterOff(t *testing.T) {
	young := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	u := newDatedUpstream(t)
	u.set(xPom, pomDate)
	u.set("/org/grails/grails-core/7.0.0/grails-core-7.0.0.pom", young)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, u.srv)
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL, u.srv.URL).Routes()

	do(h, http.MethodGet, xPom)
	do(h, http.MethodGet, "/org/grails/grails-core/7.0.0/grails-core-7.0.0.pom")
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	before := u.heads.Load() + u.gets.Load()

	if w := do(h, http.MethodHead, xPom); w.Code != http.StatusOK || lm(w) != pomDate {
		t.Errorf("old: status %d, Last-Modified %q; want 200, %q", w.Code, lm(w), pomDate)
	}
	if w := do(h, http.MethodGet, "/org/grails/grails-core/7.0.0/grails-core-7.0.0.pom"); w.Code != http.StatusForbidden {
		t.Errorf("young: status %d, want 403", w.Code)
	}
	if after := u.heads.Load() + u.gets.Load(); after != before {
		t.Errorf("stored times not used: upstream %d → %d", before, after)
	}
}

// A .pom found only in the plugin portal carries the portal's date.
func TestMavenPomXFallbackDate(t *testing.T) {
	u := newDatedUpstream(t)
	u.missing = []string{"/central" + xPom}
	u.set("/portal"+xPom, pomDate)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, u.srv)
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL+"/central", u.srv.URL+"/portal").Routes()

	if w := do(h, http.MethodGet, xPom); w.Code != http.StatusOK || lm(w) != pomDate {
		t.Errorf("status %d, Last-Modified %q; want 200, %q", w.Code, lm(w), pomDate)
	}
}

// Only the version's own .pom: a classifier file or checksum next to it
// gets no date.
func TestMavenPomXNeighbours(t *testing.T) {
	u := newDatedUpstream(t)
	for _, p := range []string{xPom, xPom + ".sha1", xDir + "grails-core-6.0.0-sources.jar", xDir + "grails-core-6.0.0.module.md5"} {
		u.set(p, pomDate)
	}
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, u.srv)
	h := NewMavenHandler(proxy, "http://localhost", u.srv.URL, u.srv.URL).Routes()

	do(h, http.MethodGet, xPom)
	for _, p := range []string{xPom + ".sha1", xDir + "grails-core-6.0.0-sources.jar", xDir + "grails-core-6.0.0.module.md5", xJar} {
		if w := do(h, http.MethodHead, p); lm(w) != "" {
			t.Errorf("%s: Last-Modified %q, want none", p, lm(w))
		}
	}
}
