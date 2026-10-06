package mockgithub

import (
	"fmt"
	"net/http"
	"strings"
)

func (s *state) loadIdentities(cfg Config) error {
	s.users, s.organizations = map[string]Object{}, map[string]Organization{}
	logins := map[string]bool{strings.ToLower(text(s.user["login"])): true}
	if strings.ContainsAny(cfg.Token, " \t\r\n") || strings.HasPrefix(cfg.Token, "ghs_mock_") {
		return fmt.Errorf("token must not contain whitespace or the ghs_mock_ prefix")
	}
	if cfg.Token != "" {
		s.users[cfg.Token] = s.user
	}
	userIDs := map[int]bool{number(s.user["id"]): true}
	for _, fixture := range cfg.Users {
		login := strings.ToLower(text(fixture.Data["login"]))
		if !validName(login) || logins[login] || number(fixture.Data["id"]) <= 0 || userIDs[number(fixture.Data["id"])] || float64(number(fixture.Data["id"])) != fixture.Data["id"] {
			return fmt.Errorf("users need unique logins and positive integer ids; the default user is configured through user/token")
		}
		if fixture.Token == "" || s.users[fixture.Token] != nil || strings.ContainsAny(fixture.Token, " \t\r\n") || strings.HasPrefix(fixture.Token, "ghs_mock_") {
			return fmt.Errorf("users need distinct nonempty tokens without whitespace or the ghs_mock_ prefix")
		}
		s.users[fixture.Token], logins[login] = fixture.Data, true
		userIDs[number(fixture.Data["id"])] = true
	}
	for token := range s.users {
		for _, app := range s.apps {
			if token == app.Token {
				return fmt.Errorf("user and app tokens must be distinct")
			}
		}
	}
	ids := map[int]bool{}
	for _, org := range cfg.Organizations {
		key := strings.ToLower(org.Login)
		if !validName(org.Login) || org.ID <= 0 || ids[org.ID] || s.organizations[key].Login != "" {
			return fmt.Errorf("organizations need unique logins and positive ids")
		}
		members := map[string]string{}
		for login, role := range org.Members {
			key := strings.ToLower(login)
			if !logins[key] || members[key] != "" || (role != "member" && role != "admin") {
				return fmt.Errorf("organization %q members must reference fixture users with member/admin roles", org.Login)
			}
			members[key] = role
		}
		org.Members = members
		s.organizations[key], ids[org.ID] = org, true
	}
	return nil
}

// requestUser requires an explicit fixture credential, including on workflow
// endpoints when ordinary REST authentication is otherwise optional.
func (s *Server) requestUser(r *http.Request) Object {
	return s.state.users[bearer(r)]
}

func (s *Server) organizationRole(r *http.Request, org Organization) string {
	return org.Members[strings.ToLower(text(s.requestUser(r)["login"]))]
}

func (s *Server) userCanSeeInstallation(r *http.Request, installation Installation) bool {
	if len(s.state.organizations) == 0 {
		return true
	}
	org := s.state.organizations[strings.ToLower(text(installation.Account["login"]))]
	return s.organizationRole(r, org) != ""
}
