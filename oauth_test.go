package mockgithub_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	mockgithub "github.com/theori-io/mock-github"
)

func exchange(t *testing.T, serverURL, body, contentType, accept string) (string, []byte) {
	t.Helper()
	request, err := http.NewRequest("POST", serverURL+"/login/oauth/access_token", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", contentType)
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("exchange status %d", response.StatusCode)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.Header.Get("Content-Type"), data
}

func TestOAuthCodeExchangeIssuesSingleUseUserToken(t *testing.T) {
	mock, server := start(t, approvalFixture(t, "http://127.0.0.1:1"))
	body := `{"client_id":"example-client","client_secret":"example-secret","code":"admin-code"}`
	_, data := exchange(t, server.URL, body, "application/json", "application/json")
	token := object(t, data)
	if token["access_token"] != "admin-token" || token["token_type"] != "bearer" {
		t.Fatalf("token: %s", data)
	}
	_, identity := call(t, server, "GET", "/user", "", token["access_token"].(string), 200)
	if object(t, identity)["login"] != "alice" {
		t.Fatal("exchanged token does not identify the user")
	}
	_, data = exchange(t, server.URL, body, "application/json", "application/json")
	if object(t, data)["error"] != "bad_verification_code" {
		t.Fatalf("reused code: %s", data)
	}
	mock.Reset()
	_, data = exchange(t, server.URL, body, "application/json", "application/json")
	if object(t, data)["access_token"] != "admin-token" {
		t.Fatalf("reset did not restore code: %s", data)
	}
}

func TestOAuthCodeExchangeRejectsBadInputs(t *testing.T) {
	_, server := start(t, approvalFixture(t, "http://127.0.0.1:1"))
	for body, want := range map[string]string{
		`{"client_id":"example-client","client_secret":"wrong","code":"member-code"}`:           "incorrect_client_credentials",
		`{"client_id":"unknown","client_secret":"example-secret","code":"member-code"}`:         "incorrect_client_credentials",
		`{"client_id":"example-client","client_secret":"example-secret","code":"unknown-code"}`: "bad_verification_code",
	} {
		_, data := exchange(t, server.URL, body, "application/json", "application/json")
		if object(t, data)["error"] != want {
			t.Errorf("%s: %s", body, data)
		}
	}
}

func TestOAuthCodeExchangeDefaultsToFormEncoding(t *testing.T) {
	_, server := start(t, approvalFixture(t, "http://127.0.0.1:1"))
	body := url.Values{"client_id": {"example-client"}, "client_secret": {"example-secret"}, "code": {"member-code"}}.Encode()
	contentType, data := exchange(t, server.URL, body, "application/x-www-form-urlencoded", "")
	values, err := url.ParseQuery(string(data))
	if err != nil || !strings.HasPrefix(contentType, "application/x-www-form-urlencoded") || values.Get("access_token") != "member-token" {
		t.Fatalf("%s: %s", contentType, data)
	}
}

func TestOAuthFixtureValidation(t *testing.T) {
	for name, edit := range map[string]func(*mockgithub.Config){
		"client id without secret": func(cfg *mockgithub.Config) { cfg.Apps[0].ClientSecret = "" },
		"duplicate code":           func(cfg *mockgithub.Config) { cfg.Users[0].OAuthCode = "member-code" },
		"code without user token":  func(cfg *mockgithub.Config) { cfg.Token = ""; cfg.Users = nil; cfg.Organizations = nil },
	} {
		cfg := approvalFixture(t, "http://127.0.0.1:1")
		edit(&cfg)
		if _, err := mockgithub.New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAppResponseOmitsClientSecret(t *testing.T) {
	_, server := start(t, approvalFixture(t, "http://127.0.0.1:1"))
	_, data := call(t, server, "GET", "/app", "", "app-jwt", 200)
	app := object(t, data)
	if app["client_id"] != "example-client" || strings.Contains(string(data), "example-secret") {
		t.Fatalf("app: %s", data)
	}
}
