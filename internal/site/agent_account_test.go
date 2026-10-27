package site

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func createAccountAgent(t *testing.T, s *Server, scope string) string {
	t.Helper()
	got := agentForm(s, "/settings/tokens", url.Values{"name": {"Account agent"}, "scope": {scope}, "days": {"30"}, "repository_access": {"all"}}, "csrf", "https://chickadee.run", "session")
	match := regexp.MustCompile(`id="agent-token">(chk_[A-Za-z0-9_-]+)</code>`).FindStringSubmatch(got.Body.String())
	if got.Code != 200 || len(match) != 2 {
		t.Fatalf("account token creation: %d %s", got.Code, got.Body.String())
	}
	return match[1]
}

func TestAccountAgentCoverageAndPermissions(t *testing.T) {
	for _, scope := range []string{"read", "manage"} {
		t.Run(scope, func(t *testing.T) {
			s, scoped := agentFixture(t)
			account := createAccountAgent(t, s, scope)
			extraAdmin := true
			old := s.http.Transport
			s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
				if strings.HasPrefix(r.URL.Path, "/user/installations/") {
					admin := "false"
					if extraAdmin {
						admin = "true"
					}
					return response(`{"total_count":2,"repositories":[{"id":99,"full_name":"tester/repo","permissions":{"admin":true}},{"id":100,"full_name":"tester/future","permissions":{"admin":` + admin + `}}]}`), nil
				}
				return old.RoundTrip(r)
			})
			for _, token := range []string{account, scoped} {
				got := agentCall(s, token, "GET", "/api/v1/repositories", "")
				if got.Code != 200 || strings.Contains(got.Body.String(), "tester/future") != (token == account) {
					t.Fatalf("dynamic coverage: %d %s", got.Code, got.Body.String())
				}
			}
			var identity struct {
				All          bool    `json:"all_repositories"`
				Repositories []int64 `json:"repository_ids"`
				Scope        string  `json:"scope"`
			}
			json.Unmarshal(agentCall(s, account, "GET", "/api/v1/me", "").Body.Bytes(), &identity)
			if !identity.All || len(identity.Repositories) != 0 || identity.Scope != scope {
				t.Fatal("account identity missing explicit scope")
			}
			got := agentCall(s, account, "POST", "/api/v1/enrollments", `{"installation_id":8,"repository_id":100}`)
			want := 202
			if scope == "read" {
				want = 403
			}
			if got.Code != want {
				t.Fatalf("%s mutation: %d %s", scope, got.Code, got.Body.String())
			}
			extraAdmin = false
			got = agentCall(s, account, "GET", "/api/v1/repositories", "")
			if got.Code != 200 || strings.Contains(got.Body.String(), "tester/future") {
				t.Fatal("lost administration still visible")
			}
			if got = agentCall(s, account, "POST", "/api/v1/enrollments", `{"installation_id":8,"repository_id":100}`); got.Code != 403 {
				t.Fatal("lost administration still writable")
			}
			// Restart retains explicit account scope and existing narrow scope.
			restored, err := newAgentVault(s.cfg.AgentTokenKey, s.cfg.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			s.agentVault = restored
			if !restored.state.Tokens[agentDigest(account)].AllRepositories || restored.state.Tokens[agentDigest(scoped)].AllRepositories {
				t.Fatal("restart changed coverage")
			}
			if err = restored.revoke(7, restored.state.Tokens[agentDigest(account)].ID); err != nil {
				t.Fatal(err)
			}
			if agentCall(s, account, "GET", "/api/v1/me", "").Code != 401 {
				t.Fatal("revoked account token accepted")
			}
			if agentCall(s, scoped, "GET", "/api/v1/me", "").Code != 200 {
				t.Fatal("revocation affected other token")
			}
		})
	}
}

func TestAccountAgentOrganizationAuthority(t *testing.T) {
	for _, scope := range []string{"read", "manage"} {
		t.Run(scope, func(t *testing.T) {
			s, scoped, e := agentOrganizationFixture(t)
			account := createAccountAgent(t, s, scope)
			e.OrganizationAccess = &OrganizationAccess{RepositorySelection: "all"}
			s.enrollments = []Enrollment{e}
			got := agentCall(s, account, "GET", "/api/v1/enrollments", "")
			if got.Code != 200 || !strings.Contains(got.Body.String(), e.ID) || !strings.Contains(got.Body.String(), `"organization_access"`) {
				t.Fatalf("owner read: %d %s", got.Code, got.Body.String())
			}
			if got = agentCall(s, scoped, "GET", "/api/v1/enrollments", ""); strings.Contains(got.Body.String(), e.ID) {
				t.Fatal("scoped token gained organization policy")
			}
			body := `{"installation_id":8,"repository_id":99,"organization_access":{"repository_selection":"selected","repository_ids":[99]},"queues":["chickadee"]}`
			got = agentCall(s, account, "POST", "/api/v1/enrollments", body)
			want := 202
			if scope == "read" {
				want = 403
			}
			if got.Code != want {
				t.Fatalf("policy update: %d %s", got.Code, got.Body.String())
			}
			if got = agentCall(s, scoped, "POST", "/api/v1/enrollments", body); got.Code != 403 {
				t.Fatal("repository token changed organization policy")
			}
			got = agentCall(s, account, "POST", "/api/v1/enrollments/"+e.ID+"/state", `{"state":"paused"}`)
			if got.Code != want {
				t.Fatalf("owner lifecycle: %d %s", got.Code, got.Body.String())
			}
			old := s.http.Transport
			s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/user/memberships/orgs/example-org" {
					return response(`{"state":"active","role":"member","organization":{"id":77},"user":{"id":7}}`), nil
				}
				return old.RoundTrip(r)
			})
			if got = agentCall(s, account, "GET", "/api/v1/enrollments", ""); got.Code != 200 || strings.Contains(got.Body.String(), e.ID) {
				t.Fatal("lost owner can still read organization policy")
			}
			if got = agentCall(s, account, "POST", "/api/v1/enrollments", body); got.Code != 403 {
				t.Fatal("lost owner can still update organization policy")
			}
			// Fresh ownership remains mandatory for shared lifecycle changes.
			got = agentCall(s, account, "POST", "/api/v1/enrollments/"+e.ID+"/state", `{"state":"disconnected"}`)
			if got.Code != 403 {
				t.Fatal("lost owner can still stop organization")
			}
		})
	}
}

func TestAccountAgentSelectionAndLegacyStore(t *testing.T) {
	s, scoped := agentFixture(t)
	for _, coverage := range [][]string{{"unknown"}, {"all", "selected"}} {
		got := agentForm(s, "/settings/tokens", url.Values{"name": {"bad"}, "scope": {"manage"}, "days": {"30"}, "repository_access": coverage, "repository_id": {"99"}}, "csrf", "https://chickadee.run", "session")
		if got.Code != 400 {
			t.Fatal("ambiguous coverage accepted")
		}
	}
	// A version-1 store reads as selected access, and rewrites to version 2.
	s.agentVault.state.Version = 1
	if err := s.agentVault.saveLocked(); err != nil {
		t.Fatal(err)
	}
	restored, err := newAgentVault(s.cfg.AgentTokenKey, s.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if restored.state.Version != 2 || restored.state.Tokens[agentDigest(scoped)].AllRepositories {
		t.Fatal("legacy scope broadened")
	}
	s.agentVault = restored
	// Full account can be granted before the App has any repositories.
	s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		return response(`{"total_count":0,"installations":[]}`), nil
	})
	account := createAccountAgent(t, s, "read")
	if !s.agentVault.state.Tokens[agentDigest(account)].AllRepositories {
		t.Fatal("empty account cannot delegate future repositories")
	}
	// Reject contradictory persisted scope, rather than treating it as all.
	token := s.agentVault.state.Tokens[agentDigest(account)]
	token.RepositoryIDs = []int64{99}
	s.agentVault.state.Tokens[agentDigest(account)] = token
	if err = s.agentVault.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if _, err = newAgentVault(s.cfg.AgentTokenKey, s.cfg.StateDir); err == nil {
		t.Fatal("contradictory metadata accepted")
	}
}

func TestAccountAgentOrganizationSetupAndValidation(t *testing.T) {
	s, scoped, e := agentOrganizationFixture(t)
	token := createAccountAgent(t, s, "manage")
	body := `{"installation_id":8,"repository_id":99,"organization_access":{"repository_selection":"all"}}`
	got := agentCall(s, token, "POST", "/api/v1/enrollments", body)
	if got.Code != 202 || len(s.enrollments) != 1 || s.enrollments[0].OrganizationAccess == nil {
		t.Fatalf("new organization setup: %d %s", got.Code, got.Body.String())
	}
	for _, policy := range []string{`{"repository_selection":"selected","repository_ids":[101]}`, `{"repository_selection":"selected","repository_ids":[99,99]}`, `{"repository_selection":"all","repository_ids":[99]}`, `{"repository_selection":"all","workflows":["attacker/repo/.github/workflows/build.yml@refs/heads/main"]}`, `{"repository_selection":"all","untrusted":true}`} {
		got = agentCall(s, token, "POST", "/api/v1/enrollments", `{"installation_id":8,"repository_id":99,"organization_access":`+policy+`}`)
		if got.Code != 400 && got.Code != 403 {
			t.Fatalf("invalid policy accepted: %d", got.Code)
		}
	}
	// Account-wide ownership covers the canonical service enrolled by another owner,
	// including a former repository anchor no longer in the installation choices.
	e.User = User{9, "former-owner"}
	e.Repository.ID = 101
	e.OrganizationAccess = &OrganizationAccess{RepositorySelection: "all"}
	s.enrollments = []Enrollment{e}
	got = agentCall(s, token, "GET", "/api/v1/enrollments", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), e.ID) {
		t.Fatal("current owner cannot inspect shared canonical service")
	}
	if got = agentCall(s, token, "POST", "/api/v1/enrollments/"+e.ID+"/state", `{"state":"paused"}`); got.Code != 202 {
		t.Fatalf("owner lifecycle handoff: %d %s", got.Code, got.Body.String())
	}
	if got = agentCall(s, scoped, "GET", "/api/v1/enrollments", ""); strings.Contains(got.Body.String(), e.ID) {
		t.Fatal("scoped token acquired owner handoff authority")
	}
}
