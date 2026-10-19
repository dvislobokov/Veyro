package veyro

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Check is a health check: return nil when the dependency is fine. ctx
// carries the probe timeout, so pass it on to pings and queries.
type Check func(ctx context.Context) error

// ProbeStatus is the status of one check or of the whole probe.
type ProbeStatus string

const (
	ProbeOK   ProbeStatus = "ok"
	ProbeFail ProbeStatus = "fail"
)

// CheckResult is the outcome of one check.
type CheckResult struct {
	Status   ProbeStatus `json:"status"`
	Error    string      `json:"error,omitzero"`
	Duration string      `json:"duration"`
}

// ProbeReport is the body of /probe/healthy.
type ProbeReport struct {
	Status ProbeStatus            `json:"status"`
	Checks map[string]CheckResult `json:"checks"`
}

// ProbeConfig tunes Probes.
type ProbeConfig struct {
	// Prefix of the probe routes. Default "/probe".
	Prefix string
	// Timeout for the whole healthy probe. Default 5s.
	Timeout time.Duration
}

type probes struct {
	mu     sync.RWMutex
	checks map[string]Check
	order  []string
}

// AddCheck registers a named health check for /probe/healthy. Names must be
// unique; registering a name twice panics. Checks may be added at any time,
// also after the server started.
func (a *App) AddCheck(name string, check Check) *App {
	if name == "" || check == nil {
		panic("veyro: AddCheck needs a name and a check")
	}
	p := &a.probes
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.checks == nil {
		p.checks = map[string]Check{}
	}
	if _, dup := p.checks[name]; dup {
		panic(fmt.Sprintf("veyro: check %q is already registered", name))
	}
	p.checks[name] = check
	p.order = append(p.order, name)
	return a
}

// RunChecks runs every registered check concurrently and reports the
// results. It is what /probe/healthy serves.
func (a *App) RunChecks(ctx context.Context) ProbeReport {
	p := &a.probes
	p.mu.RLock()
	names := append([]string(nil), p.order...)
	checks := make([]Check, len(names))
	for i, n := range names {
		checks[i] = p.checks[n]
	}
	p.mu.RUnlock()

	report := ProbeReport{Status: ProbeOK, Checks: make(map[string]CheckResult, len(names))}
	results := make([]CheckResult, len(names))
	var wg sync.WaitGroup
	for i, check := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			err := runCheck(ctx, check)
			res := CheckResult{Status: ProbeOK, Duration: time.Since(start).Round(time.Microsecond).String()}
			if err != nil {
				res.Status, res.Error = ProbeFail, err.Error()
			}
			results[i] = res
		}()
	}
	wg.Wait()
	for i, n := range names {
		report.Checks[n] = results[i]
		if results[i].Status != ProbeOK {
			report.Status = ProbeFail
		}
	}
	return report
}

// runCheck guards against panics and against checks that ignore ctx.
func runCheck(ctx context.Context, check Check) (err error) {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- check(ctx)
	}()
	select {
	case err = <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("timed out: %w", ctx.Err())
	}
}

// Probes registers the probe endpoints:
//
//	GET /probe/liveness   200 {"status":"ok"} while the process can serve
//	GET /probe/healthy    runs every AddCheck; 200 when all pass, 503 otherwise,
//	                      with each check's status and error
//
// The routes are public (AllowAnonymous), hidden from OpenAPI, and the
// healthy probe answers Cache-Control: no-store.
func (a *App) Probes(cfg ...ProbeConfig) *Router {
	c := ProbeConfig{Prefix: "/probe", Timeout: 5 * time.Second}
	if len(cfg) > 0 {
		if cfg[0].Prefix != "" {
			c.Prefix = cfg[0].Prefix
		}
		if cfg[0].Timeout > 0 {
			c.Timeout = cfg[0].Timeout
		}
	}
	g := a.Group(c.Prefix)
	g.Get("/liveness", func(ctx *Context, _ None) (map[string]ProbeStatus, error) {
		ctx.Header().Set("Cache-Control", "no-store")
		return map[string]ProbeStatus{"status": ProbeOK}, nil
	}).Hide().AllowAnonymous()
	g.Get("/healthy", func(ctx *Context, _ None) (ProbeReport, error) {
		tctx, cancel := context.WithTimeout(ctx, c.Timeout)
		defer cancel()
		report := a.RunChecks(tctx)
		ctx.Header().Set("Cache-Control", "no-store")
		if report.Status != ProbeOK {
			ctx.Status(http.StatusServiceUnavailable)
		}
		return report, nil
	}).Hide().AllowAnonymous()
	return g
}

// CheckNames returns the registered check names in registration order.
func (a *App) CheckNames() []string {
	a.probes.mu.RLock()
	defer a.probes.mu.RUnlock()
	out := append([]string(nil), a.probes.order...)
	sort.Strings(out)
	return out
}
