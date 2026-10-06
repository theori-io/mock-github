package mockgithub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type installationRequest struct {
	ID             int
	Organization   string
	AppID          int
	Requester      Object
	Repositories   []string
	CreatedAt      string
	InstallationID int
}

type installationRequestInput struct {
	AppID        int      `json:"app_id"`
	Repositories []string `json:"repositories"`
}

func (s *Server) listInstallationRequests(r *http.Request, login string) apiResponse {
	org := s.state.organizations[strings.ToLower(login)]
	if org.Login == "" {
		return errorResponse(404, "Organization not found")
	}
	if s.requestUser(r) == nil {
		return errorResponse(401, "A fixture user credential is required")
	}
	if s.organizationRole(r, org) != "admin" {
		return errorResponse(403, "Organization admin role required")
	}
	list := []Object{}
	for _, request := range s.state.installationRequests {
		if strings.EqualFold(request.Organization, org.Login) && request.InstallationID == 0 {
			list = append(list, s.renderInstallationRequest(request, r))
		}
	}
	sort.Slice(list, func(i, j int) bool { return number(list[i]["id"]) < number(list[j]["id"]) })
	return paginated(r, list, "")
}

func (s *Server) renderInstallationRequest(request installationRequest, r *http.Request) Object {
	org := s.state.organizations[strings.ToLower(request.Organization)]
	status := "pending"
	if request.InstallationID != 0 {
		status = "approved"
	}
	return Object{"id": request.ID, "app_id": request.AppID, "app_slug": s.state.apps[request.AppID].Slug, "account": renderUser(organizationAccount(org), r), "requester": renderUser(request.Requester, r), "repositories": append([]string{}, request.Repositories...), "created_at": request.CreatedAt, "status": status, "installation_id": request.InstallationID}
}

func organizationAccount(org Organization) Object {
	return Object{"login": org.Login, "id": org.ID, "type": "Organization"}
}

// These mock workflow controls use user credentials and enforce organization
// roles. The ordinary fixture/reset controls remain unauthenticated.
func (s *Server) installationAdmin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		status := http.StatusBadRequest
		if _, ok := err.(*http.MaxBytesError); ok {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, "Invalid request body")
		return
	}
	s.mu.Lock()
	response := s.prepareInstallationAction(r, body)
	s.mu.Unlock()
	response(w)
}

func (s *Server) prepareInstallationAction(r *http.Request, body []byte) apiResponse {
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	install := len(p) == 4 && p[3] == "installations"
	if r.Method != http.MethodPost || (!install && ((len(p) != 4 && len(p) != 6) || p[3] != "installation-requests" || (len(p) == 6 && p[5] != "approve"))) {
		return errorResponse(404, "Not Found")
	}
	user := s.requestUser(r)
	if user == nil {
		return errorResponse(401, "A fixture user credential is required")
	}
	org := s.state.organizations[strings.ToLower(p[2])]
	if org.Login == "" {
		return errorResponse(404, "Organization not found")
	}
	role := s.organizationRole(r, org)
	if role == "" {
		return errorResponse(403, "Organization membership required")
	}
	if len(p) == 4 && !install {
		return s.createInstallationRequest(r, org, user, body)
	}
	if role != "admin" {
		return errorResponse(403, "Organization admin role required")
	}
	if install {
		return s.installDirectly(r, org, user, body)
	}
	id, err := strconv.Atoi(p[4])
	request, ok := s.state.installationRequests[id]
	if err != nil || !ok || !strings.EqualFold(request.Organization, org.Login) {
		return errorResponse(404, "Installation request not found")
	}
	if len(body) > 0 {
		input, err := decodeObject(body)
		if err != nil || len(input) != 0 {
			return errorResponse(400, "Approval accepts an empty body or {}")
		}
	}
	return s.approveInstallationRequest(r, request, user)
}

// parseInstallationInput validates an app and repository selection for org.
func (s *Server) parseInstallationInput(org Organization, body []byte) (App, []string, apiResponse) {
	var input installationRequestInput
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		return App{}, nil, errorResponse(400, "Invalid installation request")
	}
	if d.Decode(new(any)) != io.EOF {
		return App{}, nil, errorResponse(400, "Expected exactly one JSON value")
	}
	app := s.state.apps[input.AppID]
	if app.ID == 0 {
		return App{}, nil, errorResponse(422, "app_id must reference a fixture app")
	}
	if app.Webhook == nil {
		return App{}, nil, errorResponse(422, "App needs a webhook receiver for the installation workflow")
	}
	for _, installation := range s.state.installations {
		if installation.AppID == app.ID && strings.EqualFold(text(installation.Account["login"]), org.Login) {
			return App{}, nil, errorResponse(409, "App is already installed on this organization")
		}
	}
	for _, request := range s.state.installationRequests {
		if request.AppID == app.ID && strings.EqualFold(request.Organization, org.Login) {
			return App{}, nil, errorResponse(409, "An installation request already exists for this app and organization")
		}
	}
	if len(input.Repositories) == 0 {
		return App{}, nil, errorResponse(422, "repositories must contain selected repository full names")
	}
	repos, seen := []string{}, map[string]bool{}
	for _, name := range input.Repositories {
		key := strings.ToLower(name)
		repo := s.state.repos[key]
		if repo == nil || !strings.HasPrefix(key, strings.ToLower(org.Login)+"/") || seen[key] {
			return App{}, nil, errorResponse(422, "Repositories must be unique fixture repositories owned by this organization")
		}
		seen[key] = true
		repos = append(repos, text(repo.data["full_name"]))
	}
	sort.Strings(repos)
	return app, repos, nil
}

func (s *Server) createInstallationRequest(r *http.Request, org Organization, user Object, body []byte) apiResponse {
	app, repos, failure := s.parseInstallationInput(org, body)
	if failure != nil {
		return failure
	}
	request := installationRequest{ID: s.state.nextRequest, Organization: org.Login, AppID: app.ID, Requester: clone(user), Repositories: repos, CreatedAt: s.now()}
	s.state.nextRequest++
	s.state.installationRequests[request.ID] = request
	return jsonResponse(201, s.renderInstallationRequest(request, r))
}

// installDirectly models an org admin installing the app without a request.
func (s *Server) installDirectly(r *http.Request, org Organization, user Object, body []byte) apiResponse {
	app, repos, failure := s.parseInstallationInput(org, body)
	if failure != nil {
		return failure
	}
	installationData, deliver, failure := s.createInstallation(r, app, org, repos, user, nil)
	if failure != nil {
		return failure
	}
	return func(w http.ResponseWriter) int {
		return writeJSON(w, 201, Object{"installation": installationData, "delivery": deliver(r.Context())})
	}
}

func (s *Server) approveInstallationRequest(r *http.Request, request installationRequest, user Object) apiResponse {
	if request.InstallationID != 0 {
		return errorResponse(409, "Installation request is already approved")
	}
	org := s.state.organizations[strings.ToLower(request.Organization)]
	installationData, deliver, failure := s.createInstallation(r, s.state.apps[request.AppID], org, request.Repositories, user, request.Requester)
	if failure != nil {
		return failure
	}
	request.InstallationID = number(installationData["id"])
	s.state.installationRequests[request.ID] = request
	requestData := s.renderInstallationRequest(request, r)
	return func(w http.ResponseWriter) int {
		return writeJSON(w, 201, Object{"request": requestData, "installation": installationData, "delivery": deliver(r.Context())})
	}
}

// createInstallation commits an installation and returns its created event
// delivery, which callers run after unlocking. A nil requester marks a direct
// install.
func (s *Server) createInstallation(r *http.Request, app App, org Organization, repos []string, sender, requester Object) (Object, func(context.Context) Delivery, apiResponse) {
	id := max(1, s.cfg.InstallationIDStart)
	for s.state.installations[id].ID != 0 {
		id++
	}
	permissions := map[string]string{"contents": "read", "metadata": "read", "pull_requests": "read"}
	if values, exists := app.Data["permissions"]; exists {
		permissions = nil
		encoded, _ := json.Marshal(values)
		if err := json.Unmarshal(encoded, &permissions); err != nil || permissions == nil {
			return nil, nil, errorResponse(422, "App permissions must be an object of string values")
		}
	}
	installation := Installation{ID: id, AppID: app.ID, Account: organizationAccount(org), Repositories: append([]string{}, repos...), Permissions: permissions, CreatedAt: s.now()}
	installationData := s.renderInstallation(installation, r)
	repositories := []Object{}
	for _, key := range installation.Repositories {
		repositories = append(repositories, renderRepo(s.state.repos[strings.ToLower(key)], r))
	}
	var requesterData any
	if requester != nil {
		requesterData = renderUser(requester, r)
	}
	payload, err := json.Marshal(Object{"action": "created", "installation": installationData, "repositories": repositories, "sender": renderUser(sender, r), "requester": requesterData})
	if err != nil {
		return nil, nil, errorResponse(500, fmt.Sprintf("Encode installation event: %v", err))
	}
	if err := validateWebhookEvent("installation", payload); err != nil {
		return nil, nil, errorResponse(422, err.Error())
	}
	run, err := newWebhookRun(app, "installation", payload)
	if err != nil {
		return nil, nil, errorResponse(500, err.Error())
	}
	epoch, timestamp := s.webhookContext, s.now()
	// Commit before delivery so a webhook receiver can immediately exchange a
	// token. The event owns its fixture snapshot and reset cancellation context.
	s.state.installations[id] = installation
	return installationData, func(ctx context.Context) Delivery {
		return s.sendWebhook(ctx, epoch, run, 1, timestamp)
	}, nil
}
