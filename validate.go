package veyro

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unsafe"
	"uuid"
)

// Validation rules are declared with the validate struct tag:
//
//	type CreateUser struct {
//	    Name     string   `json:"name"     validate:"required,min=2,max=64"`
//	    Email    string   `json:"email"    validate:"required,email"`
//	    Age      int      `json:"age"      validate:"gte=18,lte=130"`
//	    Role     string   `json:"role"     validate:"oneof=admin user"`
//	    Password string   `json:"password" validate:"required,min=8"`
//	    Confirm  string   `json:"confirm"  validate:"eqfield=Password"`
//	    Tags     []string `json:"tags"     validate:"max=5,unique,dive,min=2"`
//	    Phone    *string  `json:"phone"    validate:"required_without=Email,e164"`
//	}
//
// Presence (these also run on zero values):
//
//	required                   not the zero value
//	required_if=Field value    required when the sibling Field equals value
//	required_unless=Field value
//	required_with=F1 F2        required when any listed sibling is set
//	required_with_all=F1 F2    required when all listed siblings are set
//	required_without=F1 F2     required when any listed sibling is unset
//	required_without_all=F1 F2 required when all listed siblings are unset
//
// Numbers (value) and strings, slices, maps (length):
//
//	min=N max=N len=N gt=N gte=N lt=N lte=N eq=N ne=N
//	minlen=N maxlen=N          aliases of min/max for lengths
//
// Strings:
//
//	eq=s ne=s contains=s excludes=s startswith=s endswith=s
//	alpha alphanum alphaunicode alphanumunicode numeric number
//	lowercase uppercase ascii printascii boolean
//	oneof=a b c                one of the space-separated values (exact)
//	enum                       see the enum tag and Enum type: case-insensitive, canonicalised
//	regex=PATTERN              RE2 pattern; must be the last rule (it may contain commas)
//	email url http_url uri uuid ip ipv4 ipv6 cidr mac hostname fqdn
//	datetime=LAYOUT base64 json hexadecimal hexcolor e164
//
// Cross-field (sibling given by Go or JSON name):
//
//	eqfield nefield gtfield gtefield ltfield ltefield
//
// Collections:
//
//	dive                       the rules after it apply to each element
//	unique                     no duplicate elements
//
// omitempty, omitnil and structonly are accepted for validator v10
// compatibility and do nothing: zero values are skipped by default.
// A string rule on a slice of strings without dive applies to every element.
// A zero value (nil pointer, "", 0, empty slice) counts as "not provided":
// only the presence rules complain about it, so optional fields with rules
// work without pointers; use a pointer when zero must be checked too.
// Nested structs, slices and maps are validated recursively, and any type
// implementing Validator (Validate(ctx) error or Validate() error, as in
// sconf) is called after its fields were checked, at every level. Custom
// rules are added with RegisterRule.

// Enum is implemented by string-like types with a fixed set of values:
//
//	type Role string
//	func (Role) EnumValues() []string { return []string{"admin", "user"} }
//
// Values are matched case-insensitively and rewritten to the canonical
// spelling, and documented as an enum in OpenAPI.
type Enum interface {
	EnumValues() []string
}

// Validator is implemented by types that need custom validation; it runs
// after the tag rules of the type's fields, at any nesting level. Return a
// ValidationErrors for field errors (keys are relative to the type), a
// *Problem to answer with a specific status, or any other error to report
// it as a validation error of the type itself.
type Validator interface {
	Validate(ctx context.Context) error
}

// PlainValidator is the context-free form of Validator, compatible with
// sconf's bind.Validator.
type PlainValidator interface {
	Validate() error
}

// Rule is a custom validation rule for RegisterRule.
type Rule struct {
	// Check reports whether v (addressable) satisfies the rule.
	Check func(v reflect.Value, param string) bool
	// Message builds the error text; field is the error key.
	Message func(field, param string) string
	// Schema, when set, documents the rule in OpenAPI.
	Schema func(s *Schema, param string)
}

var (
	customRules   = map[string]Rule{}
	customRulesMu sync.RWMutex
)

// RegisterRule adds or replaces a rule usable in validate tags. Call it
// before routes are registered.
func RegisterRule(name string, r Rule) {
	customRulesMu.Lock()
	defer customRulesMu.Unlock()
	customRules[name] = r
}

// Validate validates v, which must be a pointer to a struct (or to any
// value with rules). It returns nil, a ValidationErrors, or the *Problem a
// Validator returned.
func Validate(ctx context.Context, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return errors.New("veyro: Validate needs a non-nil pointer")
	}
	w := walkerFor(rv.Type().Elem())
	if !w.active {
		return nil
	}
	var errs ValidationErrors
	var stack [8]pathElem
	if err := w.run(ctx, rv.UnsafePointer(), stack[:0], &errs); err != nil {
		return err
	}
	if errs != nil {
		return errs
	}
	return nil
}

// ---------- tag parsing ----------

type ruleSpec struct{ name, param string }

// parseRuleSpecs splits a validate tag. "regex=" swallows the rest of the
// tag so patterns may contain commas.
func parseRuleSpecs(tag string) []ruleSpec {
	var out []ruleSpec
	for tag != "" {
		var part string
		if strings.HasPrefix(tag, "regex=") {
			part, tag = tag, ""
		} else {
			part, tag, _ = strings.Cut(tag, ",")
		}
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, param, _ := strings.Cut(part, "=")
		out = append(out, ruleSpec{name: strings.TrimSpace(name), param: param})
	}
	return out
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

var enumType = reflect.TypeFor[Enum]()

// enumValues returns the enum set for t (through pointers and slices), or nil.
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

// ---------- compiled plans ----------

// pathElem is one segment of an error key: a name or an index.
type pathElem struct {
	name string
	idx  int
}

func keyOf(path []pathElem) string {
	var b strings.Builder
	for _, e := range path {
		if e.name == "" {
			if e.idx >= 0 {
				b.WriteString("[" + strconv.Itoa(e.idx) + "]")
			}
			continue
		}
		if e.idx >= 0 {
			b.WriteString(e.name)
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(e.idx))
			b.WriteByte(']')
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(e.name)
	}
	if b.Len() == 0 {
		return "$"
	}
	return b.String()
}

// crule is a compiled rule: a specialised check plus a lazy message.
type crule struct {
	presence bool // runs on zero values too
	check    func(p, parent unsafe.Pointer) bool
	msg      func(key string) string
	// dive, when set, applies these rules to each element instead.
	dive         []crule
	diveSize     uintptr
	diveLen      func(p unsafe.Pointer) (data unsafe.Pointer, n int)
	diveElemZero func(p unsafe.Pointer) bool
}

type fieldPlan struct {
	name   string // error key segment
	goName string
	offset uintptr
	typ    reflect.Type
	zero   func(p unsafe.Pointer) bool
	rules  []crule
	nested *walker
	flat   bool // embedded struct: no key segment
}

type walkKind uint8

const (
	walkNone walkKind = iota
	walkStruct
	walkPointer
	walkSlice
	walkArray
	walkMap
)

// walker validates one type: its fields' rules, nested walkers and the
// type's own Validator.
type walker struct {
	t         reflect.Type
	kind      walkKind
	active    bool
	fields    []fieldPlan
	elem      *walker
	elemSize  uintptr
	arrLen    int
	validator uint8 // 0 none, 1 ctx, 2 plain
}

var walkers sync.Map // reflect.Type -> *walker

var (
	validatorType      = reflect.TypeFor[Validator]()
	plainValidatorType = reflect.TypeFor[PlainValidator]()
)

func walkerFor(t reflect.Type) *walker {
	if w, ok := walkers.Load(t); ok {
		return w.(*walker)
	}
	w := &walker{t: t, active: true} // placeholder handles recursive types
	actual, loaded := walkers.LoadOrStore(t, w)
	if loaded {
		return actual.(*walker)
	}
	w.build()
	return w
}

func (w *walker) build() {
	t := w.t
	switch {
	case reflect.PointerTo(t).Implements(validatorType):
		w.validator = 1
	case reflect.PointerTo(t).Implements(plainValidatorType):
		w.validator = 2
	}
	active := w.validator != 0
	switch t.Kind() {
	case reflect.Struct:
		if t == timeType {
			w.kind = walkNone
			w.active = active
			return
		}
		w.kind = walkStruct
		for i := range t.NumField() {
			f := t.Field(i)
			fp := fieldPlan{goName: f.Name, offset: f.Offset, typ: f.Type, zero: zeroOf(f.Type)}
			flat := f.Anonymous && !hasSourceTag(f) && f.Tag.Get("json") == "" && derefType(f.Type).Kind() == reflect.Struct
			if !f.IsExported() && !flat {
				continue
			}
			fp.name = errorKey(f)
			fp.flat = flat
			if flat {
				fp.name = ""
			}
			fp.rules = compileField(t, f)
			if walkable(f.Type) {
				fp.nested = walkerFor(f.Type)
				if !fp.nested.active {
					fp.nested = nil
				}
			}
			if len(fp.rules) > 0 || fp.nested != nil {
				active = true
			}
			w.fields = append(w.fields, fp)
		}
	case reflect.Pointer:
		w.kind = walkPointer
		w.elem = walkerFor(t.Elem())
		active = active || w.elem.active
	case reflect.Slice, reflect.Array:
		if t.Kind() == reflect.Slice {
			w.kind = walkSlice
		} else {
			w.kind = walkArray
			w.arrLen = t.Len()
		}
		w.elemSize = t.Elem().Size()
		w.elem = walkerFor(t.Elem())
		active = active || w.elem.active
	case reflect.Map:
		w.kind = walkMap
		w.elem = walkerFor(t.Elem())
		active = active || w.elem.active
	}
	w.active = active
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func walkable(t reflect.Type) bool {
	t = derefType(t)
	switch t.Kind() {
	case reflect.Struct:
		return t != timeType
	case reflect.Slice, reflect.Array, reflect.Map:
		return walkable(t.Elem())
	}
	return false
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

// run validates the value of type w.t at p.
func (w *walker) run(ctx context.Context, p unsafe.Pointer, path []pathElem, errs *ValidationErrors) error {
	switch w.kind {
	case walkStruct:
		for i := range w.fields {
			f := &w.fields[i]
			fp := unsafe.Add(p, f.offset)
			if len(f.rules) > 0 {
				if err := runRules(ctx, f.rules, fp, p, f.zero, path, f.name, errs); err != nil {
					return err
				}
			}
			if f.nested != nil {
				if err := f.nested.run(ctx, fp, append(path, pathElem{name: f.name, idx: -1}), errs); err != nil {
					return err
				}
			}
		}
	case walkPointer:
		if q := *(*unsafe.Pointer)(p); q != nil {
			if err := w.elem.run(ctx, q, path, errs); err != nil {
				return err
			}
		}
	case walkSlice, walkArray:
		data, n := w.sliceData(p)
		for i := range n {
			if err := w.elem.run(ctx, unsafe.Add(data, uintptr(i)*w.elemSize), indexed(path, i), errs); err != nil {
				return err
			}
		}
		if n := len(path); n > 0 && path[n-1].name != "" {
			path[n-1].idx = -1
		}
	case walkMap:
		mv := reflect.NewAt(w.t, p).Elem()
		for it := mv.MapRange(); it.Next(); {
			ev := reflect.New(w.t.Elem())
			ev.Elem().Set(it.Value())
			if err := w.elem.run(ctx, ev.UnsafePointer(), append(path, pathElem{name: fmt.Sprint(it.Key().Interface()), idx: -1}), errs); err != nil {
				return err
			}
		}
	}
	if w.validator != 0 {
		return w.callValidator(ctx, p, path, errs)
	}
	return nil
}

type sliceHeader struct {
	data unsafe.Pointer
	len  int
	cap  int
}

func (w *walker) sliceData(p unsafe.Pointer) (unsafe.Pointer, int) {
	if w.kind == walkArray {
		return p, w.arrLen
	}
	h := (*sliceHeader)(p)
	return h.data, h.len
}

// indexed sets the index on the last path element (the field name pushed
// by the enclosing struct, which is used by nobody else) or pushes an index
// for a bare slice. No allocation on the common path.
func indexed(path []pathElem, i int) []pathElem {
	if n := len(path); n > 0 && path[n-1].name != "" {
		path[n-1].idx = i
		return path
	}
	return append(path, pathElem{name: "", idx: i})
}

func (w *walker) callValidator(ctx context.Context, p unsafe.Pointer, path []pathElem, errs *ValidationErrors) error {
	var err error
	ptr := reflect.NewAt(w.t, p).Interface()
	if w.validator == 1 {
		err = ptr.(Validator).Validate(ctx)
	} else {
		err = ptr.(PlainValidator).Validate()
	}
	if err == nil {
		return nil
	}
	var ve ValidationErrors
	var prob *Problem
	switch {
	case errors.As(err, &ve):
		prefix := keyOf(path)
		for k, msgs := range ve {
			key := k
			if prefix != "$" {
				if strings.HasPrefix(k, "[") {
					key = prefix + k
				} else {
					key = prefix + "." + k
				}
			}
			for _, m := range msgs {
				errs.add(key, m)
			}
		}
	case errors.As(err, &prob):
		return prob
	default:
		errs.add(keyOf(path), err.Error())
	}
	return nil
}

// runRules applies rules to the field at p (parent struct at parent).
func runRules(ctx context.Context, rules []crule, p, parent unsafe.Pointer, zero func(unsafe.Pointer) bool, path []pathElem, name string, errs *ValidationErrors) error {
	isZero := zero(p)
	for i := range rules {
		r := &rules[i]
		if r.diveLen != nil {
			if isZero {
				continue
			}
			data, n := r.diveLen(p)
			for j := range n {
				ep := unsafe.Add(data, uintptr(j)*r.diveSize)
				elemPath := append(path, pathElem{name: name, idx: j})
				if err := runRules(ctx, r.dive, ep, parent, r.diveElemZero, elemPath, "", errs); err != nil {
					return err
				}
			}
			continue
		}
		if isZero && !r.presence {
			continue
		}
		if !r.check(p, parent) {
			key := keyOf(append(path, pathElem{name: name, idx: -1}))
			errs.add(key, r.msg(key))
		}
	}
	return nil
}

// ---------- compilation ----------

func compileField(owner reflect.Type, f reflect.StructField) []crule {
	where := owner.Name() + "." + f.Name
	specs := parseRuleSpecs(f.Tag.Get("validate"))
	var rules []crule
	if set := fieldEnum(f); len(set) > 0 && !slices.ContainsFunc(specs, func(s ruleSpec) bool { return s.name == "oneof" || s.name == "enum" }) {
		rules = append(rules, compileEnum(f.Type, set, where))
	}
	for i := 0; i < len(specs); i++ {
		s := specs[i]
		if s.name == "omitempty" || s.name == "omitnil" || s.name == "structonly" {
			continue // validator v10 tags: zero values are skipped by default
		}
		if s.name == "dive" {
			rest := specs[i+1:]
			t := f.Type
			for t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
				panic(fmt.Sprintf("veyro: %s: dive needs a slice or array", where))
			}
			elem := t.Elem()
			d := crule{diveSize: elem.Size(), diveElemZero: zeroOf(elem)}
			d.diveLen = sliceLenOf(f.Type, t)
			for _, es := range rest {
				if es.name == "omitempty" || es.name == "omitnil" {
					continue
				}
				d.dive = append(d.dive, compileRule(owner, elem, es, where))
			}
			if len(d.dive) > 0 { // a bare "dive" only documents intent; nesting is automatic
				rules = append(rules, d)
			}
			break
		}
		if s.name == "enum" {
			rules = append(rules, compileEnum(f.Type, fieldEnum(f), where))
			continue
		}
		rules = append(rules, compileRule(owner, f.Type, s, where))
	}
	return rules
}

// sliceLenOf returns a (data, len) accessor for a slice/array field that
// may sit behind pointers.
func sliceLenOf(ft, t reflect.Type) func(unsafe.Pointer) (unsafe.Pointer, int) {
	deref := ptrDeref(ft)
	if t.Kind() == reflect.Array {
		n := t.Len()
		return func(p unsafe.Pointer) (unsafe.Pointer, int) {
			if p = deref(p); p == nil {
				return nil, 0
			}
			return p, n
		}
	}
	return func(p unsafe.Pointer) (unsafe.Pointer, int) {
		if p = deref(p); p == nil {
			return nil, 0
		}
		h := (*sliceHeader)(p)
		return h.data, h.len
	}
}

// ptrDeref returns a function that follows pointers of t (nil-safe).
func ptrDeref(t reflect.Type) func(unsafe.Pointer) unsafe.Pointer {
	depth := 0
	for t.Kind() == reflect.Pointer {
		depth++
		t = t.Elem()
	}
	if depth == 0 {
		return func(p unsafe.Pointer) unsafe.Pointer { return p }
	}
	return func(p unsafe.Pointer) unsafe.Pointer {
		for range depth {
			if p == nil {
				return nil
			}
			p = *(*unsafe.Pointer)(p)
		}
		return p
	}
}

func compileEnum(ft reflect.Type, set []string, where string) crule {
	if len(set) == 0 {
		panic(fmt.Sprintf("veyro: %s: enum rule without values", where))
	}
	lower := make([]string, len(set))
	for i, s := range set {
		lower[i] = strings.ToLower(s)
	}
	match := func(sp *string) bool {
		s := *sp
		for i, l := range lower {
			if s == set[i] {
				return true
			}
			if strings.EqualFold(s, l) {
				*sp = set[i] // canonicalise
				return true
			}
		}
		return false
	}
	list := strings.Join(set, ", ")
	return stringRule(ft, where, "enum", match,
		func(key string) string { return fmt.Sprintf("The field %s must be one of: %s.", key, list) },
		func(key string) string { return fmt.Sprintf("The field %s must contain only: %s.", key, list) })
}

// stringRule compiles a check over a string field; for slices/arrays of
// strings it applies to every element.
func stringRule(ft reflect.Type, where, name string, match func(*string) bool, msg, msgEach func(string) string) crule {
	t := derefType(ft)
	deref := ptrDeref(ft)
	switch {
	case t.Kind() == reflect.String:
		return crule{check: func(p, _ unsafe.Pointer) bool {
			if p = deref(p); p == nil {
				return true
			}
			return match((*string)(p))
		}, msg: msg}
	case (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) && t.Elem().Kind() == reflect.String:
		lenOf := sliceLenOf(ft, t)
		size := t.Elem().Size()
		return crule{check: func(p, _ unsafe.Pointer) bool {
			data, n := lenOf(p)
			for i := range n {
				if !match((*string)(unsafe.Add(data, uintptr(i)*size))) {
					return false
				}
			}
			return true
		}, msg: msgEach}
	}
	panic(fmt.Sprintf("veyro: %s: rule %q needs a string field, got %s", where, name, ft))
}

func compileRule(owner, ft reflect.Type, s ruleSpec, where string) crule {
	t := derefType(ft)
	deref := ptrDeref(ft)
	bad := func(what string) crule {
		panic(fmt.Sprintf("veyro: %s: rule %q %s", where, s.name, what))
	}
	strRule := func(match func(*string) bool, text string) crule {
		return stringRule(ft, where, s.name, match,
			func(key string) string { return fmt.Sprintf("The field %s %s.", key, text) },
			func(key string) string {
				return fmt.Sprintf("The field %s must contain only values that %s.", key, strings.TrimPrefix(text, "must "))
			})
	}
	numParam := func() float64 {
		f, err := strconv.ParseFloat(s.param, 64)
		if err != nil {
			bad("needs a numeric argument")
		}
		return f
	}
	intParam := func() int {
		n, err := strconv.Atoi(s.param)
		if err != nil {
			bad("needs an integer argument")
		}
		return n
	}

	switch s.name {
	// ----- presence -----
	case "required":
		return crule{presence: true, check: func(p, _ unsafe.Pointer) bool { return !zeroOf(ft)(p) },
			msg: func(key string) string { return fmt.Sprintf("The %s field is required.", key) }}
	case "required_if", "required_unless":
		field, value, _ := strings.Cut(s.param, " ")
		eq := siblingEquals(owner, field, value, where)
		zero := zeroOf(ft)
		want := s.name == "required_if"
		return crule{presence: true, check: func(p, parent unsafe.Pointer) bool {
			return !(eq(parent) == want && zero(p))
		}, msg: func(key string) string { return fmt.Sprintf("The %s field is required.", key) }}
	case "required_with", "required_without", "required_with_all", "required_without_all":
		var tests []func(unsafe.Pointer) bool // true when the sibling is set
		for name := range strings.FieldsSeq(s.param) {
			tests = append(tests, siblingSet(owner, name, where))
		}
		if len(tests) == 0 {
			bad("needs sibling field names")
		}
		zero := zeroOf(ft)
		withSet := s.name == "required_with" || s.name == "required_with_all"
		all := strings.HasSuffix(s.name, "_all")
		return crule{presence: true, check: func(p, parent unsafe.Pointer) bool {
			if !zero(p) {
				return true
			}
			matched := 0
			for _, tst := range tests {
				if tst(parent) == withSet {
					matched++
				}
			}
			if all {
				return matched != len(tests)
			}
			return matched == 0
		}, msg: func(key string) string { return fmt.Sprintf("The %s field is required.", key) }}

	// ----- numbers and lengths -----
	case "min", "max", "len", "gt", "gte", "lt", "lte", "minlen", "maxlen", "eq", "ne":
		name := s.name
		if name == "minlen" {
			name = "min"
		} else if name == "maxlen" {
			name = "max"
		}
		if isNumeric(t) {
			n := numParam()
			get := floatGetter(t, deref)
			var op func(float64) bool
			switch name {
			case "min", "gte":
				op = func(v float64) bool { return v >= n }
			case "max", "lte":
				op = func(v float64) bool { return v <= n }
			case "gt":
				op = func(v float64) bool { return v > n }
			case "lt":
				op = func(v float64) bool { return v < n }
			case "len", "eq":
				op = func(v float64) bool { return v == n }
			case "ne":
				op = func(v float64) bool { return v != n }
			}
			text := map[string]string{"min": "greater than or equal to", "gte": "greater than or equal to", "max": "less than or equal to",
				"lte": "less than or equal to", "gt": "greater than", "lt": "less than", "len": "equal to", "eq": "equal to", "ne": "not equal to"}[name]
			return crule{check: func(p, _ unsafe.Pointer) bool {
				v, ok := get(p)
				return !ok || op(v)
			}, msg: func(key string) string { return fmt.Sprintf("The field %s must be %s %v.", key, text, n) }}
		}
		if t.Kind() == reflect.String && (name == "eq" || name == "ne") {
			want := s.param
			if name == "eq" {
				return strRule(func(sp *string) bool { return *sp == want }, fmt.Sprintf("must be equal to '%s'", want))
			}
			return strRule(func(sp *string) bool { return *sp != want }, fmt.Sprintf("must not be equal to '%s'", want))
		}
		length := lengthGetter(ft, t, deref)
		if length == nil {
			bad("needs a number, string, slice, array or map field")
		}
		n := intParam()
		var op func(int) bool
		var text string
		switch name {
		case "min", "gte":
			op, text = func(l int) bool { return l >= n }, fmt.Sprintf("a minimum length of '%d'", n)
		case "max", "lte":
			op, text = func(l int) bool { return l <= n }, fmt.Sprintf("a maximum length of '%d'", n)
		case "gt":
			op, text = func(l int) bool { return l > n }, fmt.Sprintf("a length greater than '%d'", n)
		case "lt":
			op, text = func(l int) bool { return l < n }, fmt.Sprintf("a length less than '%d'", n)
		case "len", "eq":
			op, text = func(l int) bool { return l == n }, fmt.Sprintf("a length of '%d'", n)
		case "ne":
			op, text = func(l int) bool { return l != n }, fmt.Sprintf("a length other than '%d'", n)
		}
		return crule{check: func(p, _ unsafe.Pointer) bool {
			l, ok := length(p)
			return !ok || op(l)
		}, msg: func(key string) string {
			return fmt.Sprintf("The field %s must be a string or array type with %s.", key, text)
		}}

	// ----- strings -----
	case "oneof":
		set := strings.Fields(s.param)
		if len(set) == 0 {
			bad("needs space-separated values")
		}
		list := strings.Join(set, ", ")
		if isNumeric(t) {
			get := floatGetter(t, deref)
			nums := make([]float64, len(set))
			for i, v := range set {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					bad("has non-numeric values for a numeric field")
				}
				nums[i] = f
			}
			return crule{check: func(p, _ unsafe.Pointer) bool {
				v, ok := get(p)
				return !ok || slices.Contains(nums, v)
			}, msg: func(key string) string { return fmt.Sprintf("The field %s must be one of: %s.", key, list) }}
		}
		return stringRule(ft, where, s.name, func(sp *string) bool { return slices.Contains(set, *sp) },
			func(key string) string { return fmt.Sprintf("The field %s must be one of: %s.", key, list) },
			func(key string) string { return fmt.Sprintf("The field %s must contain only: %s.", key, list) })
	case "contains":
		return strRule(func(sp *string) bool { return strings.Contains(*sp, s.param) }, fmt.Sprintf("must contain '%s'", s.param))
	case "excludes":
		return strRule(func(sp *string) bool { return !strings.Contains(*sp, s.param) }, fmt.Sprintf("must not contain '%s'", s.param))
	case "startswith":
		return strRule(func(sp *string) bool { return strings.HasPrefix(*sp, s.param) }, fmt.Sprintf("must start with '%s'", s.param))
	case "endswith":
		return strRule(func(sp *string) bool { return strings.HasSuffix(*sp, s.param) }, fmt.Sprintf("must end with '%s'", s.param))
	case "alpha":
		return strRule(allRunes(func(r rune) bool { return r < 128 && unicode.IsLetter(r) }), "must contain only letters")
	case "alphanum":
		return strRule(allRunes(func(r rune) bool { return r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) }), "must contain only letters and digits")
	case "alphaunicode":
		return strRule(allRunes(unicode.IsLetter), "must contain only letters")
	case "alphanumunicode":
		return strRule(allRunes(func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }), "must contain only letters and digits")
	case "numeric", "number":
		return strRule(func(sp *string) bool {
			_, err := strconv.ParseFloat(*sp, 64)
			return err == nil && !strings.ContainsAny(*sp, "eEnNiI")
		}, "must be a number")
	case "lowercase":
		return strRule(func(sp *string) bool { return *sp == strings.ToLower(*sp) }, "must be lowercase")
	case "uppercase":
		return strRule(func(sp *string) bool { return *sp == strings.ToUpper(*sp) }, "must be uppercase")
	case "ascii":
		return strRule(allRunes(func(r rune) bool { return r < 128 }), "must contain only ASCII characters")
	case "printascii":
		return strRule(allRunes(func(r rune) bool { return r >= 32 && r < 127 }), "must contain only printable ASCII characters")
	case "boolean":
		return strRule(func(sp *string) bool { _, err := strconv.ParseBool(*sp); return err == nil }, "must be a boolean")
	case "regex":
		re, err := regexp.Compile(s.param)
		if err != nil {
			bad("has an invalid pattern: " + err.Error())
		}
		return strRule(func(sp *string) bool { return re.MatchString(*sp) }, fmt.Sprintf("must match the regular expression '%s'", s.param))
	case "email":
		return stringRule(ft, where, s.name, func(sp *string) bool { return looksLikeEmail(*sp) },
			func(key string) string { return fmt.Sprintf("The %s field is not a valid e-mail address.", key) },
			func(key string) string {
				return fmt.Sprintf("The field %s must contain only valid e-mail addresses.", key)
			})
	case "url", "http_url":
		httpOnly := s.name == "http_url"
		return stringRule(ft, where, s.name, func(sp *string) bool {
			scheme, ok := absoluteURL(*sp)
			if !ok {
				return false
			}
			return !httpOnly || scheme == "http" || scheme == "https"
		}, func(key string) string {
			return fmt.Sprintf("The %s field is not a valid fully-qualified http, https, or ftp URL.", key)
		}, func(key string) string { return fmt.Sprintf("The field %s must contain only valid URLs.", key) })
	case "uri":
		return strRule(func(sp *string) bool { u, err := url.Parse(*sp); return err == nil && u.Scheme != "" }, "must be a valid URI")
	case "uuid":
		return strRule(func(sp *string) bool { _, err := uuid.Parse(*sp); return err == nil }, "must be a valid UUID")
	case "ip":
		return strRule(func(sp *string) bool { _, err := netip.ParseAddr(*sp); return err == nil }, "must be a valid IP address")
	case "ipv4":
		return strRule(func(sp *string) bool { a, err := netip.ParseAddr(*sp); return err == nil && a.Is4() }, "must be a valid IPv4 address")
	case "ipv6":
		return strRule(func(sp *string) bool { a, err := netip.ParseAddr(*sp); return err == nil && a.Is6() && !a.Is4In6() }, "must be a valid IPv6 address")
	case "cidr":
		return strRule(func(sp *string) bool { _, err := netip.ParsePrefix(*sp); return err == nil }, "must be a valid CIDR")
	case "mac":
		return strRule(func(sp *string) bool { _, err := net.ParseMAC(*sp); return err == nil }, "must be a valid MAC address")
	case "hostname":
		return strRule(func(sp *string) bool { return isHostname(*sp, false) }, "must be a valid hostname")
	case "fqdn":
		return strRule(func(sp *string) bool { return isHostname(*sp, true) }, "must be a fully qualified domain name")
	case "datetime":
		layout := s.param
		if layout == "" {
			bad("needs a time layout")
		}
		return strRule(func(sp *string) bool { _, err := time.Parse(layout, *sp); return err == nil }, fmt.Sprintf("must be a date/time in the format '%s'", layout))
	case "base64":
		return strRule(func(sp *string) bool { _, err := base64.StdEncoding.DecodeString(*sp); return err == nil }, "must be valid base64")
	case "json":
		return strRule(func(sp *string) bool { return jsontext.Value(*sp).IsValid() }, "must be valid JSON")
	case "hexadecimal":
		return strRule(func(sp *string) bool {
			h := strings.TrimPrefix(strings.TrimPrefix(*sp, "0x"), "0X")
			return h != "" && allRunes(isHex)(&h)
		}, "must be a hexadecimal number")
	case "hexcolor":
		return strRule(func(sp *string) bool {
			h := strings.TrimPrefix(*sp, "#")
			return (len(h) == 3 || len(h) == 4 || len(h) == 6 || len(h) == 8) && allRunes(isHex)(&h)
		}, "must be a hex colour")
	case "e164":
		return strRule(func(sp *string) bool {
			v := *sp
			if len(v) < 3 || len(v) > 16 || v[0] != '+' || v[1] == '0' {
				return false
			}
			return allRunes(unicode.IsDigit)(ptrTo(v[1:]))
		}, "must be a phone number in E.164 format")

	// ----- cross-field -----
	case "eqfield", "nefield", "gtfield", "gtefield", "ltfield", "ltefield":
		cmp := siblingCompare(owner, ft, s.param, where)
		var ok func(int) bool
		var text string
		switch s.name {
		case "eqfield":
			ok, text = func(c int) bool { return c == 0 }, "match"
		case "nefield":
			ok, text = func(c int) bool { return c != 0 }, "differ"
		case "gtfield":
			ok, text = func(c int) bool { return c > 0 }, "be greater"
		case "gtefield":
			ok, text = func(c int) bool { return c >= 0 }, "be greater or equal"
		case "ltfield":
			ok, text = func(c int) bool { return c < 0 }, "be less"
		case "ltefield":
			ok, text = func(c int) bool { return c <= 0 }, "be less or equal"
		}
		other := s.param
		return crule{check: func(p, parent unsafe.Pointer) bool {
			c, comparable := cmp(p, parent)
			return !comparable || ok(c)
		}, msg: func(key string) string {
			if text == "match" {
				return fmt.Sprintf("'%s' and '%s' do not match.", key, other)
			}
			return fmt.Sprintf("The field %s must %s than %s.", key, text, other)
		}}

	// ----- collections -----
	case "unique":
		if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
			bad("needs a slice or array")
		}
		if t.Elem().Kind() == reflect.String {
			lenOf := sliceLenOf(ft, t)
			size := t.Elem().Size()
			return crule{check: func(p, _ unsafe.Pointer) bool {
				data, n := lenOf(p)
				seen := make(map[string]struct{}, n)
				for i := range n {
					s := *(*string)(unsafe.Add(data, uintptr(i)*size))
					if _, dup := seen[s]; dup {
						return false
					}
					seen[s] = struct{}{}
				}
				return true
			}, msg: func(key string) string { return fmt.Sprintf("The field %s must not contain duplicates.", key) }}
		}
		if !t.Elem().Comparable() {
			bad("needs comparable elements")
		}
		return crule{check: func(p, _ unsafe.Pointer) bool {
			if p = deref(p); p == nil {
				return true
			}
			v := reflect.NewAt(t, p).Elem()
			seen := make(map[any]struct{}, v.Len())
			for i := range v.Len() {
				k := v.Index(i).Interface()
				if _, dup := seen[k]; dup {
					return false
				}
				seen[k] = struct{}{}
			}
			return true
		}, msg: func(key string) string { return fmt.Sprintf("The field %s must not contain duplicates.", key) }}
	}

	// ----- custom -----
	customRulesMu.RLock()
	cr, ok := customRules[s.name]
	customRulesMu.RUnlock()
	if !ok {
		bad("is unknown")
	}
	param := s.param
	return crule{check: func(p, _ unsafe.Pointer) bool {
		return cr.Check(reflect.NewAt(ft, p).Elem(), param)
	}, msg: func(key string) string {
		if cr.Message != nil {
			return cr.Message(key, param)
		}
		return fmt.Sprintf("The field %s is invalid.", key)
	}}
}

func ptrTo(s string) *string { return &s }

// absoluteURL checks "scheme://host..." without allocating: a valid scheme,
// a non-empty host and no whitespace or control characters. It returns the
// scheme in lower case for http/https checks.
func absoluteURL(s string) (string, bool) {
	i := strings.Index(s, "://")
	if i <= 0 || i+3 >= len(s) {
		return "", false
	}
	for j, r := range s[:i] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case j > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return "", false
		}
	}
	rest := s[i+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	if end == 0 {
		return "", false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return "", false
		}
	}
	scheme := s[:i]
	if scheme == "http" || scheme == "https" || scheme == "HTTP" || scheme == "HTTPS" {
		return strings.ToLower(scheme), true
	}
	return scheme, true
}

func isHex(r rune) bool {
	return unicode.IsDigit(r) || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func allRunes(ok func(rune) bool) func(*string) bool {
	return func(sp *string) bool {
		for _, r := range *sp {
			if !ok(r) {
				return false
			}
		}
		return true
	}
}

// looksLikeEmail mirrors ASP.NET Core's EmailAddressAttribute: exactly one
// '@' that is neither first nor last.
func looksLikeEmail(s string) bool {
	i := strings.IndexByte(s, '@')
	return i > 0 && i < len(s)-1 && strings.IndexByte(s[i+1:], '@') < 0
}

func isHostname(s string, fqdn bool) bool {
	s = strings.TrimSuffix(s, ".")
	if s == "" || len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	if fqdn && len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, r := range l {
			if !(r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-')) {
				return false
			}
		}
	}
	if fqdn {
		tld := labels[len(labels)-1]
		for _, r := range tld {
			if unicode.IsDigit(r) {
				return false
			}
		}
	}
	return true
}

// ---------- typed accessors ----------

func isNumeric(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// floatGetter reads a numeric field (through pointers) as float64.
func floatGetter(t reflect.Type, deref func(unsafe.Pointer) unsafe.Pointer) func(unsafe.Pointer) (float64, bool) {
	var read func(unsafe.Pointer) float64
	switch t.Kind() {
	case reflect.Int:
		read = func(p unsafe.Pointer) float64 { return float64(*(*int)(p)) }
	case reflect.Int8:
		read = func(p unsafe.Pointer) float64 { return float64(*(*int8)(p)) }
	case reflect.Int16:
		read = func(p unsafe.Pointer) float64 { return float64(*(*int16)(p)) }
	case reflect.Int32:
		read = func(p unsafe.Pointer) float64 { return float64(*(*int32)(p)) }
	case reflect.Int64:
		read = func(p unsafe.Pointer) float64 { return float64(*(*int64)(p)) }
	case reflect.Uint:
		read = func(p unsafe.Pointer) float64 { return float64(*(*uint)(p)) }
	case reflect.Uint8:
		read = func(p unsafe.Pointer) float64 { return float64(*(*uint8)(p)) }
	case reflect.Uint16:
		read = func(p unsafe.Pointer) float64 { return float64(*(*uint16)(p)) }
	case reflect.Uint32:
		read = func(p unsafe.Pointer) float64 { return float64(*(*uint32)(p)) }
	case reflect.Uint64:
		read = func(p unsafe.Pointer) float64 { return float64(*(*uint64)(p)) }
	case reflect.Uintptr:
		read = func(p unsafe.Pointer) float64 { return float64(*(*uintptr)(p)) }
	case reflect.Float32:
		read = func(p unsafe.Pointer) float64 { return float64(*(*float32)(p)) }
	case reflect.Float64:
		read = func(p unsafe.Pointer) float64 { return *(*float64)(p) }
	}
	return func(p unsafe.Pointer) (float64, bool) {
		if p = deref(p); p == nil {
			return 0, false
		}
		return read(p), true
	}
}

// lengthGetter reads the length of a string, slice, array or map field.
func lengthGetter(ft, t reflect.Type, deref func(unsafe.Pointer) unsafe.Pointer) func(unsafe.Pointer) (int, bool) {
	switch t.Kind() {
	case reflect.String:
		return func(p unsafe.Pointer) (int, bool) {
			if p = deref(p); p == nil {
				return 0, false
			}
			return len(*(*string)(p)), true
		}
	case reflect.Slice:
		return func(p unsafe.Pointer) (int, bool) {
			if p = deref(p); p == nil {
				return 0, false
			}
			return (*sliceHeader)(p).len, true
		}
	case reflect.Array:
		n := t.Len()
		return func(p unsafe.Pointer) (int, bool) { return n, deref(p) != nil }
	case reflect.Map:
		return func(p unsafe.Pointer) (int, bool) {
			if p = deref(p); p == nil {
				return 0, false
			}
			return reflect.NewAt(t, p).Elem().Len(), true
		}
	}
	return nil
}

// zeroOf returns a zero test specialised for t.
func zeroOf(t reflect.Type) func(unsafe.Pointer) bool {
	switch t.Kind() {
	case reflect.String:
		return func(p unsafe.Pointer) bool { return *(*string)(p) == "" }
	case reflect.Bool:
		return func(p unsafe.Pointer) bool { return !*(*bool)(p) }
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		switch t.Size() {
		case 1:
			return func(p unsafe.Pointer) bool { return *(*uint8)(p) == 0 }
		case 2:
			return func(p unsafe.Pointer) bool { return *(*uint16)(p) == 0 }
		case 4:
			return func(p unsafe.Pointer) bool { return *(*uint32)(p) == 0 }
		default:
			return func(p unsafe.Pointer) bool { return *(*uint64)(p) == 0 }
		}
	case reflect.Float32:
		return func(p unsafe.Pointer) bool { return *(*float32)(p) == 0 }
	case reflect.Float64:
		return func(p unsafe.Pointer) bool { return *(*float64)(p) == 0 }
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return func(p unsafe.Pointer) bool { return *(*unsafe.Pointer)(p) == nil }
	case reflect.Slice:
		return func(p unsafe.Pointer) bool { return (*sliceHeader)(p).data == nil }
	case reflect.Interface:
		return func(p unsafe.Pointer) bool { return (*[2]unsafe.Pointer)(p)[0] == nil }
	}
	// structs, arrays, complex: reflect, correct for everything
	return func(p unsafe.Pointer) bool { return reflect.NewAt(t, p).Elem().IsZero() }
}

// ---------- sibling access for cross-field and conditional rules ----------

func siblingField(owner reflect.Type, name, where string) reflect.StructField {
	for i := range owner.NumField() {
		f := owner.Field(i)
		if f.Name == name || errorKey(f) == name {
			return f
		}
	}
	panic(fmt.Sprintf("veyro: %s: no sibling field %q", where, name))
}

// siblingSet reports whether the named sibling is non-zero.
func siblingSet(owner reflect.Type, name, where string) func(parent unsafe.Pointer) bool {
	f := siblingField(owner, name, where)
	zero := zeroOf(f.Type)
	return func(parent unsafe.Pointer) bool { return !zero(unsafe.Add(parent, f.Offset)) }
}

// siblingEquals reports whether the sibling's value formats to literal.
func siblingEquals(owner reflect.Type, name, literal, where string) func(parent unsafe.Pointer) bool {
	f := siblingField(owner, name, where)
	t := derefType(f.Type)
	deref := ptrDeref(f.Type)
	switch {
	case t.Kind() == reflect.String:
		return func(parent unsafe.Pointer) bool {
			p := deref(unsafe.Add(parent, f.Offset))
			return p != nil && *(*string)(p) == literal
		}
	case t.Kind() == reflect.Bool:
		want, _ := strconv.ParseBool(literal)
		return func(parent unsafe.Pointer) bool {
			p := deref(unsafe.Add(parent, f.Offset))
			return p != nil && *(*bool)(p) == want
		}
	case isNumeric(t):
		want, err := strconv.ParseFloat(literal, 64)
		if err != nil {
			panic(fmt.Sprintf("veyro: %s: %q is not a number for field %s", where, literal, name))
		}
		get := floatGetter(t, deref)
		return func(parent unsafe.Pointer) bool {
			v, ok := get(unsafe.Add(parent, f.Offset))
			return ok && v == want
		}
	}
	return func(parent unsafe.Pointer) bool {
		return fmt.Sprint(reflect.NewAt(f.Type, unsafe.Add(parent, f.Offset)).Elem().Interface()) == literal
	}
}

// siblingCompare compares the field at p with the named sibling: -1, 0, 1.
func siblingCompare(owner, ft reflect.Type, name, where string) func(p, parent unsafe.Pointer) (int, bool) {
	f := siblingField(owner, name, where)
	t, ot := derefType(ft), derefType(f.Type)
	deref, oderef := ptrDeref(ft), ptrDeref(f.Type)
	switch {
	case isNumeric(t) && isNumeric(ot):
		get, oget := floatGetter(t, deref), floatGetter(ot, oderef)
		return func(p, parent unsafe.Pointer) (int, bool) {
			a, ok1 := get(p)
			b, ok2 := oget(unsafe.Add(parent, f.Offset))
			if !ok1 || !ok2 {
				return 0, false
			}
			return cmpFloat(a, b), true
		}
	case t.Kind() == reflect.String && ot.Kind() == reflect.String:
		return func(p, parent unsafe.Pointer) (int, bool) {
			a, b := deref(p), oderef(unsafe.Add(parent, f.Offset))
			if a == nil || b == nil {
				return 0, false
			}
			return strings.Compare(*(*string)(a), *(*string)(b)), true
		}
	case t == timeType && ot == timeType:
		return func(p, parent unsafe.Pointer) (int, bool) {
			a, b := deref(p), oderef(unsafe.Add(parent, f.Offset))
			if a == nil || b == nil {
				return 0, false
			}
			return (*time.Time)(a).Compare(*(*time.Time)(b)), true
		}
	}
	panic(fmt.Sprintf("veyro: %s: cannot compare with field %s (%s vs %s)", where, name, ft, f.Type))
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
