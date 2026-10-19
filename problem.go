package veyro

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Problem is an RFC 9457 problem details document. It mirrors ASP.NET Core's
// ProblemDetails/ValidationProblemDetails and implements error, so handlers
// can return it directly:
//
//	return User{}, veyro.NotFound("user 42 does not exist")
type Problem struct {
	Type     string `json:"type,omitzero"`
	Title    string `json:"title,omitzero"`
	Status   int    `json:"status,omitzero"`
	Detail   string `json:"detail,omitzero"`
	Instance string `json:"instance,omitzero"`
	// Errors holds validation errors keyed by field, as in ASP.NET Core.
	Errors ValidationErrors `json:"errors,omitzero"`
	// TraceID is filled from the request when empty.
	TraceID string `json:"traceId,omitzero"`
	// Extensions are additional members merged into the document.
	Extensions map[string]any `json:",embed"`
}

// Error implements error.
func (p *Problem) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s", p.Status, p.Title)
	if p.Detail != "" {
		b.WriteString(": ")
		b.WriteString(p.Detail)
	}
	return b.String()
}

// With adds an extension member and returns p for chaining.
func (p *Problem) With(key string, value any) *Problem {
	if p.Extensions == nil {
		p.Extensions = map[string]any{}
	}
	p.Extensions[key] = value
	return p
}

// NewProblem creates a problem for an HTTP status with the RFC 9110 type URI
// and title ASP.NET Core would use.
func NewProblem(status int, detail string) *Problem {
	return &Problem{
		Type:   ProblemType(status),
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	}
}

// BadRequest returns a 400 problem.
func BadRequest(detail string) *Problem { return NewProblem(http.StatusBadRequest, detail) }

// Unauthorized returns a 401 problem.
func Unauthorized(detail string) *Problem { return NewProblem(http.StatusUnauthorized, detail) }

// Forbidden returns a 403 problem.
func Forbidden(detail string) *Problem { return NewProblem(http.StatusForbidden, detail) }

// NotFound returns a 404 problem.
func NotFound(detail string) *Problem { return NewProblem(http.StatusNotFound, detail) }

// Conflict returns a 409 problem.
func Conflict(detail string) *Problem { return NewProblem(http.StatusConflict, detail) }

// UnprocessableEntity returns a 422 problem.
func UnprocessableEntity(detail string) *Problem {
	return NewProblem(http.StatusUnprocessableEntity, detail)
}

// InternalServerError returns a 500 problem.
func InternalServerError(detail string) *Problem {
	return NewProblem(http.StatusInternalServerError, detail)
}

// ProblemType returns the RFC 9110 section URI for a status code, or
// "about:blank" when the status is not defined by RFC 9110.
func ProblemType(status int) string {
	const base = "https://tools.ietf.org/html/rfc9110#section-"
	switch {
	case status >= 400 && status <= 418:
		return fmt.Sprintf("%s15.5.%d", base, status-399)
	case status == 421:
		return base + "15.5.20"
	case status == 422:
		return base + "15.5.21"
	case status == 426:
		return base + "15.5.22"
	case status >= 500 && status <= 505:
		return fmt.Sprintf("%s15.6.%d", base, status-499)
	}
	return "about:blank"
}

// ValidationErrors maps a field name to its error messages. It implements
// error and converts to a 400 problem in the ASP.NET Core
// ValidationProblemDetails shape.
type ValidationErrors map[string][]string

// Invalid creates a ValidationErrors with a single entry.
func Invalid(field, message string) ValidationErrors {
	return ValidationErrors{field: {message}}
}

// Add appends a message for field.
func (e ValidationErrors) Add(field, message string) {
	e[field] = append(e[field], message)
}

func (e ValidationErrors) merge(o ValidationErrors) {
	for k, v := range o {
		e[k] = append(e[k], v...)
	}
}

// Error implements error.
func (e ValidationErrors) Error() string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString("validation failed")
	for i, k := range keys {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s: %s", k, strings.Join(e[k], ", "))
	}
	return b.String()
}

// Problem converts the errors to a 400 problem.
func (e ValidationErrors) Problem() *Problem {
	p := NewProblem(http.StatusBadRequest, "")
	p.Title = "One or more validation errors occurred."
	p.Errors = e
	return p
}
