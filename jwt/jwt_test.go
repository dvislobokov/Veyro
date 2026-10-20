package jwt_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dvislobokov/srog"
	"github.com/dvislobokov/veyro"
	"github.com/dvislobokov/veyro/jwt"
)

// ---- a tiny signer for tests ----

type signer struct {
	kid string
	alg jwt.Algorithm
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
	ed  ed25519.PrivateKey
	hs  []byte
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (s *signer) sign(claims map[string]any) string {
	header, _ := json.Marshal(map[string]any{"alg": string(s.alg), "kid": s.kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	input := b64(header) + "." + b64(payload)
	digest := sha256.Sum256([]byte(input))
	var sig []byte
	switch s.alg {
	case jwt.RS256:
		sig, _ = rsa.SignPKCS1v15(rand.Reader, s.rsa, crypto.SHA256, digest[:])
	case jwt.PS256:
		sig, _ = rsa.SignPSS(rand.Reader, s.rsa, crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: 32})
	case jwt.ES256:
		r, sv, _ := ecdsa.Sign(rand.Reader, s.ec, digest[:])
		sig = append(leftPad(r.Bytes(), 32), leftPad(sv.Bytes(), 32)...)
	case jwt.EdDSA:
		sig = ed25519.Sign(s.ed, []byte(input))
	case jwt.HS256:
		m := hmac.New(sha256.New, s.hs)
		m.Write([]byte(input))
		sig = m.Sum(nil)
	}
	return input + "." + b64(sig)
}

func leftPad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	return append(make([]byte, n-len(b)), b...)
}

func (s *signer) jwk() map[string]any {
	switch s.alg {
	case jwt.RS256, jwt.PS256:
		return map[string]any{"kty": "RSA", "kid": s.kid, "use": "sig", "n": b64(s.rsa.N.Bytes()), "e": b64(big.NewInt(int64(s.rsa.E)).Bytes())}
	case jwt.ES256:
		return map[string]any{"kty": "EC", "kid": s.kid, "crv": "P-256", "x": b64(leftPad(s.ec.X.Bytes(), 32)), "y": b64(leftPad(s.ec.Y.Bytes(), 32))}
	case jwt.EdDSA:
		return map[string]any{"kty": "OKP", "kid": s.kid, "crv": "Ed25519", "x": b64(s.ed.Public().(ed25519.PublicKey))}
	}
	return nil
}

func newRSA(t *testing.T, kid string, alg jwt.Algorithm) *signer {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{kid: kid, alg: alg, rsa: k}
}

func newEC(t *testing.T, kid string) *signer {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return &signer{kid: kid, alg: jwt.ES256, ec: k}
}

func newEd(t *testing.T, kid string) *signer {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	return &signer{kid: kid, alg: jwt.EdDSA, ed: priv}
}

func claims(extra map[string]any) map[string]any {
	c := map[string]any{"iss": "https://auth.example.com/realms/shop", "sub": "u42", "aud": []string{"orders-api", "web"},
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "name": "Ann"}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

// jwksServer serves a key set and a discovery document, counting hits.
type jwksServer struct {
	*httptest.Server
	keys       atomic.Pointer[[]map[string]any]
	jwksHits   atomic.Int32
	discoHits  atomic.Int32
	lastHost   atomic.Pointer[string]
	lastHeader atomic.Pointer[string]
}

func newJWKS(t *testing.T, signers ...*signer) *jwksServer {
	s := &jwksServer{}
	s.set(signers...)
	mux := http.NewServeMux()
	mux.HandleFunc("/certs", func(w http.ResponseWriter, r *http.Request) {
		s.jwksHits.Add(1)
		h := r.Host
		s.lastHost.Store(&h)
		x := r.Header.Get("X-Mesh-Token")
		s.lastHeader.Store(&x)
		_ = json.MarshalWrite(w, map[string]any{"keys": *s.keys.Load()})
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		s.discoHits.Add(1)
		// The public issuer advertises a public jwks_uri that is unreachable from the mesh.
		_ = json.MarshalWrite(w, map[string]any{"issuer": "https://auth.example.com/realms/shop", "jwks_uri": "https://auth.example.com/realms/shop/certs"})
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) set(signers ...*signer) {
	var keys []map[string]any
	for _, sg := range signers {
		keys = append(keys, sg.jwk())
	}
	s.keys.Store(&keys)
}

func TestVerifyAlgorithmsAndClaims(t *testing.T) {
	rs, ps, es, ed := newRSA(t, "rs", jwt.RS256), newRSA(t, "ps", jwt.PS256), newEC(t, "es"), newEd(t, "ed")
	srv := newJWKS(t, rs, ps, es, ed)
	v := jwt.NewVerifier(jwt.Config{
		Keys:     jwt.NewRemote(jwt.Remote{JWKSURL: srv.URL + "/certs"}),
		Issuer:   "https://auth.example.com/realms/shop",
		Audience: []string{"orders-api"},
	})
	ctx := context.Background()
	for _, s := range []*signer{rs, ps, es, ed} {
		tok, err := v.Verify(ctx, s.sign(claims(nil)))
		if err != nil {
			t.Errorf("%s: %v", s.alg, err)
			continue
		}
		if tok.Claims.String("sub") != "u42" || tok.KeyID != s.kid || tok.Algorithm != s.alg {
			t.Errorf("%s: token %+v", s.alg, tok)
		}
	}
	if srv.jwksHits.Load() != 1 {
		t.Errorf("keys must be cached, got %d fetches", srv.jwksHits.Load())
	}

	bad := map[string]struct {
		token string
		err   error
	}{
		"expired":        {rs.sign(claims(map[string]any{"exp": time.Now().Add(-2 * time.Minute).Unix()})), jwt.ErrExpired},
		"within leeway":  {rs.sign(claims(map[string]any{"exp": time.Now().Add(-10 * time.Second).Unix()})), nil},
		"not yet":        {rs.sign(claims(map[string]any{"nbf": time.Now().Add(5 * time.Minute).Unix()})), jwt.ErrNotYet},
		"no exp":         {rs.sign(map[string]any{"iss": "https://auth.example.com/realms/shop", "aud": "orders-api"}), jwt.ErrClaim},
		"wrong issuer":   {rs.sign(claims(map[string]any{"iss": "https://evil.example.com"})), jwt.ErrIssuer},
		"wrong audience": {rs.sign(claims(map[string]any{"aud": "other"})), jwt.ErrAudience},
		"malformed":      {"abc.def", jwt.ErrMalformed},
		"alg none":       {b64([]byte(`{"alg":"none"}`)) + "." + b64([]byte(`{}`)) + ".", jwt.ErrAlgorithm},
		"hs with pubkey": {(&signer{kid: "rs", alg: jwt.HS256, hs: []byte("x")}).sign(claims(nil)), jwt.ErrAlgorithm},
		"tampered":       {tamper(rs.sign(claims(nil))), jwt.ErrSignature},
		"unknown kid":    {newRSA(t, "zzz", jwt.RS256).sign(claims(nil)), jwt.ErrKeyNotFound},
	}
	for name, tc := range bad {
		_, err := v.Verify(ctx, tc.token)
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: got %v, want %v", name, err, tc.err)
		}
	}
}

func tamper(tok string) string {
	parts := strings.Split(tok, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	payload = []byte(strings.Replace(string(payload), `"u42"`, `"u1"`, 1))
	parts[1] = b64(payload)
	return strings.Join(parts, ".")
}

func TestHMACRequiresExplicitAlgorithm(t *testing.T) {
	hs := &signer{kid: "k1", alg: jwt.HS256, hs: []byte("0123456789abcdef0123456789abcdef")}
	keys := jwt.StaticKeys{jwt.HMACKey("k1", hs.hs)}
	if _, err := jwt.NewVerifier(jwt.Config{Keys: keys}).Verify(context.Background(), hs.sign(claims(nil))); !errors.Is(err, jwt.ErrAlgorithm) {
		t.Errorf("HS256 must be opt-in: %v", err)
	}
	v := jwt.NewVerifier(jwt.Config{Keys: keys, Algorithms: []jwt.Algorithm{jwt.HS256}})
	if _, err := v.Verify(context.Background(), hs.sign(claims(nil))); err != nil {
		t.Error(err)
	}
}

func TestKeyRotationAndRefreshRateLimit(t *testing.T) {
	old := newRSA(t, "old", jwt.RS256)
	srv := newJWKS(t, old)
	v := jwt.NewVerifier(jwt.Config{Keys: jwt.NewRemote(jwt.Remote{JWKSURL: srv.URL + "/certs", MinRefreshInterval: 300 * time.Millisecond})})
	ctx := context.Background()
	if _, err := v.Verify(ctx, old.sign(claims(nil))); err != nil {
		t.Fatal(err)
	}
	// Rotate: a token with a new kid triggers one refresh...
	fresh := newRSA(t, "new", jwt.RS256)
	srv.set(old, fresh)
	unknown := newRSA(t, "nope", jwt.RS256)
	if _, err := v.Verify(ctx, unknown.sign(claims(nil))); !errors.Is(err, jwt.ErrKeyNotFound) {
		t.Errorf("unknown after refresh: %v", err)
	}
	hits := srv.jwksHits.Load()
	if hits != 2 {
		t.Errorf("unknown kid must refresh once, fetches=%d", hits)
	}
	// ...but repeated unknown kids are rate-limited.
	for range 5 {
		_, _ = v.Verify(ctx, unknown.sign(claims(nil)))
	}
	if srv.jwksHits.Load() != 2 {
		t.Errorf("refresh must be rate-limited, fetches=%d", srv.jwksHits.Load())
	}
	// The rotated key already works without another fetch (it came with the refresh).
	if _, err := v.Verify(ctx, fresh.sign(claims(nil))); err != nil {
		t.Errorf("rotated key: %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	_, _ = v.Verify(ctx, unknown.sign(claims(nil)))
	if srv.jwksHits.Load() != 3 {
		t.Errorf("refresh allowed again after MinRefreshInterval, fetches=%d", srv.jwksHits.Load())
	}
}

func TestDiscoveryInsideServiceMesh(t *testing.T) {
	rs := newRSA(t, "rs", jwt.RS256)
	srv := newJWKS(t, rs)
	remote := jwt.NewRemote(jwt.Remote{
		// Discovery is reached at the in-mesh address; the metadata still
		// advertises the public issuer and a public jwks_uri.
		DiscoveryURL: srv.URL + "/.well-known/openid-configuration",
		Rewrite:      func(u string) string { return strings.Replace(u, "https://auth.example.com/realms/shop", srv.URL, 1) },
		HostHeader:   "auth.example.com",
		Headers:      map[string]string{"X-Mesh-Token": "secret"},
	})
	if err := remote.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if remote.Issuer() != "https://auth.example.com/realms/shop" {
		t.Errorf("issuer from discovery: %q", remote.Issuer())
	}
	if h := *srv.lastHost.Load(); h != "auth.example.com" {
		t.Errorf("Host header override: %q", h)
	}
	if x := *srv.lastHeader.Load(); x != "secret" {
		t.Errorf("custom header: %q", x)
	}
	v := jwt.NewVerifier(jwt.Config{Keys: remote, Issuer: "https://auth.example.com/realms/shop"})
	if _, err := v.Verify(context.Background(), rs.sign(claims(nil))); err != nil {
		t.Error(err)
	}
	if srv.discoHits.Load() != 1 || srv.jwksHits.Load() != 1 {
		t.Errorf("discovery once, jwks once: %d/%d", srv.discoHits.Load(), srv.jwksHits.Load())
	}
	// JWKSURL wins over the discovery document.
	override := jwt.NewRemote(jwt.Remote{DiscoveryURL: "http://127.0.0.1:1/nope", JWKSURL: srv.URL + "/certs"})
	if err := override.Prime(context.Background()); err != nil {
		t.Errorf("JWKSURL must bypass discovery: %v", err)
	}
}

func TestUserMapping(t *testing.T) {
	rs := newRSA(t, "rs", jwt.RS256)
	v := jwt.NewVerifier(jwt.Config{
		Keys:             jwt.StaticKeys{jwt.PublicKey("rs", &rs.rsa.PublicKey)},
		ClientRoleClaims: []string{"orders-api"},
	})
	tok, err := v.Verify(context.Background(), rs.sign(claims(map[string]any{
		"preferred_username": "ann",
		"email":              "ann@example.com",
		"realm_access":       map[string]any{"roles": []string{"user", "offline_access"}},
		"resource_access":    map[string]any{"orders-api": map[string]any{"roles": []string{"orders:write"}}},
		"scope":              "openid profile orders.read",
		"tenant":             map[string]any{"id": 7, "plan": "pro"},
		"email_verified":     true,
	})))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := v.User(context.Background(), tok)
	if !u.IsAuthenticated() || u.AuthType != "Bearer" || u.Name != "Ann" || u.ID() != "u42" {
		t.Errorf("user: %+v", u)
	}
	for _, role := range []string{"user", "offline_access", "orders:write"} {
		if !u.IsInRole(role) {
			t.Errorf("missing role %s: %v", role, u.Roles())
		}
	}
	if !u.HasClaim("scope", "orders.read") || !u.HasClaim("tenant.id", "7") || !u.HasClaim("tenant.plan", "pro") ||
		!u.HasClaim("email_verified", "true") || !u.HasClaim("aud", "web") {
		t.Errorf("flattened claims: %+v", u.Claims)
	}
	if got, _ := tok.Claims.Get("tenant.plan"); got != "pro" {
		t.Errorf("Claims.Get: %v", got)
	}
}

func TestVeyroIntegration(t *testing.T) {
	rs := newRSA(t, "rs", jwt.RS256)
	srv := newJWKS(t, rs)
	app := veyro.New(veyro.WithLogger(srog.MustNew(srog.WithWriter(io.Discard))))
	app.UseAuthentication(jwt.New(jwt.Config{
		Keys:     jwt.NewRemote(jwt.Remote{JWKSURL: srv.URL + "/certs"}),
		Issuer:   "https://auth.example.com/realms/shop",
		Audience: []string{"orders-api"},
		Realm:    "orders",
		Sources:  []jwt.TokenSource{jwt.FromHeader("Authorization", "Bearer"), jwt.FromCookie("access_token")},
	}))
	app.Get("/me", func(c *veyro.Context, _ veyro.None) (string, error) { return c.User().Name, nil }).RequireAuthorization()
	app.Get("/admin", func(c *veyro.Context, _ veyro.None) (string, error) { return "ok", nil }).RequireRole("admin")

	do := func(path string, hdr ...string) (int, string, http.Header) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String(), rec.Header()
	}
	good := rs.sign(claims(map[string]any{"roles": []string{"admin"}}))
	if code, body, _ := do("/me", "Authorization", "Bearer "+good); code != 200 || body != `"Ann"` {
		t.Errorf("bearer: %d %s", code, body)
	}
	if code, _, _ := do("/me", "Cookie", "access_token="+good); code != 200 {
		t.Errorf("cookie source: %d", code)
	}
	if code, _, _ := do("/admin", "Authorization", "Bearer "+good); code != 200 {
		t.Errorf("role from token: %d", code)
	}
	code, body, h := do("/me")
	if code != 401 || h.Get("WWW-Authenticate") != `Bearer realm="orders"` {
		t.Errorf("anonymous: %d %q %s", code, h.Get("WWW-Authenticate"), body)
	}
	expired := rs.sign(claims(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}))
	if code, body, _ := do("/me", "Authorization", "Bearer "+expired); code != 401 || !strings.Contains(body, "token has expired") {
		t.Errorf("expired: %d %s", code, body)
	}
	if ss := app.OpenAPI().Components.SecuritySchemes["bearer"]; ss == nil || ss.BearerFormat != "JWT" || !strings.Contains(ss.Description, "auth.example.com") {
		t.Errorf("security scheme: %+v", ss)
	}
}

func TestParsePEM(t *testing.T) {
	rs := newRSA(t, "rs", jwt.RS256)
	der, _ := x509MarshalPKIX(&rs.rsa.PublicKey)
	pemData := "-----BEGIN PUBLIC KEY-----\n" + base64.StdEncoding.EncodeToString(der) + "\n-----END PUBLIC KEY-----\n"
	key, err := jwt.ParsePEM("rs", []byte(pemData))
	if err != nil {
		t.Fatal(err)
	}
	v := jwt.NewVerifier(jwt.Config{Keys: jwt.StaticKeys{key}})
	if _, err := v.Verify(context.Background(), rs.sign(claims(nil))); err != nil {
		t.Error(err)
	}
}
