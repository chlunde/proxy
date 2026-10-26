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

// upstreamMeta is what upstream's response headers said about the bytes.
type upstreamMeta struct {
	// lastModified is the Last-Modified, zero if there is none.
	lastModified time.Time
	// sums are the X-Checksum-* digests, if upstream sent them.
	sums checksums
}

// fetchMeta fetches url with fetchUpstream. For Maven it also returns what
// the response headers say about the bytes. Only Maven asks: its clients
// are the known readers, and the observed fetch hashes the body with
// SHA-256 and SHA-512 on the way through. An observer already on ctx
// (Maven cooldown) still sees the headers.
func (p *Proxy) fetchMeta(ctx context.Context, ecosystem, url string, headers http.Header) (*fetch.Artifact, upstreamMeta, error) {
	if ecosystem != "maven" {
		artifact, err := p.fetchUpstream(ctx, url, headers)
		return artifact, upstreamMeta{}, err
	}
	var meta upstreamMeta
	ctx = WithUpstreamLastModified(ctx, func(t time.Time) { meta.lastModified = upstreamDate(t) })
	ctx = withUpstreamHeaders(ctx, func(h http.Header) { meta.sums = upstreamChecksums(h) })
	artifact, err := p.fetchUpstream(ctx, url, headers)
	if err != nil {
		return nil, upstreamMeta{}, err
	}
	return artifact, meta, nil
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
