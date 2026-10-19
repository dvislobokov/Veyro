package veyro_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dvislobokov/veyro"
)

func check(t *testing.T, v any, want map[string]string) {
	t.Helper()
	err := veyro.Validate(context.Background(), v)
	var ve veyro.ValidationErrors
	if err != nil && !errors.As(err, &ve) {
		t.Fatalf("unexpected error type %T: %v", err, err)
	}
	for key, msg := range want {
		got := ve[key]
		if len(got) == 0 {
			t.Errorf("%T: missing error for %q (have %v)", v, key, ve)
			continue
		}
		if msg != "" && !strings.Contains(got[0], msg) {
			t.Errorf("%T: %q = %q, want substring %q", v, key, got[0], msg)
		}
	}
	for key := range ve {
		if _, ok := want[key]; !ok {
			t.Errorf("%T: unexpected error %q: %v", v, key, ve[key])
		}
	}
}

type Signup struct {
	Email    string    `json:"email" validate:"required,email"`
	Phone    *string   `json:"phone" validate:"required_without=Email,e164"`
	Password string    `json:"password" validate:"required,min=8,max=64"`
	Confirm  string    `json:"confirm" validate:"eqfield=Password"`
	Plan     string    `json:"plan" validate:"oneof=free pro"`
	Company  string    `json:"company" validate:"required_if=Plan pro,min=2"`
	Seats    int       `json:"seats" validate:"required_if=plan pro,gte=1,lte=500"`
	Age      int       `json:"age" validate:"gt=17,lt=130"`
	Site     string    `json:"site" validate:"http_url"`
	Nick     string    `json:"nick" validate:"alphanum,len=5"`
	Tags     []string  `json:"tags" validate:"max=3,unique,dive,min=2,lowercase"`
	Codes    []string  `json:"codes" validate:"hexadecimal"` // string rule on slice applies to each element
	Start    time.Time `json:"start"`
	End      time.Time `json:"end" validate:"gtefield=Start"`
	Referrer string    `json:"referrer" validate:"regex=^[a-z]+(,[a-z]+)*$"`
}

func TestRulesPresenceAndCrossField(t *testing.T) {
	check(t, &Signup{}, map[string]string{
		"email":    "The email field is required.",
		"phone":    "The phone field is required.",
		"password": "The password field is required.",
	})
	pw := "hunter2hunter2"
	check(t, &Signup{Email: "a@b.c", Password: pw, Confirm: "nope", Plan: "pro", Seats: 0, Age: 17,
		Site: "ftp://x.org", Nick: "ab_", Tags: []string{"Go", "Go", "x"}, Codes: []string{"ff", "zz"},
		Start: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Referrer: "ab,1"},
		map[string]string{
			"confirm":  "'confirm' and 'Password' do not match.",
			"company":  "The company field is required.",
			"seats":    "The seats field is required.",
			"age":      "must be greater than 17",
			"site":     "not a valid fully-qualified http, https, or ftp URL",
			"nick":     "", // alphanum fails first
			"tags":     "must not contain duplicates",
			"tags[0]":  "must be lowercase",
			"tags[1]":  "must be lowercase",
			"tags[2]":  "minimum length of '2'",
			"codes":    "must contain only values that be a hexadecimal number",
			"end":      "must be greater or equal than Start",
			"referrer": "must match the regular expression",
		})
	check(t, &Signup{Email: "a@b.c", Password: pw, Confirm: pw, Plan: "pro", Company: "ACME", Seats: 10, Age: 30,
		Site: "https://acme.io", Nick: "ab1cd", Tags: []string{"go", "http"}, Codes: []string{"0xff", "beef"},
		End: time.Now(), Referrer: "ab,cd"}, nil)
}

type VRole string

func (VRole) EnumValues() []string { return []string{"Admin", "User"} }

type EnumDoc struct {
	VRole  VRole    `json:"role"`
	Status string   `json:"status" enum:"active,blocked"`
	Roles  []VRole  `json:"roles"`
	Level  int      `json:"level" validate:"oneof=1 2 3"`
	IPs    []string `json:"ips" validate:"dive,ip"`
}

func TestEnumCanonicalisation(t *testing.T) {
	v := &EnumDoc{VRole: "ADMIN", Status: "Active", Roles: []VRole{"user", "admin"}, Level: 2, IPs: []string{"10.0.0.1", "::1"}}
	check(t, v, nil)
	if v.VRole != "Admin" || v.Status != "active" || v.Roles[0] != "User" || v.Roles[1] != "Admin" {
		t.Errorf("not canonicalised: %+v", v)
	}
	check(t, &EnumDoc{VRole: "root", Status: "gone", Roles: []VRole{"User", "x"}, Level: 4, IPs: []string{"nope"}}, map[string]string{
		"role":   "must be one of: Admin, User",
		"status": "must be one of: active, blocked",
		"roles":  "must contain only: Admin, User",
		"level":  "must be one of: 1, 2, 3",
		"ips[0]": "must be a valid IP address",
	})
}

type Line struct {
	SKU string `json:"sku" validate:"required"`
	Qty int    `json:"qty" validate:"gte=1"`
}

func (l *Line) Validate() error { // sconf-style validator on a nested type
	if l.SKU == "GONE" {
		return errors.New("sku is discontinued")
	}
	return nil
}

type Order struct {
	Lines   []Line          `json:"lines" validate:"required,min=1"`
	ByName  map[string]Line `json:"byName"`
	Coupon  *Coupon         `json:"coupon"`
	Comment string          `json:"comment" validate:"max=10"`
}

type Coupon struct {
	Code string `json:"code" validate:"required,uppercase"`
}

func (c *Coupon) Validate(ctx context.Context) error {
	if c.Code == "TEAPOT" {
		return veyro.NewProblem(418, "no coupons for teapots")
	}
	if c.Code == "OLD" {
		return veyro.Invalid("code", "This coupon has expired.")
	}
	return nil
}

func TestNestedValidatorsAndKeys(t *testing.T) {
	check(t, &Order{Lines: []Line{{SKU: "A", Qty: 1}, {SKU: "", Qty: -1}, {SKU: "GONE", Qty: 2}},
		ByName: map[string]Line{"x": {SKU: "", Qty: 5}}, Coupon: &Coupon{Code: "OLD"}, Comment: strings.Repeat("c", 11)},
		map[string]string{
			"lines[1].sku": "required",
			"lines[1].qty": "greater than or equal to 1",
			"lines[2]":     "sku is discontinued",
			"byName.x.sku": "required",
			"coupon.code":  "This coupon has expired.",
			"comment":      "maximum length of '10'",
		})
	err := veyro.Validate(context.Background(), &Order{Lines: []Line{{SKU: "A", Qty: 1}}, Coupon: &Coupon{Code: "TEAPOT"}})
	var p *veyro.Problem
	if !errors.As(err, &p) || p.Status != 418 {
		t.Errorf("problem from nested validator must win: %v", err)
	}
}

func TestCustomRuleAndBadTags(t *testing.T) {
	veyro.RegisterRule("even", veyro.Rule{
		Check:   func(v reflect.Value, _ string) bool { return v.Int()%2 == 0 },
		Message: func(field, _ string) string { return "The field " + field + " must be even." },
	})
	type N struct {
		V int `json:"v" validate:"even"`
	}
	check(t, &N{V: 3}, map[string]string{"v": "must be even"})
	check(t, &N{V: 4}, nil)

	for name, v := range map[string]any{
		"unknown rule": &struct {
			A string `validate:"nope"`
		}{},
		"bad number": &struct {
			A int `validate:"min=x"`
		}{},
		"no sibling": &struct {
			A string `validate:"eqfield=B"`
		}{},
		"dive on int": &struct {
			A int `validate:"dive,min=1"`
		}{},
		"alpha on int": &struct {
			A int `validate:"alpha"`
		}{},
		"bad regex": &struct {
			A string `validate:"regex=("`
		}{},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil || !strings.HasPrefix(r.(string), "veyro:") {
					t.Fatalf("want compile panic, got %v", r)
				}
			}()
			_ = veyro.Validate(context.Background(), v)
		})
	}
}

func TestValidatorSignaturesAtRoot(t *testing.T) {
	type Plain struct{ A string }
	_ = Plain{}
	p := &plainRoot{}
	check(t, p, map[string]string{"$": "plain says no"})
}

type plainRoot struct{ A string }

func (*plainRoot) Validate() error { return errors.New("plain says no") }
