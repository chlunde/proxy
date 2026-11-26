package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/httpclient"
)

func testECRTokens() *ecrTokens {
	return newECRTokens(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestECRTokensCachesUntilExpiry(t *testing.T) {
	e := testECRTokens()
	calls := 0
	e.getToken = func(_ context.Context, region string) (string, time.Time, error) {
		calls++
		if region != "eu-west-1" {
			t.Errorf("region = %q, want eu-west-1", region)
		}
		return "QVdTOnNlY3JldA==", time.Now().Add(12 * time.Hour), nil
	}

	name, value := e.header("eu-west-1")
	if name != "Authorization" || value != "Basic QVdTOnNlY3JldA==" {
		t.Fatalf("header() = %q, %q", name, value)
	}

	e.header("eu-west-1")
	e.header("eu-west-1")
	if calls != 1 {
		t.Fatalf("getToken called %d times, want 1", calls)
	}
}

func TestECRTokensRefreshesWithinSkew(t *testing.T) {
	e := testECRTokens()
	calls := 0
	e.getToken = func(_ context.Context, _ string) (string, time.Time, error) {
		calls++
		return "dG9rZW4=", time.Now().Add(time.Minute), nil
	}

	e.header("us-east-1")
	e.header("us-east-1")
	if calls != 2 {
		t.Fatalf("getToken called %d times, want 2 (token within skew window)", calls)
	}
}

func TestECRTokensUsesValidCachedTokenWhenRefreshFails(t *testing.T) {
	e := testECRTokens()
	calls := 0
	e.cache["eu-west-1"] = ecrToken{
		value:     "Basic Y2FjaGVk",
		refreshAt: time.Now().Add(-time.Minute),
		expiresAt: time.Now().Add(time.Minute),
	}
	e.getToken = func(_ context.Context, _ string) (string, time.Time, error) {
		calls++
		return "", time.Time{}, errors.New("ECR unavailable")
	}

	name, value := e.header("eu-west-1")
	if name != "Authorization" || value != "Basic Y2FjaGVk" {
		t.Fatalf("header() = %q, %q; want cached token", name, value)
	}
	e.header("eu-west-1")
	if calls != 1 {
		t.Fatalf("getToken called %d times, want 1 during failure backoff", calls)
	}
}

func TestECRTokensRejectsExpiredCachedTokenWhenRefreshFails(t *testing.T) {
	e := testECRTokens()
	calls := 0
	e.cache["eu-west-1"] = ecrToken{
		value:     "Basic ZXhwaXJlZA==",
		refreshAt: time.Now().Add(-2 * time.Minute),
		expiresAt: time.Now().Add(-time.Minute),
	}
	e.getToken = func(_ context.Context, _ string) (string, time.Time, error) {
		calls++
		return "", time.Time{}, errors.New("ECR unavailable")
	}

	name, value := e.header("eu-west-1")
	if name != "" || value != "" {
		t.Fatalf("header() = %q, %q; want empty for expired token", name, value)
	}
	e.header("eu-west-1")
	if calls != 1 {
		t.Fatalf("getToken called %d times, want 1 during failure backoff", calls)
	}
}

func TestECRTokensPerRegion(t *testing.T) {
	e := testECRTokens()
	seen := map[string]int{}
	e.getToken = func(_ context.Context, region string) (string, time.Time, error) {
		seen[region]++
		return region + "-token", time.Now().Add(time.Hour), nil
	}

	e.header("eu-west-1")
	e.header("us-east-1")
	e.header("eu-west-1")

	if seen["eu-west-1"] != 1 || seen["us-east-1"] != 1 {
		t.Fatalf("per-region calls = %v, want one each", seen)
	}
}

func TestECRTokensConcurrentMissesShareOneFetch(t *testing.T) {
	e := testECRTokens()
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	e.getToken = func(_ context.Context, _ string) (string, time.Time, error) {
		calls.Add(1)
		startedOnce.Do(func() { close(started) })
		<-release
		return "dG9rZW4=", time.Now().Add(time.Hour), nil
	}

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			name, value := e.header("eu-west-1")
			if name != "Authorization" || value != "Basic dG9rZW4=" {
				t.Errorf("header() = %q, %q", name, value)
			}
		}()
	}

	<-started
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("getToken called %d times, want 1", got)
	}
}

func TestECRTokensBacksOffAfterError(t *testing.T) {
	e := testECRTokens()
	calls := 0
	e.getToken = func(_ context.Context, _ string) (string, time.Time, error) {
		calls++
		return "", time.Time{}, errors.New("no credentials")
	}

	name, value := e.header("eu-west-1")
	if name != "" || value != "" {
		t.Fatalf("header() = %q, %q; want empty on error", name, value)
	}
	e.header("eu-west-1")
	if calls != 1 {
		t.Fatalf("getToken called %d times, want 1 during failure backoff", calls)
	}

	tok, ok := e.cached("eu-west-1")
	if !ok {
		t.Fatal("failure was not cached")
	}
	tok.refreshAt = time.Now().Add(-time.Second)
	tok.expiresAt = tok.refreshAt
	e.store("eu-west-1", tok)
	e.header("eu-west-1")
	if calls != 2 {
		t.Fatalf("getToken called %d times, want retry after failure backoff", calls)
	}
}

func TestECRAuthFailureReturnsBasicChallengeResponse(t *testing.T) {
	e := testECRTokens()
	var tokenRequests atomic.Int32
	e.getToken = func(_ context.Context, _ string) (string, time.Time, error) {
		tokenRequests.Add(1)
		return "", time.Time{}, errors.New("no credentials")
	}

	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="Amazon ECR"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	s := &Server{
		ecr: e,
		cfg: &config.Config{Upstream: config.UpstreamConfig{
			Auth: map[string]config.AuthConfig{
				upstream.URL: {Type: "ecr"},
			},
		}},
	}
	client := &http.Client{Transport: httpclient.NewTransport(http.DefaultTransport, s.authForURL)}

	resp, err := client.Get(upstream.URL + "/v2/repo/manifests/latest")
	if err != nil {
		t.Fatalf("GET upstream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Basic realm="Amazon ECR"` {
		t.Errorf("WWW-Authenticate = %q, want Basic challenge", got)
	}
	if got := tokenRequests.Load(); got != 1 {
		t.Errorf("token requests = %d, want 1", got)
	}
	if got := upstreamRequests.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
}

func TestECRRegion(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"commercial", "https://123456789012.dkr.ecr.eu-west-1.amazonaws.com/v2/repo", "eu-west-1"},
		{"China", "https://123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn/v2/repo", "cn-north-1"},
		{"GovCloud", "https://123456789012.dkr.ecr.us-gov-west-1.amazonaws.com/v2/repo", "us-gov-west-1"},
		{"dual-stack", "https://123456789012.dkr-ecr.us-west-2.on.aws/v2/repo", "us-west-2"},
		{"FIPS", "https://123456789012.dkr.ecr-fips.us-east-1.amazonaws.com/v2/repo", "us-east-1"},
		{"FIPS dual-stack", "https://123456789012.dkr-ecr-fips.us-east-1.on.aws/v2/repo", "us-east-1"},
		{"case insensitive", "https://123456789012.DKR.ECR.EU-WEST-1.AMAZONAWS.COM/v2/repo", "eu-west-1"},
		{"lookalike suffix", "https://123456789012.dkr.ecr.eu-west-1.amazonaws.com.example/v2/repo", ""},
		{"not ECR", "https://registry.example.com/v2/repo", ""},
		{"invalid URL", "://invalid", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ecrRegion(tt.url); got != tt.want {
				t.Errorf("ecrRegion(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestAuthForURLInfersECRRegion(t *testing.T) {
	e := testECRTokens()
	e.getToken = func(_ context.Context, region string) (string, time.Time, error) {
		if region != "eu-west-1" {
			t.Errorf("region = %q, want eu-west-1", region)
		}
		return "QVdTOnNlY3JldA==", time.Now().Add(time.Hour), nil
	}

	s := &Server{
		ecr: e,
		cfg: &config.Config{Upstream: config.UpstreamConfig{
			Auth: map[string]config.AuthConfig{
				"https://123456789012.dkr.ecr.eu-west-1.amazonaws.com": {Type: "ecr"},
			},
		}},
	}

	name, value := s.authForURL("https://123456789012.dkr.ecr.eu-west-1.amazonaws.com/v2/repo")
	if name != "Authorization" || value != "Basic QVdTOnNlY3JldA==" {
		t.Fatalf("authForURL() = %q, %q", name, value)
	}
}

func TestAuthForURLRoutesECRType(t *testing.T) {
	e := testECRTokens()
	e.getToken = func(_ context.Context, region string) (string, time.Time, error) {
		if region != "eu-west-1" {
			t.Errorf("region = %q, want eu-west-1", region)
		}
		return "QVdTOnNlY3JldA==", time.Now().Add(time.Hour), nil
	}

	s := &Server{
		ecr: e,
		cfg: &config.Config{
			Upstream: config.UpstreamConfig{
				Auth: map[string]config.AuthConfig{
					"https://123456789012.dkr.ecr.eu-west-1.amazonaws.com": {
						Type:   "ecr",
						Region: "eu-west-1",
					},
					"https://ghcr.io": {
						Type:  "bearer",
						Token: "ghcr-token",
					},
				},
			},
		},
	}

	name, value := s.authForURL("https://123456789012.dkr.ecr.eu-west-1.amazonaws.com/v2/my/repo/manifests/latest")
	if name != "Authorization" || value != "Basic QVdTOnNlY3JldA==" {
		t.Fatalf("ecr authForURL() = %q, %q", name, value)
	}

	name, value = s.authForURL("https://ghcr.io/v2/owner/repo/blobs/sha256:abc")
	if name != "Authorization" || value != "Bearer ghcr-token" {
		t.Fatalf("bearer authForURL() = %q, %q", name, value)
	}

	name, value = s.authForURL("https://registry-1.docker.io/v2/")
	if name != "" || value != "" {
		t.Fatalf("unmatched authForURL() = %q, %q; want empty", name, value)
	}
}

func TestAuthForURLUsesECRPublicToken(t *testing.T) {
	e := testECRTokens()
	var mu sync.Mutex
	var keys []string
	e.getToken = func(_ context.Context, key string) (string, time.Time, error) {
		mu.Lock()
		keys = append(keys, key)
		mu.Unlock()
		if key == ecrPublicKey {
			return "cHVibGlj", time.Now().Add(time.Hour), nil
		}
		return "cHJpdmF0ZQ==", time.Now().Add(time.Hour), nil
	}

	s := &Server{
		ecr: e,
		cfg: &config.Config{Upstream: config.UpstreamConfig{
			Auth: map[string]config.AuthConfig{
				// region is ignored: ECR Public tokens only come from us-east-1.
				"https://public.ecr.aws":                               {Type: "ecr", Region: "eu-west-1"},
				"https://123456789012.dkr.ecr.us-east-1.amazonaws.com": {Type: "ecr"},
			},
		}},
	}

	// The registry takes the token as Bearer; /token/ takes it as Basic.
	for u, want := range map[string]string{
		"https://public.ecr.aws/v2/docker/library/alpine/manifests/3":               "Bearer cHVibGlj",
		"https://public.ecr.aws/token/?scope=repository:docker/library/alpine:pull": "Basic cHVibGlj",
	} {
		if name, value := s.authForURL(u); name != "Authorization" || value != want {
			t.Errorf("authForURL(%q) = %q, %q; want %q", u, name, value, want)
		}
	}

	// A private us-east-1 registry keeps its own token.
	name, value := s.authForURL("https://123456789012.dkr.ecr.us-east-1.amazonaws.com/v2/repo/manifests/1")
	if name != "Authorization" || value != "Basic cHJpdmF0ZQ==" {
		t.Errorf("private authForURL() = %q, %q", name, value)
	}
	if len(keys) != 2 || keys[0] != ecrPublicKey || keys[1] != "us-east-1" {
		t.Errorf("getToken keys = %q, want [%s us-east-1]", keys, ecrPublicKey)
	}
}

func TestECRPublic(t *testing.T) {
	tests := map[string]bool{
		"https://public.ecr.aws/v2/repo":                               true,
		"https://PUBLIC.ECR.AWS/token/":                                true,
		"https://public.ecr.aws.example/v2/repo":                       false,
		"https://x.public.ecr.aws/v2/repo":                             false,
		"https://123456789012.dkr.ecr.us-east-1.amazonaws.com/v2/repo": false,
		"://invalid": false,
	}
	for u, want := range tests {
		if got := ecrPublic(u); got != want {
			t.Errorf("ecrPublic(%q) = %v, want %v", u, got, want)
		}
	}
}

// fakeECRPublic answers like public.ecr.aws: 400 DENIED to Basic on /v2/,
// and /token/ echoes the Basic credential back as the token. With
// challenge set, the first /v2/ request gets a 401 to test that path too.
func fakeECRPublic(t *testing.T, challenge bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var tokenCalls atomic.Int32
	var challenged atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(r.URL.Path, "/token/") {
			tokenCalls.Add(1)
			if auth != "Basic cHVibGlj" {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, `{"token":"cHVibGlj"}`)
			return
		}
		if challenge && !challenged.Swap(true) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://public.ecr.aws/token/",service="public.ecr.aws",scope="aws"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if auth != "Bearer cHVibGlj" {
			http.Error(w, `{"errors":[{"code":"DENIED"}]}`, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	return srv, &tokenCalls
}

func TestECRPublicPull(t *testing.T) {
	for _, tc := range []struct {
		name       string
		challenge  bool
		tokenCalls int32
	}{
		{"direct", false, 0},
		{"after 401", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, tokenCalls := fakeECRPublic(t, tc.challenge)
			e := testECRTokens()
			e.getToken = func(context.Context, string) (string, time.Time, error) {
				return "cHVibGlj", time.Now().Add(time.Hour), nil
			}
			s := &Server{ecr: e, cfg: &config.Config{Upstream: config.UpstreamConfig{
				Auth: map[string]config.AuthConfig{"https://public.ecr.aws": {Type: "ecr"}},
			}}}

			// Send public.ecr.aws to the fake server.
			base := srv.Client().Transport.(*http.Transport).Clone()
			base.TLSClientConfig.ServerName = "example.com"
			base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
			}
			client := &http.Client{Transport: httpclient.NewTransport(base, s.authForURL)}

			resp, err := client.Get("https://public.ecr.aws/v2/docker/library/redis/manifests/7-alpine")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			if got := tokenCalls.Load(); got != tc.tokenCalls {
				t.Errorf("/token/ calls = %d, want %d", got, tc.tokenCalls)
			}
		})
	}
}
