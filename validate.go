package veyro

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Validation rules are declared with the validate struct tag:
//
//	type CreateUser struct {
//	    Name  string   `json:"name"  validate:"required,minlen=2,maxlen=64"`
//	    Age   int      `json:"age"   validate:"min=18,max=130"`
//	    Role  string   `json:"role"  validate:"oneof=admin user"`
//	    Email *string  `json:"email" validate:"email"`
//	}
//
// Rules:
//
//	required        value must not be the zero value (nil, "", 0, empty slice)
//	min=N, max=N    numeric bounds (ints, uints, floats, time.Duration)
//	minlen=N        minimum length of a string, slice or map
//	maxlen=N        maximum length of a string, slice or map
//	oneof=a b c     value (formatted with %v) must be one of the space-separated items
//	email           value must look like an e-mail address
//
// A zero value (nil pointer, "", 0, empty slice) counts as "not provided": only
// required applies to it, so optional fields with rules work without pointers.
// Use a pointer when zero must be checked too: *int with min=1 rejects 0. Nested structs,
// slices and maps are validated recursively; their errors are keyed like
// "address.city" and "items[0].price". Messages follow ASP.NET Core.

// Enum is implemented by string-like types with a fixed set of values:
//
//	type Role string
//	func (Role) EnumValues() []string { return []string{"admin", "user"} }
//
// Values are validated against the set and documented as an enum in OpenAPI.
type Enum interface {
	EnumValues() []string
}

var enumType = reflect.TypeFor[Enum]()

// enumValues returns the enum set for t, from the Enum interface, or nil.
func enumValues(t reflect.Type) []string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Implements(enumType) {
		return reflect.Zero(t).Interface().(Enum).EnumValues()
	}
	if reflect.PointerTo(t).Implements(enumType) {
		return reflect.New(t).Interface().(Enum).EnumValues()
	}
	return nil
}

type rule struct {
	name string
	num  float64
	n    int
	set  []string
}

type fieldRules struct {
	index  int
	name   string
	rules  []rule
	nested bool
}

type typeRules struct {
	fields []fieldRules
	any    bool // at least one field has rules or nested rules
}

var rulesCache sync.Map // reflect.Type -> *typeRules

func rulesFor(t reflect.Type) *typeRules {
	if r, ok := rulesCache.Load(t); ok {
		return r.(*typeRules)
	}
	tr := &typeRules{}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		fr := fieldRules{index: i, name: errorKey(f)}
		if tag, ok := f.Tag.Lookup("validate"); ok {
			fr.rules = parseRules(tag, t.Name()+"."+f.Name)
		}
		// `enum:"a,b"` on the field, or an Enum type, adds a oneof rule.
		if set := fieldEnum(f); len(set) > 0 && !slices.ContainsFunc(fr.rules, func(r rule) bool { return r.name == "oneof" }) {
			fr.rules = append(fr.rules, rule{name: "oneof", set: set})
		}
		fr.nested = walkable(f.Type)
		if f.Anonymous && !hasSourceTag(f) && f.Tag.Get("json") == "" {
			fr.name = "" // embedded: fields are promoted, no prefix
		}
		if len(fr.rules) > 0 || fr.nested {
			tr.any = true
		}
		tr.fields = append(tr.fields, fr)
	}
	actual, _ := rulesCache.LoadOrStore(t, tr)
	return actual.(*typeRules)
}

// errorKey is the name used for a field in validation errors: the binding
// source name for path/query/header/cookie fields, else the JSON name.
func errorKey(f reflect.StructField) string {
	if _, name, ok := sourceOf(f); ok {
		return name
	}
	if tag, ok := f.Tag.Lookup("json"); ok {
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			return name
		}
	}
	return f.Name
}

// fieldEnum returns the enum set declared for a field by tag or by type.
func fieldEnum(f reflect.StructField) []string {
	if tag, ok := f.Tag.Lookup("enum"); ok {
		var set []string
		for v := range strings.SplitSeq(tag, ",") {
			if v = strings.TrimSpace(v); v != "" {
				set = append(set, v)
			}
		}
		return set
	}
	return enumValues(f.Type)
}

func walkable(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		return t != reflect.TypeFor[time.Time]()
	case reflect.Slice, reflect.Array, reflect.Map:
		return walkable(t.Elem())
	}
	return false
}

func parseRules(tag, where string) []rule {
	var out []rule
	for part := range strings.SplitSeq(tag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, arg, hasArg := strings.Cut(part, "=")
		r := rule{name: name}
		switch name {
		case "required", "email":
		case "min", "max":
			f, err := strconv.ParseFloat(arg, 64)
			if !hasArg || err != nil {
				panic(fmt.Sprintf("veyro: %s: rule %q needs a numeric argument", where, part))
			}
			r.num = f
		case "minlen", "maxlen":
			n, err := strconv.Atoi(arg)
			if !hasArg || err != nil {
				panic(fmt.Sprintf("veyro: %s: rule %q needs an integer argument", where, part))
			}
			r.n = n
		case "oneof":
			r.set = strings.Fields(arg)
			if len(r.set) == 0 {
				panic(fmt.Sprintf("veyro: %s: rule %q needs space-separated values", where, part))
			}
		default:
			panic(fmt.Sprintf("veyro: %s: unknown validation rule %q", where, name))
		}
		out = append(out, r)
	}
	return out
}

// validateValue validates v (a struct, or a container of structs) and records
// errors in *errs (allocated on first error) with keys prefixed by prefix.
func validateValue(v reflect.Value, prefix string, errs *ValidationErrors) {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		tr := rulesFor(v.Type())
		if !tr.any {
			return
		}
		for _, fr := range tr.fields {
			fv := v.Field(fr.index)
			key := joinKey(prefix, fr.name)
			ok := true
			for _, r := range fr.rules {
				if msg := applyRule(r, fv, key); msg != "" {
					errs.add(key, msg)
					ok = false
				}
			}
			if ok && fr.nested {
				validateValue(fv, key, errs)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			validateValue(v.Index(i), fmt.Sprintf("%s[%d]", prefix, i), errs)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			validateValue(v.MapIndex(k), joinKey(prefix, fmt.Sprint(k.Interface())), errs)
		}
	}
}

func joinKey(prefix, name string) string {
	switch {
	case name == "":
		return prefix
	case prefix == "":
		return name
	}
	return prefix + "." + name
}

// applyRule returns an error message or "" when the rule passes.
func applyRule(r rule, v reflect.Value, name string) string {
	if r.name == "required" {
		if v.IsZero() {
			return fmt.Sprintf("The %s field is required.", name)
		}
		return ""
	}
	if v.IsZero() {
		return "" // not provided: only "required" complains
	}
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	switch r.name {
	case "min":
		if f, ok := asFloat(v); ok && f < r.num {
			return fmt.Sprintf("The field %s must be greater than or equal to %v.", name, r.num)
		}
	case "max":
		if f, ok := asFloat(v); ok && f > r.num {
			return fmt.Sprintf("The field %s must be less than or equal to %v.", name, r.num)
		}
	case "minlen":
		if n, ok := length(v); ok && n < r.n {
			return fmt.Sprintf("The field %s must be a string or array type with a minimum length of '%d'.", name, r.n)
		}
	case "maxlen":
		if n, ok := length(v); ok && n > r.n {
			return fmt.Sprintf("The field %s must be a string or array type with a maximum length of '%d'.", name, r.n)
		}
	case "oneof":
		if v.Kind() == reflect.Slice || v.Kind() == reflect.Array {
			for i := range v.Len() {
				if !slices.Contains(r.set, fmt.Sprint(v.Index(i).Interface())) {
					return fmt.Sprintf("The field %s must contain only: %s.", name, strings.Join(r.set, ", "))
				}
			}
			return ""
		}
		if !slices.Contains(r.set, fmt.Sprint(v.Interface())) {
			return fmt.Sprintf("The field %s must be one of: %s.", name, strings.Join(r.set, ", "))
		}
	case "email":
		if v.Kind() == reflect.String && !looksLikeEmail(v.String()) {
			return fmt.Sprintf("The %s field is not a valid e-mail address.", name)
		}
	}
	return ""
}

func asFloat(v reflect.Value) (float64, bool) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(v.Uint()), true
	case reflect.Float32, reflect.Float64:
		return v.Float(), true
	}
	return 0, false
}

func length(v reflect.Value) (int, bool) {
	switch v.Kind() {
	case reflect.String, reflect.Slice, reflect.Array, reflect.Map:
		return v.Len(), true
	}
	return 0, false
}

// looksLikeEmail mirrors ASP.NET Core's EmailAddressAttribute: exactly one
// '@' that is neither first nor last.
func looksLikeEmail(s string) bool {
	i := strings.IndexByte(s, '@')
	return i > 0 && i < len(s)-1 && strings.IndexByte(s[i+1:], '@') < 0
}
