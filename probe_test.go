package veyro_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dvislobokov/veyro"
)

func TestProbes(t *testing.T) {
	app := newApp(t)
	app.UseAuthentication(veyro.Bearer(tokenAuth))
	app.RequireAuthorization() // everything needs auth, probes must still be public
	app.Probes(veyro.ProbeConfig{Timeout: 200 * time.Millisecond})

	dbOK := true
	app.AddCheck("db", func(ctx context.Context) error {
		if !dbOK {
			return errors.New("connection refused")
		}
		return nil
	})
	app.AddCheck("cache", func(ctx context.Context) error { return nil })

	get := func(path string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	if code, out := get("/probe/liveness"); code != 200 || out["status"] != "ok" {
		t.Errorf("liveness: %d %v", code, out)
	}
	code, out := get("/probe/healthy")
	checks := out["checks"].(map[string]any)
	if code != 200 || out["status"] != "ok" || checks["db"].(map[string]any)["status"] != "ok" || checks["cache"] == nil {
		t.Errorf("healthy: %d %v", code, out)
	}

	dbOK = false
	code, out = get("/probe/healthy")
	db := out["checks"].(map[string]any)["db"].(map[string]any)
	if code != 503 || out["status"] != "fail" || db["status"] != "fail" || db["error"] != "connection refused" {
		t.Errorf("unhealthy: %d %v", code, out)
	}
	if cache := out["checks"].(map[string]any)["cache"].(map[string]any); cache["status"] != "ok" || cache["error"] != nil {
		t.Errorf("healthy check must not carry error: %v", cache)
	}

	// Slow and panicking checks are contained.
	app.AddCheck("slow", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	app.AddCheck("boom", func(ctx context.Context) error { panic("x") })
	start := time.Now()
	code, out = get("/probe/healthy")
	if code != 503 || time.Since(start) > time.Second {
		t.Errorf("timeout containment: %d in %s", code, time.Since(start))
	}
	cm := out["checks"].(map[string]any)
	if cm["slow"].(map[string]any)["status"] != "fail" || cm["boom"].(map[string]any)["error"] != "panic: x" {
		t.Errorf("slow/boom: %v", cm)
	}

	// Hidden from OpenAPI.
	if paths := app.OpenAPI().Paths; paths["/probe/healthy"] != nil || paths["/probe/liveness"] != nil {
		t.Error("probes must be hidden from the document")
	}
	defer func() {
		if recover() == nil {
			t.Error("duplicate check name must panic")
		}
	}()
	app.AddCheck("db", func(ctx context.Context) error { return nil })
}
