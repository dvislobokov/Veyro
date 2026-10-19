package veyro

import (
	"encoding"
	"fmt"
	"reflect"
	"strconv"
	"time"
)

var (
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
	durationType    = reflect.TypeFor[time.Duration]()
)

// checkConvertible reports whether values of t can be set from strings.
func checkConvertible(t reflect.Type) error {
	if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
		t = t.Elem()
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(textUnmarshaler) || t == durationType {
		return nil
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return nil
	}
	return fmt.Errorf("type %s cannot be bound from a string; implement encoding.TextUnmarshaler", t)
}

// setFromStrings sets v from one or more raw values. Slices take all values;
// scalars take the first.
func setFromStrings(v reflect.Value, vals []string) error {
	if v.Kind() == reflect.Slice && v.Type().Elem().Kind() != reflect.Uint8 {
		s := reflect.MakeSlice(v.Type(), len(vals), len(vals))
		for i, raw := range vals {
			if err := setScalar(s.Index(i), raw); err != nil {
				return err
			}
		}
		v.Set(s)
		return nil
	}
	return setScalar(v, vals[0])
}

func setScalar(v reflect.Value, raw string) error {
	if v.Kind() == reflect.Pointer {
		p := reflect.New(v.Type().Elem())
		if err := setScalar(p.Elem(), raw); err != nil {
			return err
		}
		v.Set(p)
		return nil
	}
	if v.CanAddr() {
		if u, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return u.UnmarshalText([]byte(raw))
		}
	}
	if v.Type() == durationType {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		v.SetInt(int64(d))
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(f)
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
	return nil
}
