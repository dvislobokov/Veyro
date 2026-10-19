package veyro

import (
	"encoding"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"
)

// OpenAPI generation.
//
// The document is derived from registered routes: In types become
// parameters and request bodies, Out types become responses, and the
// Problem type documents errors. Field tags refine schemas:
//
//	doc:"..."          description
//	example:"..."      example (parsed as the field type; JSON for complex types)
//	default:"..."      default (also applied when the request omits the value)
//	enum:"a,b,c"       allowed values (also validated)
//	format:"..."       JSON Schema format
//	deprecated:"true"  marks the field deprecated
//	validate:"..."     required, min/max, minlen/maxlen, oneof, email map to schema keywords
//
// Types implementing Enum are documented as string enums.

// Info is the OpenAPI info object.
type Info struct {
	Title          string   `json:"title"`
	Version        string   `json:"version"`
	Description    string   `json:"description,omitzero"`
	TermsOfService string   `json:"termsOfService,omitzero"`
	Contact        *Contact `json:"contact,omitzero"`
	License        *License `json:"license,omitzero"`
}

// Contact is the OpenAPI contact object.
type Contact struct {
	Name  string `json:"name,omitzero"`
	URL   string `json:"url,omitzero"`
	Email string `json:"email,omitzero"`
}

// License is the OpenAPI license object.
type License struct {
	Name string `json:"name"`
	URL  string `json:"url,omitzero"`
}

// Server is an OpenAPI server object.
type Server struct {
	URL         string `json:"url"`
	Description string `json:"description,omitzero"`
}

// Document is an OpenAPI 3.1 document.
type Document struct {
	OpenAPI    string                           `json:"openapi"`
	Info       Info                             `json:"info"`
	Servers    []Server                         `json:"servers,omitzero"`
	Tags       []Tag                            `json:"tags,omitzero"`
	Paths      map[string]map[string]*Operation `json:"paths"`
	Components *Components                      `json:"components,omitzero"`
}

// Components holds reusable schemas and security schemes.
type Components struct {
	Schemas         map[string]*Schema         `json:"schemas,omitzero"`
	SecuritySchemes map[string]*SecurityScheme `json:"securitySchemes,omitzero"`
}

// Operation is an OpenAPI operation object.
type Operation struct {
	OperationID string                `json:"operationId,omitzero"`
	Summary     string                `json:"summary,omitzero"`
	Description string                `json:"description,omitzero"`
	Tags        []string              `json:"tags,omitzero"`
	Deprecated  bool                  `json:"deprecated,omitzero"`
	Parameters  []*Parameter          `json:"parameters,omitzero"`
	RequestBody *RequestBody          `json:"requestBody,omitzero"`
	Responses   map[string]*Response  `json:"responses"`
	Security    []map[string][]string `json:"security,omitzero"`
}

// Parameter is an OpenAPI parameter object.
type Parameter struct {
	Name        string  `json:"name"`
	In          string  `json:"in"`
	Description string  `json:"description,omitzero"`
	Required    bool    `json:"required,omitzero"`
	Deprecated  bool    `json:"deprecated,omitzero"`
	Schema      *Schema `json:"schema"`
	Example     any     `json:"example,omitzero"`
}

// RequestBody is an OpenAPI request body object.
type RequestBody struct {
	Description string                `json:"description,omitzero"`
	Required    bool                  `json:"required,omitzero"`
	Content     map[string]*MediaType `json:"content"`
}

// MediaType is an OpenAPI media type object.
type MediaType struct {
	Schema  *Schema `json:"schema"`
	Example any     `json:"example,omitzero"`
}

// Response is an OpenAPI response object.
type Response struct {
	Description string                `json:"description"`
	Content     map[string]*MediaType `json:"content,omitzero"`
}

// SecurityScheme is an OpenAPI security scheme object.
type SecurityScheme struct {
	Type             string      `json:"type"`
	Description      string      `json:"description,omitzero"`
	Scheme           string      `json:"scheme,omitzero"`
	BearerFormat     string      `json:"bearerFormat,omitzero"`
	In               string      `json:"in,omitzero"`
	Name             string      `json:"name,omitzero"`
	OpenIDConnectURL string      `json:"openIdConnectUrl,omitzero"`
	Flows            *OAuthFlows `json:"flows,omitzero"`
}

// Schema is a JSON Schema (2020-12) object as used by OpenAPI 3.1.
type Schema struct {
	Ref                  string             `json:"$ref,omitzero"`
	Type                 any                `json:"type,omitzero"` // string or []string
	Format               string             `json:"format,omitzero"`
	Title                string             `json:"title,omitzero"`
	Description          string             `json:"description,omitzero"`
	Properties           map[string]*Schema `json:"properties,omitzero"`
	Required             []string           `json:"required,omitzero"`
	Items                *Schema            `json:"items,omitzero"`
	AdditionalProperties any                `json:"additionalProperties,omitzero"` // *Schema or bool
	Enum                 []any              `json:"enum,omitzero"`
	Default              any                `json:"default,omitzero"`
	Example              any                `json:"example,omitzero"`
	Minimum              *float64           `json:"minimum,omitzero"`
	Maximum              *float64           `json:"maximum,omitzero"`
	MinLength            *int               `json:"minLength,omitzero"`
	MaxLength            *int               `json:"maxLength,omitzero"`
	MinItems             *int               `json:"minItems,omitzero"`
	MaxItems             *int               `json:"maxItems,omitzero"`
	Deprecated           bool               `json:"deprecated,omitzero"`
}

// SecuritySchemer is implemented by authenticators that can describe
// themselves for OpenAPI. Bearer implements it.
type SecuritySchemer interface {
	SecurityScheme() (name string, scheme *SecurityScheme)
}

// SecurityScheme implements SecuritySchemer.
func (bearer) SecurityScheme() (string, *SecurityScheme) {
	return "bearer", &SecurityScheme{Type: "http", Scheme: "bearer"}
}

// OpenAPI builds the OpenAPI 3.1 document for the routes registered so far,
// using App.OpenAPIConfig for info, servers, tags and security schemes.
func (a *App) OpenAPI() *Document {
	g := &generator{
		app:     a,
		schemas: map[string]*Schema{},
		names:   map[reflect.Type]string{},
		taken:   map[string]reflect.Type{},
		usage:   map[reflect.Type]int{},
	}
	return g.build()
}

// ServeDocs serves the OpenAPI document at <path>/openapi.json and, when ui
// is not nil, an interactive reference at <path>, where path comes from
// OpenAPIConfig (default /docs). The document is generated on first request,
// so routes registered later are included.
//
//	app.ServeDocs(scalar.UI)
func (a *App) ServeDocs(ui DocsUI) *App {
	cfg := a.OpenAPIConfig()
	path := cfg.path
	var (
		once sync.Once
		spec *Static
		err  error
	)
	specURL := path + "/openapi.json"
	a.Raw(http.MethodGet, specURL, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Generated once, on the first request, so routes registered after
		// ServeDocs are included; then served from memory with ETag/304 and
		// a precompressed gzip body. "no-cache" means "revalidate", so a
		// restarted server with new routes is picked up immediately.
		once.Do(func() {
			var body []byte
			if body, err = json.Marshal(a.OpenAPI(), json.Deterministic(true)); err == nil {
				spec = NewStatic("application/json; charset=utf-8", "no-cache", body)
			}
		})
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		spec.ServeHTTP(w, r)
	})).Hide()
	if ui == nil {
		return a
	}
	h := ui(cfg.info.Title, specURL, path)
	a.Raw(http.MethodGet, path, h).Hide()
	a.Raw(http.MethodGet, path+"/{asset...}", h).Hide()
	return a
}

const (
	usedIn  = 1
	usedOut = 2
)

type generator struct {
	app        *App
	schemas    map[string]*Schema
	names      map[reflect.Type]string
	taken      map[string]reflect.Type
	usage      map[reflect.Type]int
	secNames   []string
	secSchemes map[string]*SecurityScheme
}

var (
	problemType     = reflect.TypeFor[Problem]()
	timeType        = reflect.TypeFor[time.Time]()
	uuidType        = reflect.TypeFor[uuid.UUID]()
	rawValueType    = reflect.TypeFor[jsontext.Value]()
	textMarshaler   = reflect.TypeFor[encoding.TextMarshaler]()
	jsonMarshaler   = reflect.TypeFor[json.Marshaler]()
	jsonMarshalerTo = reflect.TypeFor[json.MarshalerTo]()
)

func (g *generator) build() *Document {
	cfg := g.app.OpenAPIConfig()
	doc := &Document{OpenAPI: "3.1.0", Info: cfg.info, Servers: cfg.servers, Tags: cfg.tags, Paths: map[string]map[string]*Operation{}}
	g.secNames, g.secSchemes = g.app.securitySchemes()

	// Pass 1: how each type is used decides which fields are required.
	for _, rt := range g.app.routes {
		if rt.meta.hidden || rt.in == nil {
			continue
		}
		g.mark(rt.in, usedIn, map[reflect.Type]bool{})
		g.mark(rt.out, usedOut, map[reflect.Type]bool{})
	}
	g.mark(problemType, usedOut, map[reflect.Type]bool{})

	// Pass 2: operations.
	for _, rt := range g.app.routes {
		if rt.meta.hidden || rt.in == nil {
			continue
		}
		path := oasPath(rt.Pattern)
		method := strings.ToLower(rt.Method)
		if method == "" {
			method = "get"
		}
		if doc.Paths[path] == nil {
			doc.Paths[path] = map[string]*Operation{}
		}
		doc.Paths[path][method] = g.operation(rt, path, method)
	}

	comps := &Components{Schemas: g.schemas}
	if len(g.secSchemes) > 0 {
		comps.SecuritySchemes = g.secSchemes
	}
	doc.Components = comps
	return doc
}

func (g *generator) mark(t reflect.Type, mode int, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] || isScalarStruct(t) {
		return
	}
	seen[t] = true
	g.usage[t] |= mode
	for i := range t.NumField() {
		if f := t.Field(i); f.IsExported() || f.Anonymous {
			g.mark(f.Type, mode, seen)
		}
	}
}

func (g *generator) operation(rt *Route, path, method string) *Operation {
	op := &Operation{
		OperationID: rt.meta.operationID,
		Summary:     rt.meta.summary,
		Description: rt.meta.description,
		Deprecated:  rt.meta.deprecated,
		Responses:   map[string]*Response{},
	}
	if op.OperationID == "" {
		op.OperationID = operationID(method, path)
	}
	op.Tags = append(slices.Clone(rt.router.inheritedTags()), rt.meta.tags...)
	if len(op.Tags) == 0 {
		if seg := strings.Trim(strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0], "{}"); seg != "" {
			op.Tags = []string{seg}
		}
	}

	// Parameters and request body.
	b := rt.binder
	if b != nil && !b.isNone {
		for _, bf := range b.fields {
			f := rt.in.FieldByIndex(bf.index)
			sch := g.fieldSchema(f, rt.in)
			p := &Parameter{
				Name:        bf.name,
				In:          sourceTags[bf.src],
				Description: sch.Description,
				Required:    bf.src == srcPath || hasRule(f, "required"),
				Deprecated:  sch.Deprecated,
				Schema:      sch,
				Example:     sch.Example,
			}
			sch.Description, sch.Deprecated, sch.Example = "", false, nil
			op.Parameters = append(op.Parameters, p)
		}
		if b.hasBody {
			var body *Schema
			required := false
			if rt.in.Kind() == reflect.Struct {
				if len(b.fields) == 0 {
					body = g.schemaFor(rt.in)
				} else {
					body = g.structSchema(rt.in, func(f reflect.StructField) bool { return hasSourceTag(f) })
				}
				required = len(bodyRequired(g, rt.in)) > 0
			} else {
				body = g.schemaFor(rt.in)
				required = true
			}
			op.RequestBody = &RequestBody{
				Required: required,
				Content:  map[string]*MediaType{"application/json": {Schema: body}},
			}
		}
		op.Responses["400"] = g.problemResponse(http.StatusBadRequest)
	}

	// Success response.
	status := rt.meta.success
	if status == 0 {
		status = http.StatusOK
		if rt.out == noneType {
			status = http.StatusNoContent
		}
	}
	resp := &Response{Description: http.StatusText(status)}
	if rt.out != noneType && status != http.StatusNoContent {
		resp.Content = map[string]*MediaType{"application/json": {Schema: g.schemaFor(rt.out)}}
	}
	op.Responses[strconv.Itoa(status)] = resp

	// Errors.
	if rt.secured() {
		op.Responses["401"] = g.problemResponse(http.StatusUnauthorized)
		op.Responses["403"] = g.problemResponse(http.StatusForbidden)
		if rt.meta.security != nil {
			op.Security = rt.meta.security
		} else {
			for _, name := range g.secNames {
				op.Security = append(op.Security, map[string][]string{name: {}})
			}
		}
	}
	for _, code := range rt.meta.errors {
		op.Responses[strconv.Itoa(code)] = g.problemResponse(code)
	}
	return op
}

func (g *generator) problemResponse(status int) *Response {
	return &Response{
		Description: http.StatusText(status),
		Content:     map[string]*MediaType{"application/problem+json": {Schema: g.schemaFor(problemType)}},
	}
}

// schemaFor returns the schema for t: a $ref for named structs, inline otherwise.
func (g *generator) schemaFor(t reflect.Type) *Schema {
	switch {
	case t == nil:
		return &Schema{}
	case t.Kind() == reflect.Pointer:
		s := g.schemaFor(t.Elem())
		if s.Ref == "" && s.Type != nil {
			if str, ok := s.Type.(string); ok {
				s.Type = []string{str, "null"}
			}
		}
		return s
	case t == timeType:
		return &Schema{Type: "string", Format: "date-time"}
	case t == uuidType:
		return &Schema{Type: "string", Format: "uuid"}
	case t == durationType:
		return &Schema{Type: "string", Format: "duration", Example: "1m30s"}
	case t == rawValueType:
		return &Schema{}
	}
	if set := enumValues(t); set != nil && t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
		return &Schema{Type: "string", Enum: toAny(set)}
	}
	if t.Kind() != reflect.Struct && (t.Implements(jsonMarshaler) || t.Implements(jsonMarshalerTo)) {
		return &Schema{}
	}
	if t.Kind() != reflect.Struct && t.Kind() != reflect.Slice && t.Kind() != reflect.Array && t.Kind() != reflect.Map && t.Implements(textMarshaler) {
		return &Schema{Type: "string"}
	}
	switch t.Kind() {
	case reflect.Bool:
		return &Schema{Type: "boolean"}
	case reflect.Int, reflect.Int64:
		return &Schema{Type: "integer", Format: "int64"}
	case reflect.Int8, reflect.Int16, reflect.Int32:
		return &Schema{Type: "integer", Format: "int32"}
	case reflect.Uint, reflect.Uint64, reflect.Uintptr:
		return &Schema{Type: "integer", Format: "int64", Minimum: ptr(0.0)}
	case reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return &Schema{Type: "integer", Format: "int32", Minimum: ptr(0.0)}
	case reflect.Float32:
		return &Schema{Type: "number", Format: "float"}
	case reflect.Float64:
		return &Schema{Type: "number", Format: "double"}
	case reflect.String:
		return &Schema{Type: "string"}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return &Schema{Type: "string", Format: "byte"}
		}
		return &Schema{Type: "array", Items: g.schemaFor(t.Elem())}
	case reflect.Array:
		n := t.Len()
		return &Schema{Type: "array", Items: g.schemaFor(t.Elem()), MinItems: &n, MaxItems: &n}
	case reflect.Map:
		return &Schema{Type: "object", AdditionalProperties: g.schemaFor(t.Elem())}
	case reflect.Interface:
		return &Schema{}
	case reflect.Struct:
		if isScalarStruct(t) {
			return &Schema{Type: "string"}
		}
		if t.Name() == "" {
			return g.structSchema(t, nil)
		}
		return &Schema{Ref: "#/components/schemas/" + g.component(t)}
	}
	return &Schema{}
}

// isScalarStruct reports whether a struct marshals as a JSON string.
func isScalarStruct(t reflect.Type) bool {
	return t == timeType || t == uuidType || (t.Kind() == reflect.Struct && t.Implements(textMarshaler) && !t.Implements(jsonMarshaler))
}

// component registers a named struct in components and returns its name.
func (g *generator) component(t reflect.Type) string {
	if name, ok := g.names[t]; ok {
		return name
	}
	name := sanitizeName(t.Name())
	if other, taken := g.taken[name]; taken && other != t {
		pkg := t.PkgPath()
		name = sanitizeName(pkg[strings.LastIndexByte(pkg, '/')+1:] + "." + t.Name())
	}
	g.names[t], g.taken[name] = name, t
	g.schemas[name] = nil // reserve: handles recursive types
	g.schemas[name] = g.structSchema(t, nil)
	return name
}

func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, s)
}

// structSchema builds an object schema; fields matching skip are omitted.
func (g *generator) structSchema(t reflect.Type, skip func(reflect.StructField) bool) *Schema {
	s := &Schema{Type: "object", Properties: map[string]*Schema{}}
	g.addFields(s, t, skip)
	return s
}

func (g *generator) addFields(s *Schema, t reflect.Type, skip func(reflect.StructField) bool) {
	outOnly := g.usage[t] == usedOut
	for i := range t.NumField() {
		f := t.Field(i)
		name, opts, ok := jsonName(f)
		if f.Anonymous && !hasSourceTag(f) && f.Tag.Get("json") == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				g.addFields(s, ft, skip)
				continue
			}
		}
		if !f.IsExported() || !ok || (skip != nil && skip(f)) {
			continue
		}
		if strings.Contains(opts, "embed") {
			if f.Type.Kind() == reflect.Map {
				s.AdditionalProperties = g.schemaFor(f.Type.Elem())
			}
			continue
		}
		s.Properties[name] = g.fieldSchema(f, t)
		required := hasRule(f, "required")
		if !required && outOnly && !isPointerLike(f.Type.Kind()) && !strings.Contains(opts, "omit") {
			required = true
		}
		if required {
			s.Required = append(s.Required, name)
		}
	}
	slices.Sort(s.Required)
}

// isPointerLike reports kinds that can be absent/null in JSON output.
func isPointerLike(k reflect.Kind) bool {
	return k == reflect.Pointer || k == reflect.Interface || k == reflect.Map || k == reflect.Slice
}

func jsonName(f reflect.StructField) (name, opts string, ok bool) {
	tag, has := f.Tag.Lookup("json")
	if !has {
		return f.Name, "", true
	}
	name, opts, _ = strings.Cut(tag, ",")
	if name == "-" && opts == "" {
		return "", "", false
	}
	if name == "" {
		name = f.Name
	}
	return name, opts, true
}

// bodyRequired lists required body fields of an input struct.
func bodyRequired(g *generator, t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		if f.IsExported() && !hasSourceTag(f) && hasRule(f, "required") {
			out = append(out, f.Name)
		}
	}
	return out
}

func hasRule(f reflect.StructField, name string) bool {
	for part := range strings.SplitSeq(f.Tag.Get("validate"), ",") {
		if n, _, _ := strings.Cut(strings.TrimSpace(part), "="); n == name {
			return true
		}
	}
	return false
}

// fieldSchema builds the schema for a struct field, applying field tags.
func (g *generator) fieldSchema(f reflect.StructField, owner reflect.Type) *Schema {
	s := g.schemaFor(f.Type)
	if s.Ref != "" {
		// JSON Schema 2020-12 allows keywords next to $ref; copy so the
		// component itself stays untouched.
		s = &Schema{Ref: s.Ref}
	}
	if v, ok := f.Tag.Lookup("doc"); ok {
		s.Description = v
	}
	if v, ok := f.Tag.Lookup("format"); ok {
		s.Format = v
	}
	if v, ok := f.Tag.Lookup("deprecated"); ok && v != "false" {
		s.Deprecated = true
	}
	if set := fieldEnum(f); len(set) > 0 {
		target := s
		if f.Type.Kind() == reflect.Slice || f.Type.Kind() == reflect.Array {
			if s.Items == nil {
				s.Items = &Schema{Type: "string"}
			}
			target = s.Items
		}
		target.Enum = toAny(set)
	}
	if v, ok := f.Tag.Lookup("example"); ok {
		s.Example = parseLiteral(f.Type, v)
	}
	if v, ok := f.Tag.Lookup("default"); ok {
		s.Default = parseLiteral(f.Type, v)
	}
	for _, r := range parseRules(f.Tag.Get("validate"), owner.Name()+"."+f.Name) {
		switch r.name {
		case "min":
			s.Minimum = ptr(r.num)
		case "max":
			s.Maximum = ptr(r.num)
		case "minlen":
			if isArrayType(f.Type) {
				s.MinItems = ptr(r.n)
			} else {
				s.MinLength = ptr(r.n)
			}
		case "maxlen":
			if isArrayType(f.Type) {
				s.MaxItems = ptr(r.n)
			} else {
				s.MaxLength = ptr(r.n)
			}
		case "oneof":
			s.Enum = toAny(r.set)
		case "email":
			s.Format = "email"
		}
	}
	return s
}

func isArrayType(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return (t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8) || t.Kind() == reflect.Array
}

// parseLiteral turns an example/default tag into a typed value for the
// document: scalars via the parameter conversion, complex types via JSON,
// and the raw string as a last resort.
func parseLiteral(t reflect.Type, raw string) any {
	if checkConvertible(t) == nil {
		v := reflect.New(t).Elem()
		if err := setFromStrings(v, strings.Split(raw, ",")); err == nil {
			return jsonRoundTrip(v.Interface())
		}
	}
	v := reflect.New(t)
	if err := json.Unmarshal([]byte(raw), v.Interface()); err == nil {
		return jsonRoundTrip(v.Elem().Interface())
	}
	return raw
}

// jsonRoundTrip normalises a Go value to what JSON clients will see
// (uuid.UUID as a string, time.Time as RFC 3339, ...).
func jsonRoundTrip(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return fmt.Sprint(v)
	}
	return out
}

// oasPath converts a ServeMux pattern to an OpenAPI path template.
func oasPath(pattern string) string {
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		pattern = pattern[i:] // drop host
	}
	pattern = strings.ReplaceAll(pattern, "...}", "}")
	pattern = strings.TrimSuffix(pattern, "{$}")
	if pattern == "" {
		return "/"
	}
	return pattern
}

// operationID derives "getUsersById" from "get" and "/users/{id}".
func operationID(method, path string) string {
	var b strings.Builder
	b.WriteString(method)
	for seg := range strings.SplitSeq(strings.Trim(path, "/"), "/") {
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, "{") {
			b.WriteString("By")
			seg = strings.Trim(seg, "{}")
		}
		seg = strings.Map(func(r rune) rune {
			if r == '-' || r == '_' || r == '.' {
				return ' '
			}
			return r
		}, seg)
		for word := range strings.FieldsSeq(seg) {
			b.WriteString(strings.ToUpper(word[:1]) + word[1:])
		}
	}
	return b.String()
}

func toAny(set []string) []any {
	out := make([]any, len(set))
	for i, s := range set {
		out[i] = s
	}
	return out
}

func ptr[T any](v T) *T { return &v }
