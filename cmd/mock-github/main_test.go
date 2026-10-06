package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	mockgithub "github.com/theori-io/mock-github"
)

func TestBinaryLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM lifecycle test")
	}
	dir := t.TempDir()
	received := make(chan string, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte("test-secret"))
		_, _ = mac.Write(body)
		if r.Header.Get("X-Hub-Signature-256") != fmt.Sprintf("sha256=%x", mac.Sum(nil)) || r.Header.Get("X-GitHub-Delivery") == "" {
			t.Error("binary sent an invalid webhook signature or delivery ID")
		}
		received <- r.Header.Get("X-GitHub-Event")
		w.WriteHeader(202)
	}))
	defer receiver.Close()
	fixtureData, err := os.ReadFile("../../fixtures/example.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mockgithub.DecodeConfig(bytes.NewReader(fixtureData))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Apps[0].Webhook.URL = receiver.URL
	fixtureData, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fixtureFile := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(fixtureFile, fixtureData, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "mock-github")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), time.Minute)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-trimpath", "-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	readyFile := filepath.Join(dir, "ready.json")
	cmd := exec.CommandContext(ctx, binary, "-addr", "127.0.0.1:0", "-fixture", fixtureFile, "-ready-file", readyFile)
	// Keep the binary test independent of developer authentication settings.
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GITHUB_MOCK_TOKEN=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	started := make(chan []byte, 1)
	go func() {
		scanner := bufio.NewScanner(pipe)
		if scanner.Scan() {
			started <- append([]byte(nil), scanner.Bytes()...)
		} else {
			started <- nil
		}
	}()
	var line []byte
	select {
	case line = <-started:
	case <-ctx.Done():
		t.Fatal("startup timed out")
	}
	var ready struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(line, &ready); err != nil || ready.URL == "" {
		t.Fatalf("invalid startup JSON: %s", line)
	}
	file, err := os.ReadFile(readyFile)
	if err != nil || !bytes.Equal(bytes.TrimSpace(file), line) {
		t.Fatalf("ready file mismatch: %v: %s", err, file)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	installationToken := ""
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		r, err := http.NewRequest(method, ready.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if installationToken != "" {
			r.Header.Set("Authorization", "Bearer "+installationToken)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("%s %s: status %d: %s", method, path, response.StatusCode, data)
		}
		return data
	}
	call("GET", "/__mock/health", "", 200)
	call("GET", "/app", "", 200)
	firstDeliveryID := ""
	for _, fixture := range cfg.WebhookEvents {
		result := call("POST", "/__mock/webhooks/deliver", fmt.Sprintf(`{"fixture":%q}`, fixture.Name), 201)
		var delivery mockgithub.Delivery
		if err := json.Unmarshal(result, &delivery); err != nil || delivery.ResponseStatus != 202 || delivery.Error != "" {
			t.Fatalf("binary webhook: %s", result)
		}
		if firstDeliveryID == "" {
			firstDeliveryID = delivery.ID
		}
		if event := <-received; event != fixture.Event {
			t.Fatalf("binary webhook event: %q", event)
		}
	}
	call("POST", "/__mock/webhooks/deliveries/"+firstDeliveryID+"/redeliver", "", 201)
	if event := <-received; event != "push" {
		t.Fatalf("binary redelivery event: %q", event)
	}
	var deliveries []mockgithub.Delivery
	if err := json.Unmarshal(call("GET", "/__mock/webhooks/deliveries", "", 200), &deliveries); err != nil || len(deliveries) != 4 || deliveries[3].Attempt != 2 || deliveries[3].ID != firstDeliveryID {
		t.Fatalf("binary delivery history: %+v", deliveries)
	}
	call("GET", "/installation/repositories", "", 401)
	issued := call("POST", "/app/installations/42/access_tokens", `{}`, 201)
	var credentials struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(issued, &credentials); err != nil || credentials.Token == "" {
		t.Fatalf("invalid installation credentials: %s", issued)
	}
	installationToken = credentials.Token
	call("GET", "/installation/repositories", "", 200)
	call("GET", "/repos/acme/demo/branches/main", "", 200)
	archive := call("GET", "/repos/acme/demo/zipball/main", "", 200)
	if !bytes.HasPrefix(archive, []byte("PK")) {
		t.Fatal("binary did not serve a zip archive")
	}
	call("POST", "/__mock/stubs", `{"method":"GET","path":"/repos/acme/demo","responses":[{"status":503,"body":{"message":"unavailable"}}]}`, 201)
	call("GET", "/repos/acme/demo", "", 503)
	requests := call("GET", "/__mock/requests", "", 200)
	if !bytes.Contains(requests, []byte("zipball")) {
		t.Fatal("binary did not record archive download")
	}
	call("POST", "/__mock/reset", "", 204)
	if data := call("GET", "/__mock/webhooks/deliveries", "", 200); strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("binary reset retained deliveries: %s", data)
	}
	call("POST", "/__mock/webhooks/deliveries/"+firstDeliveryID+"/redeliver", "", 404)
	call("GET", "/installation/repositories", "", 401)
	installationToken = ""
	call("GET", "/repos/acme/demo", "", 200)
	approvalData, err := os.ReadFile("../../fixtures/installation-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	approvalConfig, err := mockgithub.DecodeConfig(bytes.NewReader(approvalData))
	if err != nil {
		t.Fatal(err)
	}
	approvalConfig.Apps[0].Webhook.URL = receiver.URL
	approvalData, err = json.Marshal(approvalConfig)
	if err != nil {
		t.Fatal(err)
	}
	call("PUT", "/__mock/fixtures", string(approvalData), 204)
	installationToken = "member-token"
	created := call("POST", "/__mock/orgs/acme/installation-requests", `{"app_id":7,"repositories":["acme/demo"]}`, 201)
	var pendingRequest struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(created, &pendingRequest); err != nil || pendingRequest.ID == 0 {
		t.Fatalf("binary installation request: %s", created)
	}
	approvePath := fmt.Sprintf("/__mock/orgs/acme/installation-requests/%d/approve", pendingRequest.ID)
	call("POST", approvePath, "", 403)
	installationToken = "admin-token"
	if requests := call("GET", "/orgs/acme/installation-requests", "", 200); !bytes.Contains(requests, []byte("octocat")) {
		t.Fatalf("binary missing pending request: %s", requests)
	}
	approved := call("POST", approvePath, "", 201)
	var approval struct {
		Installation mockgithub.Installation `json:"installation"`
		Delivery     mockgithub.Delivery     `json:"delivery"`
	}
	if err := json.Unmarshal(approved, &approval); err != nil || approval.Installation.ID == 0 || approval.Delivery.ResponseStatus != 202 {
		t.Fatalf("binary approval: %s", approved)
	}
	if event := <-received; event != "installation" {
		t.Fatalf("binary approval webhook event: %q", event)
	}
	call("POST", approvePath, "", 409)
	installationToken = "app-jwt"
	call("GET", fmt.Sprintf("/app/installations/%d", approval.Installation.ID), "", 200)
	issued = call("POST", fmt.Sprintf("/app/installations/%d/access_tokens", approval.Installation.ID), `{}`, 201)
	if err := json.Unmarshal(issued, &credentials); err != nil {
		t.Fatal(err)
	}
	installationToken = credentials.Token
	call("GET", "/installation/repositories", "", 200)
	call("POST", "/__mock/reset", "", 204)
	call("GET", "/installation/repositories", "", 401)
	installationToken = "app-jwt"
	call("GET", fmt.Sprintf("/app/installations/%d", approval.Installation.ID), "", 404)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("graceful shutdown: %v: %s", err, stderr.String())
	}
	if _, err := os.Stat(readyFile); !os.IsNotExist(err) {
		t.Fatal("ready file survived shutdown")
	}
}

func TestCLIRejectsInvalidInput(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(file, []byte(`{"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-unknown"}, {"extra"}, {"-fixture", file}, {"-fixture", filepath.Join(dir, "missing.json")}, {"-addr", "invalid"}} {
		if err := run(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
	if err := run(context.Background(), []string{"-help"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}
