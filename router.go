package veyro

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
)

// tree is a radix-style router with net/http.ServeMux pattern syntax:
//
//	/users            exactly "/users"
//	/users/{id}       one non-empty segment, bound to "id"
//	/files/{path...}  the rest of the path after "/files/", possibly empty
//	/static/          "/static/" and everything below it (a subtree)
//	/users/{$}        exactly "/users/"; "/{$}" is exactly "/"
//	/                 every path (a subtree at the root)
//
// Precedence per segment: literal, then {param}, then subtree/catch-all,
// with backtracking, so the most specific pattern wins as in ServeMux.
// Methods are matched after the path, which yields 405 with Allow; GET
// patterns also serve HEAD. Host-prefixed patterns are not supported.
type tree struct {
	root     node
	patterns map[string]bool // "METHOD pattern" for duplicate detection
}

type node struct {
	children  map[string]*node
	param     *node
	paramName string
	catchAll  map[string]*Route // {name...} routes at this node
	catchName string

	exact      map[string]*Route // path ends here without a trailing slash
	exactSlash map[string]*Route // path ends here with a trailing slash ({$})
	subtree    map[string]*Route // path ends here with "/" or continues below
}

func newTree() *tree { return &tree{patterns: map[string]bool{}} }

// add registers rt for method (empty = any method) and pattern. It panics
// on malformed or duplicate patterns, at startup rather than in production.
func (t *tree) add(method, pattern string, rt *Route) {
	if !strings.HasPrefix(pattern, "/") {
		panic(fmt.Sprintf("veyro: pattern %q must start with '/' (host patterns are not supported)", pattern))
	}
	key := method + " " + pattern
	if t.patterns[key] {
		panic(fmt.Sprintf("veyro: pattern %q is registered twice for method %q", pattern, method))
	}
	t.patterns[key] = true

	if strings.Count(pattern, "{") > maxParams {
		panic(fmt.Sprintf("veyro: pattern %q has more than %d wildcards", pattern, maxParams))
	}
	n := &t.root
	var table *map[string]*Route
	rest := pattern[1:]
	for table == nil {
		seg, remaining, more := strings.Cut(rest, "/")
		switch {
		case seg == "" && !more: // pattern ends with "/": subtree
			table = &n.subtree
		case seg == "{$}":
			if more {
				panic(fmt.Sprintf("veyro: pattern %q: {$} must be the last segment", pattern))
			}
			table = &n.exactSlash
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "...}"):
			if more {
				panic(fmt.Sprintf("veyro: pattern %q: catch-all must be the last segment", pattern))
			}
			name := seg[1 : len(seg)-4]
			if n.catchAll != nil && n.catchName != name {
				panic(fmt.Sprintf("veyro: pattern %q: {%s...} conflicts with {%s...}", pattern, name, n.catchName))
			}
			n.catchName = name
			table = &n.catchAll
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
			name := seg[1 : len(seg)-1]
			if name == "" || strings.ContainsAny(name, "{}") {
				panic(fmt.Sprintf("veyro: pattern %q: bad wildcard %q", pattern, seg))
			}
			if n.param == nil {
				n.param = &node{}
				n.paramName = name
			} else if n.paramName != name {
				panic(fmt.Sprintf("veyro: pattern %q: {%s} conflicts with {%s} at the same position", pattern, name, n.paramName))
			}
			n = n.param
			if !more {
				table = &n.exact
			}
		case seg == "":
			panic(fmt.Sprintf("veyro: pattern %q has an empty segment", pattern))
		default:
			if n.children == nil {
				n.children = map[string]*node{}
			}
			c := n.children[seg]
			if c == nil {
				c = &node{}
				n.children[seg] = c
			}
			n = c
			if !more {
				table = &n.exact
			}
		}
		rest = remaining
	}
	if *table == nil {
		*table = map[string]*Route{}
	}
	if _, dup := (*table)[method]; dup {
		panic(fmt.Sprintf("veyro: pattern %q is registered twice for method %q", pattern, method))
	}
	(*table)[method] = rt
}

// param is one bound path value.
type param struct{ name, value string }

// maxParams is the number of wildcards a single pattern may bind.
const maxParams = 8

// matchResult is the outcome of a lookup. Exactly one of route, redirect or
// "nothing" (404, possibly with allow for 405) applies. It is returned by
// value and holds the bound parameters inline, so a match allocates nothing.
type matchResult struct {
	route    *Route
	params   [maxParams]param
	n        int
	redirect string
	allow    []string
}

// match resolves a request. It works on the escaped path, like ServeMux,
// and unescapes wildcard values.
func (t *tree) match(method, escapedPath string) (res matchResult) {
	if escapedPath == "" {
		escapedPath = "/"
	}
	if clean := cleanPath(escapedPath); clean != escapedPath {
		return matchResult{redirect: clean}
	}
	trailing := strings.HasSuffix(escapedPath, "/")
	rest := strings.TrimSuffix(escapedPath[1:], "/")

	var buf [maxParams]param
	m := matcher{method: method}
	if rt, params, ok := m.walk(&t.root, rest, trailing, buf[:0]); ok {
		res.route = rt
		res.n = copy(res.params[:], params)
		return res
	}
	// "/foo" when only the subtree "/foo/" exists: redirect, as ServeMux does.
	if !trailing {
		probe := matcher{method: method}
		if _, _, ok := probe.walk(&t.root, rest, true, buf[:0]); ok && probe.viaSubtree {
			res.redirect = escapedPath + "/"
			return res
		}
	}
	if len(m.allow) > 0 {
		for am := range m.allow {
			res.allow = append(res.allow, am)
		}
		sort.Strings(res.allow)
	}
	return res
}

type matcher struct {
	method     string
	allow      map[string]bool
	viaSubtree bool
}

// walk matches rest (no leading or trailing slash) against n.
func (m *matcher) walk(n *node, rest string, trailing bool, params []param) (*Route, []param, bool) {
	if rest == "" {
		if trailing {
			if rt := m.pick(n.exactSlash); rt != nil {
				return rt, params, true
			}
			if rt := m.pick(n.subtree); rt != nil {
				m.viaSubtree = true
				return rt, params, true
			}
			if rt := m.pick(n.catchAll); rt != nil {
				return rt, append(params, param{n.catchName, ""}), true
			}
			return nil, nil, false
		}
		if rt := m.pick(n.exact); rt != nil {
			return rt, params, true
		}
		return nil, nil, false
	}

	seg, remaining, _ := strings.Cut(rest, "/")
	if c := n.children[seg]; c != nil {
		if rt, p, ok := m.walk(c, remaining, trailing, params); ok {
			return rt, p, true
		}
	}
	if n.param != nil {
		if rt, p, ok := m.walk(n.param, remaining, trailing, append(params, param{n.paramName, unescape(seg)})); ok {
			return rt, p, true
		}
	}
	if rt := m.pick(n.subtree); rt != nil {
		m.viaSubtree = true
		return rt, params, true
	}
	if rt := m.pick(n.catchAll); rt != nil {
		value := rest
		if trailing {
			value += "/"
		}
		return rt, append(params, param{n.catchName, unescape(value)}), true
	}
	return nil, nil, false
}

// pick selects the route for the method, falling back to GET for HEAD and
// to the any-method entry. It records other methods for a 405 answer.
func (m *matcher) pick(table map[string]*Route) *Route {
	if table == nil {
		return nil
	}
	if rt := table[m.method]; rt != nil {
		return rt
	}
	if m.method == http.MethodHead {
		if rt := table[http.MethodGet]; rt != nil {
			return rt
		}
	}
	if rt := table[""]; rt != nil {
		return rt
	}
	if m.allow == nil {
		m.allow = map[string]bool{}
	}
	for method := range table {
		m.allow[method] = true
		if method == http.MethodGet {
			m.allow[http.MethodHead] = true
		}
	}
	return nil
}

func unescape(s string) string {
	if strings.IndexByte(s, '%') < 0 {
		return s
	}
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// cleanPath is path.Clean that keeps a trailing slash, with a fast path for
// paths that are already clean (the overwhelming majority).
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] == '/' && !strings.Contains(p, "//") && !strings.Contains(p, "/.") {
		return p
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if np != "/" && p[len(p)-1] == '/' {
		np += "/"
	}
	return np
}
