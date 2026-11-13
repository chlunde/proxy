package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const (
	pomDatePath = "/org/grails/grails-core/6.0.0/grails-core-6.0.0.pom"
	pomDate     = "Tue, 05 Mar 2024 10:11:12 GMT"
)

// pomDateUpstream serves a .pom with the Last-Modified in *date, none
// when it is empty, and counts its requests.
func pomDateUpstream(t *testing.T, date *atomic.Value) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if d, _ := date.Load().(string); d != "" {
			w.Header().Set("Last-Modified", d)
		}
		_, _ = io.WriteString(w, "<project/>")
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func pomRequest(h http.Handler, method string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, pomDatePath, nil))
	return w
}

// Dependabot's Maven cooldown dates a release by the Last-Modified of HEAD
// on its .pom; without it cooldown is silently off. With proxy cooldown
// off, the .pom still carries the version's publish time, on the miss and
// on hits.
func TestMavenPomCarriesPublishTime(t *testing.T) {
	var date atomic.Value
	date.Store(pomDate)
	up, requests := pomDateUpstream(t, &date)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, up)
	h := NewMavenHandler(proxy, "http://localhost", up.URL, up.URL).Routes()

	for _, step := range []struct{ name, method string }{
		{"cold HEAD", http.MethodHead},
		{"cached GET", http.MethodGet},
		{"cached HEAD", http.MethodHead},
	} {
		w := pomRequest(h, step.method)
		if w.Code != http.StatusOK || w.Header().Get("Last-Modified") != pomDate {
			t.Errorf("%s: status %d, Last-Modified %q; want 200, %q", step.name, w.Code, w.Header().Get("Last-Modified"), pomDate)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("upstream requests = %d, want 1", n)
	}
}

// A .pom whose upstream sends no Last-Modified gets none: never the time
// the proxy fetched it, which would make an old release look new.
func TestMavenPomNoInventedDate(t *testing.T) {
	var date atomic.Value
	date.Store("")
	up, _ := pomDateUpstream(t, &date)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, up)
	h := NewMavenHandler(proxy, "http://localhost", up.URL, up.URL).Routes()

	for _, name := range []string{"cold", "cached"} {
		if w := pomRequest(h, http.MethodGet); w.Code != http.StatusOK || w.Header().Get("Last-Modified") != "" {
			t.Errorf("%s: status %d, Last-Modified %q; want 200 and none", name, w.Code, w.Header().Get("Last-Modified"))
		}
	}
}

// A .pom cached before its time was recorded gets it on its first HEAD,
// from a HEAD upstream, as Maven cooldown reads it. A GET, which build
// tools send, never waits on upstream for it.
func TestMavenPomTimeBackfilledOnHead(t *testing.T) {
	var date atomic.Value
	date.Store("")
	up, requests := pomDateUpstream(t, &date)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, up)
	h := NewMavenHandler(proxy, "http://localhost", up.URL, up.URL).Routes()

	pomRequest(h, http.MethodGet)
	date.Store(pomDate)

	if w := pomRequest(h, http.MethodGet); w.Header().Get("Last-Modified") != "" || requests.Load() != 1 {
		t.Errorf("cached GET: Last-Modified %q, upstream requests %d; want none, 1", w.Header().Get("Last-Modified"), requests.Load())
	}
	for _, name := range []string{"first HEAD", "second HEAD"} {
		if w := pomRequest(h, http.MethodHead); w.Header().Get("Last-Modified") != pomDate {
			t.Errorf("%s: Last-Modified %q, want %q", name, w.Header().Get("Last-Modified"), pomDate)
		}
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("upstream requests = %d, want 2 (one HEAD fills the time)", n)
	}
}

// A hanging upstream must not hold a cached .pom's HEAD for the HTTP
// timeout: the backfill gives up after pomTimeWait and the file is sent
// without a date.
func TestMavenPomBackfillBounded(t *testing.T) {
	var hang atomic.Bool
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() && r.Method == http.MethodHead {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = io.WriteString(w, "<project/>")
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, srv)
	h := NewMavenHandler(proxy, "http://localhost", srv.URL, srv.URL).Routes()
	defer func(d time.Duration) { pomTimeWait = d }(pomTimeWait)
	pomTimeWait = 50 * time.Millisecond

	pomRequest(h, http.MethodGet)
	hang.Store(true)
	start := time.Now()
	w := pomRequest(h, http.MethodHead)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("HEAD took %v, want under 2s", took)
	}
	if w.Code != http.StatusOK || w.Header().Get("Last-Modified") != "" {
		t.Errorf("status %d, Last-Modified %q; want 200 and none", w.Code, w.Header().Get("Last-Modified"))
	}
}

// Only a release's .pom and .module carry the version's time: a jar of
// the same version, and a SNAPSHOT, whose files change, get none. A date
// ahead of the proxy's clock goes out as the response time.
func TestMavenPomDateScope(t *testing.T) {
	var date atomic.Value
	date.Store(pomDate)
	up, _ := pomDateUpstream(t, &date)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, up)
	h := NewMavenHandler(proxy, "http://localhost", up.URL, up.URL).Routes()
	get := func(path string) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodHead, path, nil))
		return w.Header().Get("Last-Modified")
	}

	pomRequest(h, http.MethodGet)
	if lm := get("/org/grails/grails-core/6.0.0/grails-core-6.0.0.jar"); lm != "" {
		t.Errorf("jar: Last-Modified %q, want none", lm)
	}
	if lm := get("/org/grails/grails-core/6.1.0-SNAPSHOT/grails-core-6.1.0-SNAPSHOT.pom"); lm != "" {
		t.Errorf("SNAPSHOT pom: Last-Modified %q, want none", lm)
	}
	date.Store("Fri, 01 Jan 2100 00:00:00 GMT")
	lm, err := http.ParseTime(get("/org/grails/grails-core/7.0.0/grails-core-7.0.0.pom"))
	if err != nil || lm.After(time.Now()) {
		t.Errorf("future date: Last-Modified %v (%v), want at most now", lm, err)
	}
}
