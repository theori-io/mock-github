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
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func approvalFixture(t *testing.T, receiver string) mockgithub.Config {
	t.Helper()
	file, err := os.Open("fixtures/installation-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cfg, err := mockgithub.DecodeConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Apps[0].Webhook.URL = receiver
	return cfg
}

const requestPath = "/__mock/orgs/acme/installation-requests"
const requestBody = `{"app_id":7,"repositories":["acme/demo"]}`

func TestInstallationRequestApprovalLifecycle(t *testing.T) {
	var apiURL string
	events := make(chan mockgithub.Object, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte("test-secret"))
		_, _ = mac.Write(body)
		if r.Header.Get("X-GitHub-Event") != "installation" || r.Header.Get("X-Hub-Signature-256") != fmt.Sprintf("sha256=%x", mac.Sum(nil)) {
			t.Error("invalid generated installation webhook")
		}
		event := object(t, body)
		installation := event["installation"].(map[string]any)
		request, _ := http.NewRequest("POST", fmt.Sprintf("%s/app/installations/%d/access_tokens", apiURL, int(installation["id"].(float64))), strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer app-jwt")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		defer response.Body.Close()
		if response.StatusCode != 201 {
			t.Error("installation not committed before event")
		}
		events <- event
		w.WriteHeader(202)
	}))
	defer receiver.Close()
	mock, server := start(t, approvalFixture(t, receiver.URL))
	apiURL = server.URL
	_, identity := call(t, server, "GET", "/user", "", "admin-token", 200)
	if object(t, identity)["login"] != "alice" {
		t.Fatal("actor identity ignored")
	}
	_, created := call(t, server, "POST", requestPath, requestBody, "member-token", 201)
	request := object(t, created)
	if request["status"] != "pending" || request["installation_id"] != float64(0) || request["requester"].(map[string]any)["login"] != "octocat" {
		t.Fatalf("request: %s", created)
	}
	if len(mock.Deliveries()) != 0 {
		t.Fatal("request emitted an installation event")
	}
	_, pending := call(t, server, "GET", "/orgs/acme/installation-requests", "", "admin-token", 200)
	var requests []mockgithub.Object
	if err := json.Unmarshal(pending, &requests); err != nil || len(requests) != 1 {
		t.Fatalf("pending list: %s", pending)
	}
	_, before := call(t, server, "GET", "/app/installations", "", "app-jwt", 200)
	if strings.TrimSpace(string(before)) != "[]" {
		t.Fatal("pending request exposed as installation")
	}
	call(t, server, "POST", "/app/installations/1/access_tokens", `{}`, "app-jwt", 404)
	approve := fmt.Sprintf("%s/%d/approve", requestPath, int(request["id"].(float64)))
	call(t, server, "POST", approve, `{}`, "member-token", 403)
	_, approved := call(t, server, "POST", approve, `{ }`, "admin-token", 201)
	result := object(t, approved)
	installation := result["installation"].(map[string]any)
	id := int(installation["id"].(float64))
	if result["request"].(map[string]any)["status"] != "approved" || result["delivery"].(map[string]any)["response_status"] != float64(202) {
		t.Fatalf("approval: %s", approved)
	}
	event := <-events
	if event["action"] != "created" || event["sender"].(map[string]any)["login"] != "alice" || event["requester"].(map[string]any)["login"] != "octocat" || event["installation"].(map[string]any)["app_id"] != float64(7) {
		t.Fatalf("event: %+v", event)
	}
	if installation["account"].(map[string]any)["login"] != "acme" || installation["account"].(map[string]any)["id"] != float64(2) {
		t.Fatalf("installation account: %+v", installation)
	}
	call(t, server, "GET", fmt.Sprintf("/app/installations/%d", id), "", "app-jwt", 200)
	_, credentials := call(t, server, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", id), `{}`, "app-jwt", 201)
	token := object(t, credentials)["token"].(string)
	_, repos := call(t, server, "GET", "/installation/repositories", "", token, 200)
	if object(t, repos)["total_count"] != float64(1) {
		t.Fatal("approved repository grants missing")
	}
	_, visible := call(t, server, "GET", "/user/installations", "", "member-token", 200)
	if object(t, visible)["total_count"] != float64(1) {
		t.Fatal("member cannot discover approved installation")
	}
	_, hidden := call(t, server, "GET", "/user/installations", "", "outsider-token", 200)
	if object(t, hidden)["total_count"] != float64(0) {
		t.Fatal("installation exposed to outsider")
	}
	_, after := call(t, server, "GET", "/orgs/acme/installation-requests", "", "admin-token", 200)
	if strings.TrimSpace(string(after)) != "[]" {
		t.Fatal("approved request remained pending")
	}
	call(t, server, "POST", approve, "", "admin-token", 409)
	call(t, server, "POST", requestPath, requestBody, "member-token", 409)
	if len(mock.Deliveries()) != 1 {
		t.Fatal("duplicate approval emitted another webhook")
	}
	mock.Reset()
	call(t, server, "POST", approve, "", "admin-token", 404)
	call(t, server, "GET", "/installation/repositories", "", token, 401)
	_, reset := call(t, server, "GET", "/app/installations", "", "app-jwt", 200)
	if strings.TrimSpace(string(reset)) != "[]" || len(mock.Deliveries()) != 0 {
		t.Fatal("reset retained approval state")
	}
	call(t, server, "POST", requestPath, requestBody, "member-token", 201)
}

func TestInstallationRequestsEnforceActorsOrganizationsAndInputs(t *testing.T) {
	cfg := approvalFixture(t, "http://127.0.0.1:1")
	cfg.Repositories = append(cfg.Repositories, mockgithub.Repository{Owner: "other-org", Name: "private"})
	_, server := start(t, cfg)
	for _, token := range []string{"", "bad-token", "app-jwt", "ghs_mock_1_1"} {
		call(t, server, "POST", requestPath, requestBody, token, 401)
	}
	call(t, server, "POST", requestPath, requestBody, "outsider-token", 403)
	call(t, server, "GET", "/orgs/acme/installation-requests", "", "member-token", 403)
	call(t, server, "GET", "/orgs/acme/installation-requests", "", "outsider-token", 403)
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`null`, 422}, {`{}`, 422}, {`{"app_id":999,"repositories":["acme/demo"]}`, 422},
		{`{"app_id":7,"repositories":["other-org/private"]}`, 422}, {`{"app_id":7,"repositories":["missing/repo"]}`, 422},
		{`{"app_id":7,"repositories":["acme/demo","ACME/demo"]}`, 422},
		{`{"app_id":7,"repositories":["acme/demo"],"role":"admin"}`, 400},
		{requestBody + ` {}`, 400}, {`{"app_id":7,"repositories":[]}`, 422},
	} {
		call(t, server, "POST", requestPath, tc.body, "member-token", tc.status)
	}
	call(t, server, "POST", requestPath, strings.Repeat("x", (1<<20)+1), "member-token", 413)
	_, created := call(t, server, "POST", requestPath, requestBody, "member-token", 201)
	id := int(object(t, created)["id"].(float64))
	approve := fmt.Sprintf("%s/%d/approve", requestPath, id)
	call(t, server, "POST", requestPath, requestBody, "member-token", 409)
	call(t, server, "POST", approve, `{"permissions":{"contents":"write"}}`, "admin-token", 400)
	call(t, server, "POST", fmt.Sprintf("/__mock/orgs/other-org/installation-requests/%d/approve", id), "", "outsider-token", 404)
	call(t, server, "POST", fmt.Sprintf("/__mock/orgs/other-org/installation-requests/%d/approve", id), "", "admin-token", 403)
	call(t, server, "POST", "/__mock/orgs/missing/installation-requests", requestBody, "admin-token", 404)
	call(t, server, "POST", "/orgs/acme/installations", requestBody, "member-token", 404)
}

func TestConcurrentApprovalCreatesOneInstallationAndFailedDeliveryCanBeRetried(t *testing.T) {
	var attempts atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(202)
		}
	}))
	defer receiver.Close()
	cfg := approvalFixture(t, receiver.URL)
	cfg.Apps[0].Data["permissions"] = mockgithub.Object{"metadata": "read"}
	mock, server := start(t, cfg)
	_, created := call(t, server, "POST", requestPath, requestBody, "member-token", 201)
	approve := fmt.Sprintf("%s/%d/approve", requestPath, int(object(t, created)["id"].(float64)))
	var wg sync.WaitGroup
	var approved, conflicts atomic.Int32
	for range 8 {
		wg.Go(func() {
			r, _ := http.NewRequest("POST", server.URL+approve, nil)
			r.Header.Set("Authorization", "Bearer admin-token")
			response, err := server.Client().Do(r)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			switch response.StatusCode {
			case 201:
				approved.Add(1)
			case 409:
				conflicts.Add(1)
			default:
				t.Errorf("approval status %d", response.StatusCode)
			}
		})
	}
	wg.Wait()
	if approved.Load() != 1 || conflicts.Load() != 7 || attempts.Load() != 1 {
		t.Fatal("approval transition was not atomic")
	}
	deliveries := mock.Deliveries()
	if len(deliveries) != 1 || deliveries[0].ResponseStatus != 503 {
		t.Fatalf("failed delivery history: %+v", deliveries)
	}
	_, installed := call(t, server, "GET", "/app/installations", "", "app-jwt", 200)
	var installations []mockgithub.Object
	if err := json.Unmarshal(installed, &installations); err != nil || len(installations) != 1 {
		t.Fatalf("installation missing after receiver failure: %s", installed)
	}
	permissions := installations[0]["permissions"].(map[string]any)
	if len(permissions) != 1 || permissions["metadata"] != "read" {
		t.Fatal("approval added permissions not requested by app")
	}
	delivery, err := mock.RedeliverWebhook(context.Background(), deliveries[0].ID)
	if err != nil || delivery.ResponseStatus != 202 || delivery.ID != deliveries[0].ID || delivery.Attempt != 2 {
		t.Fatalf("retry: %+v %v", delivery, err)
	}
}

func TestResetCancelsApprovalDeliveryAndClearsInstallation(t *testing.T) {
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
	mock, server := start(t, approvalFixture(t, receiver.URL))
	_, created := call(t, server, "POST", requestPath, requestBody, "member-token", 201)
	approve := fmt.Sprintf("%s/%d/approve", requestPath, int(object(t, created)["id"].(float64)))
	done := make(chan []byte, 1)
	go func() { _, result := call(t, server, "POST", approve, "", "admin-token", 201); done <- result }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("approval delivery never started")
	}
	call(t, server, "GET", "/app/installations/1", "", "app-jwt", 200)
	mock.Reset()
	select {
	case result := <-done:
		if object(t, result)["delivery"].(map[string]any)["error"] == nil {
			t.Fatal("reset failed to cancel approval webhook")
		}
	case <-time.After(time.Second):
		t.Fatal("reset blocked approval")
	}
	call(t, server, "GET", "/app/installations/1", "", "app-jwt", 404)
	if len(mock.Deliveries()) != 0 {
		t.Fatal("old approval delivery restored cleared log")
	}
}

func TestInvalidActorFixturesLeaveRequestsIntact(t *testing.T) {
	cfg := approvalFixture(t, "http://127.0.0.1:1")
	mock, server := start(t, cfg)
	call(t, server, "POST", requestPath, requestBody, "member-token", 201)
	for _, mutate := range []func(*mockgithub.Config){
		func(c *mockgithub.Config) { c.Users[0].Token = "member-token" },
		func(c *mockgithub.Config) { c.Users[0].Token = "app-jwt" },
		func(c *mockgithub.Config) { c.Users[0].Token = "" },
		func(c *mockgithub.Config) { c.Users[0].Data["id"] = 1 },
		func(c *mockgithub.Config) { c.Users[0].Data["login"] = "octocat" },
		func(c *mockgithub.Config) { c.Organizations[0].Members["alice"] = "owner" },
		func(c *mockgithub.Config) { c.Organizations[0].Members["missing-user"] = "admin" },
		func(c *mockgithub.Config) { c.Organizations[0].ID = 0 },
		func(c *mockgithub.Config) { c.Organizations = append(c.Organizations, c.Organizations[0]) },
	} {
		bad := approvalFixture(t, "http://127.0.0.1:1")
		mutate(&bad)
		if err := mock.Load(bad); err == nil {
			t.Fatal("invalid actor fixture accepted")
		}
	}
	_, pending := call(t, server, "GET", "/orgs/acme/installation-requests", "", "admin-token", 200)
	if !strings.Contains(string(pending), "octocat") {
		t.Fatal("invalid load discarded pending request")
	}
}

func TestApprovalPreservesSeededInstallationsAndRejectsMissingReceiver(t *testing.T) {
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer receiver.Close()
	cfg := approvalFixture(t, receiver.URL)
	cfg.Repositories = append(cfg.Repositories, mockgithub.Repository{Owner: "other-org", Name: "demo"})
	cfg.Installations = []mockgithub.Installation{{ID: 1, AppID: 7, Account: mockgithub.Object{"login": "other-org", "id": 5, "type": "Organization"}, Repositories: []string{"other-org/demo"}}}
	_, server := start(t, cfg)
	call(t, server, "POST", "/__mock/orgs/other-org/installation-requests", `{"app_id":7,"repositories":["other-org/demo"]}`, "outsider-token", 409)
	call(t, server, "POST", requestPath, requestBody, "member-token", 201)
	_, approved := call(t, server, "POST", requestPath+"/1/approve", "", "admin-token", 201)
	if object(t, approved)["installation"].(map[string]any)["id"] != float64(2) {
		t.Fatal("approval reused seeded installation ID")
	}
	_, seeded := call(t, server, "GET", "/app/installations/1", "", "app-jwt", 200)
	if object(t, seeded)["account"].(map[string]any)["login"] != "other-org" {
		t.Fatal("approval replaced seeded installation")
	}
	cfg.Installations = nil
	cfg.Apps[0].Webhook = nil
	_, noReceiver := start(t, cfg)
	call(t, noReceiver, "POST", requestPath, requestBody, "member-token", 422)
}
