package server

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecrpublic"
	"golang.org/x/sync/singleflight"
)

const (
	ecrTokenTimeout         = 10 * time.Second
	ecrTokenSkew            = 5 * time.Minute
	ecrTokenFailureBackoff  = 30 * time.Second
	ecrDefaultTokenLifetime = 12 * time.Hour

	// ecrPublicKey caches the ECR Public token apart from private ECR ones.
	// It is not an AWS region, so it cannot collide with one.
	ecrPublicKey = "public"
	// ecrPublicRegion is the only region with the ECR Public API.
	ecrPublicRegion = "us-east-1"
)

// ecrTokens caches AWS ECR authorization tokens per region and refreshes them
// on demand when they expire. Tokens are obtained via the AWS SDK default
// credential chain, so IAM roles for service accounts, instance profiles, and
// environment credentials all work without extra configuration.
type ecrTokens struct {
	logger *slog.Logger

	mu    sync.Mutex
	cache map[string]ecrToken
	sf    singleflight.Group

	// getToken fetches a fresh authorization token for the given region and
	// returns the raw base64 "AWS:password" value plus its expiry. Overridable
	// in tests.
	getToken func(ctx context.Context, region string) (string, time.Time, error)
}

type ecrToken struct {
	value     string
	refreshAt time.Time
	expiresAt time.Time
}

func newECRTokens(logger *slog.Logger) *ecrTokens {
	return &ecrTokens{
		logger:   logger,
		cache:    make(map[string]ecrToken),
		getToken: fetchECRToken,
	}
}

// header returns an Authorization header for the given region, fetching and
// caching a token on first use and shortly before expiry. Concurrent refreshes
// for the same region share a single GetAuthorizationToken call. If a refresh
// fails, a cached token remains available until its actual expiry and another
// refresh is delayed briefly.
func (e *ecrTokens) header(region string) (name, value string) {
	if tok, ok := e.fresh(region); ok {
		return tok.header()
	}

	v, err, _ := e.sf.Do(region, func() (any, error) {
		if tok, ok := e.fresh(region); ok {
			return tok, nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), ecrTokenTimeout)
		defer cancel()

		raw, expiresAt, err := e.getToken(ctx, region)
		if err != nil {
			e.logger.Error("fetching ECR authorization token", "region", region, "error", err)
			return e.cacheFailure(region), nil
		}
		if raw == "" {
			e.logger.Error("ECR authorization token response was empty", "region", region)
			return e.cacheFailure(region), nil
		}

		tok := ecrToken{
			value:     "Basic " + raw,
			refreshAt: expiresAt.Add(-ecrTokenSkew),
			expiresAt: expiresAt,
		}
		e.store(region, tok)
		return tok, nil
	})
	if err != nil {
		return "", ""
	}

	return v.(ecrToken).header()
}

func (t ecrToken) header() (name, value string) {
	if t.value == "" {
		return "", ""
	}
	return "Authorization", t.value
}

func (e *ecrTokens) fresh(region string) (ecrToken, bool) {
	tok, ok := e.cached(region)
	return tok, ok && time.Now().Before(tok.refreshAt)
}

func (e *ecrTokens) cacheFailure(region string) ecrToken {
	now := time.Now()
	retryAt := now.Add(ecrTokenFailureBackoff)
	tok, ok := e.cached(region)
	if ok && now.Before(tok.expiresAt) {
		if retryAt.After(tok.expiresAt) {
			retryAt = tok.expiresAt
		}
		tok.refreshAt = retryAt
	} else {
		tok = ecrToken{refreshAt: retryAt, expiresAt: retryAt}
	}
	e.store(region, tok)
	return tok
}

func (e *ecrTokens) cached(region string) (ecrToken, bool) {
	e.mu.Lock()
	tok, ok := e.cache[region]
	e.mu.Unlock()
	return tok, ok
}

func (e *ecrTokens) store(region string, tok ecrToken) {
	e.mu.Lock()
	e.cache[region] = tok
	e.mu.Unlock()
}

func ecrRegion(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	labels := strings.Split(strings.ToLower(parsed.Hostname()), ".")
	for i := 1; i < len(labels); i++ {
		if labels[i] == "dkr" && i+4 < len(labels) && (labels[i+1] == "ecr" || labels[i+1] == "ecr-fips") {
			region := labels[i+2]
			suffix := strings.Join(labels[i+3:], ".")
			if region != "" && (suffix == "amazonaws.com" || suffix == "amazonaws.com.cn") {
				return region
			}
		}

		if (labels[i] == "dkr-ecr" || labels[i] == "dkr-ecr-fips") && i+3 < len(labels) {
			region := labels[i+1]
			if region != "" && strings.Join(labels[i+2:], ".") == "on.aws" {
				return region
			}
		}
	}

	return ""
}

// ecrPublic reports whether rawURL is on ECR Public, whose registry and
// token endpoint (public.ecr.aws/token/) share one host.
func ecrPublic(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(parsed.Hostname(), "public.ecr.aws")
}

// publicHeader returns the ECR Public header for rawURL. The registry
// answers 400 DENIED to Basic on /v2/ but takes the token as Bearer. Its
// /token/ endpoint takes Basic and returns the same value, so a 401
// challenge ends with the same Bearer.
func (e *ecrTokens) publicHeader(rawURL string) (name, value string) {
	name, value = e.header(ecrPublicKey)
	parsed, err := url.Parse(rawURL)
	if value == "" || err != nil || strings.HasPrefix(parsed.Path, "/token/") {
		return name, value
	}
	return name, "Bearer " + strings.TrimPrefix(value, "Basic ")
}

func fetchECRToken(ctx context.Context, region string) (string, time.Time, error) {
	if region == ecrPublicKey {
		return fetchECRPublicToken(ctx)
	}
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return "", time.Time{}, err
	}

	out, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", time.Time{}, err
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return "", time.Time{}, nil
	}

	data := out.AuthorizationData[0]
	expiresAt := time.Now().Add(ecrDefaultTokenLifetime)
	if data.ExpiresAt != nil {
		expiresAt = *data.ExpiresAt
	}
	return *data.AuthorizationToken, expiresAt, nil
}

// fetchECRPublicToken gets a token from the ecr-public API. Pulls work
// anonymously too, but an authenticated token raises the pull rate limit.
func fetchECRPublicToken(ctx context.Context) (string, time.Time, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(ecrPublicRegion))
	if err != nil {
		return "", time.Time{}, err
	}

	out, err := ecrpublic.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecrpublic.GetAuthorizationTokenInput{})
	if err != nil {
		return "", time.Time{}, err
	}
	data := out.AuthorizationData
	if data == nil || data.AuthorizationToken == nil {
		return "", time.Time{}, nil
	}

	expiresAt := time.Now().Add(ecrDefaultTokenLifetime)
	if data.ExpiresAt != nil {
		expiresAt = *data.ExpiresAt
	}
	return *data.AuthorizationToken, expiresAt, nil
}
