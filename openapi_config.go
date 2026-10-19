package veyro

import (
	"net/http"
	"strings"
)

// OpenAPIConfig holds everything about the generated document that cannot
// be derived from routes. Get it with App.OpenAPIConfig; every method
// returns the config for chaining:
//
//	app.OpenAPIConfig().
//	    Title("Users API").Version("2.1.0").
//	    Server("https://api.example.com", "production").
//	    BearerJWT()
//	app.ServeDocs(scalar.UI)
//
// Defaults: title "API", version "1.0.0", one server "/" and docs at /docs.
type OpenAPIConfig struct {
	info     Info
	servers  []Server
	path     string
	tags     []Tag
	security map[string]*SecurityScheme
	order    []string // security scheme names in insertion order
}

// Tag is an OpenAPI tag object.
type Tag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
}

// OAuthFlows is the OpenAPI OAuth flows object.
type OAuthFlows struct {
	Implicit          *OAuthFlow `json:"implicit,omitzero"`
	Password          *OAuthFlow `json:"password,omitzero"`
	ClientCredentials *OAuthFlow `json:"clientCredentials,omitzero"`
	AuthorizationCode *OAuthFlow `json:"authorizationCode,omitzero"`
}

// OAuthFlow is a single OAuth 2.0 flow.
type OAuthFlow struct {
	AuthorizationURL string            `json:"authorizationUrl,omitzero"`
	TokenURL         string            `json:"tokenUrl,omitzero"`
	RefreshURL       string            `json:"refreshUrl,omitzero"`
	Scopes           map[string]string `json:"scopes"`
}

func newOpenAPIConfig() *OpenAPIConfig {
	return &OpenAPIConfig{
		info:     Info{Title: "API", Version: "1.0.0"},
		servers:  []Server{{URL: "/"}},
		path:     "/docs",
		security: map[string]*SecurityScheme{},
	}
}

// OpenAPIConfig returns the document configuration, creating it with
// defaults on first call.
func (a *App) OpenAPIConfig() *OpenAPIConfig {
	if a.oapi == nil {
		a.oapi = newOpenAPIConfig()
	}
	return a.oapi
}

// Title sets info.title.
func (c *OpenAPIConfig) Title(s string) *OpenAPIConfig { c.info.Title = s; return c }

// Version sets info.version.
func (c *OpenAPIConfig) Version(s string) *OpenAPIConfig { c.info.Version = s; return c }

// Description sets info.description (CommonMark).
func (c *OpenAPIConfig) Description(s string) *OpenAPIConfig { c.info.Description = s; return c }

// TermsOfService sets info.termsOfService.
func (c *OpenAPIConfig) TermsOfService(url string) *OpenAPIConfig {
	c.info.TermsOfService = url
	return c
}

// Contact sets info.contact. Empty strings are omitted.
func (c *OpenAPIConfig) Contact(name, url, email string) *OpenAPIConfig {
	c.info.Contact = &Contact{Name: name, URL: url, Email: email}
	return c
}

// License sets info.license.
func (c *OpenAPIConfig) License(name, url string) *OpenAPIConfig {
	c.info.License = &License{Name: name, URL: url}
	return c
}

// Info replaces the whole info object.
func (c *OpenAPIConfig) Info(info Info) *OpenAPIConfig { c.info = info; return c }

// Server adds a server. The first call replaces the default "/".
func (c *OpenAPIConfig) Server(url, description string) *OpenAPIConfig {
	if len(c.servers) == 1 && c.servers[0] == (Server{URL: "/"}) {
		c.servers = nil
	}
	c.servers = append(c.servers, Server{URL: url, Description: description})
	return c
}

// Servers replaces the server list.
func (c *OpenAPIConfig) Servers(servers ...Server) *OpenAPIConfig { c.servers = servers; return c }

// Path sets where ServeDocs mounts the documentation. Default "/docs".
func (c *OpenAPIConfig) Path(p string) *OpenAPIConfig { c.path = strings.TrimSuffix(p, "/"); return c }

// Tag documents a tag used by routes (Router.Tags / Route.Tags).
func (c *OpenAPIConfig) Tag(name, description string) *OpenAPIConfig {
	for i := range c.tags {
		if c.tags[i].Name == name {
			c.tags[i].Description = description
			return c
		}
	}
	c.tags = append(c.tags, Tag{Name: name, Description: description})
	return c
}

// SecurityScheme adds or replaces a named security scheme. Secured routes
// (Require*) accept any registered scheme unless Route.Security says otherwise.
func (c *OpenAPIConfig) SecurityScheme(name string, scheme *SecurityScheme) *OpenAPIConfig {
	if _, ok := c.security[name]; !ok {
		c.order = append(c.order, name)
	}
	c.security[name] = scheme
	return c
}

// Basic documents HTTP Basic authentication as scheme "basic".
func (c *OpenAPIConfig) Basic() *OpenAPIConfig {
	return c.SecurityScheme("basic", &SecurityScheme{Type: "http", Scheme: "basic"})
}

// Bearer documents HTTP Bearer authentication as scheme "bearer".
// format is a hint such as "JWT" or "opaque"; empty omits it.
func (c *OpenAPIConfig) Bearer(format string) *OpenAPIConfig {
	return c.SecurityScheme("bearer", &SecurityScheme{Type: "http", Scheme: "bearer", BearerFormat: format})
}

// BearerJWT documents Bearer authentication with JWT tokens.
func (c *OpenAPIConfig) BearerJWT() *OpenAPIConfig { return c.Bearer("JWT") }

// APIKey documents an API key sent in a header, query parameter or cookie
// (in is "header", "query" or "cookie") as scheme "apiKey".
func (c *OpenAPIConfig) APIKey(name, in string) *OpenAPIConfig {
	return c.SecurityScheme("apiKey", &SecurityScheme{Type: "apiKey", Name: name, In: in})
}

// OIDC documents OpenID Connect discovery as scheme "oidc".
func (c *OpenAPIConfig) OIDC(discoveryURL string) *OpenAPIConfig {
	return c.SecurityScheme("oidc", &SecurityScheme{Type: "openIdConnect", OpenIDConnectURL: discoveryURL})
}

// OAuth2 documents OAuth 2.0 with the given flows as scheme "oauth2".
func (c *OpenAPIConfig) OAuth2(flows OAuthFlows) *OpenAPIConfig {
	return c.SecurityScheme("oauth2", &SecurityScheme{Type: "oauth2", Flows: &flows})
}

// OAuth2AuthorizationCode documents the authorization code flow.
func (c *OpenAPIConfig) OAuth2AuthorizationCode(authorizationURL, tokenURL string, scopes map[string]string) *OpenAPIConfig {
	return c.OAuth2(OAuthFlows{AuthorizationCode: &OAuthFlow{
		AuthorizationURL: authorizationURL, TokenURL: tokenURL, Scopes: nonNilScopes(scopes),
	}})
}

// OAuth2ClientCredentials documents the client credentials flow.
func (c *OpenAPIConfig) OAuth2ClientCredentials(tokenURL string, scopes map[string]string) *OpenAPIConfig {
	return c.OAuth2(OAuthFlows{ClientCredentials: &OAuthFlow{TokenURL: tokenURL, Scopes: nonNilScopes(scopes)}})
}

func nonNilScopes(s map[string]string) map[string]string {
	if s == nil {
		return map[string]string{}
	}
	return s
}

// securitySchemes merges explicitly configured schemes with those the
// authenticators passed to UseAuthentication describe. Explicit wins.
func (a *App) securitySchemes() (names []string, schemes map[string]*SecurityScheme) {
	cfg := a.OpenAPIConfig()
	schemes = map[string]*SecurityScheme{}
	for _, name := range cfg.order {
		schemes[name] = cfg.security[name]
		names = append(names, name)
	}
	for _, s := range a.schemes {
		if ss, ok := s.(SecuritySchemer); ok {
			name, scheme := ss.SecurityScheme()
			if _, exists := schemes[name]; !exists {
				schemes[name] = scheme
				names = append(names, name)
			}
		}
	}
	return names, schemes
}

// DocsUI builds the handler for an interactive API reference. It receives
// the document title, the URL of the OpenAPI JSON and the base path it is
// mounted on, and must serve the page at base and any assets below it.
// The scalar sub-package provides one that embeds Scalar with no external
// requests.
type DocsUI func(title, specURL, base string) http.Handler
