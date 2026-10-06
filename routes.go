package mockgithub

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Server) route(r *http.Request, body []byte) apiResponse {
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.Method == http.MethodGet && r.URL.Path == "/app" {
		return jsonResponse(200, renderApp(s.requestApp(r)))
	}
	if r.Method == http.MethodGet && len(p) == 2 && p[0] == "apps" {
		for _, app := range s.state.apps {
			if app.Slug == p[1] {
				return jsonResponse(200, renderApp(app))
			}
		}
		return errorResponse(404, "Not Found")
	}
	if r.Method == http.MethodGet && r.URL.Path == "/user" {
		user := s.requestUser(r)
		if user == nil {
			user = s.state.user
		}
		return jsonResponse(200, renderUser(user, r))
	}
	if len(p) == 3 && p[0] == "orgs" && p[2] == "installation-requests" && r.Method == http.MethodGet {
		return s.listInstallationRequests(r, p[1])
	}
	if r.URL.Path == "/installation/token" && r.Method == http.MethodDelete {
		token := bearer(r)
		if _, exists := s.state.tokens[token]; !exists {
			return errorResponse(401, "Bad credentials")
		}
		issued := s.state.tokens[token]
		issued.expires = time.Time{}
		s.state.tokens[token] = issued
		return jsonResponse(204, nil)
	}
	if r.URL.Path == "/user/repos" || r.URL.Path == "/installation/repositories" || (len(p) == 3 && p[0] == "orgs" && p[2] == "repos") {
		if r.Method != http.MethodGet {
			return errorResponse(404, "Not Found")
		}
		list := []Object{}
		for _, key := range sortedKeys(s.state.repos) {
			if len(p) == 3 && !strings.HasPrefix(key, strings.ToLower(p[1])+"/") {
				continue
			}
			if !s.canAccess(r, key) {
				continue
			}
			list = append(list, renderRepo(s.state.repos[key], r))
		}
		if r.URL.Path == "/installation/repositories" {
			return paginated(r, list, "repositories")
		}
		return paginated(r, list, "")
	}
	if r.URL.Path == "/app/installations" || r.URL.Path == "/user/installations" {
		if r.Method != http.MethodGet {
			return errorResponse(404, "Not Found")
		}
		list := []Object{}
		for _, installation := range s.state.installations {
			if r.URL.Path == "/app/installations" && installation.AppID != s.requestApp(r).ID {
				continue
			}
			if r.URL.Path == "/user/installations" && !s.userCanSeeInstallation(r, installation) {
				continue
			}
			list = append(list, s.renderInstallation(installation, r))
		}
		sort.Slice(list, func(i, j int) bool { return number(list[i]["id"]) < number(list[j]["id"]) })
		if r.URL.Path == "/user/installations" {
			return paginated(r, list, "installations")
		}
		return paginated(r, list, "")
	}
	if len(p) >= 3 && p[0] == "app" && p[1] == "installations" {
		id, err := strconv.Atoi(p[2])
		installation := s.state.installations[id]
		if err != nil || installation.ID == 0 || installation.AppID != s.requestApp(r).ID {
			return errorResponse(404, "Not Found")
		}
		if len(p) == 3 && r.Method == http.MethodGet {
			return jsonResponse(200, s.renderInstallation(installation, r))
		}
		if len(p) == 4 && p[3] == "access_tokens" && r.Method == http.MethodPost {
			return s.issueToken(r, installation, body)
		}
	}
	if len(p) < 3 || p[0] != "repos" {
		return errorResponse(404, "Not Found")
	}
	key := repoKey(p[1], p[2])
	repo := s.state.repos[key]
	if repo == nil || !s.canAccess(r, key) {
		return errorResponse(404, "Not Found")
	}
	if r.Method != http.MethodGet {
		return errorResponse(404, "Not Found")
	}
	if len(p) == 3 {
		return jsonResponse(200, renderRepo(repo, r))
	}
	if len(p) == 4 && p[3] == "installation" {
		for _, installation := range s.state.installations {
			if installation.AppID != s.requestApp(r).ID {
				continue
			}
			for _, name := range installation.Repositories {
				if strings.EqualFold(name, key) {
					return jsonResponse(200, s.renderInstallation(installation, r))
				}
			}
		}
		return errorResponse(404, "Not Found")
	}
	ref := strings.Join(p[4:], "/")
	switch p[3] {
	case "branches":
		if len(p) == 4 {
			list := []Object{}
			for _, name := range sortedKeys(repo.branches) {
				branch := repo.branches[name]
				if protected := r.URL.Query().Get("protected"); protected != "" && strconv.FormatBool(branch.Protected) != protected {
					continue
				}
				list = append(list, renderBranch(repo, branch, r))
			}
			return paginated(r, list, "")
		}
		if branch, exists := repo.branches[ref]; exists {
			return jsonResponse(200, renderBranch(repo, branch, r))
		}
	case "commits":
		if len(p) > 4 {
			if commit, exists := repo.resolve(ref); exists {
				return jsonResponse(200, renderCommit(repo, commit, r))
			}
		}
	case "pulls":
		if len(p) == 4 {
			state := r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			if state != "open" && state != "closed" && state != "all" {
				return errorResponse(422, "Invalid state")
			}
			list := []Object{}
			for _, pull := range repo.pulls {
				if state != "all" && pull["state"] != state {
					continue
				}
				if base := r.URL.Query().Get("base"); base != "" && asObject(pull["base"])["ref"] != base {
					continue
				}
				list = append(list, renderPull(repo, pull, r))
			}
			sort.Slice(list, func(i, j int) bool {
				if r.URL.Query().Get("direction") == "asc" {
					return number(list[i]["number"]) < number(list[j]["number"])
				}
				return number(list[i]["number"]) > number(list[j]["number"])
			})
			return paginated(r, list, "")
		}
		if len(p) == 5 {
			n, err := strconv.Atoi(p[4])
			if err == nil && repo.pulls[n] != nil {
				return jsonResponse(200, renderPull(repo, repo.pulls[n], r))
			}
		}
	case "zipball", "tarball":
		if commit, exists := repo.resolve(ref); exists {
			return archiveResponse(repo, commit, p[3])
		}
	}
	return errorResponse(404, "Not Found")
}

func bearer(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || (!strings.EqualFold(parts[0], "Bearer") && !strings.EqualFold(parts[0], "token")) {
		return ""
	}
	return parts[1]
}

func appRequest(r *http.Request) bool {
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	return r.URL.Path == "/app" || strings.HasPrefix(r.URL.Path, "/app/") || (len(p) == 4 && p[0] == "repos" && p[3] == "installation")
}

func (s *Server) requestApp(r *http.Request) App {
	for _, app := range s.state.apps {
		if app.Token == "" || app.Token == bearer(r) {
			return app
		}
	}
	return App{}
}

func (s *Server) authorized(r *http.Request) bool {
	token := bearer(r)
	if issued, exists := s.state.tokens[token]; exists {
		return !appRequest(r) && s.clock().Before(issued.expires) && (strings.HasPrefix(r.URL.Path, "/repos/") || strings.HasPrefix(r.URL.Path, "/installation/"))
	}
	if strings.HasPrefix(token, "ghs_mock_") || strings.HasPrefix(r.URL.Path, "/installation/") {
		return false
	}
	if appRequest(r) {
		return s.requestApp(r).ID != 0
	}
	return s.requestUser(r) != nil || (s.cfg.Token == "" && len(s.cfg.Users) == 0)
}

func (s *Server) canAccess(r *http.Request, key string) bool {
	if issued, exists := s.state.tokens[bearer(r)]; exists {
		return issued.repositories[key]
	}
	return true
}

func (s *Server) issueToken(r *http.Request, installation Installation, body []byte) apiResponse {
	data := Object{}
	var err error
	if len(body) > 0 {
		data, err = decodeObject(body)
		if err != nil {
			return errorResponse(400, "Invalid JSON object")
		}
	}
	if _, names := data["repositories"]; names {
		if _, ids := data["repository_ids"]; ids {
			return errorResponse(422, "Use repositories or repository_ids")
		}
	}
	allowed := map[string]bool{}
	for _, name := range installation.Repositories {
		allowed[strings.ToLower(name)] = true
	}
	selected := allowed
	for _, field := range []string{"repositories", "repository_ids"} {
		if values, exists := data[field]; exists {
			array, ok := values.([]any)
			if !ok {
				return errorResponse(422, field+" must be an array")
			}
			selected = map[string]bool{}
			for _, value := range array {
				found := false
				for key := range allowed {
					repo := s.state.repos[key]
					if (field == "repositories" && text(value) == text(repo.data["name"])) || (field == "repository_ids" && value == float64(number(repo.data["id"]))) {
						selected[key] = true
						found = true
					}
				}
				if !found {
					return errorResponse(422, "Repository is not accessible to this installation")
				}
			}
		}
	}
	if _, exists := data["permissions"]; exists {
		return errorResponse(422, "Custom token permissions are not supported by this mock")
	}
	token := fmt.Sprintf("ghs_mock_%d_%d", s.generation, s.state.nextToken)
	s.state.nextToken++
	expires := s.clock().Add(time.Hour)
	s.state.tokens[token] = issuedToken{repositories: selected, expires: expires}
	repositories := []Object{}
	for _, key := range sortedKeys(selected) {
		repositories = append(repositories, renderRepo(s.state.repos[key], r))
	}
	return jsonResponse(201, Object{"token": token, "expires_at": expires.UTC().Format(time.RFC3339), "permissions": clone(installation.Permissions), "repositories": repositories, "repository_selection": "selected"})
}

func origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func renderUser(data Object, r *http.Request) Object {
	result := clone(data)
	login := url.PathEscape(text(data["login"]))
	defaults(result, Object{"type": "User"})
	result["url"], result["html_url"] = origin(r)+"/users/"+login, "https://github.com/"+login
	return result
}

func renderRepo(repo *repo, r *http.Request) Object {
	result := clone(repo.data)
	fullName := text(result["full_name"])
	result["url"], result["html_url"] = origin(r)+"/repos/"+fullName, "https://github.com/"+fullName
	result["archive_url"] = origin(r) + "/repos/" + fullName + "/{archive_format}{/ref}"
	result["owner"] = renderUser(asObject(result["owner"]), r)
	return result
}

func renderBranch(repo *repo, branch Branch, r *http.Request) Object {
	return Object{"name": branch.Name, "protected": branch.Protected, "commit": Object{"sha": branch.SHA, "url": origin(r) + "/repos/" + text(repo.data["full_name"]) + "/commits/" + branch.SHA}}
}

func renderCommit(repo *repo, commit Commit, r *http.Request) Object {
	result := clone(commit.Data)
	if result == nil {
		result = Object{}
	}
	defaults(result, Object{"commit": Object{"message": "Mock commit"}, "parents": []any{}})
	result["sha"], result["url"] = commit.SHA, origin(r)+"/repos/"+text(repo.data["full_name"])+"/commits/"+commit.SHA
	return result
}

func renderPull(repo *repo, pull Object, r *http.Request) Object {
	result := clone(pull)
	fullName := text(repo.data["full_name"])
	result["url"] = fmt.Sprintf("%s/repos/%s/pulls/%d", origin(r), fullName, number(pull["number"]))
	result["html_url"] = fmt.Sprintf("https://github.com/%s/pull/%d", fullName, number(pull["number"]))
	result["user"] = renderUser(asObject(result["user"]), r)
	for _, key := range []string{"head", "base"} {
		if branch := asObject(result[key]); branch != nil && branch["repo"] == nil {
			branch["repo"] = renderRepo(repo, r)
		}
	}
	return result
}

func (s *Server) renderInstallation(installation Installation, r *http.Request) Object {
	return Object{"id": installation.ID, "app_id": installation.AppID, "app_slug": s.state.apps[installation.AppID].Slug, "target_type": text(installation.Account["type"]), "account": renderUser(installation.Account, r), "permissions": clone(installation.Permissions), "repository_selection": "selected", "created_at": installation.CreatedAt, "updated_at": installation.CreatedAt, "suspended_at": nil, "events": renderApp(s.state.apps[installation.AppID])["events"], "access_tokens_url": fmt.Sprintf("%s/app/installations/%d/access_tokens", origin(r), installation.ID), "repositories_url": origin(r) + "/installation/repositories"}
}

func renderApp(app App) Object {
	result := clone(app.Data)
	if result == nil {
		result = Object{}
	}
	defaults(result, Object{"name": app.Slug, "owner": Object{"login": "mock-owner", "id": 1}, "permissions": Object{"contents": "read", "metadata": "read", "pull_requests": "read"}, "events": []string{"push", "pull_request"}})
	result["id"], result["slug"], result["html_url"] = app.ID, app.Slug, "https://github.com/apps/"+app.Slug
	return result
}

func paginated(r *http.Request, list []Object, envelope string) apiResponse {
	page, size := 1, 30
	var err error
	if value := r.URL.Query().Get("page"); value != "" {
		page, err = strconv.Atoi(value)
	}
	if err != nil || page < 1 {
		return errorResponse(422, "page must be a positive integer")
	}
	if value := r.URL.Query().Get("per_page"); value != "" {
		size, err = strconv.Atoi(value)
	}
	if err != nil || size < 1 {
		return errorResponse(422, "per_page must be a positive integer")
	}
	if size > 100 {
		size = 100
	}
	last := (len(list) + size - 1) / size
	if last < 1 {
		last = 1
	}
	links := []string{}
	link := func(n int, rel string) {
		query := r.URL.Query()
		query.Set("page", strconv.Itoa(n))
		query.Set("per_page", strconv.Itoa(size))
		links = append(links, fmt.Sprintf("<%s%s?%s>; rel=%q", origin(r), r.URL.EscapedPath(), query.Encode(), rel))
	}
	if page < last {
		link(page+1, "next")
		link(last, "last")
	}
	if page > 1 {
		link(1, "first")
		previous := page - 1
		if previous > last {
			previous = last
		}
		link(previous, "prev")
	}
	result := []Object{}
	if page <= last {
		start := (page - 1) * size
		end := start + size
		if end > len(list) {
			end = len(list)
		}
		result = list[start:end]
	}
	var data any = result
	if envelope != "" {
		data = Object{"total_count": len(list), envelope: result}
	}
	return func(w http.ResponseWriter) int {
		if len(links) > 0 {
			w.Header().Set("Link", strings.Join(links, ", "))
		}
		return writeJSON(w, 200, data)
	}
}
