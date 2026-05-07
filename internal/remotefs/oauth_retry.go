package remotefs

import (
	"context"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// tokenCache is an [oauth2.TokenSource] with an [Invalidate] hook —
// drop-in replacement for the [oauth2.ReuseTokenSource] that
// [oauth2.NewClient] builds implicitly. Holding the cache in our own
// type lets a 401 from the upstream force the next request to refetch
// regardless of whether oauth2's expiry-based view says the token is
// still valid (server-side revocation, clock skew, an Expiry that
// over-promised relative to Google's accounting).
//
// Each cache miss spins up a one-shot inner refresher — a fresh
// [oauth2.Config.TokenSource] seeded with an already-expired token —
// so the inner source's own cache never holds anything across calls
// and Invalidate is the only knob that gates a real refresh.
type tokenCache struct {
	config       *oauth2.Config
	refreshToken string
	ctx          context.Context

	mu  sync.Mutex
	cur *oauth2.Token
}

func newTokenCache(ctx context.Context, oc *oauth2.Config, seed *oauth2.Token) *tokenCache {
	return &tokenCache{
		config:       oc,
		refreshToken: seed.RefreshToken,
		ctx:          ctx,
		cur:          seed,
	}
}

func (c *tokenCache) Token() (*oauth2.Token, error) {
	c.mu.Lock()
	if c.cur != nil && c.cur.Valid() {
		t := c.cur
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	// Refresh outside the lock — it's a network call.
	expired := &oauth2.Token{
		RefreshToken: c.refreshToken,
		Expiry:       time.Now().Add(-time.Hour),
	}
	t, err := c.config.TokenSource(c.ctx, expired).Token()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cur = t
	c.mu.Unlock()
	return t, nil
}

// Invalidate drops the cached token so the next [Token] call refreshes.
func (c *tokenCache) Invalidate() {
	c.mu.Lock()
	c.cur = nil
	c.mu.Unlock()
}

// retryOn401RoundTripper retries a request once when the upstream
// returns 401 — but only after invalidating the token cache so the
// retry uses a freshly-refreshed access token. Catches the case
// oauth2's default expiry-driven refresh misses: server-side
// revocation, clock skew, or an Expiry that over-promised. Requests
// with non-replayable bodies (streaming uploads with no GetBody)
// surface the 401 directly — retrying after consuming the body would
// re-send empty content.
type retryOn401RoundTripper struct {
	base       http.RoundTripper
	invalidate func()
}

func (r *retryOn401RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	if req.Body != nil && req.GetBody == nil {
		return resp, nil
	}
	_ = resp.Body.Close()
	r.invalidate()
	if req.GetBody != nil {
		body, berr := req.GetBody()
		if berr != nil {
			return nil, berr
		}
		req.Body = body
	}
	return r.base.RoundTrip(req)
}
