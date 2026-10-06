package mockgithub

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

const oauthTokenPath = "/login/oauth/access_token"

const oauthErrorURI = "https://docs.github.com/apps/oauth-apps/maintaining-oauth-apps/troubleshooting-oauth-app-access-token-request-errors"

func (s *state) loadOAuthCodes(cfg Config) error {
	s.oauthCodes = map[string]string{}
	add := func(code, token string) error {
		if code == "" {
			return nil
		}
		if token == "" || s.oauthCodes[code] != "" || strings.ContainsAny(code, " \t\r\n") {
			return fmt.Errorf("oauth codes need a user token and must be distinct without whitespace")
		}
		s.oauthCodes[code] = token
		return nil
	}
	if err := add(cfg.OAuthCode, cfg.Token); err != nil {
		return err
	}
	for _, user := range cfg.Users {
		if err := add(user.OAuthCode, user.Token); err != nil {
			return err
		}
	}
	return nil
}

// exchangeOAuthCode models the web flow code exchange. GitHub reports failures
// with status 200 and an error field.
func (s *Server) exchangeOAuthCode(r *http.Request, body []byte) apiResponse {
	if r.Method != http.MethodPost {
		return errorResponse(404, "Not Found")
	}
	params, err := oauthParams(r, body)
	if err != nil {
		return errorResponse(400, "Invalid request body")
	}
	asJSON := strings.Contains(r.Header.Get("Accept"), "application/json")
	if !s.validClient(params.Get("client_id"), params.Get("client_secret")) {
		return oauthResponse(asJSON, oauthError("incorrect_client_credentials", "The client_id and/or client_secret passed are incorrect."))
	}
	code := params.Get("code")
	token := s.state.oauthCodes[code]
	if token == "" {
		return oauthResponse(asJSON, oauthError("bad_verification_code", "The code passed is incorrect or expired."))
	}
	delete(s.state.oauthCodes, code)
	return oauthResponse(asJSON, url.Values{"access_token": {token}, "token_type": {"bearer"}, "scope": {""}})
}

func (s *Server) validClient(id, secret string) bool {
	for _, app := range s.state.apps {
		if app.ClientID != "" && app.ClientID == id {
			return app.ClientSecret == secret
		}
	}
	return false
}

// oauthParams merges query parameters with a JSON or form-encoded body.
func oauthParams(r *http.Request, body []byte) (url.Values, error) {
	params := r.URL.Query()
	if len(body) == 0 {
		return params, nil
	}
	var fields url.Values
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType == "application/json" {
		var data map[string]string
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, err
		}
		fields = url.Values{}
		for key, value := range data {
			fields.Set(key, value)
		}
	} else {
		var err error
		if fields, err = url.ParseQuery(string(body)); err != nil {
			return nil, err
		}
	}
	for key, values := range fields {
		params[key] = values
	}
	return params, nil
}

func oauthError(code, description string) url.Values {
	return url.Values{"error": {code}, "error_description": {description}, "error_uri": {oauthErrorURI}}
}

func oauthResponse(asJSON bool, values url.Values) apiResponse {
	if asJSON {
		data := Object{}
		for key := range values {
			data[key] = values.Get(key)
		}
		return jsonResponse(200, data)
	}
	return func(w http.ResponseWriter) int {
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(values.Encode()))
		return 200
	}
}
