package site

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func agentFixture(t *testing.T) (*Server, string) {
	t.Helper()
	s := fixture(t)
	s.cfg.AgentTokenKey = bytes.Repeat([]byte{42}, 32)
	var err error
	s.agentVault, err = newAgentVault(s.cfg.AgentTokenKey, s.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	s.sessions["session"] = session{User: User{7, "tester"}, Token: "ghu_agent_test_secret", CSRF: "csrf", Expires: time.Now().Add(time.Hour), RefreshToken: "ghr_agent_test_secret", GitHubExpires: time.Now().Add(8 * time.Hour), RefreshExpires: time.Now().Add(180 * 24 * time.Hour)}
	s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer ghu_agent_test_secret" {
			t.Error("GitHub credential changed")
		}
		if strings.HasPrefix(r.URL.Path, "/user/installations/") {
			return response(`{"total_count":1,"repositories":[{"id":99,"full_name":"tester/repo","private":true,"permissions":{"admin":true}}]}`), nil
		}
		return response(`{"total_count":1,"installations":[{"id":8,"app_id":42,"account":{"id":7,"login":"tester","type":"User"},"permissions":{"administration":"write"}}]}`), nil
	})
	rec := agentForm(s, "/settings/tokens", url.Values{"name": {"Agent test"}, "scope": {"manage"}, "days": {"30"}, "repository_id": {"99"}}, "csrf", "https://chickadee.run", "session")
	match := regexp.MustCompile(`id="agent-token">(chk_[A-Za-z0-9_-]+)</code>`).FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || len(match) != 2 {
		t.Fatalf("token creation failed: %d %s", rec.Code, rec.Body.String())
	}
	return s, match[1]
}
func agentForm(s *Server, path string, values url.Values, csrf, origin, sid string) *httptest.ResponseRecorder {
	values.Set("csrf", csrf)
	r := httptest.NewRequest("POST", path, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", origin)
	r.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: sid})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}
func agentCall(s *Server, token, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}
func TestAgentCredentialLifecycle(t *testing.T) {
	s, token := agentFixture(t)
	if got := agentCall(s, token, "GET", "/api/v1/me", ""); got.Code != 200 || strings.Contains(got.Body.String(), "ghu_") {
		t.Fatal("identity failed or leaked credential")
	}
	page := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/settings/tokens", nil)
	r.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
	s.ServeHTTP(page, r)
	if strings.Contains(page.Body.String(), token) || strings.Contains(page.Body.String(), "analytics.plover.digital") || strings.Contains(page.Header().Get("Content-Security-Policy"), "analytics.plover.digital") {
		t.Fatal("token redisplayed or third party enabled")
	}
	for _, csrf := range []string{"", "wrong"} {
		if got := agentForm(s, "/settings/tokens", url.Values{"scope": {"manage"}}, csrf, "https://chickadee.run", "session"); got.Code != 403 {
			t.Fatal("creation missing CSRF")
		}
	}
	for _, origin := range []string{"", "null", "https://evil.example"} {
		if got := agentForm(s, "/settings/tokens", url.Values{"scope": {"read"}}, "csrf", origin, "session"); got.Code != 403 {
			t.Fatal("creation accepted foreign Origin")
		}
	}
	credential := s.agentVault.state.Tokens[agentDigest(token)]
	if credential.Name != "Agent test" || len(credential.RepositoryIDs) != 1 || credential.RepositoryIDs[0] != 99 || credential.Expires.Sub(credential.Created) != 30*24*time.Hour {
		t.Fatal("wrong token metadata")
	}
	s.sessions["other"] = session{User: User{9, "other"}, CSRF: "other", Expires: time.Now().Add(time.Hour)}
	agentForm(s, "/settings/tokens/revoke", url.Values{"id": {credential.ID}}, "other", "https://chickadee.run", "other")
	if agentCall(s, token, "GET", "/api/v1/me", "").Code != 200 {
		t.Fatal("another user revoked token")
	}
	agentForm(s, "/logout", url.Values{}, "csrf", "https://chickadee.run", "session")
	if agentCall(s, token, "GET", "/api/v1/me", "").Code != 200 {
		t.Fatal("browser logout revoked independent token")
	}
	restarted, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if agentCall(restarted, token, "GET", "/api/v1/me", "").Code != 200 {
		t.Fatal("token lost on restart")
	}
	if err := restarted.agentVault.revoke(7, credential.ID); err != nil {
		t.Fatal(err)
	}
	restarted, err = New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if agentCall(restarted, token, "GET", "/api/v1/me", "").Code != 401 || len(restarted.agentVault.state.Grants) != 0 {
		t.Fatal("revocation not durable or unused credential retained")
	}
	s, token = agentFixture(t)
	c := s.agentVault.state.Tokens[agentDigest(token)]
	c.Expires = time.Now().Add(-time.Second)
	s.agentVault.state.Tokens[agentDigest(token)] = c
	if agentCall(s, token, "GET", "/api/v1/me", "").Code != 401 {
		t.Fatal("expired token accepted")
	}
}
func TestAgentAuthenticationAndScopes(t *testing.T) {
	s, token := agentFixture(t)
	for _, raw := range []string{"", "invalid", token + "x"} {
		rec := agentCall(s, raw, "GET", "/api/v1/me", "")
		if rec.Code != 401 || !json.Valid(rec.Body.Bytes()) || rec.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("invalid bearer accepted")
		}
	}
	for _, path := range []string{"/api/v1/me", "/api/v1/me?token=" + token} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: "__Host-chickadee-session", Value: "session"})
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		if rec.Code != 401 {
			t.Fatal("cookie or URL authenticated API")
		}
	}
	r := httptest.NewRequest("GET", "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatal("foreign origin accepted")
	}
	digest := agentDigest(token)
	c := s.agentVault.state.Tokens[digest]
	c.Scope = "read"
	s.agentVault.state.Tokens[digest] = c
	if agentCall(s, token, "POST", "/api/v1/enrollments", `{"installation_id":8,"repository_id":99}`).Code != 403 || len(s.enrollments) != 0 {
		t.Fatal("read scope mutated service")
	}
	if agentCall(s, token, "GET", "/api/v1/repositories", "").Code != 200 {
		t.Fatal("read scope could not discover repos")
	}
	if agentCall(s, token, "GET", "/api/v2/me", "").Code != 404 {
		t.Fatal("unknown version accepted")
	}
	if agentCall(s, token, "PUT", "/api/v1/me", "").Code != 403 {
		t.Fatal("read scope accepted mutation method")
	}
}
func TestAgentEnrollmentAndManagement(t *testing.T) {
	s, token := agentFixture(t)
	for _, body := range []string{`{"installation_id":8,"repository_id":100}`, `{"installation_id":9,"repository_id":99}`} {
		if got := agentCall(s, token, "POST", "/api/v1/enrollments", body); got.Code != 403 {
			t.Fatalf("forged repository accepted: %d", got.Code)
		}
	}
	valid := `{"installation_id":8,"repository_id":99,"queues":["chickadee","chickadee-small-ubuntu-2604"]}`
	for i := 0; i < 2; i++ {
		got := agentCall(s, token, "POST", "/api/v1/enrollments", valid)
		if got.Code != 202 || !json.Valid(got.Body.Bytes()) || got.Header().Get("Location") != "" {
			t.Fatalf("enrollment failed: %d %s", got.Code, got.Body.String())
		}
	}
	if len(s.enrollments) != 1 || s.enrollments[0].Status != "pending" || len(s.enrollments[0].EnabledQueues) != 0 || !s.enrollments[0].Authenticated {
		t.Fatal("enrollment claimed applied capacity or duplicated")
	}
	id := s.enrollments[0].ID
	got := agentCall(s, token, "POST", "/api/v1/enrollments/"+id+"/state", `{"state":"paused"}`)
	if got.Code != 202 || s.enrollments[0].Status != "pending" || s.enrollments[0].DesiredState != "paused" {
		t.Fatal("management bypassed reconciliation")
	}
	// Projection omits operator messages, credential material, and other users.
	s.enrollments[0].Message = "private operator message"
	other := s.enrollments[0]
	other.ID = "another-user"
	other.User = User{88, "other"}
	s.enrollments = append(s.enrollments, other)
	got = agentCall(s, token, "GET", "/api/v1/enrollments", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), id) || strings.Contains(got.Body.String(), "another-user") || strings.Contains(got.Body.String(), "private operator") || strings.Contains(got.Body.String(), "authenticated") {
		t.Fatal("enrollment visibility failed")
	}
	if agentCall(s, token, "POST", "/api/v1/enrollments/another-user/state", `{"state":"active"}`).Code != 403 {
		t.Fatal("another user managed")
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.StateDir, "enrollments.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) || strings.Contains(string(data), "ghu_") || strings.Contains(string(data), "csrf") {
		t.Fatal("credential persisted")
	}
	s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/user/installations/") {
			return response(`{"total_count":0,"repositories":[]}`), nil
		}
		return response(`{"total_count":0,"installations":[]}`), nil
	})
	if got := agentCall(s, token, "GET", "/api/v1/enrollments", ""); got.Code != 200 || strings.Contains(got.Body.String(), id) {
		t.Fatal("lost access exposed enrollment")
	}
	if agentCall(s, token, "POST", "/api/v1/enrollments/"+id+"/state", `{"state":"active"}`).Code != 403 {
		t.Fatal("lost access managed service")
	}
	s.http.Transport = transport(func(*http.Request) (*http.Response, error) { return response(`not JSON`), nil })
	if got := agentCall(s, token, "GET", "/api/v1/enrollments", ""); got.Code != 502 || strings.Contains(got.Body.String(), id) {
		t.Fatal("unverified GitHub state returned")
	}
}
func TestAgentRequestLimitsAndDiscovery(t *testing.T) {
	s, token := agentFixture(t)
	for _, body := range []string{`{}`, `null`, `[]`, `{"installation_id":8,"repository_id":99,"authenticated":true}`, `{"installation_id":8,"repository_id":99} {}`, `{"installation_id":8,"repository_id":99,"queues":["unknown"]}`, `{"workflow_path":"` + strings.Repeat("x", 5000) + `"}`} {
		if got := agentCall(s, token, "POST", "/api/v1/enrollments", body); got.Code != 400 || !json.Valid(got.Body.Bytes()) {
			t.Fatalf("bad body accepted: %d", got.Code)
		}
	}
	r := httptest.NewRequest("POST", "/api/v1/enrollments", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != 415 {
		t.Fatal("non-JSON accepted")
	}
	if got := agentCall(s, token, "DELETE", "/api/v1/enrollments", ""); got.Code != 405 || got.Header().Get("Allow") != "GET, HEAD, POST" {
		t.Fatal("method contract failed")
	}
	for _, path := range []string{"/enrollments", "/status", "/telemetry", "/import-enrollment", "/account-usage"} {
		if got := agentCall(s, token, "POST", path, `{}`); got.Code != 404 {
			t.Fatal("private route exposed")
		}
	}
	for _, path := range []string{"/agents", "/llms.txt"} {
		got := agentCall(s, "", "GET", path, "")
		if got.Code != 200 || !strings.Contains(got.Body.String(), "/api/v1") || got.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("guide unavailable: %s", path)
		}
	}
	for i := 1; i < 32; i++ {
		if agentForm(s, "/settings/tokens", url.Values{"name": {"Read agent"}, "scope": {"read"}, "days": {"7"}, "repository_id": {"99"}}, "csrf", "https://chickadee.run", "session").Code != 200 {
			t.Fatal("token limit early")
		}
	}
	if agentForm(s, "/settings/tokens", url.Values{"name": {"Read agent"}, "scope": {"read"}, "days": {"7"}, "repository_id": {"99"}}, "csrf", "https://chickadee.run", "session").Code != 409 {
		t.Fatal("unbounded token creation")
	}
}

func TestAgentRepositoryScopeAndCreationChecks(t *testing.T) {
	s, token := agentFixture(t)
	s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/user/installations/") {
			return response(`{"total_count":2,"repositories":[{"id":99,"full_name":"tester/repo","permissions":{"admin":true}},{"id":100,"full_name":"tester/other","permissions":{"admin":true}}]}`), nil
		}
		return response(`{"total_count":1,"installations":[{"id":8,"app_id":42,"account":{"id":7,"login":"tester","type":"User"},"permissions":{"administration":"write"}}]}`), nil
	})
	rec := agentCall(s, token, "GET", "/api/v1/repositories", "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "tester/other") || !strings.Contains(rec.Body.String(), "tester/repo") {
		t.Fatal("repository scope leaked")
	}
	if got := agentCall(s, token, "POST", "/api/v1/enrollments", `{"installation_id":8,"repository_id":100}`); got.Code != 403 {
		t.Fatal("out-of-scope repository mutated")
	}
	for _, ids := range [][]string{nil, {"101"}, {"99", "99"}} {
		got := agentForm(s, "/settings/tokens", url.Values{"name": {"Agent"}, "scope": {"read"}, "days": {"30"}, "repository_id": ids}, "csrf", "https://chickadee.run", "session")
		if got.Code == 200 {
			t.Fatal("invalid token repository restriction accepted")
		}
	}
	for _, days := range []string{"0", "365", "-1"} {
		if agentForm(s, "/settings/tokens", url.Values{"name": {"Agent"}, "scope": {"read"}, "days": {days}, "repository_id": {"99"}}, "csrf", "https://chickadee.run", "session").Code != 400 {
			t.Fatal("invalid lifetime accepted")
		}
	}
	s.agentVault = nil
	if agentCall(s, token, "GET", "/api/v1/me", "").Code != 503 {
		t.Fatal("unconfigured API accepted token")
	}
}

func TestAgentEncryptedStoreAndRefresh(t *testing.T) {
	s, token := agentFixture(t)
	path := filepath.Join(s.cfg.StateDir, "agent-credentials.enc")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{token, "ghu_agent_test_secret", "ghr_agent_test_secret", "Agent test"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("unencrypted credential store")
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe encrypted file permissions")
	}
	credential := s.agentVault.state.Tokens[agentDigest(token)]
	grant := s.agentVault.state.Grants[credential.GrantID]
	grant.Expires = time.Now().Add(-time.Minute)
	s.agentVault.state.Grants[credential.GrantID] = grant
	calls := 0
	s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "github.com" {
			calls++
			_ = r.ParseForm()
			if r.Method != "POST" || r.FormValue("refresh_token") != "ghr_agent_test_secret" || r.FormValue("client_secret") != "fake-client-secret" || r.FormValue("grant_type") != "refresh_token" || r.URL.RawQuery != "" {
				t.Fatal("invalid refresh exchange")
			}
			return response(`{"access_token":"ghu_rotated","refresh_token":"ghr_rotated","expires_in":28800,"refresh_token_expires_in":15897600}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer ghu_rotated" || r.URL.Path != "/user" {
			t.Fatal("refresh identity not checked")
		}
		return response(`{"id":7,"login":"tester"}`), nil
	})
	for i := 0; i < 2; i++ {
		if got := agentCall(s, token, "GET", "/api/v1/me", ""); got.Code != 200 {
			t.Fatalf("refresh failed %d %s", got.Code, got.Body.String())
		}
	}
	if calls != 1 {
		t.Fatal("refresh replayed")
	}
	restarted, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	saved := restarted.agentVault.state.Grants[credential.GrantID]
	if saved.Token != "ghu_rotated" || saved.RefreshToken != "ghr_rotated" {
		t.Fatal("rotation not persisted")
	}
	// Wrong key and modified ciphertext must fail closed at startup.
	other := s.cfg
	other.AgentTokenKey = bytes.Repeat([]byte{99}, 32)
	if _, err = New(other); err == nil {
		t.Fatal("wrong encryption key accepted")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = New(s.cfg); err == nil {
		t.Fatal("tampered credential store accepted")
	}
}
func TestAgentRefreshFailureAndIdentity(t *testing.T) {
	for _, mode := range []string{"identity", "denied", "save"} {
		t.Run(mode, func(t *testing.T) {
			s, token := agentFixture(t)
			credential := s.agentVault.state.Tokens[agentDigest(token)]
			grant := s.agentVault.state.Grants[credential.GrantID]
			grant.Expires = time.Now().Add(-time.Minute)
			s.agentVault.state.Grants[credential.GrantID] = grant
			calls := 0
			s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "github.com" {
					calls++
					if mode == "denied" {
						res := response(`{"error":"invalid_grant","private":"never-reflect"}`)
						res.StatusCode = 400
						return res, nil
					}
					return response(`{"access_token":"ghu_rotated","refresh_token":"ghr_rotated","expires_in":28800,"refresh_token_expires_in":15897600}`), nil
				}
				if mode == "identity" {
					return response(`{"id":88,"login":"other"}`), nil
				}
				return response(`{"id":7,"login":"tester"}`), nil
			})
			if mode == "save" {
				s.agentVault.path = filepath.Join(s.cfg.StateDir, "absent", "agent.enc")
			}
			got := agentCall(s, token, "GET", "/api/v1/me", "")
			expected := 401
			if mode == "save" {
				expected = 503
			}
			if got.Code != expected || strings.Contains(got.Body.String(), "never-reflect") || strings.Contains(got.Body.String(), "ghu_") {
				t.Fatalf("refresh failed open: %d", got.Code)
			}
			if mode == "save" {
				s.agentVault.path = filepath.Join(s.cfg.StateDir, "agent-credentials.enc")
				if agentCall(s, token, "GET", "/api/v1/me", "").Code != 200 || calls != 1 {
					t.Fatal("consumed refresh replayed after save failure")
				}
			}
		})
	}
}

func TestAgentOAuthDelegationOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			s := fixture(t)
			if enabled {
				s.cfg.AgentTokenKey = bytes.Repeat([]byte{42}, 32)
				var err error
				s.agentVault, err = newAgentVault(s.cfg.AgentTokenKey, s.cfg.StateDir)
				if err != nil {
					t.Fatal(err)
				}
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
			target, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "github.com" {
					return response(`{"access_token":"ghu_delegated","refresh_token":"ghr_delegated","expires_in":28800,"refresh_token_expires_in":15897600}`), nil
				}
				return response(`{"id":7,"login":"tester"}`), nil
			})
			req := httptest.NewRequest("GET", "/auth/github/callback?code=synthetic&state="+url.QueryEscape(target.Query().Get("state")), nil)
			req.AddCookie(rec.Result().Cookies()[0])
			rec = httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != 303 {
				t.Fatal("OAuth callback failed")
			}
			for _, v := range s.sessions {
				if enabled && (v.RefreshToken != "ghr_delegated" || v.GitHubExpires.IsZero() || v.RefreshExpires.IsZero()) {
					t.Fatal("delegation metadata missing")
				}
				if !enabled && (v.RefreshToken != "" || !v.GitHubExpires.IsZero()) {
					t.Fatal("disabled delegation retained refresh token")
				}
			}
			if _, err := os.Stat(filepath.Join(s.cfg.StateDir, "agent-credentials.enc")); !os.IsNotExist(err) {
				t.Fatal("login persisted credentials without explicit token creation")
			}
		})
	}
}

func TestAgentSharedGrantConcurrentRefresh(t *testing.T) {
	s, first := agentFixture(t)
	second, err := s.agentVault.create("session", s.sessions["session"], "Second agent", "read", []int64{99}, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	original := s.agentVault.state.Tokens[agentDigest(first)]
	grant := s.agentVault.state.Grants[original.GrantID]
	grant.Expires = time.Now().Add(-time.Minute)
	s.agentVault.state.Grants[original.GrantID] = grant
	calls := 0
	s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "github.com" {
			calls++
			return response(`{"access_token":"ghu_rotated","refresh_token":"ghr_rotated","expires_in":28800,"refresh_token_expires_in":15897600}`), nil
		}
		return response(`{"id":7,"login":"tester"}`), nil
	})
	results := make(chan int, 16)
	for i := 0; i < 16; i++ {
		go func(i int) {
			token := first
			if i%2 == 1 {
				token = second
			}
			results <- agentCall(s, token, "GET", "/api/v1/me", "").Code
		}(i)
	}
	for i := 0; i < 16; i++ {
		if <-results != 200 {
			t.Fatal("concurrent authentication failed")
		}
	}
	if calls != 1 {
		t.Fatal("shared refresh token consumed more than once")
	}
	if err = s.agentVault.revoke(7, original.ID); err != nil {
		t.Fatal(err)
	}
	if agentCall(s, first, "GET", "/api/v1/me", "").Code != 401 || agentCall(s, second, "GET", "/api/v1/me", "").Code != 200 {
		t.Fatal("individual revocation removed sibling grant or failed")
	}
}
