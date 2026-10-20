package jwt

import (
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// KeySource provides verification keys. Implementations: static keys or a
// JWKS endpoint that is fetched and refreshed.
type KeySource interface {
	// Keys returns the current keys. refresh asks for a fresh fetch
	// because a token named an unknown key id; sources may rate-limit it.
	Keys(ctx context.Context, refresh bool) ([]Key, error)
}

// StaticKeys is a KeySource over fixed keys.
type StaticKeys []Key

// Keys implements KeySource.
func (s StaticKeys) Keys(context.Context, bool) ([]Key, error) { return s, nil }

// Remote describes where to fetch keys. Either JWKSURL or DiscoveryURL
// is required. In a service mesh the host used to reach the identity
// provider usually differs from the issuer printed in tokens: fetch from
// the internal address and keep Config.Issuer as the public one.
type Remote struct {
	// JWKSURL is the key set endpoint. When DiscoveryURL is also set, it
	// overrides the jwks_uri found in the metadata.
	JWKSURL string
	// DiscoveryURL is the OpenID Connect metadata document, usually
	// "<issuer>/.well-known/openid-configuration". It may point to an
	// internal address.
	DiscoveryURL string
	// Rewrite adjusts URLs taken from the discovery document (jwks_uri)
	// before use, for example swapping the public host for the mesh one.
	Rewrite func(url string) string

	// HostHeader overrides the Host header of key requests, for ingress
	// routing by virtual host when the URL points at an internal address.
	HostHeader string
	// Headers are added to key requests (mesh auth, API keys).
	Headers map[string]string
	// TLS configures the connection (ServerName for SNI, RootCAs, client
	// certificates for mTLS). Ignored when Client is set.
	TLS *tls.Config
	// Client, when set, is used as is.
	Client *http.Client
	// Timeout bounds each request. Default 10s.
	Timeout time.Duration

	// RefreshInterval re-fetches keys in the background of requests at
	// most this often, so rotations are picked up. Default 1h.
	RefreshInterval time.Duration
	// MinRefreshInterval rate-limits refreshes triggered by unknown key
	// ids, which an attacker could otherwise spam. Default 1m.
	MinRefreshInterval time.Duration
}

// NewRemote builds a caching KeySource. The first fetch happens on the
// first token; call Prime to do it at startup instead.
func NewRemote(r Remote) *RemoteKeys {
	if r.JWKSURL == "" && r.DiscoveryURL == "" {
		panic("jwt: Remote needs JWKSURL or DiscoveryURL")
	}
	if r.Timeout <= 0 {
		r.Timeout = 10 * time.Second
	}
	if r.RefreshInterval <= 0 {
		r.RefreshInterval = time.Hour
	}
	if r.MinRefreshInterval <= 0 {
		r.MinRefreshInterval = time.Minute
	}
	if r.Client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if r.TLS != nil {
			tr.TLSClientConfig = r.TLS
		}
		r.Client = &http.Client{Transport: tr, Timeout: r.Timeout}
	}
	return &RemoteKeys{cfg: r}
}

// RemoteKeys is the KeySource returned by NewRemote.
type RemoteKeys struct {
	cfg Remote

	mu        sync.RWMutex
	keys      []Key
	jwksURL   string
	fetchedAt time.Time
	missAt    time.Time // last refresh triggered by an unknown key id
	issuer    string    // from discovery, informational
	inflight  chan struct{}
}

// Prime fetches the keys now, so startup fails fast on a bad URL.
func (r *RemoteKeys) Prime(ctx context.Context) error {
	return r.fetch(ctx)
}

// Issuer returns the issuer advertised by the discovery document, or "".
func (r *RemoteKeys) Issuer() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.issuer
}

// Keys implements KeySource with caching, periodic refresh and
// rate-limited refresh on unknown key ids.
func (r *RemoteKeys) Keys(ctx context.Context, refresh bool) ([]Key, error) {
	r.mu.RLock()
	keys, fetchedAt, missAt := r.keys, r.fetchedAt, r.missAt
	r.mu.RUnlock()

	switch {
	case fetchedAt.IsZero():
		if err := r.fetch(ctx); err != nil {
			return nil, err
		}
	case refresh:
		if time.Since(missAt) < r.cfg.MinRefreshInterval {
			return keys, nil // an attacker spamming unknown kids must not hammer the IdP
		}
		r.mu.Lock()
		r.missAt = time.Now()
		r.mu.Unlock()
		if err := r.fetch(ctx); err != nil {
			return keys, nil // keep serving the cached set
		}
	case time.Since(fetchedAt) >= r.cfg.RefreshInterval:
		go r.fetch(context.WithoutCancel(ctx)) // background, best effort
		return keys, nil
	default:
		return keys, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.keys, nil
}

// fetch loads the discovery document (once) and the key set, collapsing
// concurrent fetches into one.
func (r *RemoteKeys) fetch(ctx context.Context) error {
	r.mu.Lock()
	if r.inflight != nil {
		ch := r.inflight
		r.mu.Unlock()
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.inflight = make(chan struct{})
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		close(r.inflight)
		r.inflight = nil
		r.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	jwksURL := r.cfg.JWKSURL
	if jwksURL == "" {
		r.mu.RLock()
		jwksURL = r.jwksURL
		r.mu.RUnlock()
	}
	if jwksURL == "" {
		var meta struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := r.getJSON(ctx, r.cfg.DiscoveryURL, &meta); err != nil {
			return fmt.Errorf("jwt: discovery %s: %w", r.cfg.DiscoveryURL, err)
		}
		if meta.JWKSURI == "" {
			return errors.New("jwt: discovery document has no jwks_uri")
		}
		if r.cfg.Rewrite != nil {
			meta.JWKSURI = r.cfg.Rewrite(meta.JWKSURI)
		}
		jwksURL = meta.JWKSURI
		r.mu.Lock()
		r.jwksURL, r.issuer = jwksURL, meta.Issuer
		r.mu.Unlock()
	}

	body, err := r.get(ctx, jwksURL)
	if err != nil {
		return fmt.Errorf("jwt: JWKS %s: %w", jwksURL, err)
	}
	keys, err := ParseJWKS(body)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.keys, r.fetchedAt = keys, time.Now()
	r.mu.Unlock()
	return nil
}

func (r *RemoteKeys) getJSON(ctx context.Context, url string, v any) error {
	body, err := r.get(ctx, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func (r *RemoteKeys) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range r.cfg.Headers {
		req.Header.Set(k, v)
	}
	if r.cfg.HostHeader != "" {
		req.Host = r.cfg.HostHeader
	}
	resp, err := r.cfg.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// DiscoveryURL builds the OpenID Connect metadata URL for an issuer.
func DiscoveryURL(issuer string) string {
	return strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
}
