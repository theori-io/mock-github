package mockgithub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxBody = 1 << 20

type runtimeStub struct {
	stub Stub
	used int
}

// apiResponse owns the selected response data and writes it after unlocking.
type apiResponse func(http.ResponseWriter) int

func jsonResponse(status int, data any) apiResponse {
	return func(w http.ResponseWriter) int { return writeJSON(w, status, data) }
}

func errorResponse(status int, message string) apiResponse {
	return jsonResponse(status, Object{"message": message, "documentation_url": "https://docs.github.com/rest", "status": fmt.Sprint(status)})
}

// Server is a concurrency-safe http.Handler. Use httptest.NewServer for unit
// tests, or mount it in an http.Server. The zero value is not ready for use.
type Server struct {
	mu             sync.Mutex
	initial        []byte
	cfg            Config
	state          *state
	stubs          []runtimeStub
	requests       []Request
	clock          func() time.Time
	generation     uint64
	webhookContext context.Context
	webhookCancel  context.CancelFunc
	deliveries     []storedDelivery
}

// New creates an isolated server and validates the fixture and response rules.
func New(cfg Config) (*Server, error) {
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	s := &Server{clock: clock}
	if err := s.load(cfg); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) now() string { return s.clock().UTC().Format(time.RFC3339) }

// Load replaces the fixtures and reset baseline atomically. Invalid configs
// leave the server unchanged. Clock is fixed at construction time.
func (s *Server) Load(cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(cfg)
}

func (s *Server) load(cfg Config) error {
	initial, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	var owned Config
	if err := json.Unmarshal(initial, &owned); err != nil {
		return err
	}
	if owned.MaxRequests < 0 {
		return fmt.Errorf("max_requests must be nonnegative")
	}
	if owned.MaxRequests == 0 {
		owned.MaxRequests = 1000
	}
	if owned.MaxDeliveries < 0 {
		return fmt.Errorf("max_deliveries must be nonnegative")
	}
	if owned.MaxDeliveries == 0 {
		owned.MaxDeliveries = 100
	}
	stubs := make([]runtimeStub, 0, len(owned.Stubs))
	for _, stub := range owned.Stubs {
		if err := validateStub(stub); err != nil {
			return err
		}
		stubs = append(stubs, runtimeStub{stub: stub})
	}
	state, err := newState(owned, s.now())
	if err != nil {
		return err
	}
	if err := validateWebhookEvents(owned, state); err != nil {
		return err
	}
	s.initial, s.cfg, s.state, s.stubs = initial, owned, state, stubs
	s.requests = nil
	if s.webhookCancel != nil {
		s.webhookCancel()
	}
	s.webhookContext, s.webhookCancel = context.WithCancel(context.Background())
	s.deliveries = nil
	s.generation++
	return nil
}

// Reset restores fixtures and clears tokens/logs. In-flight webhooks are canceled
// and cannot repopulate the cleared delivery log.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cfg Config
	if err := json.Unmarshal(s.initial, &cfg); err != nil {
		panic(err)
	}
	if err := s.load(cfg); err != nil {
		panic(err)
	}
}

// AddStub appends a rule, without changing the reset baseline.
func (s *Server) AddStub(stub Stub) error {
	if _, err := json.Marshal(stub); err != nil {
		return err
	}
	if err := validateStub(stub); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stubs = append(s.stubs, runtimeStub{stub: clone(stub)})
	return nil
}

// Requests returns an owned snapshot of the most recent completed API calls.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := append([]Request{}, s.requests...)
	return clone(result)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/__mock/") {
		s.admin(w, r)
		return
	}
	request := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Headers: r.Header.Clone()}
	for _, key := range []string{"Authorization", "Cookie"} {
		if request.Headers.Get(key) != "" {
			request.Headers.Set(key, "[REDACTED]")
		}
	}
	s.mu.Lock()
	generation := s.generation
	s.mu.Unlock()
	status := http.StatusOK
	defer func() {
		request.Status = status
		s.mu.Lock()
		defer s.mu.Unlock()
		if generation != s.generation {
			return
		}
		if len(s.requests) == s.cfg.MaxRequests {
			s.requests = s.requests[1:]
		}
		s.requests = append(s.requests, request)
	}()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Media-Type", "github.v3; format=json")
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "5000")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		status = http.StatusBadRequest
		if _, ok := err.(*http.MaxBytesError); ok {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, "Invalid request body")
		return
	}
	request.Body = string(body)
	response, delay := s.prepare(r, body)
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			status = 499
			return
		}
	}
	status = response(w)
}

func (s *Server) prepare(r *http.Request, body []byte) (apiResponse, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.authorized(r) {
		return errorResponse(http.StatusUnauthorized, "Bad credentials"), 0
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "repos" && !s.canAccess(r, repoKey(parts[1], parts[2])) {
		return errorResponse(http.StatusNotFound, "Not Found"), 0
	}
	response, matched := s.matchStub(r, body)
	if matched {
		response = clone(response)
		status := response.Status
		if status == 0 {
			status = http.StatusOK
		}
		return func(w http.ResponseWriter) int {
			for key, value := range response.Headers {
				w.Header().Set(key, value)
			}
			w.WriteHeader(status)
			if len(response.Body) > 0 {
				_, _ = w.Write(response.Body)
			}
			return status
		}, time.Duration(response.DelayMS) * time.Millisecond
	}
	return s.route(r, body), 0
}

func (s *Server) matchStub(r *http.Request, body []byte) (Response, bool) {
	for n := range s.stubs {
		rule := &s.stubs[n]
		stub := rule.stub
		if stub.Method != r.Method || !matchPath(stub.Path, r.URL.Path) || (stub.Times > 0 && rule.used >= stub.Times) {
			continue
		}
		matches := true
		for key, value := range stub.Query {
			if !r.URL.Query().Has(key) || r.URL.Query().Get(key) != value {
				matches = false
			}
		}
		for key, value := range stub.Headers {
			if r.Header.Get(key) != value {
				matches = false
			}
		}
		if !matches || (len(stub.Body) > 0 && !jsonEqual(stub.Body, body)) {
			continue
		}
		index := rule.used
		if index >= len(stub.Responses) {
			index = len(stub.Responses) - 1
		}
		rule.used++
		return stub.Responses[index], true
	}
	return Response{}, false
}

func matchPath(pattern, path string) bool {
	if pattern == path {
		return true
	}
	parts := strings.Split(strings.Trim(pattern, "/"), "/")
	actual := strings.Split(strings.Trim(path, "/"), "/")
	for n, part := range parts {
		if part == "*" {
			return true
		}
		if n >= len(actual) || actual[n] == "" {
			return false
		}
		if !strings.HasPrefix(part, ":") && part != actual[n] {
			return false
		}
	}
	return len(parts) == len(actual)
}

func writeJSON(w http.ResponseWriter, status int, data any) int {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(data)
	}
	return status
}

func writeError(w http.ResponseWriter, status int, message string) int {
	return errorResponse(status, message)(w)
}

func decodeObject(body []byte) (Object, error) {
	var data Object
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("body must be a JSON object")
	}
	return data, nil
}

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/__mock/orgs/") {
		s.installationAdmin(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/__mock/webhooks/") {
		s.webhookAdmin(w, r)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /__mock/health":
		writeJSON(w, http.StatusOK, Object{"status": "ok"})
	case "GET /__mock/requests":
		writeJSON(w, http.StatusOK, s.Requests())
	case "DELETE /__mock/requests":
		s.mu.Lock()
		s.requests = nil
		s.generation++
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "POST /__mock/reset":
		s.Reset()
		w.WriteHeader(http.StatusNoContent)
	case "PUT /__mock/fixtures":
		cfg, err := DecodeConfig(http.MaxBytesReader(w, r.Body, maxBody))
		if err == nil {
			err = s.Load(cfg)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "POST /__mock/stubs":
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		d.DisallowUnknownFields()
		var stub Stub
		err := d.Decode(&stub)
		if err == nil && d.Decode(new(any)) != io.EOF {
			err = fmt.Errorf("expected exactly one JSON value")
		}
		if err == nil {
			err = s.AddStub(stub)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusCreated)
	default:
		writeError(w, http.StatusNotFound, "Not Found")
	}
}
