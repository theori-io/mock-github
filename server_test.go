package mockgithub_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mockgithub "github.com/theori-io/mock-github"
)

const mainSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const featureSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func fixture(t *testing.T) mockgithub.Config {
	t.Helper()
	file, err := os.Open("fixtures/example.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cfg, err := mockgithub.DecodeConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Clock = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	return cfg
}

func start(t *testing.T, cfg mockgithub.Config) (*mockgithub.Server, *httptest.Server) {
	t.Helper()
	mock, err := mockgithub.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mock)
	t.Cleanup(server.Close)
	return mock, server
}

func call(t *testing.T, server *httptest.Server, method, path, body, token string, want int) (http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.StatusCode, want, data)
	}
	return response.Header, data
}

func object(t *testing.T, data []byte) mockgithub.Object {
	t.Helper()
	var result mockgithub.Object
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestInProcessHandler(t *testing.T) {
	cfg := fixture(t)
	cfg.Repositories[0].PullRequests[0]["head"].(map[string]any)["repo"] = mockgithub.Object{"full_name": "contributor/demo"}
	cfg.Repositories = append(cfg.Repositories, mockgithub.Repository{Owner: "acme", Name: "second"})
	mock, err := mockgithub.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://github.test/repos/acme/demo/pulls/1", nil)
	w := httptest.NewRecorder()
	mock.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("in-process handler: %d %s", w.Code, w.Body.String())
	}
	if object(t, w.Body.Bytes())["head"].(map[string]any)["repo"].(map[string]any)["full_name"] != "contributor/demo" {
		t.Fatal("fork PR lost its source repository")
	}
	if len(mock.Requests()) != 1 {
		t.Fatal("in-process call was not recorded")
	}
	bad := fixture(t)
	bad.Repositories[0].Branches[0].SHA = featureSHA + "bad"
	if err := mock.Load(bad); err == nil {
		t.Fatal("invalid fixture load accepted")
	}
	w = httptest.NewRecorder()
	mock.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("invalid load replaced prior state")
	}
}

func TestRepositoryAndScanTargetSelection(t *testing.T) {
	_, server := start(t, fixture(t))
	call(t, server, "GET", "/user", "", "", 200)
	_, data := call(t, server, "GET", "/user/repos", "", "", 200)
	if !bytes.Contains(data, []byte("acme/demo")) {
		t.Fatalf("repo picker missing repo: %s", data)
	}
	call(t, server, "GET", "/orgs/acme/repos", "", "", 200)
	_, data = call(t, server, "GET", "/repos/ACME/DEMO", "", "", 200)
	if object(t, data)["url"] != server.URL+"/repos/acme/demo" {
		t.Fatal("API url must point to the mock")
	}
	headers, data := call(t, server, "GET", "/repos/acme/demo/branches?per_page=1", "", "", 200)
	if !strings.Contains(headers.Get("Link"), `rel="next"`) {
		t.Fatal("missing branch pagination")
	}
	if !bytes.Contains(data, []byte("feature/fix")) {
		t.Fatal("branch list is not deterministic")
	}
	_, data = call(t, server, "GET", "/repos/acme/demo/branches/feature%2Ffix", "", "", 200)
	if object(t, data)["commit"].(map[string]any)["sha"] != featureSHA {
		t.Fatal("branch resolved to wrong commit")
	}
	_, data = call(t, server, "GET", "/repos/acme/demo/branches?protected=true", "", "", 200)
	if bytes.Contains(data, []byte("feature/fix")) {
		t.Fatal("protected branch filtering failed")
	}
	for _, ref := range []string{"main", "refs/heads/main", mainSHA} {
		_, data = call(t, server, "GET", "/repos/acme/demo/commits/"+ref, "", "", 200)
		if object(t, data)["sha"] != mainSHA {
			t.Fatalf("ref %s resolved incorrectly", ref)
		}
	}
	_, data = call(t, server, "GET", "/repos/acme/demo/pulls?base=main", "", "", 200)
	if !bytes.Contains(data, []byte("Fix the source")) {
		t.Fatal("PR list is missing scan target")
	}
	_, data = call(t, server, "GET", "/repos/acme/demo/pulls/1", "", "", 200)
	head := object(t, data)["head"].(map[string]any)
	if head["sha"] != featureSHA || head["repo"].(map[string]any)["full_name"] != "acme/demo" {
		t.Fatal("PR head missing source identity")
	}
	call(t, server, "GET", "/repos/acme/demo/commits/refs/pull/1/head", "", "", 200)
	for _, path := range []string{"issues", "actions/runs", "contents/README.md"} {
		call(t, server, "GET", "/repos/acme/demo/"+path, "", "", 404)
	}
	call(t, server, "PUT", "/repos/acme/demo/pulls/1/merge", `{}`, "", 404)
}

func zipFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, file := range archive.File {
		if file.FileInfo().IsDir() {
			continue
		}
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name] = string(content)
	}
	return files
}

func tarFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	files := map[string]string{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		content, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = string(content)
	}
	return files
}

func TestSourceArchivesResolveBranchesAndPRs(t *testing.T) {
	_, server := start(t, fixture(t))
	headers, main := call(t, server, "GET", "/repos/acme/demo/zipball", "", "", 200)
	if headers.Get("Content-Type") != "application/zip" {
		t.Fatal("wrong zip content type")
	}
	files := zipFiles(t, main)
	if files["demo-aaaaaaa/src/main.py"] != "print('main')\n" {
		t.Fatalf("main snapshot missing: %+v", files)
	}
	_, repeated := call(t, server, "GET", "/repos/acme/demo/zipball/main", "", "", 200)
	if !bytes.Equal(main, repeated) {
		t.Fatal("archives must be deterministic")
	}
	_, feature := call(t, server, "GET", "/repos/acme/demo/zipball/refs/pull/1/head", "", "", 200)
	if zipFiles(t, feature)["demo-bbbbbbb/src/main.py"] != "print('feature')\n" {
		t.Fatal("PR archive used default branch")
	}
	headers, compressed := call(t, server, "GET", "/repos/acme/demo/tarball/feature%2Ffix", "", "", 200)
	if headers.Get("Content-Type") != "application/gzip" {
		t.Fatal("wrong tar content type")
	}
	files = tarFiles(t, compressed)
	if files["demo-bbbbbbb/src/main.py"] != "print('feature')\n" {
		t.Fatal("tar snapshot mismatch")
	}
	call(t, server, "GET", "/repos/acme/demo/zipball/missing", "", "", 404)
}

func TestInstallationTokenLifecycleAndRepositoryScope(t *testing.T) {
	cfg := fixture(t)
	cfg.Token = "user-token"
	cfg.AppToken = "app-jwt"
	cfg.Repositories = append(cfg.Repositories, mockgithub.Repository{Owner: "other", Name: "private", Data: mockgithub.Object{"id": 20}})
	var now atomic.Int64
	now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	cfg.Clock = func() time.Time { return time.Unix(now.Load(), 0) }
	mock, server := start(t, cfg)
	call(t, server, "GET", "/user", "", "", 401)
	call(t, server, "GET", "/user", "", "user-token", 200)
	_, data := call(t, server, "GET", "/user/installations", "", "user-token", 200)
	if object(t, data)["total_count"] != float64(1) {
		t.Fatal("installation discovery failed")
	}
	call(t, server, "GET", "/app/installations", "", "user-token", 401)
	call(t, server, "GET", "/app/installations/42", "", "app-jwt", 200)
	call(t, server, "GET", "/repos/acme/demo/installation", "", "app-jwt", 200)
	issue := func(body string) string {
		t.Helper()
		_, data := call(t, server, "POST", "/app/installations/42/access_tokens", body, "app-jwt", 201)
		return object(t, data)["token"].(string)
	}
	token := issue(`{"repository_ids":[10]}`)
	_, data = call(t, server, "GET", "/installation/repositories", "", token, 200)
	if object(t, data)["total_count"] != float64(1) || bytes.Contains(data, []byte("other/private")) {
		t.Fatal("installation token leaked repository")
	}
	_, named := call(t, server, "GET", "/repos/acme/demo", "", token, 200)
	_, numeric := call(t, server, "GET", "/repositories/10", "", token, 200)
	if !bytes.Equal(named, numeric) {
		t.Fatal("numeric lookup differs from named repository")
	}
	if err := mock.AddStub(mockgithub.Stub{Method: "GET", Path: "/repositories/20", Responses: []mockgithub.Response{{Status: 200}}}); err != nil {
		t.Fatal(err)
	}
	call(t, server, "GET", "/repositories/20", "", token, 404)
	call(t, server, "GET", "/repositories/999", "", token, 404)
	call(t, server, "GET", "/repositories/invalid", "", token, 404)
	call(t, server, "GET", "/repos/acme/demo/zipball/main", "", token, 200)
	if err := mock.AddStub(mockgithub.Stub{Method: "GET", Path: "/repos/other/private", Responses: []mockgithub.Response{{Status: 200}}}); err != nil {
		t.Fatal(err)
	}
	call(t, server, "GET", "/repos/other/private", "", token, 404)
	call(t, server, "GET", "/user", "", token, 401)
	call(t, server, "DELETE", "/installation/token", "", token, 204)
	call(t, server, "GET", "/installation/repositories", "", token, 401)
	call(t, server, "GET", "/repositories/10", "", token, 401)
	call(t, server, "GET", "/app/installations", "", token, 401)
	token = issue(`{"repositories":["demo"]}`)
	now.Add(3600)
	call(t, server, "GET", "/installation/repositories", "", token, 401)
	call(t, server, "GET", "/repos/acme/demo", "", token, 401)
	token = issue(`{}`)
	mock.Reset()
	call(t, server, "GET", "/repos/acme/demo", "", token, 401)
	newToken := issue(`{}`)
	if token == newToken {
		t.Fatal("reset reused credentials")
	}
	call(t, server, "POST", "/app/installations/42/access_tokens", `{"repositories":["private"]}`, "app-jwt", 422)
	call(t, server, "POST", "/app/installations/42/access_tokens", `{"repositories":[],"repository_ids":[]}`, "app-jwt", 422)
}

func TestInstallationRepositoryDiscoveryRequiresIssuedToken(t *testing.T) {
	for _, requireAuth := range []bool{false, true} {
		t.Run(fmt.Sprintf("configured-auth=%t", requireAuth), func(t *testing.T) {
			cfg := fixture(t)
			if requireAuth {
				cfg.Token, cfg.AppToken = "user-token", "app-jwt"
			}
			cfg.Repositories = append(cfg.Repositories, mockgithub.Repository{Owner: "other", Name: "private"})
			_, server := start(t, cfg)
			for _, token := range []string{"", "user-token", "app-jwt", "unknown"} {
				call(t, server, "GET", "/installation/repositories", "", token, 401)
			}
			// User repository discovery still supports PAT authentication.
			call(t, server, "GET", "/user/repos", "", "user-token", 200)
			_, issued := call(t, server, "POST", "/app/installations/42/access_tokens", `{}`, "app-jwt", 201)
			token := object(t, issued)["token"].(string)
			_, data := call(t, server, "GET", "/installation/repositories", "", token, 200)
			if object(t, data)["total_count"] != float64(1) || bytes.Contains(data, []byte("other/private")) {
				t.Fatalf("installation scope was not enforced: %s", data)
			}
		})
	}
}

type blockedResponseWriter struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedResponseWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return w.ResponseRecorder.Write(data)
}

func TestBlockedResponseDoesNotBlockStateOperations(t *testing.T) {
	for _, path := range []string{"/user", "/repos/acme/demo/zipball/main", "/repos/acme/demo/tarball/main"} {
		t.Run(path, func(t *testing.T) {
			cfg := fixture(t)
			mock, err := mockgithub.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			writer := &blockedResponseWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
			done := make(chan struct{})
			var release sync.Once
			unblock := func() { release.Do(func() { close(writer.release) }) }
			t.Cleanup(func() { unblock(); <-done })
			go func() { defer close(done); mock.ServeHTTP(writer, httptest.NewRequest("GET", "http://mock"+path, nil)) }()
			select {
			case <-writer.started:
			case <-time.After(time.Second):
				t.Fatal("response did not reach writer")
			}
			// Timeouts bound failures; success is synchronized through channels.
			run := func(name string, operation func()) {
				t.Helper()
				finished := make(chan struct{})
				go func() { defer close(finished); operation() }()
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Fatalf("%s blocked behind the response writer", name)
				}
			}
			other := httptest.NewRecorder()
			run("API call", func() { mock.ServeHTTP(other, httptest.NewRequest("GET", "http://mock/repos/acme/demo", nil)) })
			if other.Code != 200 {
				t.Fatalf("parallel API call failed: %d", other.Code)
			}
			var requests []mockgithub.Request
			run("request inspection", func() { requests = mock.Requests() })
			if len(requests) != 1 || requests[0].Path != "/repos/acme/demo" {
				t.Fatalf("pending response recorded as complete: %+v", requests)
			}
			run("reset", mock.Reset)
			cfg.User["login"] = "replacement"
			cfg.Repositories[0].Commits[0].Files["src/main.py"] = "replacement\n"
			var loadErr error
			run("fixture reload", func() { loadErr = mock.Load(cfg) })
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			unblock()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("response did not complete after unblocking")
			}
			if writer.Code != 200 {
				t.Fatalf("blocked response failed: %d", writer.Code)
			}
			if path == "/user" {
				if object(t, writer.Body.Bytes())["login"] != "octocat" {
					t.Fatal("fixture reload changed the selected response")
				}
			} else {
				var files map[string]string
				if strings.Contains(path, "/zipball/") {
					files = zipFiles(t, writer.Body.Bytes())
				} else {
					files = tarFiles(t, writer.Body.Bytes())
				}
				if files["demo-aaaaaaa/src/main.py"] != "print('main')\n" {
					t.Fatal("fixture reload changed the selected archive")
				}
			}
			if len(mock.Requests()) != 0 {
				t.Fatal("pre-reset response leaked into the new request log")
			}
		})
	}
}

func TestStubsRetriesAndRequestAssertions(t *testing.T) {
	cfg := fixture(t)
	cfg.Token = "secret"
	cfg.MaxRequests = 2
	cfg.Stubs = []mockgithub.Stub{{Method: "GET", Path: "/repos/:owner/:repo", Query: map[string]string{"retry": "yes"}, Times: 2,
		Responses: []mockgithub.Response{{Status: 429, Headers: map[string]string{"Retry-After": "1"}, Body: json.RawMessage(`{"message":"rate limited"}`)}, {Status: 503, Body: json.RawMessage(`{"message":"unavailable"}`)}}}}
	mock, server := start(t, cfg)
	call(t, server, "GET", "/repos/acme/demo", "", "secret", 200)
	headers, _ := call(t, server, "GET", "/repos/acme/demo?retry=yes", "", "secret", 429)
	if headers.Get("Retry-After") != "1" {
		t.Fatal("missing stub header")
	}
	call(t, server, "GET", "/repos/acme/demo?retry=yes", "", "secret", 503)
	call(t, server, "GET", "/repos/acme/demo?retry=yes", "", "secret", 200)
	records := mock.Requests()
	if len(records) != 2 || records[0].Headers.Get("Authorization") != "[REDACTED]" || records[0].Status != 503 {
		t.Fatalf("invalid records: %+v", records)
	}
	records[0].Headers.Set("X-Mutated", "yes")
	if mock.Requests()[0].Headers.Get("X-Mutated") != "" {
		t.Fatal("request snapshot aliases state")
	}
	_, data := call(t, server, "GET", "/__mock/requests", "", "", 200)
	if bytes.Contains(data, []byte("secret")) || len(mock.Requests()) != 2 {
		t.Fatal("admin was logged or credential leaked")
	}
	mock.Reset()
	if len(mock.Requests()) != 0 {
		t.Fatal("reset retained requests")
	}
	call(t, server, "GET", "/repos/acme/demo?retry=yes", "", "secret", 429)
	if err := mock.AddStub(mockgithub.Stub{Method: "POST", Path: "/custom/*", Headers: map[string]string{"X-Test": "yes"}, Body: json.RawMessage(`{"a":1,"b":2}`), Responses: []mockgithub.Response{{Body: json.RawMessage(`{"ok":true}`)}}}); err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("POST", server.URL+"/custom/a/b", strings.NewReader(`{"b":2,"a":1}`))
	r.Header.Set("Authorization", "token secret")
	r.Header.Set("X-Test", "yes")
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("semantic JSON or header match failed")
	}
	mock.Reset()
	call(t, server, "POST", "/custom/a/b", `{"b":2,"a":1}`, "secret", 404)
}

func TestControlAPIIsolationAndConcurrentRequests(t *testing.T) {
	cfg := fixture(t)
	mock, server := start(t, cfg)
	cfg.Repositories[0].Commits[0].Files["src/main.py"] = "mutated"
	_, other := start(t, fixture(t))
	call(t, server, "POST", "/__mock/stubs", `{"method":"GET","path":"/repos/acme/demo","responses":[{"status":503}]}`, "", 201)
	call(t, server, "GET", "/repos/acme/demo", "", "", 503)
	call(t, other, "GET", "/repos/acme/demo", "", "", 200)
	call(t, server, "POST", "/__mock/reset", "", "", 204)
	_, data := call(t, server, "GET", "/repos/acme/demo/zipball", "", "", 200)
	if zipFiles(t, data)["demo-aaaaaaa/src/main.py"] != "print('main')\n" {
		t.Fatal("config aliases reset baseline")
	}
	call(t, server, "PUT", "/__mock/fixtures", `{"unknown":true}`, "", 400)
	call(t, server, "GET", "/repos/acme/demo", "", "", 200)
	call(t, server, "PUT", "/__mock/fixtures", `{"user":{"login":"replacement"}}`, "", 204)
	_, data = call(t, server, "GET", "/user", "", "", 200)
	if object(t, data)["login"] != "replacement" {
		t.Fatal("fixture reload failed")
	}
	call(t, server, "DELETE", "/__mock/requests", "", "", 204)
	if len(mock.Requests()) != 0 {
		t.Fatal("clear requests failed")
	}
	if err := mock.Load(fixture(t)); err != nil {
		t.Fatal(err)
	}
	const count = 30
	var wg sync.WaitGroup
	errors := make(chan error, count)
	for range count {
		wg.Go(func() {
			r, _ := http.NewRequest("POST", server.URL+"/app/installations/42/access_tokens", strings.NewReader(`{}`))
			response, err := server.Client().Do(r)
			if err != nil {
				errors <- err
				return
			}
			defer response.Body.Close()
			if response.StatusCode != 201 {
				errors <- fmt.Errorf("status %d", response.StatusCode)
			}
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	records := mock.Requests()
	if len(records) != count {
		t.Fatalf("lost concurrent requests: %d", len(records))
	}
}

func TestDelayCancellationDoesNotBlockServer(t *testing.T) {
	mock, server := start(t, fixture(t))
	if err := mock.AddStub(mockgithub.Stub{Method: "GET", Path: "/slow", Responses: []mockgithub.Response{{DelayMS: 5000}}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/slow", nil)
	result := make(chan error, 1)
	go func() {
		response, err := server.Client().Do(r)
		if response != nil {
			response.Body.Close()
		}
		result <- err
	}()
	call(t, server, "GET", "/user", "", "", 200)
	if err := <-result; err == nil {
		t.Fatal("delayed request should time out")
	}
}

func TestInvalidInputs(t *testing.T) {
	_, server := start(t, fixture(t))
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/repos/missing/repo", "", 404},
		{"GET", "/repos/acme/demo/branches?page=0", "", 422},
		{"GET", "/repos/acme/demo/branches?per_page=oops", "", 422},
		{"GET", "/repos/acme/demo/pulls?state=invalid", "", 422},
		{"POST", "/app/installations/42/access_tokens", `null`, 400},
		{"POST", "/app/installations/42/access_tokens", strings.Repeat("x", (1<<20)+1), 413},
	} {
		_, data := call(t, server, tc.method, tc.path, tc.body, "", tc.status)
		if object(t, data)["message"] == nil {
			t.Fatal("missing GitHub error message")
		}
	}
	_, data := call(t, server, "GET", "/repos/acme/demo/branches?page=999999999999999", "", "", 200)
	if strings.TrimSpace(string(data)) != "[]" {
		t.Fatal("large page must return empty list")
	}
	for _, data := range []string{`{"typo":true}`, `{} {}`, `{"repositories":[{"owner":"a/b","name":"c"}]}`} {
		cfg, err := mockgithub.DecodeConfig(strings.NewReader(data))
		if err == nil {
			_, err = mockgithub.New(cfg)
		}
		if err == nil {
			t.Fatalf("invalid config accepted: %s", data)
		}
	}
	bad := fixture(t)
	bad.Repositories[0].Commits[0].Files["../escape"] = "x"
	if _, err := mockgithub.New(bad); err == nil {
		t.Fatal("archive path traversal accepted")
	}
	for _, stub := range []mockgithub.Stub{
		{Method: "get", Path: "/x", Responses: []mockgithub.Response{{}}},
		{Method: "GET", Path: "/x?y=1", Responses: []mockgithub.Response{{}}},
		{Method: "GET", Path: "/x/*/y", Responses: []mockgithub.Response{{}}},
		{Method: "GET", Path: "/x", Responses: []mockgithub.Response{{Status: 99}}},
		{Method: "GET", Path: "/x", Responses: []mockgithub.Response{{DelayMS: -1}}},
		{Method: "GET", Path: "/x", Responses: []mockgithub.Response{{Body: json.RawMessage(`{`)}}},
		{Method: "GET", Path: "/x", Responses: []mockgithub.Response{{Status: 204, Body: json.RawMessage(`{}`)}}},
		{Method: "GET", Path: "/__mock/health", Responses: []mockgithub.Response{{}}},
		{Method: "GET", Path: "/x"},
	} {
		if _, err := mockgithub.New(mockgithub.Config{Stubs: []mockgithub.Stub{stub}}); err == nil {
			t.Fatalf("invalid stub accepted: %+v", stub)
		}
	}
}

func TestResponseDefaultsPreserveFixtureMetadata(t *testing.T) {
	cfg := fixture(t)
	cfg.User["name"] = "Fixture User"
	cfg.User["site_admin"] = true
	cfg.Repositories[0].Data["has_discussions"] = true
	cfg.Repositories[0].Data["forks_count"] = 17
	_, server := start(t, cfg)
	_, data := call(t, server, "GET", "/user", "", "", 200)
	user := object(t, data)
	if user["name"] != "Fixture User" || user["site_admin"] != true || user["public_repos"] != float64(0) || user["node_id"] == nil {
		t.Fatal("user defaults overwrote metadata or omitted required fields")
	}
	_, data = call(t, server, "GET", "/repositories/10", "", "", 200)
	repo := object(t, data)
	if repo["has_discussions"] != true || repo["forks_count"] != float64(17) || repo["license"] != nil || repo["hooks_url"] == nil {
		t.Fatal("repository defaults overwrote metadata or omitted required fields")
	}
	_, data = call(t, server, "GET", "/app/installations/42", "", "", 200)
	installation := object(t, data)
	if installation["target_id"] != float64(2) || installation["html_url"] == nil || installation["single_file_name"] != nil || installation["suspended_by"] != nil {
		t.Fatal("installation response omitted schema fields")
	}
}
