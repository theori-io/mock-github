// Package mockgithub provides an isolated, stateful GitHub REST mock for tests.
package mockgithub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Object holds GitHub fields, including fields not modeled by this package.
type Object map[string]any

// Config seeds a server. Each server owns an independent copy of its fixtures.
type Config struct {
	User                Object           `json:"user,omitempty"`
	Users               []User           `json:"users,omitempty"`
	Organizations       []Organization   `json:"organizations,omitempty"`
	Repositories        []Repository     `json:"repositories,omitempty"`
	Apps                []App            `json:"apps,omitempty"`
	Installations       []Installation   `json:"installations,omitempty"`
	WebhookEvents       []WebhookEvent   `json:"webhook_events,omitempty"`
	Stubs               []Stub           `json:"stubs,omitempty"`
	Token               string           `json:"token,omitempty"`
	AppToken            string           `json:"app_token,omitempty"`
	InstallationIDStart int              `json:"installation_id_start,omitempty"`
	MaxRequests         int              `json:"max_requests,omitempty"`
	MaxDeliveries       int              `json:"max_deliveries,omitempty"`
	Clock               func() time.Time `json:"-"`
}

// User identifies a test actor by an opaque user/PAT credential.
type User struct {
	Data  Object `json:"data"`
	Token string `json:"token"`
}

// Organization assigns fixture users the GitHub roles member or admin.
type Organization struct {
	Login   string            `json:"login"`
	ID      int               `json:"id"`
	Members map[string]string `json:"members"`
}

// Repository seeds the scan targets and source snapshots exposed to the app under test.
type Repository struct {
	Owner        string   `json:"owner"`
	Name         string   `json:"name"`
	Data         Object   `json:"data,omitempty"`
	Branches     []Branch `json:"branches,omitempty"`
	Commits      []Commit `json:"commits,omitempty"`
	PullRequests []Object `json:"pull_requests,omitempty"`
}

type Branch struct {
	Name      string `json:"name"`
	SHA       string `json:"sha"`
	Protected bool   `json:"protected,omitempty"`
}

// Commit contains text source files used to build real zip and tar.gz archives.
type Commit struct {
	SHA   string            `json:"sha"`
	Data  Object            `json:"data,omitempty"`
	Files map[string]string `json:"files,omitempty"`
}

// App owns installations and an optional webhook receiver. Token is an opaque JWT
// substitute. Multiple apps require distinct, nonempty tokens.
type App struct {
	ID      int            `json:"id"`
	Slug    string         `json:"slug"`
	Data    Object         `json:"data,omitempty"`
	Token   string         `json:"token,omitempty"`
	Webhook *WebhookConfig `json:"webhook,omitempty"`
}

// Installation installs an app on an account and grants repository access.
type Installation struct {
	ID           int               `json:"id"`
	AppID        int               `json:"app_id,omitempty"`
	Account      Object            `json:"account"`
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions,omitempty"`
	CreatedAt    string            `json:"created_at,omitempty"`
}

// Stub overrides an endpoint. Path accepts :parameter segments and a final *.
// Query and Headers match a subset; Body matches an entire JSON value.
// Responses are consumed in order, then the final response repeats. Times zero
// means unlimited matches. Earlier stubs take precedence.
type Stub struct {
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Query     map[string]string `json:"query,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      json.RawMessage   `json:"body,omitempty"`
	Responses []Response        `json:"responses"`
	Times     int               `json:"times,omitempty"`
}

// Response is a canned JSON response. Status defaults to 200.
type Response struct {
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	DelayMS int               `json:"delay_ms,omitempty"`
}

// Request records a completed API call. Authorization and Cookie are redacted.
type Request struct {
	Method  string      `json:"method"`
	Path    string      `json:"path"`
	Query   string      `json:"query,omitempty"`
	Headers http.Header `json:"headers"`
	Body    string      `json:"body,omitempty"`
	Status  int         `json:"status"`
}

// DecodeConfig reads exactly one JSON config and rejects misspelled fields.
func DecodeConfig(r io.Reader) (Config, error) {
	var cfg Config
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode config: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return cfg, fmt.Errorf("config must contain exactly one JSON value")
	}
	return cfg, nil
}

func validateStub(stub Stub) error {
	if stub.Method == "" || stub.Method != strings.ToUpper(stub.Method) || strings.ContainsAny(stub.Method, " \t\r\n") {
		return fmt.Errorf("stub method must be an uppercase HTTP method")
	}
	if !strings.HasPrefix(stub.Path, "/") || strings.ContainsAny(stub.Path, "?#") || strings.HasPrefix(stub.Path, "/__mock") {
		return fmt.Errorf("stub path must be an API path without a query string")
	}
	segments := strings.Split(strings.Trim(stub.Path, "/"), "/")
	for i, segment := range segments {
		if strings.Contains(segment, "*") && (segment != "*" || i != len(segments)-1) {
			return fmt.Errorf("stub wildcard must be a final * segment")
		}
		if segment == ":" {
			return fmt.Errorf("stub path parameter needs a name")
		}
	}
	if stub.Times < 0 || len(stub.Responses) == 0 {
		return fmt.Errorf("stub needs responses and a nonnegative times value")
	}
	if len(stub.Body) > 0 && !json.Valid(stub.Body) {
		return fmt.Errorf("stub body must be valid JSON")
	}
	for _, response := range stub.Responses {
		if response.Status != 0 && (response.Status < 200 || response.Status > 599) {
			return fmt.Errorf("stub response status must be between 200 and 599")
		}
		if response.DelayMS < 0 || response.DelayMS > 300000 {
			return fmt.Errorf("stub delay_ms must be between 0 and 300000")
		}
		if len(response.Body) > 0 && !json.Valid(response.Body) {
			return fmt.Errorf("stub response body must be valid JSON")
		}
		if (response.Status == 204 || response.Status == 304) && len(response.Body) > 0 {
			return fmt.Errorf("status %d cannot include a body", response.Status)
		}
		for name, value := range response.Headers {
			if name == "" || strings.ContainsAny(name, " \t\r\n:") || strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("invalid stub response header")
			}
		}
	}
	return nil
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}

func clone[T any](value T) T {
	b, err := json.Marshal(value)
	if err != nil {
		panic("mockgithub: internal non-JSON value: " + err.Error())
	}
	var result T
	if err := json.Unmarshal(b, &result); err != nil {
		panic("mockgithub: internal JSON decode: " + err.Error())
	}
	return result
}
