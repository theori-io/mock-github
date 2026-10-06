package mockgithub_test

import (
	"encoding/json"
	"testing"

	mockgithub "github.com/theori-io/mock-github"
)

func TestAppsOwnInstallations(t *testing.T) {
	cfg := fixture(t)
	cfg.Apps[0].Token = "example-jwt"
	cfg.Apps = append(cfg.Apps, mockgithub.App{ID: 8, Slug: "other-app", Token: "other-jwt"})
	cfg.Installations = append(cfg.Installations, mockgithub.Installation{ID: 43, AppID: 8, Account: mockgithub.Object{"login": "acme", "type": "Organization"}, Repositories: []string{"acme/demo"}})
	_, server := start(t, cfg)
	call(t, server, "GET", "/app", "", "", 401)
	for _, tc := range []struct {
		token          string
		appID          int
		installationID int
		slug           string
	}{{"example-jwt", 7, 42, "example-app"}, {"other-jwt", 8, 43, "other-app"}} {
		_, data := call(t, server, "GET", "/app", "", tc.token, 200)
		app := object(t, data)
		if app["id"] != float64(tc.appID) || app["slug"] != tc.slug || app["token"] != nil || app["webhook"] != nil {
			t.Fatalf("wrong app or leaked credentials: %s", data)
		}
		_, data = call(t, server, "GET", "/app/installations", "", tc.token, 200)
		var installations []mockgithub.Object
		if err := json.Unmarshal(data, &installations); err != nil || len(installations) != 1 || installations[0]["id"] != float64(tc.installationID) || installations[0]["app_id"] != float64(tc.appID) || installations[0]["app_slug"] != tc.slug {
			t.Fatalf("wrong installations: %s", data)
		}
		_, data = call(t, server, "GET", "/repos/acme/demo/installation", "", tc.token, 200)
		if object(t, data)["id"] != float64(tc.installationID) {
			t.Fatalf("wrong repository installation: %s", data)
		}
	}
	call(t, server, "GET", "/app/installations/43", "", "example-jwt", 404)
	call(t, server, "POST", "/app/installations/43/access_tokens", `{}`, "example-jwt", 404)
	call(t, server, "GET", "/app/installations/42", "", "other-jwt", 404)
	call(t, server, "POST", "/app/installations/42/access_tokens", `{}`, "other-jwt", 404)
	_, issued := call(t, server, "POST", "/app/installations/43/access_tokens", `{}`, "other-jwt", 201)
	call(t, server, "GET", "/installation/repositories", "", object(t, issued)["token"].(string), 200)
	_, profile := call(t, server, "GET", "/apps/example-app", "", "", 200)
	if object(t, profile)["name"] != "Example App" {
		t.Fatalf("app metadata lost: %s", profile)
	}
	_, all := call(t, server, "GET", "/user/installations", "", "", 200)
	if object(t, all)["total_count"] != float64(2) {
		t.Fatalf("user installations missing an app: %s", all)
	}
}

func TestInvalidAppRelationshipsPreserveState(t *testing.T) {
	mock, _ := start(t, fixture(t))
	for _, mutate := range []func(*mockgithub.Config){
		func(c *mockgithub.Config) { c.Installations[0].AppID = 999 },
		func(c *mockgithub.Config) { c.Apps = append(c.Apps, c.Apps[0]) },
		func(c *mockgithub.Config) {
			c.Apps = append(c.Apps, mockgithub.App{ID: 8, Slug: "example-app", Token: "other"})
		},
		func(c *mockgithub.Config) { c.Apps = append(c.Apps, mockgithub.App{ID: 8, Slug: "other"}) },
		func(c *mockgithub.Config) {
			c.Apps[0].Token = "same"
			c.Apps = append(c.Apps, mockgithub.App{ID: 8, Slug: "other", Token: "same"})
		},
		func(c *mockgithub.Config) {
			c.AppToken = "global"
			c.Apps = append(c.Apps, mockgithub.App{ID: 8, Slug: "other", Token: "other"})
		},
		func(c *mockgithub.Config) {
			c.Installations = append(c.Installations, mockgithub.Installation{ID: 43, AppID: 7, Account: mockgithub.Object{"login": "acme"}, Repositories: []string{"acme/demo"}})
		},
	} {
		cfg := fixture(t)
		mutate(&cfg)
		if err := mock.Load(cfg); err == nil {
			t.Fatal("invalid app relationship accepted")
		}
	}
	mock.Reset()
}
