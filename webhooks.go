package mockgithub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// WebhookConfig defines the app's receiver. Empty Secret sends unsigned events.
// TimeoutMS defaults to 10000. Redirects are never followed.
type WebhookConfig struct {
	URL       string `json:"url"`
	Secret    string `json:"secret,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// WebhookEvent is a named, exact JSON payload. Events do not mutate REST state.
type WebhookEvent struct {
	Name    string          `json:"name"`
	AppID   int             `json:"app_id,omitempty"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

// WebhookRequest selects a named fixture, or supplies AppID, Event, and Payload.
type WebhookRequest struct {
	Fixture string          `json:"fixture,omitempty"`
	AppID   int             `json:"app_id,omitempty"`
	Event   string          `json:"event,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Delivery records one completed attempt. Receiver failures are recorded in
// ResponseStatus or Error. ID stays the same across explicit redeliveries.
type Delivery struct {
	ID                string          `json:"id"`
	Attempt           int             `json:"attempt"`
	AppID             int             `json:"app_id"`
	Event             string          `json:"event"`
	URL               string          `json:"url"`
	Payload           json.RawMessage `json:"payload"`
	Body              string          `json:"body"`
	Headers           http.Header     `json:"headers"`
	DeliveredAt       string          `json:"delivered_at"`
	DurationMS        int64           `json:"duration_ms"`
	ResponseStatus    int             `json:"response_status"`
	ResponseBody      string          `json:"response_body,omitempty"`
	ResponseTruncated bool            `json:"response_truncated,omitempty"`
	Error             string          `json:"error,omitempty"`
}

type webhookRun struct {
	config   WebhookConfig
	id       string
	appID    int
	event    string
	payload  json.RawMessage
	attempts int // guarded by Server.mu
}

type storedDelivery struct {
	delivery Delivery
	run      *webhookRun
}

// ErrDeliveryNotFound means the delivery is absent or has been evicted/reset.
var ErrDeliveryNotFound = errors.New("webhook delivery not found")

func validateWebhookConfig(cfg *WebhookConfig) error {
	if cfg == nil {
		return nil
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("webhook url must be an absolute HTTP(S) URL without credentials or a fragment")
	}
	if cfg.TimeoutMS < 0 || cfg.TimeoutMS > 300000 {
		return fmt.Errorf("webhook timeout_ms must be between 0 and 300000")
	}
	return nil
}

func validateWebhookEvent(event string, payload json.RawMessage) error {
	if event == "" || strings.Trim(event, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" {
		return fmt.Errorf("webhook event must contain only letters, digits, or underscores")
	}
	if len(payload) > maxBody {
		return fmt.Errorf("webhook payload exceeds 1 MiB")
	}
	if _, err := decodeObject(payload); err != nil {
		return fmt.Errorf("webhook payload must be a JSON object")
	}
	return nil
}

func validateWebhookEvents(cfg Config, state *state) error {
	names := map[string]bool{}
	for _, event := range cfg.WebhookEvents {
		if event.Name == "" || names[event.Name] {
			return fmt.Errorf("webhook fixture needs a unique nonempty name")
		}
		names[event.Name] = true
		if state.apps[state.defaultAppID(event.AppID)].Webhook == nil {
			return fmt.Errorf("webhook fixture %q needs an app with a webhook receiver", event.Name)
		}
		if err := validateWebhookEvent(event.Event, event.Payload); err != nil {
			return fmt.Errorf("webhook fixture %q: %w", event.Name, err)
		}
	}
	return nil
}

// DeliverWebhook posts an event and waits for the receiver. Setup failures return
// an error; HTTP failures, timeouts, and cancellations produce a Delivery.
func (s *Server) DeliverWebhook(ctx context.Context, request WebhookRequest) (Delivery, error) {
	s.mu.Lock()
	if request.Fixture != "" {
		if request.AppID != 0 || request.Event != "" || len(request.Payload) != 0 {
			s.mu.Unlock()
			return Delivery{}, fmt.Errorf("use fixture or app_id/event/payload")
		}
		found := false
		for _, fixture := range s.cfg.WebhookEvents {
			if fixture.Name == request.Fixture {
				request.AppID, request.Event, request.Payload = fixture.AppID, fixture.Event, fixture.Payload
				found = true
				break
			}
		}
		if !found {
			s.mu.Unlock()
			return Delivery{}, fmt.Errorf("unknown webhook fixture %q", request.Fixture)
		}
	}
	app := s.state.apps[s.state.defaultAppID(request.AppID)]
	if app.Webhook == nil {
		s.mu.Unlock()
		return Delivery{}, fmt.Errorf("app_id must select an app with a webhook receiver")
	}
	if err := validateWebhookEvent(request.Event, request.Payload); err != nil {
		s.mu.Unlock()
		return Delivery{}, err
	}
	run, err := newWebhookRun(app, request.Event, request.Payload)
	if err != nil {
		s.mu.Unlock()
		return Delivery{}, err
	}
	epoch, timestamp := s.webhookContext, s.now()
	s.mu.Unlock()
	return s.sendWebhook(ctx, epoch, run, 1, timestamp), nil
}

func newWebhookRun(app App, event string, payload json.RawMessage) (*webhookRun, error) {
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return nil, fmt.Errorf("generate delivery id: %w", err)
	}
	uuid[6], uuid[8] = (uuid[6]&0x0f)|0x40, (uuid[8]&0x3f)|0x80
	return &webhookRun{config: *app.Webhook, id: fmt.Sprintf("%x-%x-%x-%x-%x", uuid[:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:]), appID: app.ID, event: event, payload: append(json.RawMessage(nil), payload...), attempts: 1}, nil
}

// RedeliverWebhook reuses a retained delivery's ID, exact payload, receiver, and
// signing secret. Failed attempts are never automatically retried.
func (s *Server) RedeliverWebhook(ctx context.Context, id string) (Delivery, error) {
	s.mu.Lock()
	for n := len(s.deliveries) - 1; n >= 0; n-- {
		run := s.deliveries[n].run
		if run.id == id {
			run.attempts++
			attempt, epoch, timestamp := run.attempts, s.webhookContext, s.now()
			s.mu.Unlock()
			return s.sendWebhook(ctx, epoch, run, attempt, timestamp), nil
		}
	}
	s.mu.Unlock()
	return Delivery{}, ErrDeliveryNotFound
}

// Deliveries returns owned snapshots of retained completed webhook attempts.
func (s *Server) Deliveries() []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Delivery, 0, len(s.deliveries))
	for _, stored := range s.deliveries {
		result = append(result, clone(stored.delivery))
	}
	return result
}

func (s *Server) sendWebhook(ctx, epoch context.Context, run *webhookRun, attempt int, timestamp string) Delivery {
	timeout := time.Duration(run.config.TimeoutMS) * time.Millisecond
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	stop := context.AfterFunc(epoch, cancel)
	defer func() { stop(); cancel() }()
	headers := http.Header{"Content-Type": {"application/json"}, "User-Agent": {"GitHub-Hookshot/mock-github"}, "X-Github-Event": {run.event}, "X-Github-Delivery": {run.id}, "X-Github-Hook-Installation-Target-Id": {strconv.Itoa(run.appID)}, "X-Github-Hook-Installation-Target-Type": {"integration"}}
	if run.config.Secret != "" {
		sha256MAC := hmac.New(sha256.New, []byte(run.config.Secret))
		_, _ = sha256MAC.Write(run.payload)
		headers.Set("X-Hub-Signature-256", fmt.Sprintf("sha256=%x", sha256MAC.Sum(nil)))
		sha1MAC := hmac.New(sha1.New, []byte(run.config.Secret))
		_, _ = sha1MAC.Write(run.payload)
		headers.Set("X-Hub-Signature", fmt.Sprintf("sha1=%x", sha1MAC.Sum(nil)))
	}
	delivery := Delivery{ID: run.id, Attempt: attempt, AppID: run.appID, Event: run.event, URL: run.config.URL, Payload: append(json.RawMessage(nil), run.payload...), Body: string(run.payload), Headers: headers.Clone(), DeliveredAt: timestamp}
	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, run.config.URL, bytes.NewReader(run.payload))
	if err == nil {
		request.Header = headers
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		var response *http.Response
		response, err = client.Do(request)
		if err == nil {
			delivery.ResponseStatus = response.StatusCode
			const maxResponse = 64 << 10
			var body []byte
			body, err = io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
			_ = response.Body.Close()
			if len(body) > maxResponse {
				body, delivery.ResponseTruncated = body[:maxResponse], true
			}
			delivery.ResponseBody = string(body)
		}
	}
	if err != nil {
		delivery.Error = err.Error()
	}
	delivery.DurationMS = time.Since(started).Milliseconds()
	s.mu.Lock()
	if epoch == s.webhookContext {
		if len(s.deliveries) == s.cfg.MaxDeliveries {
			s.deliveries = s.deliveries[1:]
		}
		s.deliveries = append(s.deliveries, storedDelivery{delivery: clone(delivery), run: run})
	}
	s.mu.Unlock()
	return delivery
}

func (s *Server) webhookAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/__mock/webhooks/deliveries" {
		writeJSON(w, http.StatusOK, s.Deliveries())
		return
	}
	var delivery Delivery
	var err error
	if r.Method == http.MethodPost && r.URL.Path == "/__mock/webhooks/deliver" {
		var request WebhookRequest
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		d.DisallowUnknownFields()
		err = d.Decode(&request)
		if err == nil && d.Decode(new(any)) != io.EOF {
			err = fmt.Errorf("expected exactly one JSON value")
		}
		if err == nil {
			delivery, err = s.DeliverWebhook(r.Context(), request)
		}
	} else if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); r.Method == http.MethodPost && len(parts) == 5 && parts[2] == "deliveries" && parts[4] == "redeliver" {
		delivery, err = s.RedeliverWebhook(r.Context(), parts[3])
	} else {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrDeliveryNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, delivery)
}
