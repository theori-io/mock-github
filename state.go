package mockgithub

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

type repo struct {
	data     Object
	branches map[string]Branch
	commits  map[string]Commit
	pulls    map[int]Object
}

type issuedToken struct {
	repositories map[string]bool
	expires      time.Time
}

type state struct {
	user                 Object
	users                map[string]Object
	organizations        map[string]Organization
	repos                map[string]*repo
	apps                 map[int]App
	installations        map[int]Installation
	tokens               map[string]issuedToken
	nextToken            int
	installationRequests map[int]installationRequest
	nextRequest          int
}

func newState(cfg Config, now string) (*state, error) {
	if cfg.InstallationIDStart < 0 {
		return nil, fmt.Errorf("installation_id_start must be nonnegative")
	}
	s := &state{user: cfg.User, repos: map[string]*repo{}, apps: map[int]App{}, installations: map[int]Installation{}, tokens: map[string]issuedToken{}, nextToken: 1}
	apps := cfg.Apps
	if len(apps) == 0 {
		apps = []App{{ID: 1, Slug: "mock-app"}}
	}
	if len(apps) > 1 && cfg.AppToken != "" {
		return nil, fmt.Errorf("app_token requires a single app; use apps[].token for multiple apps")
	}
	slugs, tokens := map[string]bool{}, map[string]bool{}
	for _, app := range apps {
		if app.ID <= 0 || s.apps[app.ID].ID != 0 || !validName(app.Slug) || slugs[app.Slug] {
			return nil, fmt.Errorf("app needs a unique positive id and slug")
		}
		if cfg.AppToken != "" {
			if app.Token != "" && app.Token != cfg.AppToken {
				return nil, fmt.Errorf("app_token conflicts with apps[].token")
			}
			app.Token = cfg.AppToken
		}
		if (len(apps) > 1 && app.Token == "") || (app.Token != "" && tokens[app.Token]) || strings.HasPrefix(app.Token, "ghs_mock_") || strings.ContainsAny(app.Token, " \t\r\n") {
			return nil, fmt.Errorf("apps need distinct opaque tokens without whitespace or the ghs_mock_ prefix")
		}
		if err := validateWebhookConfig(app.Webhook); err != nil {
			return nil, fmt.Errorf("app %d: %w", app.ID, err)
		}
		s.apps[app.ID], slugs[app.Slug], tokens[app.Token] = app, true, true
	}
	if s.user == nil {
		s.user = Object{"login": "mock-user", "id": 1}
	}
	if !validName(text(s.user["login"])) {
		return nil, fmt.Errorf("user.login is required")
	}
	if s.user["id"] == nil {
		s.user["id"] = 1
	}
	if err := s.loadIdentities(cfg); err != nil {
		return nil, err
	}
	s.installationRequests, s.nextRequest = map[int]installationRequest{}, 1
	reservedIDs := map[int]bool{}
	for _, fixture := range cfg.Repositories {
		if value, exists := fixture.Data["id"]; exists {
			id := number(value)
			if id <= 0 || float64(id) != value || reservedIDs[id] {
				return nil, fmt.Errorf("repository id must be a unique positive integer")
			}
			reservedIDs[id] = true
		}
	}
	nextID := 1
	for _, fixture := range cfg.Repositories {
		if !validName(fixture.Owner) || !validName(fixture.Name) {
			return nil, fmt.Errorf("repository owner and name must be nonempty path segments")
		}
		key := repoKey(fixture.Owner, fixture.Name)
		if s.repos[key] != nil {
			return nil, fmt.Errorf("duplicate repository %q", key)
		}
		data := fixture.Data
		if data == nil {
			data = Object{}
		}
		if _, exists := data["id"]; !exists {
			for reservedIDs[nextID] {
				nextID++
			}
			data["id"] = nextID
			reservedIDs[nextID] = true
			nextID++
		}
		defaults(data, Object{"private": true, "default_branch": "main", "description": nil, "created_at": now, "updated_at": now})
		data["name"], data["full_name"] = fixture.Name, fixture.Owner+"/"+fixture.Name
		if data["owner"] == nil {
			data["owner"] = Object{"login": fixture.Owner, "type": "Organization"}
		}
		if owner := asObject(data["owner"]); owner == nil || !validName(text(owner["login"])) {
			return nil, fmt.Errorf("%s owner must be an object with login", key)
		}
		r := &repo{data: data, branches: map[string]Branch{}, commits: map[string]Commit{}, pulls: map[int]Object{}}
		for _, commit := range fixture.Commits {
			if !validSHA(commit.SHA) || r.commits[commit.SHA].SHA != "" {
				return nil, fmt.Errorf("%s commit needs a unique 40-character lowercase hex SHA", key)
			}
			for name := range commit.Files {
				if name == "." || path.IsAbs(name) || path.Clean(name) != name || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") {
					return nil, fmt.Errorf("%s invalid archive file path %q", key, name)
				}
			}
			r.commits[commit.SHA] = commit
		}
		for _, branch := range fixture.Branches {
			if branch.Name == "" || r.branches[branch.Name].Name != "" || r.commits[branch.SHA].SHA == "" {
				return nil, fmt.Errorf("%s branch needs a unique name and an existing commit SHA", key)
			}
			r.branches[branch.Name] = branch
		}
		for _, pull := range fixture.PullRequests {
			n := number(pull["number"])
			if n < 1 || float64(n) != pull["number"] || r.pulls[n] != nil {
				return nil, fmt.Errorf("%s pull request needs a unique positive integer number", key)
			}
			head := asObject(pull["head"])
			base := asObject(pull["base"])
			if head == nil || base == nil || r.commits[text(head["sha"])].SHA == "" {
				return nil, fmt.Errorf("%s PR #%d needs head/base objects and an existing head commit", key, n)
			}
			defaults(pull, Object{"id": n, "state": "open", "draft": false, "title": "Mock pull request", "body": "", "user": s.user, "created_at": now, "updated_at": now})
			if user := asObject(pull["user"]); user == nil || !validName(text(user["login"])) {
				return nil, fmt.Errorf("%s PR user must be an object with login", key)
			}
			if pull["state"] != "open" && pull["state"] != "closed" {
				return nil, fmt.Errorf("invalid pull request state")
			}
			r.pulls[n] = pull
		}
		s.repos[key] = r
	}
	installedRepos := map[string]bool{}
	for _, installation := range cfg.Installations {
		installation.AppID = s.defaultAppID(installation.AppID)
		if installation.ID <= 0 || s.installations[installation.ID].ID != 0 || !validName(text(installation.Account["login"])) {
			return nil, fmt.Errorf("installation needs a unique positive id and account.login")
		}
		if s.apps[installation.AppID].ID == 0 {
			return nil, fmt.Errorf("installation %d needs an existing app_id", installation.ID)
		}
		for _, key := range installation.Repositories {
			if s.repos[strings.ToLower(key)] == nil {
				return nil, fmt.Errorf("installation %d refers to unknown repository %q", installation.ID, key)
			}
			installedKey := fmt.Sprintf("%d:%s", installation.AppID, strings.ToLower(key))
			if installedRepos[installedKey] {
				return nil, fmt.Errorf("repository %q belongs to more than one installation of app %d", key, installation.AppID)
			}
			installedRepos[installedKey] = true
		}
		if installation.Permissions == nil {
			installation.Permissions = map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}
		}
		if installation.CreatedAt == "" {
			installation.CreatedAt = now
		}
		s.installations[installation.ID] = installation
	}
	return s, nil
}

func (s *state) defaultAppID(id int) int {
	if id == 0 && len(s.apps) == 1 {
		for key := range s.apps {
			return key
		}
	}
	return id
}

func (r *repo) resolve(ref string) (Commit, bool) {
	if ref == "" {
		ref = text(r.data["default_branch"])
	}
	ref = strings.TrimPrefix(ref, "refs/heads/")
	var n int
	if _, err := fmt.Sscanf(ref, "refs/pull/%d/head", &n); err == nil && ref == fmt.Sprintf("refs/pull/%d/head", n) && r.pulls[n] != nil {
		ref = text(asObject(r.pulls[n]["head"])["sha"])
	}
	if branch, exists := r.branches[ref]; exists {
		ref = branch.SHA
	}
	commit, exists := r.commits[ref]
	return commit, exists
}

func defaults(data, values Object) {
	for key, value := range values {
		if _, exists := data[key]; !exists {
			data[key] = value
		}
	}
}
func repoKey(owner, name string) string { return strings.ToLower(owner + "/" + name) }
func text(value any) string             { valueString, _ := value.(string); return valueString }
func number(value any) int {
	switch value := value.(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}
func asObject(value any) Object {
	switch value := value.(type) {
	case map[string]any:
		return value
	case Object:
		return value
	default:
		return nil
	}
}
func validName(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/?# \t\r\n")
}
func validSHA(value string) bool {
	return len(value) == 40 && strings.Trim(value, "0123456789abcdef") == ""
}
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
