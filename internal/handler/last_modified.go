package handler

// Dependabot's Maven cooldown dates a release by the Last-Modified of HEAD
// on its .pom when the directory listing has no date. The proxy keeps the
// Last-Modified upstream sent with a Maven file and sends it with those
// bytes:
//
//	upstream Last-Modified ─► fetchedArtifact.lastModified ─► artifacts.last_modified
//	                                   │ (miss)                     │ (hit, same query)
//	                                   └──────────► Last-Modified ◄─┘

import (
	"context"
	"net/http"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

// fetchDated fetches url with fetchUpstream. For Maven it also returns the
// upstream Last-Modified, zero if there is none. Only Maven asks:
// Dependabot's Maven cooldown is its one known reader, and the observed
// fetch hashes the body with SHA-256 and SHA-512 on the way through. A
// Last-Modified callback already on ctx (Maven cooldown) still gets the
// date.
func (p *Proxy) fetchDated(ctx context.Context, ecosystem, url string, headers http.Header) (*fetch.Artifact, time.Time, error) {
	if ecosystem != "maven" {
		artifact, err := p.fetchUpstream(ctx, url, headers)
		return artifact, time.Time{}, err
	}
	var date time.Time
	ctx = WithUpstreamLastModified(ctx, func(t time.Time) { date = upstreamDate(t) })
	artifact, err := p.fetchUpstream(ctx, url, headers)
	if err != nil {
		return nil, time.Time{}, err
	}
	return artifact, date, nil
}

// The production fetcher; a wrapper without the observed path would
// silently stop recording dates.
var _ observedFetcher = (*fetch.CircuitBreakerFetcher)(nil)

// maxFutureSkew is how far past the fetch a Last-Modified may lie and still
// count as clock skew. serveArtifactResponse sends a skewed date as the
// response time; anything later is bogus.
const maxFutureSkew = 24 * time.Hour

// upstreamDate filters a parsed Last-Modified. One at or before the Unix
// epoch, which servers send for "unknown", and one more than maxFutureSkew
// ahead are zero: sent as "now" on every request, a far-future date would
// make the release look 0 days old until then, and no date beats a made-up
// one.
func upstreamDate(t time.Time) time.Time {
	if !t.After(time.Unix(0, 0)) || t.After(time.Now().Add(maxFutureSkew)) {
		return time.Time{}
	}
	return t
}
