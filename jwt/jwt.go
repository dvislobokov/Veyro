// Package jwt verifies JSON Web Tokens with the standard library and turns
// them into Veyro users. Keys come from static material or from a JWKS
// endpoint, optionally located through OpenID Connect discovery, with
// caching, rotation and the knobs a service mesh needs (internal fetch
// address distinct from the public issuer, Host header, SNI, mTLS).
//
//	auth := jwt.New(jwt.Config{
//	    Issuer:   "https://auth.example.com/realms/shop",            // what tokens say
//	    Audience: []string{"orders-api"},
//	    Keys: jwt.NewRemote(jwt.Remote{
//	        DiscoveryURL: "http://keycloak.auth.svc:8080/realms/shop/.well-known/openid-configuration", // where to fetch
//	        Rewrite: func(u string) string { return strings.Replace(u, "https://auth.example.com", "http://keycloak.auth.svc:8080", 1) },
//	    }),
//	})
//	app.UseAuthentication(auth)
package jwt

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dvislobokov/veyro"
)

// Errors returned by Verify; the Authenticator reports them to clients
// through the 401 problem detail.
var (
	ErrMalformed   = errors.New("token is malformed")
	ErrAlgorithm   = errors.New("token algorithm is not allowed")
	ErrKeyNotFound = errors.New("no key matches the token")
	ErrSignature   = errors.New("token signature is invalid")
	ErrExpired     = errors.New("token has expired")
	ErrNotYet      = errors.New("token is not valid yet")
	ErrIssuer      = errors.New("token issuer is not trusted")
	ErrAudience    = errors.New("token audience does not match")
	ErrClaim       = errors.New("token is missing a required claim")
)

// Config describes how tokens are verified and mapped to users.
type Config struct {
	// Keys verifies signatures. Required.
	Keys KeySource
	// Algorithms allowed in tokens. Default: every asymmetric algorithm
	// (RS*, PS*, ES*, EdDSA). Add HS* explicitly when using shared secrets.
	Algorithms []Algorithm

	// Issuer, or Issuers, are the trusted "iss" values exactly as printed in
	// tokens, which in a mesh is the public URL even when keys are fetched
	// internally. Empty skips the check.
	Issuer  string
	Issuers []string
	// Audience lists accepted "aud" values; a token matches when any of its
	// audiences is listed. Empty skips the check.
	Audience []string
	// RequireAudience rejects tokens without "aud" even when Audience is empty.
	RequireAudience bool
	// Leeway tolerates clock skew on exp, nbf and iat. Default 30s.
	Leeway time.Duration
	// MaxAge rejects tokens whose iat is older than this. Zero disables.
	MaxAge time.Duration
	// RequireExpiry rejects tokens without "exp". Default true (set
	// AllowNoExpiry to accept them).
	AllowNoExpiry bool
	// RequiredClaims must be present (dotted paths allowed).
	RequiredClaims []string

	// Sources extract the raw token from a request, tried in order.
	// Default: the Authorization: Bearer header.
	Sources []TokenSource
	// Realm goes into the WWW-Authenticate challenge.
	Realm string
	// Scheme is the AuthType stamped on users. Default "Bearer".
	Scheme string

	// NameClaims are tried in order for User.Name. Default: name,
	// preferred_username, email, sub.
	NameClaims []string
	// RoleClaims are read for role claims (strings or arrays, dotted paths
	// for nested objects). Default covers Keycloak, Auth0-style, Cognito and
	// plain "roles"/"groups": roles, role, groups, realm_access.roles,
	// cognito:groups.
	RoleClaims []string
	// ClientRoleClaims reads Keycloak-style per-client roles from
	// resource_access.<client>.roles for each listed client.
	ClientRoleClaims []string
	// ScopeClaims are space-separated scope strings mapped to "scope"
	// claims. Default: scope, scp.
	ScopeClaims []string
	// MapUser, when set, replaces the default claim mapping entirely.
	MapUser func(ctx context.Context, t *Token) (*veyro.User, error)
	// Validate runs extra checks after the standard ones (tenant, banned
	// users, ...). Returning an error rejects the token.
	Validate func(ctx context.Context, t *Token) error
}

// TokenSource extracts a raw token from a request, or "" when absent.
type TokenSource func(r *http.Request) string

// FromHeader reads "<scheme> <token>" from a header (Authorization: Bearer).
func FromHeader(header, scheme string) TokenSource {
	return func(r *http.Request) string {
		s, token, ok := strings.Cut(r.Header.Get(header), " ")
		if !ok || !strings.EqualFold(s, scheme) {
			return ""
		}
		return strings.TrimSpace(token)
	}
}

// FromCookie reads the token from a cookie.
func FromCookie(name string) TokenSource {
	return func(r *http.Request) string {
		if c, err := r.Cookie(name); err == nil {
			return c.Value
		}
		return ""
	}
}

// FromQuery reads the token from a query parameter (for websockets and
// downloads; avoid it elsewhere, URLs end up in logs).
func FromQuery(name string) TokenSource {
	return func(r *http.Request) string { return r.URL.Query().Get(name) }
}

// Token is a verified token.
type Token struct {
	Raw       string
	Header    map[string]any
	Claims    Claims
	Algorithm Algorithm
	KeyID     string
}

// Claims is the token payload. Nested objects are reachable with dotted
// paths through Get.
type Claims map[string]any

// Get returns a claim by name or dotted path ("realm_access.roles").
func (c Claims) Get(path string) (any, bool) {
	var cur any = map[string]any(c)
	for part := range strings.SplitSeq(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// String returns a claim as a string (numbers and bools are formatted).
func (c Claims) String(path string) string {
	v, ok := c.Get(path)
	if !ok || v == nil {
		return ""
	}
	return claimString(v)
}

// Strings returns a claim that is a string or an array of strings.
func (c Claims) Strings(path string) []string {
	v, ok := c.Get(path)
	if !ok {
		return nil
	}
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, claimString(e))
		}
		return out
	}
	return nil
}

// Time returns a numeric date claim (exp, iat, nbf).
func (c Claims) Time(path string) (time.Time, bool) {
	v, ok := c.Get(path)
	if !ok {
		return time.Time{}, false
	}
	switch x := v.(type) {
	case float64:
		sec, frac := int64(x), x-float64(int64(x))
		return time.Unix(sec, int64(frac*1e9)), true
	case int64:
		return time.Unix(x, 0), true
	case string:
		if n, err := strconv.ParseFloat(x, 64); err == nil {
			return time.Unix(int64(n), 0), true
		}
	}
	return time.Time{}, false
}

func claimString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Verifier verifies tokens according to a Config.
type Verifier struct {
	cfg Config
	now func() time.Time
}

var defaultAlgorithms = []Algorithm{RS256, RS384, RS512, PS256, PS384, PS512, ES256, ES384, ES512, EdDSA}

// NewVerifier prepares a Verifier. It panics without Keys.
func NewVerifier(cfg Config) *Verifier {
	if cfg.Keys == nil {
		panic("jwt: Config.Keys is required")
	}
	if len(cfg.Algorithms) == 0 {
		cfg.Algorithms = defaultAlgorithms
	}
	if cfg.Leeway == 0 {
		cfg.Leeway = 30 * time.Second
	}
	if cfg.Issuer != "" {
		cfg.Issuers = append(cfg.Issuers, cfg.Issuer)
	}
	if len(cfg.Sources) == 0 {
		cfg.Sources = []TokenSource{FromHeader("Authorization", "Bearer")}
	}
	if cfg.Scheme == "" {
		cfg.Scheme = "Bearer"
	}
	if len(cfg.NameClaims) == 0 {
		cfg.NameClaims = []string{"name", "preferred_username", "email", "sub"}
	}
	if len(cfg.RoleClaims) == 0 {
		cfg.RoleClaims = []string{"roles", "role", "groups", "realm_access.roles", "cognito:groups"}
	}
	if len(cfg.ScopeClaims) == 0 {
		cfg.ScopeClaims = []string{"scope", "scp"}
	}
	return &Verifier{cfg: cfg, now: time.Now}
}

// Verify parses and verifies a compact JWS and validates its claims.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Token, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, ErrMalformed
	}
	alg := Algorithm(header.Alg)
	if !slices.Contains(v.cfg.Algorithms, alg) {
		return nil, ErrAlgorithm
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformed
	}
	signingInput := []byte(raw[:len(parts[0])+1+len(parts[1])])

	if err := v.verifySignature(ctx, alg, header.Kid, signingInput, sig); err != nil {
		return nil, err
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformed
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, ErrMalformed
	}
	var hdr map[string]any
	_ = json.Unmarshal(headerJSON, &hdr)
	t := &Token{Raw: raw, Header: hdr, Claims: claims, Algorithm: alg, KeyID: header.Kid}
	if err := v.validateClaims(t); err != nil {
		return nil, err
	}
	if v.cfg.Validate != nil {
		if err := v.cfg.Validate(ctx, t); err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (v *Verifier) verifySignature(ctx context.Context, alg Algorithm, kid string, input, sig []byte) error {
	try := func(keys []Key) (matched bool, err error) {
		for i := range keys {
			k := &keys[i]
			if kid != "" && k.ID != "" && k.ID != kid {
				continue
			}
			if !k.supports(alg) {
				continue
			}
			matched = true
			if err := k.verify(alg, input, sig); err == nil {
				return true, nil
			}
		}
		if matched {
			return true, ErrSignature
		}
		return false, ErrKeyNotFound
	}
	keys, err := v.cfg.Keys.Keys(ctx, false)
	if err != nil {
		return fmt.Errorf("jwt: keys unavailable: %w", err)
	}
	matched, err := try(keys)
	if err == nil {
		return nil
	}
	if !matched { // unknown kid: maybe the keys rotated
		if fresh, ferr := v.cfg.Keys.Keys(ctx, true); ferr == nil {
			if _, err2 := try(fresh); err2 == nil {
				return nil
			} else {
				return err2
			}
		}
	}
	return err
}

func (v *Verifier) validateClaims(t *Token) error {
	now := v.now()
	c := t.Claims
	if exp, ok := c.Time("exp"); ok {
		if now.After(exp.Add(v.cfg.Leeway)) {
			return ErrExpired
		}
	} else if !v.cfg.AllowNoExpiry {
		return fmt.Errorf("%w: exp", ErrClaim)
	}
	if nbf, ok := c.Time("nbf"); ok && now.Add(v.cfg.Leeway).Before(nbf) {
		return ErrNotYet
	}
	if iat, ok := c.Time("iat"); ok {
		if now.Add(v.cfg.Leeway).Before(iat) {
			return ErrNotYet
		}
		if v.cfg.MaxAge > 0 && now.Sub(iat) > v.cfg.MaxAge+v.cfg.Leeway {
			return ErrExpired
		}
	}
	if len(v.cfg.Issuers) > 0 && !slices.Contains(v.cfg.Issuers, c.String("iss")) {
		return ErrIssuer
	}
	if len(v.cfg.Audience) > 0 || v.cfg.RequireAudience {
		aud := c.Strings("aud")
		if len(aud) == 0 {
			return ErrAudience
		}
		if len(v.cfg.Audience) > 0 && !slices.ContainsFunc(aud, func(a string) bool { return slices.Contains(v.cfg.Audience, a) }) {
			return ErrAudience
		}
	}
	for _, path := range v.cfg.RequiredClaims {
		if _, ok := c.Get(path); !ok {
			return fmt.Errorf("%w: %s", ErrClaim, path)
		}
	}
	return nil
}

// User maps a verified token to a Veyro user using the configured claims.
func (v *Verifier) User(ctx context.Context, t *Token) (*veyro.User, error) {
	if v.cfg.MapUser != nil {
		return v.cfg.MapUser(ctx, t)
	}
	u := &veyro.User{AuthType: v.cfg.Scheme}
	for _, nc := range v.cfg.NameClaims {
		if s := t.Claims.String(nc); s != "" {
			u.Name = s
			break
		}
	}
	for k, val := range t.Claims {
		flatten(u, k, val)
	}
	for _, rc := range v.cfg.RoleClaims {
		for _, role := range t.Claims.Strings(rc) {
			if !u.HasClaim(veyro.ClaimRole, role) {
				u.AddClaim(veyro.ClaimRole, role)
			}
		}
	}
	for _, client := range v.cfg.ClientRoleClaims {
		for _, role := range t.Claims.Strings("resource_access." + client + ".roles") {
			if !u.HasClaim(veyro.ClaimRole, role) {
				u.AddClaim(veyro.ClaimRole, role)
			}
		}
	}
	for _, sc := range v.cfg.ScopeClaims {
		for _, s := range t.Claims.Strings(sc) {
			for scope := range strings.FieldsSeq(s) {
				if !u.HasClaim("scope", scope) {
					u.AddClaim("scope", scope)
				}
			}
		}
	}
	return u, nil
}

// flatten copies a claim into the user: scalars as one claim, arrays as
// one claim per element, objects as dotted keys.
func flatten(u *veyro.User, key string, val any) {
	switch x := val.(type) {
	case map[string]any:
		for k, v := range x {
			flatten(u, key+"."+k, v)
		}
	case []any:
		for _, e := range x {
			if _, nested := e.(map[string]any); nested {
				u.AddClaim(key, claimString(e))
			} else {
				u.AddClaim(key, claimString(e))
			}
		}
	default:
		u.AddClaim(key, claimString(val))
	}
}

// Authenticator is a veyro.Authenticator backed by a Verifier.
type Authenticator struct {
	*Verifier
}

// New builds an Authenticator for veyro.UseAuthentication.
func New(cfg Config) *Authenticator { return &Authenticator{Verifier: NewVerifier(cfg)} }

// Authenticate implements veyro.Authenticator: no token means anonymous,
// an invalid token is an error reported where authorization is required.
func (a *Authenticator) Authenticate(r *http.Request) (*veyro.User, error) {
	var raw string
	for _, src := range a.cfg.Sources {
		if raw = src(r); raw != "" {
			break
		}
	}
	if raw == "" {
		return nil, nil
	}
	t, err := a.Verify(r.Context(), raw)
	if err != nil {
		return nil, err
	}
	return a.User(r.Context(), t)
}

// Challenge implements veyro.Challenger.
func (a *Authenticator) Challenge() string {
	if a.cfg.Realm == "" {
		return a.cfg.Scheme
	}
	return a.cfg.Scheme + ` realm="` + a.cfg.Realm + `"`
}

// SecurityScheme implements veyro.SecuritySchemer: HTTP bearer with JWT.
func (a *Authenticator) SecurityScheme() (string, *veyro.SecurityScheme) {
	desc := ""
	if len(a.cfg.Issuers) > 0 {
		desc = "JWT issued by " + strings.Join(a.cfg.Issuers, ", ")
	}
	return "bearer", &veyro.SecurityScheme{Type: "http", Scheme: "bearer", BearerFormat: "JWT", Description: desc}
}
