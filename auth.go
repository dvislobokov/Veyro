package veyro

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

// Standard claim types, matching the JWT registered names.
const (
	ClaimSubject = "sub"
	ClaimName    = "name"
	ClaimEmail   = "email"
	ClaimRole    = "role"
)

// Claim is a statement about a user: a type and a value.
type Claim struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// User is the authenticated principal, modelled on ClaimsPrincipal in
// ASP.NET Core. The zero value is an anonymous user.
type User struct {
	// Name is the display or login name (the "name" claim in JWTs).
	Name string `json:"name,omitzero"`
	// AuthType is the scheme that authenticated the user ("Bearer", "Cookie").
	// Empty means anonymous.
	AuthType string `json:"authType,omitzero"`
	// Claims carries everything else: subject, roles, e-mail, tenant...
	Claims []Claim `json:"claims,omitzero"`
}

var anonymous = &User{}

// Anonymous returns an unauthenticated user.
func Anonymous() *User { return &User{} }

// NewUser creates an authenticated user.
func NewUser(authType, name string, claims ...Claim) *User {
	return &User{Name: name, AuthType: authType, Claims: claims}
}

// IsAuthenticated reports whether the user was authenticated.
func (u *User) IsAuthenticated() bool { return u != nil && u.AuthType != "" }

// ID returns the subject ("sub") claim, or "".
func (u *User) ID() string {
	v, _ := u.FindFirst(ClaimSubject)
	return v
}

// Roles returns every "role" claim.
func (u *User) Roles() []string { return u.FindAll(ClaimRole) }

// IsInRole reports whether the user has the given role claim.
func (u *User) IsInRole(role string) bool { return u.HasClaim(ClaimRole, role) }

// HasClaim reports whether the user has a claim with this type and value.
func (u *User) HasClaim(typ, value string) bool {
	if u == nil {
		return false
	}
	return slices.Contains(u.Claims, Claim{Type: typ, Value: value})
}

// FindFirst returns the first claim value of the given type.
func (u *User) FindFirst(typ string) (string, bool) {
	if u == nil {
		return "", false
	}
	for _, c := range u.Claims {
		if c.Type == typ {
			return c.Value, true
		}
	}
	return "", false
}

// FindAll returns every claim value of the given type.
func (u *User) FindAll(typ string) []string {
	if u == nil {
		return nil
	}
	var out []string
	for _, c := range u.Claims {
		if c.Type == typ {
			out = append(out, c.Value)
		}
	}
	return out
}

// AddClaim appends a claim and returns u for chaining.
func (u *User) AddClaim(typ, value string) *User {
	u.Claims = append(u.Claims, Claim{Type: typ, Value: value})
	return u
}

// --- authentication ---

// Authenticator establishes the user for a request. It returns (nil, nil)
// when the request carries no credentials for this scheme, so the next
// scheme can be tried; an error means credentials were present but invalid.
type Authenticator interface {
	Authenticate(r *http.Request) (*User, error)
}

// Challenger is implemented by authenticators that can tell clients how to
// authenticate; the value goes into WWW-Authenticate on 401 responses.
type Challenger interface {
	Challenge() string
}

// AuthenticatorFunc adapts a function to Authenticator.
type AuthenticatorFunc func(r *http.Request) (*User, error)

// Authenticate implements Authenticator.
func (f AuthenticatorFunc) Authenticate(r *http.Request) (*User, error) { return f(r) }

type bearer struct {
	validate func(ctx context.Context, token string) (*User, error)
}

// Bearer authenticates requests carrying "Authorization: Bearer <token>".
// validate turns the token into a User (verify a JWT, look up an API key).
// The returned user's AuthType is set to "Bearer" when empty.
func Bearer(validate func(ctx context.Context, token string) (*User, error)) Authenticator {
	return bearer{validate: validate}
}

func (b bearer) Authenticate(r *http.Request) (*User, error) {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil, nil
	}
	u, err := b.validate(r.Context(), strings.TrimSpace(token))
	if err != nil || u == nil {
		return nil, err
	}
	if u.AuthType == "" {
		u.AuthType = "Bearer"
	}
	return u, nil
}

func (bearer) Challenge() string { return "Bearer" }

type basic struct {
	realm    string
	validate func(ctx context.Context, username, password string) (*User, error)
}

// Basic authenticates requests carrying HTTP Basic credentials. validate
// checks the username and password and returns the User; its AuthType is
// set to "Basic" when empty. realm goes into the WWW-Authenticate challenge.
func Basic(realm string, validate func(ctx context.Context, username, password string) (*User, error)) Authenticator {
	return basic{realm: realm, validate: validate}
}

func (b basic) Authenticate(r *http.Request) (*User, error) {
	username, password, ok := r.BasicAuth()
	if !ok {
		return nil, nil
	}
	u, err := b.validate(r.Context(), username, password)
	if err != nil || u == nil {
		return nil, err
	}
	if u.AuthType == "" {
		u.AuthType = "Basic"
	}
	return u, nil
}

func (b basic) Challenge() string {
	if b.realm == "" {
		return "Basic"
	}
	return `Basic realm="` + strings.ReplaceAll(b.realm, `"`, `\"`) + `"`
}

// SecurityScheme implements SecuritySchemer.
func (basic) SecurityScheme() (string, *SecurityScheme) {
	return "basic", &SecurityScheme{Type: "http", Scheme: "basic"}
}

type apiKey struct {
	name, in string
	validate func(ctx context.Context, key string) (*User, error)
}

// APIKey authenticates requests carrying a key in a header, query parameter
// or cookie (in is "header", "query" or "cookie"). The returned user's
// AuthType is set to "ApiKey" when empty.
func APIKey(name, in string, validate func(ctx context.Context, key string) (*User, error)) Authenticator {
	switch in {
	case "header", "query", "cookie":
	default:
		panic(`veyro: APIKey: in must be "header", "query" or "cookie"`)
	}
	return apiKey{name: name, in: in, validate: validate}
}

func (a apiKey) Authenticate(r *http.Request) (*User, error) {
	var key string
	switch a.in {
	case "header":
		key = r.Header.Get(a.name)
	case "query":
		key = r.URL.Query().Get(a.name)
	case "cookie":
		if c, err := r.Cookie(a.name); err == nil {
			key = c.Value
		}
	}
	if key == "" {
		return nil, nil
	}
	u, err := a.validate(r.Context(), key)
	if err != nil || u == nil {
		return nil, err
	}
	if u.AuthType == "" {
		u.AuthType = "ApiKey"
	}
	return u, nil
}

// SecurityScheme implements SecuritySchemer.
func (a apiKey) SecurityScheme() (string, *SecurityScheme) {
	return "apiKey", &SecurityScheme{Type: "apiKey", Name: a.name, In: a.in}
}

type (
	userKey      struct{}
	authStateKey struct{}
)

type authState struct {
	challenges []string
	err        error // credentials were present but rejected
}

// Authentication returns middleware that runs the authenticators in order
// and stores the first user found in the request context. Requests without
// credentials proceed as anonymous; rejecting them is Authorize's job, so
// public endpoints keep working.
func Authentication(auths ...Authenticator) Middleware {
	var challenges []string
	for _, a := range auths {
		if ch, ok := a.(Challenger); ok {
			challenges = append(challenges, ch.Challenge())
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			st := &authState{challenges: challenges}
			ctx := context.WithValue(r.Context(), authStateKey{}, st)
			for _, a := range auths {
				u, err := a.Authenticate(r)
				if err != nil {
					st.err = err
					break
				}
				if u != nil {
					ctx = context.WithValue(ctx, userKey{}, u)
					break
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserFrom returns the user stored by Authentication, or nil. Inside handlers
// prefer Context.User, which never returns nil.
func UserFrom(ctx context.Context) *User {
	u, _ := ctx.Value(userKey{}).(*User)
	return u
}

// --- authorization ---

// Policy decides whether a user may proceed.
type Policy func(u *User) bool

// InRole requires at least one of the roles.
func InRole(roles ...string) Policy {
	return func(u *User) bool {
		return slices.ContainsFunc(roles, u.IsInRole)
	}
}

// HasClaim requires a claim of the given type; when values are given, one
// of them must match.
func HasClaim(typ string, values ...string) Policy {
	return func(u *User) bool {
		if len(values) == 0 {
			_, ok := u.FindFirst(typ)
			return ok
		}
		return slices.ContainsFunc(values, func(v string) bool { return u.HasClaim(typ, v) })
	}
}

// Authorize returns middleware that requires an authenticated user and, when
// policies are given, that every policy passes. Anonymous requests get 401
// with a WWW-Authenticate challenge; authenticated users failing a policy
// get 403. Both are problem responses.
func Authorize(policies ...Policy) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rt := RouteFrom(r.Context()); rt != nil && rt.anonymous {
				next.ServeHTTP(w, r)
				return
			}
			app := appFrom(r.Context())
			u := UserFrom(r.Context())
			if !u.IsAuthenticated() {
				p := NewProblem(http.StatusUnauthorized, "")
				if st, _ := r.Context().Value(authStateKey{}).(*authState); st != nil {
					if st.err != nil {
						p.Detail = st.err.Error()
					}
					for _, ch := range st.challenges {
						w.Header().Add("WWW-Authenticate", ch)
					}
				}
				app.writeProblem(w, r, p)
				return
			}
			for _, policy := range policies {
				if !policy(u) {
					app.writeProblem(w, r, NewProblem(http.StatusForbidden, ""))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
