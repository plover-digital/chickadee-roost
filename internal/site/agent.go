package site

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Server) agentRoutes() {
	s.mux.HandleFunc("GET /settings/tokens", s.tokenSettings)
	s.mux.HandleFunc("POST /settings/tokens", s.createAgentToken)
	s.mux.HandleFunc("POST /settings/tokens/revoke", s.revokeAgentToken)
	s.mux.HandleFunc("GET /agents.md", s.agentInstructions)
	s.mux.HandleFunc("GET /llms.txt", s.agentIndex)
	s.mux.HandleFunc("GET /openapi.json", s.agentSchema)
	s.mux.HandleFunc("GET /agents", s.agentGuide)
	s.mux.HandleFunc("/api/", s.agentAPI)
}

func (s *Server) tokenPage(w http.ResponseWriter, r *http.Request, v session, raw string) {
	p := page{Title: "Agent access", User: &v.User, CSRF: v.CSRF, AgentAccess: true, NewAgentToken: raw, AgentReady: s.agentVault != nil}
	if s.agentVault != nil {
		p.AgentTokens = s.agentVault.list(v.User.ID)
		var err error
		p.Choices, err = s.choices(r.Context(), v)
		if err != nil {
			p.AgentNotice = "GitHub access could not be verified. Retry or sign in again."
			p.ChoicesUnavailable = true
		}
		if raw != "" {
			createdID := s.createdTokenID(raw)
			for _, t := range p.AgentTokens {
				if t.ID == createdID {
					p.AgentExpires = t.Expires
				}
			}
		}
	}
	sort.Slice(p.AgentTokens, func(i, j int) bool { return p.AgentTokens[i].Created.Before(p.AgentTokens[j].Created) })
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	s.render(w, p)
}
func (s *Server) createdTokenID(raw string) string {
	s.agentVault.mu.Lock()
	defer s.agentVault.mu.Unlock()
	return s.agentVault.state.Tokens[agentDigest(raw)].ID
}
func (s *Server) tokenSettings(w http.ResponseWriter, r *http.Request) {
	_, v, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/login", 303)
		return
	}
	s.tokenPage(w, r, v, "")
}
func (s *Server) createAgentToken(w http.ResponseWriter, r *http.Request) {
	sid, v, ok := s.postSession(w, r)
	if !ok {
		return
	}
	if s.agentVault == nil {
		http.Error(w, "Agent access is not configured", 503)
		return
	}
	scope := r.PostForm.Get("scope")
	name := strings.TrimSpace(r.PostForm.Get("name"))
	days, err := strconv.Atoi(r.PostForm.Get("days"))
	if (scope != "read" && scope != "manage") || len(name) == 0 || len(name) > 80 || strings.ContainsAny(name, "\r\n") || err != nil || (days != 1 && days != 7 && days != 30 && days != 90) {
		http.Error(w, "Choose a name, read/manage scope and a supported expiry", 400)
		return
	}
	choices, err := s.choices(r.Context(), v)
	if err != nil {
		http.Error(w, "GitHub access could not be verified", 502)
		return
	}
	selected := r.PostForm["repository_id"]
	if len(selected) == 0 || len(selected) > 500 {
		http.Error(w, "Select at least one repository", 400)
		return
	}
	ids := []int64{}
	seen := map[int64]bool{}
	for _, raw := range selected {
		id, err := strconv.ParseInt(raw, 10, 64)
		authorized := false
		for _, c := range choices {
			if c.Repository.ID == id {
				authorized = true
				break
			}
		}
		if err != nil || !authorized || seen[id] {
			http.Error(w, "Invalid repository selection", 403)
			return
		}
		ids = append(ids, id)
		seen[id] = true
	}
	raw, err := s.agentVault.create(sid, v, name, scope, ids, time.Duration(days)*24*time.Hour)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	s.tokenPage(w, r, v, raw)
}
func (s *Server) revokeAgentToken(w http.ResponseWriter, r *http.Request) {
	_, v, ok := s.postSession(w, r)
	if !ok {
		return
	}
	if s.agentVault == nil {
		http.Error(w, "Agent access is not configured", 503)
		return
	}
	if s.agentVault.revoke(v.User.ID, r.PostForm.Get("id")) != nil {
		http.Error(w, "Revocation could not be saved; retry", 500)
		return
	}
	http.Redirect(w, r, "/settings/tokens", 303)
}

func apiJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func apiError(w http.ResponseWriter, status int, code, message string) {
	apiJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func operationError(w http.ResponseWriter, r *http.Request, message string, status int) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		codes := map[int]string{400: "invalid_request", 403: "forbidden", 409: "conflict", 500: "save_failed", 502: "github_unavailable", 503: "capacity_limit"}
		apiError(w, status, codes[status], message)
		return
	}
	http.Error(w, message, status)
}
func operationAccepted(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		apiJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "message": "Request saved; applied service state is unchanged until reconciliation.", "status_url": "/api/v1/enrollments"})
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}
func decodeAgentJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		apiError(w, 415, "unsupported_media_type", "Use application/json")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		apiError(w, 400, "invalid_request", "Expected one JSON object with documented fields (maximum 4096 bytes)")
		return false
	}
	return true
}

func (s *Server) agentAPI(w http.ResponseWriter, r *http.Request) {
	// Cookies and query parameters never authenticate the API. Reject browser
	// cross-origin requests even if a caller supplies an Authorization header.
	if origin := r.Header.Get("Origin"); origin != "" && origin != s.cfg.PublicURL {
		apiError(w, 403, "forbidden", "Foreign Origin is not allowed")
		return
	}
	headers := r.Header.Values("Authorization")
	var raw string
	if len(headers) == 1 {
		scheme, token, ok := strings.Cut(headers[0], " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			raw = token
		}
	}
	var credential agentToken
	var v session
	status, message := 401, "Create a token after GitHub sign-in at /settings/tokens"
	if len(raw) == 47 && strings.HasPrefix(raw, "chk_") {
		credential, v, status, message = s.authorizeAgent(r, raw)
	}
	if status != 0 {
		if status == 401 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="chickadee"`)
		}
		code := map[int]string{401: "unauthorized", 502: "github_unavailable", 503: "credentials_unavailable"}[status]
		apiError(w, status, code, message)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && credential.Scope != "manage" {
		apiError(w, 403, "insufficient_scope", "A manage token is required")
		return
	}
	switch r.URL.Path {
	case "/api/v1/me":
		if r.Method != "GET" && r.Method != "HEAD" {
			break
		}
		apiJSON(w, 200, map[string]any{"user": v.User, "scope": credential.Scope, "expires_at": credential.Expires, "name": credential.Name, "repository_ids": credential.RepositoryIDs})
		return
	case "/api/v1/queues":
		if r.Method != "GET" && r.Method != "HEAD" {
			break
		}
		apiJSON(w, 200, map[string]any{"queues": append([]string{"chickadee"}, queueCatalog(s.cfg.MacOSPreview)...), "default_queue": "chickadee"})
		return
	case "/api/v1/repositories":
		if r.Method != "GET" && r.Method != "HEAD" {
			break
		}
		choices, err := s.choices(r.Context(), v)
		if err != nil {
			apiError(w, 502, "github_unavailable", "GitHub access could not be verified")
			return
		}
		type repositoryChoice struct {
			Installation Installation `json:"installation"`
			Repository   Repository   `json:"repository"`
			Notice       string       `json:"enrollment_notice,omitempty"`
		}
		out := []repositoryChoice{}
		s.mu.Lock()
		for _, c := range choices {
			out = append(out, repositoryChoice{c.Installation, c.Repository, agentOrganizationConstraint(s.enrollments, v.User, c)})
		}
		s.mu.Unlock()
		apiJSON(w, 200, map[string]any{"repositories": out, "automatic_activation": s.cfg.AutomaticActivation})
		return
	case "/api/v1/enrollments":
		if r.Method == "GET" || r.Method == "HEAD" {
			s.agentEnrollments(w, r, v)
			return
		}
		if r.Method != "POST" {
			break
		}
		var input struct {
			InstallationID int64    `json:"installation_id"`
			RepositoryID   int64    `json:"repository_id"`
			Queues         []string `json:"queues"`
			WorkflowPath   string   `json:"workflow_path"`
		}
		if !decodeAgentJSON(w, r, &input) {
			return
		}
		r.PostForm = url.Values{"installation_id": {strconv.FormatInt(input.InstallationID, 10)}, "repository_id": {strconv.FormatInt(input.RepositoryID, 10)}, "queue": input.Queues, "workflow_path": {input.WorkflowPath}}
		r.Form = r.PostForm
		s.enrollRequest(w, r, v)
		return
	default:
		if strings.HasPrefix(r.URL.Path, "/api/v1/enrollments/") && strings.HasSuffix(r.URL.Path, "/state") {
			if r.Method != "POST" {
				w.Header().Set("Allow", "POST")
				apiError(w, 405, "method_not_allowed", "Use POST")
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/enrollments/"), "/state")
			if id == "" || strings.Contains(id, "/") || len(id) > 100 {
				apiError(w, 404, "not_found", "Unknown API route")
				return
			}
			var input struct {
				State string `json:"state"`
			}
			if !decodeAgentJSON(w, r, &input) {
				return
			}
			r.PostForm = url.Values{"id": {id}, "action": {input.State}}
			r.Form = r.PostForm
			s.manageRequest(w, r, v)
			return
		}
		apiError(w, 404, "not_found", "Unknown API route")
		return
	}
	allow := "GET, HEAD"
	if r.URL.Path == "/api/v1/enrollments" {
		allow += ", POST"
	}
	w.Header().Set("Allow", allow)
	apiError(w, 405, "method_not_allowed", "Unsupported method for this API route")
}

func (s *Server) agentEnrollments(w http.ResponseWriter, r *http.Request, v session) {
	choices, err := s.choices(r.Context(), v)
	if err != nil {
		apiError(w, 502, "github_unavailable", "GitHub access could not be verified; no current service state is inferred")
		return
	}
	// Explicit customer-facing projection: no operator messages, usage snapshots,
	// authenticated provenance, or another user's enrollments are serialized.
	type enrollmentView struct {
		ID                    string     `json:"id"`
		InstallationID        int64      `json:"installation_id"`
		Repository            Repository `json:"repository"`
		Scope                 string     `json:"scope"`
		Status                string     `json:"status"`
		DesiredState          string     `json:"desired_state"`
		Queues                []string   `json:"queues"`
		EnabledQueues         []string   `json:"enabled_queues"`
		WorkflowPath          string     `json:"workflow_path,omitempty"`
		EnabledWorkflowPath   string     `json:"enabled_workflow_path,omitempty"`
		EnabledWorkflowAccess string     `json:"enabled_workflow_access,omitempty"`
		Updated               time.Time  `json:"updated_at"`
	}
	out := []enrollmentView{}
	s.mu.Lock()
	for _, e := range s.enrollments {
		if !agentEnrollmentAllowed(v, e, s.enrollments) {
			continue
		}
		for _, c := range choices {
			if c.Installation.ID != e.InstallationID || c.Repository.ID != e.Repository.ID {
				continue
			}
			requested := append([]string{}, e.Queues...)
			if len(requested) == 0 {
				requested = []string{"chickadee"}
			}
			out = append(out, enrollmentView{e.ID, e.InstallationID, c.Repository, e.Scope, e.Status, e.DesiredState, requested, append([]string{}, e.EnabledQueues...), e.WorkflowPath, e.EnabledWorkflowPath, e.EnabledWorkflowAccess, e.Updated})
			break
		}
	}
	s.mu.Unlock()
	apiJSON(w, 200, map[string]any{"enrollments": out})
}

func (s *Server) agentInstructions(w http.ResponseWriter, r *http.Request) {
	data, _ := assets.ReadFile("assets/agents.md")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}
func (s *Server) agentGuide(w http.ResponseWriter, r *http.Request) {
	_, v, ok := s.current(r)
	p := page{Title: "Agent API", AgentGuide: true}
	if ok {
		p.User = &v.User
		p.CSRF = v.CSRF
	}
	s.render(w, p)
}

func (s *Server) agentIndex(w http.ResponseWriter, r *http.Request) {
	data, _ := assets.ReadFile("assets/llms.txt")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}
func (s *Server) agentSchema(w http.ResponseWriter, r *http.Request) {
	data, _ := assets.ReadFile("assets/openapi.json")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}

// API v1 delegates exact repositories, not the broader organization-owner
// authority used by browser setup. Explicit desired or applied organization
// policy remains browser-only, including transitions between the two policies.
const agentOrganizationSetupMessage = "Organization-wide service controls require browser owner setup; API v1 supports existing repository services."

func agentEnrollmentAllowed(v session, e Enrollment, entries []Enrollment) bool {
	if e.User.ID != v.User.ID || !v.AgentRepositories[e.Repository.ID] || e.OrganizationAccess != nil || e.AppliedOrganizationAccess != nil {
		return false
	}
	if e.Account.Type == "Organization" {
		current := canonicalOrganization(entries, e.Account.ID)
		if current != nil && (current.OrganizationAccess != nil || current.AppliedOrganizationAccess != nil) {
			return false
		}
	}
	return true
}
func agentOrganizationConstraint(entries []Enrollment, user User, c Choice) string {
	if c.Installation.Account.Type == "Organization" {
		current := canonicalOrganization(entries, c.Installation.Account.ID)
		if current != nil && (current.OrganizationAccess != nil || current.AppliedOrganizationAccess != nil) {
			return agentOrganizationSetupMessage
		}
	}
	return organizationRequestConstraint(entries, user, c, nil)
}
