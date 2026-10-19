package bench

import (
	"context"
	"testing"

	"github.com/dvislobokov/veyro"
	"github.com/go-playground/validator/v10"
)

// A realistic input: nested struct, slice of structs, cross-field rule,
// formats. Tags carry both Veyro and validator v10 syntax.

type Address struct {
	Street string `json:"street" validate:"required,min=3,max=100"`
	City   string `json:"city" validate:"required,alpha"`
	Zip    string `json:"zip" validate:"required,numeric,len=5"`
}

type Item struct {
	SKU string  `json:"sku" validate:"required,alphanum,len=8"`
	Qty int     `json:"qty" validate:"gte=1,lte=100"`
	URL string  `json:"url" validate:"omitempty,url"`
	P   float64 `json:"price" validate:"gt=0"`
}

type Checkout struct {
	Email    string   `json:"email" validate:"required,email"`
	Password string   `json:"password" validate:"required,min=8"`
	Confirm  string   `json:"confirm" validate:"eqfield=Password"`
	Age      int      `json:"age" validate:"gte=18,lte=130"`
	Plan     string   `json:"plan" validate:"oneof=free pro team"`
	Billing  Address  `json:"billing"`
	Shipping *Address `json:"shipping"`
	Items    []Item   `json:"items" validate:"required,min=1,max=50,dive"`
	Tags     []string `json:"tags" validate:"max=5,unique,dive,min=2,max=20"`
	Promo    string   `json:"promo" validate:"omitempty,uppercase,len=6"`
}

var valid = Checkout{
	Email: "ann@example.com", Password: "hunter2hunter2", Confirm: "hunter2hunter2", Age: 30, Plan: "pro",
	Billing:  Address{Street: "Main St 1", City: "Berlin", Zip: "10115"},
	Shipping: &Address{Street: "Side St 2", City: "Hamburg", Zip: "20095"},
	Items: []Item{
		{SKU: "ABCD1234", Qty: 2, URL: "https://shop.example.com/a", P: 9.99},
		{SKU: "EFGH5678", Qty: 1, P: 19.5},
		{SKU: "IJKL9012", Qty: 10, URL: "https://shop.example.com/c", P: 1},
	},
	Tags: []string{"gift", "express"}, Promo: "SUMMER",
}

var invalid = func() Checkout {
	c := valid
	c.Email, c.Confirm, c.Age, c.Plan = "nope", "other", 12, "gold"
	c.Billing.Zip = "1a"
	c.Items = append([]Item(nil), c.Items...)
	c.Items[1].Qty = 0
	c.Tags = []string{"a", "gift", "gift"}
	return c
}()

func BenchmarkValidate_Veyro_Valid(b *testing.B) {
	ctx := context.Background()
	in := valid
	b.ReportAllocs()
	for b.Loop() {
		if err := veyro.Validate(ctx, &in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidate_V10_Valid(b *testing.B) {
	v := validator.New(validator.WithRequiredStructEnabled())
	in := valid
	b.ReportAllocs()
	for b.Loop() {
		if err := v.Struct(&in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidate_Veyro_Invalid(b *testing.B) {
	ctx := context.Background()
	in := invalid
	b.ReportAllocs()
	for b.Loop() {
		if err := veyro.Validate(ctx, &in); err == nil {
			b.Fatal("expected errors")
		}
	}
}

func BenchmarkValidate_V10_Invalid(b *testing.B) {
	v := validator.New(validator.WithRequiredStructEnabled())
	in := invalid
	b.ReportAllocs()
	for b.Loop() {
		if err := v.Struct(&in); err == nil {
			b.Fatal("expected errors")
		}
	}
}

func TestBothValidatorsAgree(t *testing.T) {
	v := validator.New(validator.WithRequiredStructEnabled())
	in := valid
	if err := v.Struct(&in); err != nil {
		t.Fatalf("v10 rejects the valid input: %v", err)
	}
	if err := veyro.Validate(context.Background(), &in); err != nil {
		t.Fatalf("veyro rejects the valid input: %v", err)
	}
	bad := invalid
	if err := v.Struct(&bad); err == nil {
		t.Fatal("v10 accepts the invalid input")
	}
	if err := veyro.Validate(context.Background(), &bad); err == nil {
		t.Fatal("veyro accepts the invalid input")
	}
}
