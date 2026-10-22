package handler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const (
	lastModPom = "/org/grails/grails-core/6.0.0/grails-core-6.0.0.pom"
	lastModA   = "Tue, 05 Mar 2024 10:11:12 GMT"
	lastModB   = "Wed, 06 Mar 2024 08:00:00 GMT"
)

// datedUpstream serves body for every path, with Last-Modified set to
// lastModified unless it is empty, and counts its requests.
func datedUpstream(t *testing.T, body, lastModified string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if lastModified != "" {
			w.Header().Set("Last-Modified", lastModified)
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func serveMaven(h http.Handler, method, path string, header http.Header) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		r.Header[k] = v
	}
	h.ServeHTTP(w, r)
	return w
}

// Dependabot's Maven cooldown dates a release by the Last-Modified of HEAD
// on its .pom; without it cooldown is silently off. The upstream value is
// sent on the response that fetched the file and on every hit after it.
func TestMavenReplaysUpstreamLastModified(t *testing.T) {
	up, requests := datedUpstream(t, "<project/>", lastModA)
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	useRealFetcher(t, proxy, up)
	h := NewMavenHandler(proxy, "http://localhost", up.URL, up.URL).Routes()

	for _, step := range []struct{ name, method string }{
		{"cold HEAD", http.MethodHead},
		{"cached GET", http.MethodGet},
		{"cached HEAD", http.MethodHead},
	} {
		w := serveMaven(h, step.method, lastModPom, nil)
		if w.Code != http.StatusOK || w.Header().Get("Last-Modified") != lastModA {
			t.Errorf("%s: status %d, Last-Modified %q; want 200, %q", step.name, w.Code, w.Header().Get("Last-Modified"), lastModA)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("upstream requests = %d, want 1", n)
	}
}

// A file whose upstream sends no Last-Modified gets none from the proxy:
// never the time the proxy fetched it, which would make an old release look
// new and hold it back.
func TestMavenSendsNoInventedLastModified(t *testing.T) {
	up, _ := datedUpstream(t, "<project/>", "")
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	useRealFetcher(t, proxy, up)
	h := NewMavenHandler(proxy, "http://localhost", up.URL, up.URL).Routes()

	for _, name := range []string{"cold", "cached"} {
		if w := getMaven(h, lastModPom); w.Code != http.StatusOK || w.Header().Get("Last-Modified") != "" {
			t.Errorf("%s: status %d, Last-Modified %q; want 200 and none", name, w.Code, w.Header().Get("Last-Modified"))
		}
	}
}

// The date belongs to the stored bytes. Repointing a named repository from
// A to B and back serves each upstream's file with that upstream's date, and
// a client's If-Modified-Since never turns into a 304 for bytes it has not
// seen.
func TestMavenLastModifiedFollowsUpstream(t *testing.T) {
	a, aRequests := datedUpstream(t, "<project>a</project>", lastModA)
	b, _ := datedUpstream(t, "<project>b</project>", lastModB)
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	useRealFetcher(t, proxy, a)
	repo := func(upstream string) http.Handler {
		return NewMavenHandler(proxy, "http://localhost", "http://central.invalid", "http://portal.invalid").
			WithRepositories(map[string]string{"r": upstream}).Routes()
	}
	// Newer than both dates: a 304 here would keep whatever the client had.
	ims := http.Header{"If-Modified-Since": {"Fri, 01 Jan 2027 00:00:00 GMT"}}

	for _, step := range []struct {
		name, upstream, body, lastModified string
	}{
		{"A", a.URL, "<project>a</project>", lastModA},
		{"B", b.URL, "<project>b</project>", lastModB},
		{"A again", a.URL, "<project>a</project>", lastModA},
	} {
		w := serveMaven(repo(step.upstream), http.MethodGet, "/r"+lastModPom, ims)
		if w.Code != http.StatusOK || w.Body.String() != step.body || w.Header().Get("Last-Modified") != step.lastModified {
			t.Errorf("%s: status %d, body %q, Last-Modified %q; want 200, %q, %q",
				step.name, w.Code, w.Body.String(), w.Header().Get("Last-Modified"), step.body, step.lastModified)
		}
	}
	if n := aRequests.Load(); n != 1 {
		t.Errorf("upstream A requests = %d, want 1 (A again is a hit)", n)
	}
}

// The default repository keeps its cache across an upstream change, so its
// date only changes when the file is fetched again. A refetch from an
// upstream that sends no Last-Modified drops the old date rather than
// keeping one that described other bytes.
func TestMavenLastModifiedReplacedOnRefetch(t *testing.T) {
	a, _ := datedUpstream(t, "<project>a</project>", lastModA)
	b, _ := datedUpstream(t, "<project>b</project>", "")
	proxy, db, store, _ := setupTestProxy(t)
	store.seekable = true
	useRealFetcher(t, proxy, a)
	evict := func() {
		t.Helper()
		art, err := db.GetArtifact("pkg:maven/org.grails/grails-core@6.0.0", "grails-core-6.0.0.pom")
		if err != nil || art == nil {
			t.Fatalf("GetArtifact: %+v, %v", art, err)
		}
		if _, err := db.ClearArtifactCache(art.VersionPURL, art.Filename, art.StoragePath.String); err != nil {
			t.Fatalf("ClearArtifactCache: %v", err)
		}
	}

	if w := getMaven(NewMavenHandler(proxy, "http://localhost", a.URL, a.URL).Routes(), lastModPom); w.Header().Get("Last-Modified") != lastModA {
		t.Fatalf("A: Last-Modified %q, want %q", w.Header().Get("Last-Modified"), lastModA)
	}
	evict()
	hb := NewMavenHandler(proxy, "http://localhost", b.URL, b.URL).Routes()
	for _, name := range []string{"B cold", "B cached"} {
		if w := getMaven(hb, lastModPom); w.Body.String() != "<project>b</project>" || w.Header().Get("Last-Modified") != "" {
			t.Errorf("%s: body %q, Last-Modified %q; want B's bytes and none", name, w.Body.String(), w.Header().Get("Last-Modified"))
		}
	}
	evict()
	if w := getMaven(NewMavenHandler(proxy, "http://localhost", a.URL, a.URL).Routes(), lastModPom); w.Header().Get("Last-Modified") != lastModA {
		t.Errorf("A again: Last-Modified %q, want %q", w.Header().Get("Last-Modified"), lastModA)
	}
}

// Replicas share the database, so one that never fetched the file sends the
// same date as the one that did. A separate instance with its own database
// fetches and sends the same date too: it is upstream's, not either
// instance's fetch time.
func TestMavenLastModifiedAgreesAcrossInstances(t *testing.T) {
	up, requests := datedUpstream(t, "<project/>", lastModA)
	first, db, store, _ := setupTestProxy(t)
	store.seekable = true
	useRealFetcher(t, first, up)
	second := NewProxy(db, store, first.Fetcher, first.Resolver, slog.New(slog.NewTextHandler(io.Discard, nil)))
	separate, _, separateStore, _ := setupTestProxy(t)
	separateStore.seekable = true
	useRealFetcher(t, separate, up)

	for _, step := range []struct {
		name  string
		proxy *Proxy
		want  int32
	}{
		{"first", first, 1},
		{"replica", second, 1},
		{"separate database", separate, 2},
	} {
		h := NewMavenHandler(step.proxy, "http://localhost", up.URL, up.URL).Routes()
		if w := serveMaven(h, http.MethodHead, lastModPom, nil); w.Code != http.StatusOK || w.Header().Get("Last-Modified") != lastModA {
			t.Errorf("%s: status %d, Last-Modified %q; want 200, %q", step.name, w.Code, w.Header().Get("Last-Modified"), lastModA)
		}
		if n := requests.Load(); n != step.want {
			t.Errorf("%s: upstream requests = %d, want %d", step.name, n, step.want)
		}
	}
}

// Only Maven records the date: Dependabot's Maven cooldown is the one known
// reader, and other clients (npm, pip, CDNs) would gain a heuristic-freshness
// validator nobody has asked for. A PyPI file fetched through the same path
// carries no Last-Modified.
func TestArtifactLastModifiedMavenOnly(t *testing.T) {
	up, _ := datedUpstream(t, "wheel", lastModA)
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	useRealFetcher(t, proxy, up)

	for _, name := range []string{"miss", "hit"} {
		result, err := proxy.GetOrFetchArtifactFromURL(t.Context(), "pypi", "six", "1.16.0", "six-1.16.0-py2.py3-none-any.whl", up.URL+"/six.whl")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		w := httptest.NewRecorder()
		ServeArtifactRequest(w, httptest.NewRequest(http.MethodGet, "/six.whl", nil), result)
		if w.Header().Get("Last-Modified") != "" {
			t.Errorf("%s: Last-Modified %q, want none", name, w.Header().Get("Last-Modified"))
		}
	}
}

// A date slightly in the future (upstream clock skew) is sent as the time
// of the response, never later (RFC 9110 8.8.2.1). A date far in the future
// is bogus: sent as "now" on every request it would make the release look
// 0 days old until then, so it counts as no date, like the Unix epoch, which
// servers send for "unknown".
func TestMavenLastModifiedBounds(t *testing.T) {
	for _, tc := range []struct {
		name, lastModified string
		clamped            bool
	}{
		{"skewed", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat), true},
		{"far future", "Fri, 01 Jan 2100 00:00:00 GMT", false},
		{"epoch", "Thu, 01 Jan 1970 00:00:00 GMT", false},
	} {
		up, _ := datedUpstream(t, "<project/>", tc.lastModified)
		proxy, _, store, _ := setupTestProxy(t)
		store.seekable = true
		useRealFetcher(t, proxy, up)
		h := NewMavenHandler(proxy, "http://localhost", up.URL, up.URL).Routes()
		for _, step := range []string{"cold", "cached"} {
			before := time.Now().Add(-time.Second)
			got := getMaven(h, lastModPom).Header().Get("Last-Modified")
			if !tc.clamped {
				if got != "" {
					t.Errorf("%s %s: Last-Modified %q, want none", tc.name, step, got)
				}
				continue
			}
			sent, err := http.ParseTime(got)
			if err != nil || sent.Before(before) || sent.After(time.Now()) {
				t.Errorf("%s %s: Last-Modified %q, want the response time", tc.name, step, got)
			}
		}
	}
}

// With stream_artifacts nothing is stored; the date comes straight from the
// upstream response.
func TestMavenLastModifiedStreamed(t *testing.T) {
	up, _ := datedUpstream(t, "<project/>", lastModA)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, up)
	proxy.StreamArtifacts = true
	h := NewMavenHandler(proxy, "http://localhost", up.URL, up.URL).Routes()

	if w := getMaven(h, lastModPom); w.Code != http.StatusOK || w.Header().Get("Last-Modified") != lastModA {
		t.Errorf("status %d, Last-Modified %q; want 200, %q", w.Code, w.Header().Get("Last-Modified"), lastModA)
	}
}

// A miss streamed to the client while it is stored (as the server does for
// GET) carries the date too, and so does the next hit.
func TestMavenLastModifiedStreamedMiss(t *testing.T) {
	up, requests := datedUpstream(t, "<project/>", lastModA)
	proxy, _, _, _ := setupTestProxy(t)
	useRealFetcher(t, proxy, up)
	h := NewMavenHandler(proxy, "http://localhost", up.URL, up.URL).Routes()

	for _, step := range []string{"miss", "hit"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, lastModPom, nil)
		h.ServeHTTP(w, r.WithContext(StreamCacheMiss(r.Context())))
		if w.Code != http.StatusOK || w.Header().Get("Last-Modified") != lastModA {
			t.Errorf("%s: status %d, Last-Modified %q; want 200, %q", step, w.Code, w.Header().Get("Last-Modified"), lastModA)
		}
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("upstream requests = %d, want 1", n)
	}
}

// openHook runs before each Open of the storage it wraps.
type openHook struct {
	*mockStorage
	before func()
}

func (s openHook) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	s.before()
	return s.mockStorage.Open(ctx, path)
}

// A miss sends the date of the bytes it serves, as its own fetch got it, not
// whatever the record for the same file says by then. Here another replica
// commits the same file dated B between this fetch's commit and the read of
// its bytes; the response must still say A. Reading the date back by
// version and filename after the commit, as an earlier version did, says B.
func TestLastModifiedFollowsServedBytes(t *testing.T) {
	up, _ := datedUpstream(t, "<project/>", lastModA)
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	useRealFetcher(t, proxy, up)
	dateB, _ := http.ParseTime(lastModB)
	replica := testArtifact("<project/>", "pkg:maven/org.grails/grails-core@6.0.0", "grails-core-6.0.0.pom", "text/xml")
	proxy.Storage = openHook{mockStorage: store, before: func() {
		if err := proxy.updateCacheDB("maven", "org.grails:grails-core", "pkg:maven/org.grails/grails-core",
			"https://replica.example/b.pom", "maven/other-replica.pom", replica, "", dateB); err != nil {
			t.Errorf("other replica's commit: %v", err)
		}
	}}
	h := NewMavenHandler(proxy, "http://localhost", up.URL, up.URL).Routes()

	if w := getMaven(h, lastModPom); w.Code != http.StatusOK || w.Header().Get("Last-Modified") != lastModA {
		t.Errorf("status %d, Last-Modified %q; want 200 and this fetch's %q", w.Code, w.Header().Get("Last-Modified"), lastModA)
	}
}
