// Package scalar bundles the Scalar API reference UI (https://github.com/scalar/scalar)
// so Veyro can serve interactive documentation with no external requests:
// the JavaScript ships inside your binary, default fonts, the request proxy
// and telemetry are switched off.
//
//	app.ServeDocs("/docs", veyro.Info{Title: "Users API", Version: "1.0.0"}, scalar.UI)
//
// Scalar is MIT licensed; see LICENSE.scalar.
package scalar

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/dvislobokov/veyro"
)

// Version of @scalar/api-reference embedded in this package.
const Version = "1.75.0"

//go:embed scalar.js.gz
var scriptGz []byte

var (
	etag      = func() string { sum := sha256.Sum256(scriptGz); return `"` + hex.EncodeToString(sum[:16]) + `"` }()
	plainOnce sync.Once
	plain     []byte
	plainErr  error
)

func plainScript() ([]byte, error) {
	plainOnce.Do(func() {
		zr, err := gzip.NewReader(bytes.NewReader(scriptGz))
		if err != nil {
			plainErr = err
			return
		}
		plain, plainErr = io.ReadAll(zr)
	})
	return plain, plainErr
}

// Options tune the reference page. The zero value is a sensible default.
type Options struct {
	// Theme is a Scalar theme name: "default", "alternate", "moon", "purple",
	// "solarized", "bluePlanet", "saturn", "kepler", "mars", "deepSpace".
	Theme string
	// DarkMode forces dark mode on; otherwise the viewer's preference is used.
	DarkMode bool
	// Layout is "modern" (default) or "classic".
	Layout string
	// CustomCSS is injected into the page.
	CustomCSS string
	// ShowScalarTools keeps Scalar's own toolbar and promotional buttons
	// ("Ask AI", "Deploy", "Generate SDK", ...). They link to scalar.com
	// services, so they are hidden by default.
	ShowScalarTools bool
	// Configuration holds any further Scalar configuration keys; they are
	// merged over the defaults (which disable fonts, proxy and telemetry).
	Configuration map[string]any
}

// UI is a veyro.DocsUI serving Scalar with default options.
func UI(title, specURL, base string) http.Handler { return Options{}.UI(title, specURL, base) }

// UI returns a veyro.DocsUI with these options.
func (o Options) UI(title, specURL, base string) http.Handler {
	cfg := map[string]any{
		"url":              specURL,
		"withDefaultFonts": false, // no Google Fonts
		"proxyUrl":         "",    // no proxy.scalar.com for "Try it"
		"telemetry":        false,
		"hideClientButton": true, // no links to client.scalar.com
	}
	if o.Theme != "" {
		cfg["theme"] = o.Theme
	}
	if o.DarkMode {
		cfg["darkMode"] = true
	}
	if o.Layout != "" {
		cfg["layout"] = o.Layout
	}
	css := o.CustomCSS
	if !o.ShowScalarTools {
		css = hideToolsCSS + "\n" + css
	}
	if css != "" {
		cfg["customCss"] = css
	}
	for k, v := range o.Configuration {
		cfg[k] = v
	}
	cfgJSON, _ := json.Marshal(cfg, json.Deterministic(true))
	scriptURL := base + "/scalar.js"
	page := []byte(fmt.Sprintf(pageHTML, html.EscapeString(title), html.EscapeString(string(cfgJSON)), scriptURL))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, base) {
		case "", "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(page)
		case "/scalar.js":
			serveScript(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func serveScript(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", etag)
	h.Set("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		_, _ = w.Write(scriptGz)
		return
	}
	b, err := plainScript()
	if err != nil {
		http.Error(w, "cannot decode embedded script", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(b)
}

// hideToolsCSS removes UI that only makes sense with scalar.com's hosted
// services: the top toolbar (Developer Tools, Configure, Share, Deploy,
// Generate SDK), the "Generate SDKs & MCP" card and the "Ask AI" button.
const hideToolsCSS = `header.api-reference-toolbar,.explore-scalar,.generate-sdk-button,aside button.bg-sidebar-b-search,.selected-client-row button.scalar-button{display:none!important}`

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>%s</title>
<style>body{margin:0}</style>
</head>
<body>
<script id="api-reference" data-configuration="%s"></script>
<script src="%s"></script>
</body>
</html>
`

var _ veyro.DocsUI = UI
