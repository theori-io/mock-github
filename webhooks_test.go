package mockgithub_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	mockgithub "github.com/theori-io/mock-github"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func decodeDelivery(t *testing.T, data []byte) mockgithub.Delivery {
	t.Helper()
	var result mockgithub.Delivery
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSignedWebhookDeliveryAndRedelivery(t *testing.T) {
	payload := "{\n  \"action\": \"opened\", \"installation\": {\"id\": 42}\n}"
	var attempts atomic.Int32
	ids := make(chan string, 2)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte("test-secret"))
		_, _ = mac.Write(body)
		if r.Method != "POST" || string(body) != payload || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-GitHub-Event") != "pull_request" || r.Header.Get("X-GitHub-Hook-Installation-Target-ID") != "7" || r.Header.Get("X-Hub-Signature-256") != fmt.Sprintf("sha256=%x", mac.Sum(nil)) {
			t.Error("incorrect payload, signature, or headers")
		}
		ids <- r.Header.Get("X-GitHub-Delivery")
		if attempts.Add(1) == 1 {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, "try again")
		} else {
			w.WriteHeader(202)
		}
	}))
	defer receiver.Close()
	cfg := fixture(t)
	cfg.Apps[0].Webhook.URL = receiver.URL
	mock, server := start(t, cfg)
	_, data := call(t, server, "POST", "/__mock/webhooks/deliver", `{"app_id":7,"event":"pull_request","payload":`+payload+`}`, "", 201)
	first := decodeDelivery(t, data)
	if first.Attempt != 1 || first.ResponseStatus != 503 || first.ResponseBody != "try again" || first.Error != "" || first.Body != payload {
		t.Fatalf("first: %+v", first)
	}
	_, data = call(t, server, "POST", "/__mock/webhooks/deliveries/"+first.ID+"/redeliver", "", "", 201)
	second := decodeDelivery(t, data)
	if second.ID != first.ID || second.Attempt != 2 || second.ResponseStatus != 202 || attempts.Load() != 2 {
		t.Fatalf("redelivery: %+v", second)
	}
	if id := <-ids; len(id) != 36 || id != <-ids {
		t.Fatal("redelivery did not preserve UUID")
	}
	_, data = call(t, server, "GET", "/__mock/webhooks/deliveries", "", "", 200)
	var history []mockgithub.Delivery
	if err := json.Unmarshal(data, &history); err != nil || len(history) != 2 {
		t.Fatalf("history: %s", data)
	}
	owned := mock.Deliveries()
	owned[0].Headers.Set("X-GitHub-Event", "changed")
	if mock.Deliveries()[0].Headers.Get("X-GitHub-Event") != "pull_request" || len(mock.Requests()) != 0 {
		t.Fatal("history not isolated or controls recorded as API calls")
	}
	call(t, server, "DELETE", "/__mock/requests", "", "", 204)
	if len(mock.Deliveries()) != 2 {
		t.Fatal("clearing REST log cleared webhooks")
	}
	mock.Reset()
	if len(mock.Deliveries()) != 0 {
		t.Fatal("reset retained history")
	}
	call(t, server, "POST", "/__mock/webhooks/deliveries/"+first.ID+"/redeliver", "", "", 404)
}

func TestFixtureWebhookCanFetchSource(t *testing.T) {
	var apiURL, token string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			After      string `json:"after"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
			Installation struct {
				ID int `json:"id"`
			} `json:"installation"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Installation.ID != 42 || r.Header.Get("X-GitHub-Event") != "push" {
			t.Error("invalid push fixture")
		}
		request, _ := http.NewRequest("GET", apiURL+"/repos/"+payload.Repository.FullName+"/zipball/"+payload.After, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		defer response.Body.Close()
		archive, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 || zipFiles(t, archive)["demo-aaaaaaa/src/main.py"] != "print('main')\n" {
			t.Error("receiver could not fetch advertised commit")
		}
		w.WriteHeader(202)
	}))
	defer receiver.Close()
	cfg := fixture(t)
	cfg.Apps[0].Webhook.URL = receiver.URL
	mock, server := start(t, cfg)
	apiURL = server.URL
	_, issued := call(t, server, "POST", "/app/installations/42/access_tokens", `{}`, "", 201)
	token = object(t, issued)["token"].(string)
	delivery, err := mock.DeliverWebhook(context.Background(), mockgithub.WebhookRequest{Fixture: "push-main"})
	if err != nil || delivery.ResponseStatus != 202 || delivery.Error != "" {
		t.Fatalf("push scan: %+v, %v", delivery, err)
	}
}

func TestWebhookFailuresLimitsAndUnsignedEvents(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	for _, mode := range []string{"timeout", "redirect", "large", "unsigned"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "timeout":
					select {
					case <-r.Context().Done():
					case <-release:
					}
				case "redirect":
					http.Redirect(w, r, destination.URL, 307)
				case "large":
					_, _ = io.WriteString(w, strings.Repeat("x", 100000))
				case "unsigned":
					if r.Header.Get("X-Hub-Signature-256") != "" || r.Header.Get("X-Hub-Signature") != "" {
						t.Error("unsigned event had signature")
					}
					w.WriteHeader(204)
				}
			}))
			defer receiver.Close()
			defer close(release)
			cfg := fixture(t)
			cfg.MaxDeliveries = 1
			cfg.Apps[0].Webhook.URL = receiver.URL
			if mode == "timeout" {
				cfg.Apps[0].Webhook.TimeoutMS = 30
			}
			if mode == "unsigned" {
				cfg.Apps[0].Webhook.Secret = ""
			}
			mock, _ := start(t, cfg)
			first, err := mock.DeliverWebhook(context.Background(), mockgithub.WebhookRequest{Fixture: "push-main"})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "timeout":
				if first.Error == "" || first.ResponseStatus != 0 {
					t.Fatalf("timeout: %+v", first)
				}
			case "redirect":
				if first.ResponseStatus != 307 || redirected.Load() != 0 {
					t.Fatalf("redirect: %+v", first)
				}
			case "large":
				if !first.ResponseTruncated || len(first.ResponseBody) != 64<<10 {
					t.Fatal("unbounded response")
				}
			case "unsigned":
				if first.ResponseStatus != 204 {
					t.Fatalf("unsigned: %+v", first)
				}
			}
			_, err = mock.DeliverWebhook(context.Background(), mockgithub.WebhookRequest{Fixture: "pull-request-opened"})
			if err != nil || len(mock.Deliveries()) != 1 {
				t.Fatalf("history limit: %v", err)
			}
			if _, err := mock.RedeliverWebhook(context.Background(), first.ID); err != mockgithub.ErrDeliveryNotFound {
				t.Fatalf("eviction: %v", err)
			}
		})
	}
}

func TestResetAndLoadCancelWebhooksWithoutBlockingAPI(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(fmt.Sprint(reload), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer receiver.Close()
			defer close(release)
			cfg := fixture(t)
			cfg.Apps[0].Webhook.URL = receiver.URL
			mock, server := start(t, cfg)
			done := make(chan mockgithub.Delivery, 1)
			go func() {
				delivery, _ := mock.DeliverWebhook(context.Background(), mockgithub.WebhookRequest{Fixture: "push-main"})
				done <- delivery
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("receiver never started")
			}
			call(t, server, "GET", "/repos/acme/demo", "", "", 200)
			call(t, server, "DELETE", "/__mock/requests", "", "", 204)
			if reload {
				if err := mock.Load(cfg); err != nil {
					t.Fatal(err)
				}
			} else {
				mock.Reset()
			}
			select {
			case delivery := <-done:
				if delivery.Error == "" {
					t.Fatal("reset did not cancel delivery")
				}
			case <-time.After(time.Second):
				t.Fatal("reset did not interrupt delivery")
			}
			if len(mock.Deliveries()) != 0 {
				t.Fatal("old delivery repopulated history")
			}
		})
	}
}

func TestConcurrentRedeliveriesAndInvalidRequests(t *testing.T) {
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer receiver.Close()
	cfg := fixture(t)
	cfg.Apps[0].Webhook.URL = receiver.URL
	mock, server := start(t, cfg)
	first, err := mock.DeliverWebhook(context.Background(), mockgithub.WebhookRequest{Fixture: "installation-created"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := mock.RedeliverWebhook(context.Background(), first.ID); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	seen := map[int]bool{}
	for _, delivery := range mock.Deliveries() {
		if seen[delivery.Attempt] {
			t.Fatal("duplicate attempt number")
		}
		seen[delivery.Attempt] = true
	}
	if len(seen) != 11 {
		t.Fatal("missing attempts")
	}
	for _, body := range []string{`null`, `{}`, `{"fixture":"missing"}`, `{"fixture":"push-main","event":"push"}`, `{"app_id":999,"event":"push","payload":{}}`, `{"event":"push","payload":null}`, `{"event":"push\r\nInvalid","payload":{}}`, `{"fixture":"push-main","typo":true}`, `{"fixture":"push-main"} {}`} {
		call(t, server, "POST", "/__mock/webhooks/deliver", body, "", 400)
	}
	call(t, server, "POST", "/__mock/webhooks/deliveries/missing/redeliver", "", "", 404)
	for _, mutate := range []func(*mockgithub.Config){
		func(c *mockgithub.Config) { c.Apps[0].Webhook.URL = "file:///tmp/x" },
		func(c *mockgithub.Config) { c.Apps[0].Webhook.URL = "http://user:secret@localhost" },
		func(c *mockgithub.Config) { c.Apps[0].Webhook.TimeoutMS = -1 },
		func(c *mockgithub.Config) { c.MaxDeliveries = -1 },
		func(c *mockgithub.Config) { c.WebhookEvents[0].AppID = 999 },
		func(c *mockgithub.Config) { c.WebhookEvents[0].Payload = json.RawMessage(`[]`) },
		func(c *mockgithub.Config) { c.WebhookEvents = append(c.WebhookEvents, c.WebhookEvents[0]) },
	} {
		bad := fixture(t)
		mutate(&bad)
		if err := mock.Load(bad); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	if len(mock.Deliveries()) != 11 {
		t.Fatal("invalid load discarded history")
	}
}

func TestWebhooksUseTheirOwningAppReceiverAndSecret(t *testing.T) {
	cfg := fixture(t)
	cfg.Apps[0].Token = "example-jwt"
	cfg.Apps = append(cfg.Apps, mockgithub.App{ID: 8, Slug: "other", Token: "other-jwt"})
	for n := range cfg.Apps {
		appID := cfg.Apps[n].ID
		secret := fmt.Sprintf("secret-%d", appID)
		receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mac := hmac.New(sha256.New, []byte(secret))
			_, _ = mac.Write(body)
			if r.Header.Get("X-GitHub-Hook-Installation-Target-ID") != fmt.Sprint(appID) || r.Header.Get("X-Hub-Signature-256") != fmt.Sprintf("sha256=%x", mac.Sum(nil)) {
				t.Error("webhook used another app's identity or secret")
			}
			w.WriteHeader(202)
		}))
		t.Cleanup(receiver.Close)
		cfg.Apps[n].Webhook = &mockgithub.WebhookConfig{URL: receiver.URL, Secret: secret}
	}
	mock, _ := start(t, cfg)
	for _, app := range cfg.Apps {
		delivery, err := mock.DeliverWebhook(context.Background(), mockgithub.WebhookRequest{AppID: app.ID, Event: "ping", Payload: json.RawMessage(`{}`)})
		if err != nil || delivery.ResponseStatus != 202 || delivery.URL != app.Webhook.URL {
			t.Fatalf("wrong receiver: %+v, %v", delivery, err)
		}
	}
	if _, err := mock.DeliverWebhook(context.Background(), mockgithub.WebhookRequest{Event: "ping", Payload: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("ambiguous app ID accepted")
	}
}
