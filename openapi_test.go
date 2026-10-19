package veyro_test

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/dvislobokov/veyro"
)

type Role string

func (Role) EnumValues() []string { return []string{"admin", "user"} }

type Profile struct {
	Bio     string    `json:"bio,omitzero" doc:"Short biography" example:"Gopher since 2012"`
	Website *string   `json:"website" format:"uri"`
	Joined  time.Time `json:"joined"`
}

type Account struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name" validate:"required,minlen=2,maxlen=64" example:"Ann"`
	Role    Role      `json:"role" default:"user"`
	Status  string    `json:"status" enum:"active,blocked" default:"active" doc:"Lifecycle state"`
	Age     *int      `json:"age" validate:"min=18,max=130" example:"30"`
	Tags    []string  `json:"tags" validate:"maxlen=5"`
	Profile *Profile  `json:"profile"`
	Legacy  string    `json:"legacy,omitzero" deprecated:"true"`
	Secret  string    `json:"-"`
}

type Stats struct {
	Total   int     `json:"total"`
	Next    *string `json:"next"`
	Oldest  string  `json:"oldest,omitzero"`
	Perhaps []int   `json:"perhaps"`
}

func (Account) Example() any {
	return Account{Name: "Ann", Role: "admin", Status: "active", Tags: []string{"vip"}}
}

type ListAccounts struct {
	Page    int    `query:"page" default:"1" validate:"min=1" doc:"Page number" example:"2"`
	PerPage int    `query:"per_page" default:"20" validate:"max=100"`
	Sort    string `query:"sort" enum:"name,created" default:"name"`
	Roles   []Role `query:"role"`
	Tenant  string `header:"X-Tenant" validate:"required"`
}

type PatchAccount struct {
	ID   uuid.UUID `path:"id"`
	Name string    `json:"name" validate:"required"`
	Role Role      `json:"role"`
}

func TestOpenAPIDocument(t *testing.T) {
	app := newApp(t)
	app.UseAuthentication(veyro.Bearer(tokenAuth))

	acc := app.Group("/accounts").Tags("accounts")
	acc.Get("", func(c *veyro.Context, in ListAccounts) ([]Account, error) { return nil, nil }).
		Summary("List accounts")
	acc.Post("", func(c *veyro.Context, in Account) (Account, error) { return in, nil }).
		Success(201).Errors(409).RequireRole("admin")
	acc.Patch("/{id}", func(c *veyro.Context, in PatchAccount) (Account, error) { return Account{}, nil }).
		Errors(404).Deprecated()
	acc.Delete("/{id}", func(c *veyro.Context, in struct {
		ID uuid.UUID `path:"id"`
	}) (veyro.None, error) {
		return veyro.None{}, nil
	})
	acc.Get("/stats", func(c *veyro.Context, _ veyro.None) (Stats, error) { return Stats{}, nil })
	app.Get("/hidden", func(c *veyro.Context, _ veyro.None) (string, error) { return "", nil }).Hide()
	app.OpenAPIConfig().Title("Accounts API").Version("1.0.0")
	app.ServeDocs(nil)

	// Served document.
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/openapi.json", nil))
	if rec.Code != 200 {
		t.Fatalf("openapi.json: %d %s", rec.Code, rec.Body)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rec.Code != 404 {
		t.Errorf("docs page without UI must be 404, got %d", rec.Code)
	}

	get := func(path string) any {
		var cur any = doc
		for _, k := range strings.Split(path, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("%s: %T is not an object at %q", path, cur, k)
			}
			cur, ok = m[k]
			if !ok {
				t.Fatalf("%s: missing %q (have %v)", path, k, keys(m))
			}
		}
		return cur
	}

	if get("openapi") != "3.1.0" || get("info.title") != "Accounts API" {
		t.Errorf("header: %v %v", get("openapi"), get("info"))
	}
	paths := get("paths").(map[string]any)
	for _, p := range []string{"/accounts", "/accounts/{id}"} {
		if paths[p] == nil {
			t.Errorf("missing path %s", p)
		}
	}
	for _, p := range []string{"/hidden", "/docs", "/docs/openapi.json"} {
		if paths[p] != nil {
			t.Errorf("path %s must be hidden", p)
		}
	}

	// GET /accounts: parameters from query/header with defaults, enums, docs.
	list := get("paths./accounts.get").(map[string]any)
	if list["operationId"] != "getAccounts" || list["summary"] != "List accounts" || !containsAny(list["tags"], "accounts") {
		t.Errorf("list op: %v", list)
	}
	params := map[string]map[string]any{}
	for _, p := range list["parameters"].([]any) {
		pm := p.(map[string]any)
		params[pm["name"].(string)] = pm
	}
	page := params["page"]
	pageSchema := page["schema"].(map[string]any)
	if page["in"] != "query" || page["description"] != "Page number" || page["example"] != float64(2) ||
		pageSchema["default"] != float64(1) || pageSchema["minimum"] != float64(1) || pageSchema["type"] != "integer" {
		t.Errorf("page param: %v", page)
	}
	if s := params["sort"]["schema"].(map[string]any); !containsAny(s["enum"], "name", "created") || s["default"] != "name" {
		t.Errorf("sort param: %v", s)
	}
	if s := params["role"]["schema"].(map[string]any); s["type"] != "array" || !containsAny(s["items"].(map[string]any)["enum"], "admin", "user") {
		t.Errorf("role param (Enum type in slice): %v", s)
	}
	if tenant := params["X-Tenant"]; tenant["in"] != "header" || tenant["required"] != true {
		t.Errorf("tenant param: %v", tenant)
	}
	if list["requestBody"] != nil {
		t.Error("GET with only parameters must not have a request body")
	}
	if _, ok := list["responses"].(map[string]any)["401"]; ok {
		t.Error("public route must not document 401")
	}

	// POST /accounts: body ref, 201, 409, security.
	post := get("paths./accounts.post").(map[string]any)
	body := get("paths./accounts.post.requestBody.content.application/json.schema").(map[string]any)
	if body["$ref"] != "#/components/schemas/Account" {
		t.Errorf("body schema: %v", body)
	}
	resps := post["responses"].(map[string]any)
	for _, code := range []string{"201", "400", "401", "403", "409"} {
		if resps[code] == nil {
			t.Errorf("POST missing response %s: %v", code, keys(resps))
		}
	}
	if resps["200"] != nil {
		t.Error("POST must not document 200 when Success(201) is set")
	}
	if ct := resps["409"].(map[string]any)["content"].(map[string]any); ct["application/problem+json"] == nil {
		t.Errorf("problem response content: %v", ct)
	}
	if sec := post["security"].([]any); len(sec) != 1 || sec[0].(map[string]any)["bearer"] == nil {
		t.Errorf("security: %v", post["security"])
	}
	if get("components.securitySchemes.bearer.scheme") != "bearer" {
		t.Error("bearer security scheme missing")
	}

	// PATCH /accounts/{id}: mixed struct -> path param + inline body without ID.
	patch := get("paths./accounts/{id}.patch").(map[string]any)
	if patch["deprecated"] != true || patch["operationId"] != "patchAccountsById" {
		t.Errorf("patch op: %v", patch)
	}
	pb := get("paths./accounts/{id}.patch.requestBody.content.application/json.schema").(map[string]any)
	props := pb["properties"].(map[string]any)
	if props["name"] == nil || props["role"] == nil || props["id"] != nil || props["ID"] != nil {
		t.Errorf("patch body properties: %v", keys(props))
	}
	if !containsAny(pb["required"], "name") || get("paths./accounts/{id}.patch.requestBody.required") != true {
		t.Errorf("patch body required: %v", pb["required"])
	}

	// DELETE: 204 without content.
	del := get("paths./accounts/{id}.delete.responses").(map[string]any)
	if del["204"] == nil || del["200"] != nil {
		t.Errorf("delete responses: %v", keys(del))
	}

	// Exampler: whole-object example on the component, the request body and the response.
	for _, path := range []string{
		"components.schemas.Account.example",
		"paths./accounts.post.requestBody.content.application/json.example",
		"paths./accounts.post.responses.201.content.application/json.example",
	} {
		ex, _ := get(path).(map[string]any)
		if ex["name"] != "Ann" || ex["role"] != "admin" || !containsAny(ex["tags"], "vip") || ex["id"] != "00000000-0000-0000-0000-000000000000" {
			t.Errorf("%s: %v", path, ex)
		}
	}
	if _, has := get("components.schemas.Profile").(map[string]any)["example"]; has {
		t.Error("types without Example() must not get one")
	}

	// Components: Account schema with tag-driven keywords.
	accSchema := get("components.schemas.Account").(map[string]any)
	ap := accSchema["properties"].(map[string]any)
	name := ap["name"].(map[string]any)
	if name["minLength"] != float64(2) || name["maxLength"] != float64(64) || name["example"] != "Ann" {
		t.Errorf("name schema: %v", name)
	}
	if role := ap["role"].(map[string]any); !containsAny(role["enum"], "admin", "user") || role["default"] != "user" {
		t.Errorf("role schema (Enum interface): %v", role)
	}
	if st := ap["status"].(map[string]any); !containsAny(st["enum"], "active", "blocked") || st["description"] != "Lifecycle state" {
		t.Errorf("status schema (enum tag): %v", st)
	}
	if age := ap["age"].(map[string]any); age["minimum"] != float64(18) || age["example"] != float64(30) || !containsAny(age["type"], "integer", "null") {
		t.Errorf("age schema (nullable + bounds): %v", age)
	}
	if tags := ap["tags"].(map[string]any); tags["maxItems"] != float64(5) {
		t.Errorf("tags schema: %v", tags)
	}
	if ap["legacy"].(map[string]any)["deprecated"] != true || ap["Secret"] != nil || ap["-"] != nil {
		t.Errorf("legacy/secret: %v", keys(ap))
	}
	if ap["id"].(map[string]any)["format"] != "uuid" || ap["profile"].(map[string]any)["$ref"] != "#/components/schemas/Profile" {
		t.Errorf("id/profile: %v %v", ap["id"], ap["profile"])
	}
	// Account is used as input too, so only validate:"required" fields are required.
	if !containsAny(accSchema["required"], "name") || containsAny(accSchema["required"], "id") {
		t.Errorf("Account.required: %v", accSchema["required"])
	}
	// Profile is reachable from an input type, so nothing is required by default.
	prof := get("components.schemas.Profile").(map[string]any)
	if prof["required"] != nil {
		t.Errorf("Profile.required: %v", prof["required"])
	}
	// Stats is output-only: non-nullable fields are required, pointers/slices/omitzero are not.
	stats := get("components.schemas.Stats").(map[string]any)
	if !containsAny(stats["required"], "total") || containsAny(stats["required"], "next") ||
		containsAny(stats["required"], "oldest") || containsAny(stats["required"], "perhaps") {
		t.Errorf("Stats.required: %v", stats["required"])
	}
	pp := prof["properties"].(map[string]any)
	if pp["joined"].(map[string]any)["format"] != "date-time" || pp["website"].(map[string]any)["format"] != "uri" || pp["bio"].(map[string]any)["example"] != "Gopher since 2012" {
		t.Errorf("Profile props: %v", pp)
	}
	if get("components.schemas.Problem.properties.errors.additionalProperties.type") != "array" {
		t.Error("Problem.errors schema")
	}
}

func TestDefaultsAndEnumsAtRuntime(t *testing.T) {
	app := newApp(t)
	app.Get("/accounts", func(c *veyro.Context, in ListAccounts) (ListAccounts, error) { return in, nil })
	app.Post("/accounts", func(c *veyro.Context, in Account) (Account, error) { return in, nil })

	resp, out := do(t, app, http.MethodGet, "/accounts?role=admin&role=user", "", "X-Tenant", "acme")
	if resp.StatusCode != 200 || out["Page"] != float64(1) || out["PerPage"] != float64(20) || out["Sort"] != "name" {
		t.Errorf("defaults: %d %v", resp.StatusCode, out)
	}
	resp, out = do(t, app, http.MethodGet, "/accounts?sort=size&role=root", "", "X-Tenant", "acme")
	errs, _ := out["errors"].(map[string]any)
	if resp.StatusCode != 400 || errs["sort"] == nil || errs["role"] == nil {
		t.Errorf("enum validation on params: %d %v", resp.StatusCode, out)
	}

	resp, out = do(t, app, http.MethodPost, "/accounts", `{"name":"Ann"}`)
	if resp.StatusCode != 200 || out["role"] != "user" || out["status"] != "active" {
		t.Errorf("body defaults: %d %v", resp.StatusCode, out)
	}
	resp, out = do(t, app, http.MethodPost, "/accounts", `{"name":"Ann","role":"root","status":"gone"}`)
	errs, _ = out["errors"].(map[string]any)
	if resp.StatusCode != 400 || errs["role"] == nil || errs["status"] == nil {
		t.Errorf("enum validation on body: %d %v", resp.StatusCode, out)
	}
	if got, _ := errs["role"].([]any); len(got) != 1 || got[0] != "The field role must be one of: admin, user." {
		t.Errorf("enum message: %v", got)
	}
}

func TestBadDefaultPanics(t *testing.T) {
	app := newApp(t)
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "bad default") {
			t.Fatalf("want panic about default, got %v", r)
		}
	}()
	app.Get("/x", func(c *veyro.Context, in struct {
		N int `query:"n" default:"ten"`
	}) (veyro.None, error) {
		return veyro.None{}, nil
	})
}

func containsAny(list any, want ...string) bool {
	items, ok := list.([]any)
	if !ok {
		return false
	}
	for _, w := range want {
		found := false
		for _, it := range items {
			if it == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ = context.Background

func TestOpenAPIConfigDefaultsAndSecurityHelpers(t *testing.T) {
	app := newApp(t)
	doc := app.OpenAPI()
	if doc.Info.Title != "API" || doc.Info.Version != "1.0.0" || len(doc.Servers) != 1 || doc.Servers[0].URL != "/" {
		t.Errorf("defaults: %+v %+v", doc.Info, doc.Servers)
	}

	cfg := app.OpenAPIConfig().
		Title("Shop").Version("2.0.0").Description("desc").
		Contact("Team", "https://example.com", "team@example.com").
		License("MIT", "https://opensource.org/license/mit").
		Server("https://api.example.com", "production").
		Server("http://localhost:8080", "local").
		Tag("orders", "Order management").
		Basic().BearerJWT().APIKey("X-Api-Key", "header").
		OIDC("https://id.example.com/.well-known/openid-configuration").
		OAuth2AuthorizationCode("https://id.example.com/authorize", "https://id.example.com/token",
			map[string]string{"read:orders": "Read orders"}).
		Path("/api-docs/")

	app.Group("/orders").Tags("orders").
		Get("", func(c *veyro.Context, _ veyro.None) ([]string, error) { return nil, nil }).
		Security("oauth2", "read:orders").Security("apiKey")
	app.Get("/me", func(c *veyro.Context, _ veyro.None) (string, error) { return "", nil }).RequireAuthorization()
	app.ServeDocs(nil)

	doc = app.OpenAPI()
	if doc.Info.Title != "Shop" || doc.Info.Contact.Email != "team@example.com" || doc.Info.License.Name != "MIT" {
		t.Errorf("info: %+v", doc.Info)
	}
	if len(doc.Servers) != 2 || doc.Servers[0].URL != "https://api.example.com" || doc.Servers[1].Description != "local" {
		t.Errorf("Server() must replace the default and append: %+v", doc.Servers)
	}
	if len(doc.Tags) != 1 || doc.Tags[0].Description != "Order management" {
		t.Errorf("tags: %+v", doc.Tags)
	}
	ss := doc.Components.SecuritySchemes
	want := map[string]string{"basic": "http", "bearer": "http", "apiKey": "apiKey", "oidc": "openIdConnect", "oauth2": "oauth2"}
	for name, typ := range want {
		if ss[name] == nil || ss[name].Type != typ {
			t.Errorf("scheme %s: %+v", name, ss[name])
		}
	}
	if ss["bearer"].BearerFormat != "JWT" || ss["apiKey"].Name != "X-Api-Key" || ss["apiKey"].In != "header" ||
		!strings.HasSuffix(ss["oidc"].OpenIDConnectURL, "openid-configuration") ||
		ss["oauth2"].Flows.AuthorizationCode.Scopes["read:orders"] != "Read orders" {
		t.Errorf("scheme details: %+v %+v %+v %+v", ss["bearer"], ss["apiKey"], ss["oidc"], ss["oauth2"].Flows.AuthorizationCode)
	}

	// Route.Security overrides the default "any scheme".
	orders := doc.Paths["/orders"]["get"]
	if len(orders.Security) != 2 || orders.Security[0]["oauth2"][0] != "read:orders" || orders.Security[1]["apiKey"] == nil {
		t.Errorf("route security: %v", orders.Security)
	}
	if orders.Responses["401"] == nil {
		t.Error("Security() implies 401 docs")
	}
	// Default: every registered scheme, in registration order.
	me := doc.Paths["/me"]["get"]
	if len(me.Security) != 5 || me.Security[0]["basic"] == nil || me.Security[4]["oauth2"] == nil {
		t.Errorf("default security: %v", me.Security)
	}

	// Path() moved the docs.
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api-docs/openapi.json", nil))
	if rec.Code != 200 {
		t.Errorf("custom docs path: %d", rec.Code)
	}
	_ = cfg
}

func TestBasicAndAPIKeyAuthenticators(t *testing.T) {
	app := newApp(t)
	app.UseAuthentication(
		veyro.Basic("shop", func(ctx context.Context, user, pass string) (*veyro.User, error) {
			if user == "ann" && pass == "pw" {
				return veyro.NewUser("", "ann"), nil
			}
			return nil, errors.New("bad credentials")
		}),
		veyro.APIKey("X-Api-Key", "header", func(ctx context.Context, key string) (*veyro.User, error) {
			if key == "k1" {
				return veyro.NewUser("", "svc").AddClaim(veyro.ClaimRole, "service"), nil
			}
			return nil, errors.New("unknown key")
		}),
	)
	app.Get("/me", func(c *veyro.Context, _ veyro.None) (map[string]string, error) {
		return map[string]string{"name": c.User().Name, "type": c.User().AuthType}, nil
	}).RequireAuthorization()

	resp, out := do(t, app, http.MethodGet, "/me", "")
	if resp.StatusCode != 401 || len(resp.Header.Values("WWW-Authenticate")) != 1 || resp.Header.Get("WWW-Authenticate") != `Basic realm="shop"` {
		t.Errorf("anonymous: %d %v %v", resp.StatusCode, resp.Header, out)
	}
	resp, out = do(t, app, http.MethodGet, "/me", "", "Authorization", "Basic "+basicToken("ann", "pw"))
	if resp.StatusCode != 200 || out["name"] != "ann" || out["type"] != "Basic" {
		t.Errorf("basic ok: %d %v", resp.StatusCode, out)
	}
	resp, out = do(t, app, http.MethodGet, "/me", "", "Authorization", "Basic "+basicToken("ann", "nope"))
	if resp.StatusCode != 401 || out["detail"] != "bad credentials" {
		t.Errorf("basic bad: %d %v", resp.StatusCode, out)
	}
	resp, out = do(t, app, http.MethodGet, "/me", "", "X-Api-Key", "k1")
	if resp.StatusCode != 200 || out["name"] != "svc" || out["type"] != "ApiKey" {
		t.Errorf("api key: %d %v", resp.StatusCode, out)
	}

	ss := app.OpenAPI().Components.SecuritySchemes
	if ss["basic"] == nil || ss["apiKey"] == nil || ss["apiKey"].Name != "X-Api-Key" {
		t.Errorf("schemes from authenticators: %+v", ss)
	}
	// Explicit config wins over the authenticator's description.
	app.OpenAPIConfig().SecurityScheme("basic", &veyro.SecurityScheme{Type: "http", Scheme: "basic", Description: "Staff accounts"})
	if app.OpenAPI().Components.SecuritySchemes["basic"].Description != "Staff accounts" {
		t.Error("explicit scheme must override")
	}
}

func basicToken(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
