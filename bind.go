package veyro

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

type source uint8

const (
	srcBody source = iota
	srcPath
	srcQuery
	srcHeader
	srcCookie
)

var sourceTags = [...]string{srcPath: "path", srcQuery: "query", srcHeader: "header", srcCookie: "cookie"}

// boundField is a struct field filled from a non-body source.
type boundField struct {
	index []int
	name  string
	src   source
	def   *reflect.Value // parsed `default` tag, applied when the source has no value
}

// binder knows how to fill one input type from a request.
type binder struct {
	typ      reflect.Type
	isNone   bool
	hasBody  bool
	fields   []boundField
	defaults []boundField // body fields with a `default` tag
	cfg      *config
}

func newBinder(t reflect.Type, pattern string, cfg *config) *binder {
	b := &binder{typ: t, cfg: cfg}
	if t == noneType {
		b.isNone = true
		return b
	}
	if t.Kind() != reflect.Struct {
		b.hasBody = true // the whole value is the JSON body
		return b
	}
	params := patternParams(pattern)
	b.collect(t, nil, params, pattern)
	return b
}

func (b *binder) collect(t reflect.Type, prefix []int, params map[string]bool, pattern string) {
	for i := range t.NumField() {
		f := t.Field(i)
		idx := append(append([]int(nil), prefix...), i)
		if f.Anonymous && !hasSourceTag(f) {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && f.Type.Kind() == reflect.Struct {
				b.collect(ft, idx, params, pattern)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		src, name, ok := sourceOf(f)
		if !ok {
			if f.Tag.Get("json") != "-" {
				b.hasBody = true
				if def, ok := f.Tag.Lookup("default"); ok {
					b.defaults = append(b.defaults, boundField{index: idx, name: f.Name, def: parseDefault(f, def, t, pattern)})
				}
			}
			continue
		}
		if src == srcPath && !params[name] {
			panic(fmt.Sprintf("veyro: route %q: field %s.%s is tagged path:%q but the pattern has no {%s}",
				pattern, t.Name(), f.Name, name, name))
		}
		if err := checkConvertible(f.Type); err != nil {
			panic(fmt.Sprintf("veyro: route %q: field %s.%s: %v", pattern, t.Name(), f.Name, err))
		}
		bf := boundField{index: idx, name: name, src: src}
		if def, ok := f.Tag.Lookup("default"); ok {
			bf.def = parseDefault(f, def, t, pattern)
		}
		b.fields = append(b.fields, bf)
	}
}

// parseDefault parses a `default` tag into a value of the field's type.
// Scalars use the same conversion as query parameters; anything else is JSON.
func parseDefault(f reflect.StructField, def string, owner reflect.Type, pattern string) *reflect.Value {
	v := reflect.New(f.Type).Elem()
	var err error
	if checkConvertible(f.Type) == nil {
		err = setFromStrings(v, strings.Split(def, ","))
	} else {
		err = json.Unmarshal([]byte(def), v.Addr().Interface())
	}
	if err != nil {
		panic(fmt.Sprintf("veyro: route %q: field %s.%s: bad default %q: %v", pattern, owner.Name(), f.Name, def, err))
	}
	return &v
}

func hasSourceTag(f reflect.StructField) bool {
	_, _, ok := sourceOf(f)
	return ok
}

func sourceOf(f reflect.StructField) (source, string, bool) {
	for src := srcPath; src <= srcCookie; src++ {
		if v, ok := f.Tag.Lookup(sourceTags[src]); ok {
			name, _, _ := strings.Cut(v, ",")
			if name == "" {
				name = f.Name
			}
			return src, name, true
		}
	}
	return srcBody, "", false
}

// patternParams extracts {name} and {name...} wildcards from a ServeMux pattern.
func patternParams(pattern string) map[string]bool {
	out := map[string]bool{}
	rest := pattern
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return out
		}
		name := strings.TrimSuffix(rest[i+1:i+j], "...")
		if name != "$" {
			out[name] = true
		}
		rest = rest[i+j+1:]
	}
}

// bind fills v (an addressable value of b.typ). It returns per-field errors
// as ValidationErrors and a non-nil error only for request-level failures
// (unsupported media type, body too large).
func (b *binder) bind(r *http.Request, v reflect.Value) (ValidationErrors, error) {
	var errs ValidationErrors
	add := func(field, msg string) {
		if errs == nil {
			errs = ValidationErrors{}
		}
		errs.Add(field, msg)
	}

	for _, f := range b.defaults {
		v.FieldByIndex(f.index).Set(*f.def)
	}
	if b.hasBody && hasBody(r) {
		if err := b.decodeBody(r, v); err != nil {
			var mbe *http.MaxBytesError
			var p *Problem
			switch {
			case errors.As(err, &mbe), errors.As(err, &p):
				return nil, err
			}
			field, msg := describeJSONError(err)
			add(field, msg)
		}
		// Values for path/query/header/cookie fields never come from the body.
		for _, f := range b.fields {
			v.FieldByIndex(f.index).SetZero()
		}
	}

	var query map[string][]string
	for _, f := range b.fields {
		var vals []string
		switch f.src {
		case srcPath:
			if s := r.PathValue(f.name); s != "" {
				vals = []string{s}
			}
		case srcQuery:
			if query == nil {
				query = r.URL.Query()
			}
			vals = query[f.name]
		case srcHeader:
			vals = r.Header.Values(f.name)
		case srcCookie:
			if c, err := r.Cookie(f.name); err == nil {
				vals = []string{c.Value}
			}
		}
		if len(vals) == 0 {
			if f.def != nil {
				v.FieldByIndex(f.index).Set(*f.def)
			}
			continue
		}
		if err := setFromStrings(v.FieldByIndex(f.index), vals); err != nil {
			add(f.name, fmt.Sprintf("The value '%s' is not valid.", strings.Join(vals, ",")))
		}
	}
	return errs, nil
}

func hasBody(r *http.Request) bool {
	return r.Body != nil && r.Body != http.NoBody && (r.ContentLength > 0 || r.ContentLength == -1)
}

func (b *binder) decodeBody(r *http.Request, v reflect.Value) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || !(mt == "application/json" || strings.HasSuffix(mt, "+json")) {
			return NewProblem(http.StatusUnsupportedMediaType,
				fmt.Sprintf("Content-Type %q is not supported; use application/json.", ct))
		}
	}
	body := http.MaxBytesReader(nil, r.Body, b.cfg.maxBody)
	err := json.UnmarshalRead(body, v.Addr().Interface(), b.cfg.jsonOpts...)
	if errors.Is(err, io.EOF) {
		return nil // empty body: leave the zero value, validation decides
	}
	return err
}

// describeJSONError turns a json/v2 error into an ASP.NET-style field error.
// Syntax errors and unknown problems are reported under "$".
func describeJSONError(err error) (field, msg string) {
	var sem *json.SemanticError
	if errors.As(err, &sem) {
		field = pointerToField(sem.JSONPointer)
		if sem.GoType != nil {
			return field, fmt.Sprintf("The JSON value could not be converted to %s.", sem.GoType)
		}
		return field, "The JSON value is not valid."
	}
	var syn *jsontext.SyntacticError
	if errors.As(err, &syn) {
		return "$", fmt.Sprintf("The request body is not valid JSON: %v.", syn.Err)
	}
	return "$", "The request body is not valid JSON."
}

// pointerToField converts an RFC 6901 pointer ("/items/0/price") to the
// field notation used in error keys ("items[0].price"). The root is "$".
func pointerToField(p jsontext.Pointer) string {
	if p == "" {
		return "$"
	}
	var b strings.Builder
	for tok := range p.Tokens() {
		if _, err := strconv.Atoi(tok); err == nil {
			b.WriteString("[" + tok + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(tok)
	}
	return b.String()
}
