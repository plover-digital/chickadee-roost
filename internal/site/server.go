// Package site implements a small GitHub App onboarding site, separate from VM control.
package site

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed assets/*
var assets embed.FS
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

type Config struct {
	PublicURL, AppSlug, ClientID, ClientSecret, StateDir string
	AppID                                                int64
	AdminUserID                                          int64
	AgentTokenKey                                        []byte
	AutomaticActivation                                  bool
	MacOSPreview                                         bool
}
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}
type Account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}
type Installation struct {
	ID          int64             `json:"id"`
	AppID       int64             `json:"app_id"`
	Account     Account           `json:"account"`
	Permissions map[string]string `json:"permissions"`
}
type Repository struct {
	ID          int64           `json:"id"`
	Name        string          `json:"full_name"`
	Private     bool            `json:"private"`
	Permissions map[string]bool `json:"permissions"`
}
type Choice struct {
	OrganizationAdmin  bool
	RepositoryOptions  []Repository
	LegacyRepository   Repository
	OrganizationAccess *OrganizationAccess

	Installation          Installation
	Repository            Repository
	SelectedQueues        []string
	WorkflowPath          string
	EnabledWorkflowAccess string
	EnabledWorkflowPath   string
	EnrollmentNotice      string
}

func (c Choice) HasQueue(queue string) bool {
	for _, q := range c.SelectedQueues {
		if q == queue {
			return true
		}
	}
	return false
}

type Enrollment struct {
	OrganizationAccess        *OrganizationAccess `json:"organization_access,omitempty"`
	AppliedOrganizationAccess *OrganizationAccess `json:"applied_organization_access,omitempty"`

	Authenticated         bool       `json:"authenticated,omitempty"`
	ID                    string     `json:"id"`
	User                  User       `json:"user"`
	InstallationID        int64      `json:"installation_id"`
	Account               Account    `json:"account"`
	Repository            Repository `json:"repository"`
	Scope                 string     `json:"scope"`
	Status                string     `json:"status"`
	Queues                []string   `json:"queues,omitempty"`
	EnabledQueues         []string   `json:"enabled_queues,omitempty"`
	DesiredState          string     `json:"desired_state,omitempty"`
	Message               string     `json:"message,omitempty"`
	Updated               time.Time  `json:"updated_at,omitempty"`
	Created               time.Time  `json:"created_at"`
	Usage                 []UsageDay `json:"usage,omitempty"`
	WorkflowPath          string     `json:"workflow_path,omitempty"`
	EnabledWorkflowPath   string     `json:"enabled_workflow_path,omitempty"`
	EnabledWorkflowAccess string     `json:"enabled_workflow_access,omitempty"`
}
type oauthState struct {
	Cookie, Verifier string
	Expires          time.Time
}
type session struct {
	User              User
	Token, CSRF       string
	Expires           time.Time
	RefreshToken      string
	GitHubExpires     time.Time
	RefreshExpires    time.Time
	AgentRepositories map[int64]bool
}
type Server struct {
	cfg          Config
	templates    *template.Template
	mux          *http.ServeMux
	http         *http.Client
	api, oauth   string
	mu           sync.Mutex
	states       map[string]oauthState
	sessions     map[string]session
	agentVault   *agentVault
	enrollments  []Enrollment
	telemetry    []FleetSample
	accountUsage []AccountUsage
}
type page struct {
	PublicURL            string
	AgentGuide           bool
	AgentAccess          bool
	AgentTokens          []agentToken
	NewAgentToken        string
	AgentExpires         time.Time
	AgentReady           bool
	AgentNotice          string
	Title, Message, CSRF string
	User                 *User
	Choices              []Choice
	ChoicesUnavailable   bool
	Enrollments          []Enrollment
	ExtraQueues          []string
	LoginReady           bool
	IsAdmin              bool
	AccountUsage         []AccountUsage
	AutomaticActivation  bool
	MacOSPreview         bool
}

func New(c Config) (*Server, error) {
	u, e := url.Parse(c.PublicURL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, errors.New("public URL must be an origin")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
		return nil, errors.New("public URL requires HTTPS")
	}
	if !slugPattern.MatchString(c.AppSlug) || !filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) != c.StateDir || c.StateDir == "/" {
		return nil, errors.New("valid App slug and dedicated absolute state directory required")
	}
	if c.ClientSecret != "" && (c.ClientID == "" || c.AppID <= 0) {
		return nil, errors.New("OAuth requires App/client identity")
	}
	if e = os.MkdirAll(c.StateDir, 0700); e != nil {
		return nil, e
	}
	st, e := os.Lstat(c.StateDir)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("site state directory must be private and not a symlink")
	}
	t, e := template.ParseFS(assets, "assets/*.html")
	if e != nil {
		return nil, e
	}
	s := &Server{cfg: c, templates: t, mux: http.NewServeMux(), http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, api: "https://api.github.com", oauth: "https://github.com", states: map[string]oauthState{}, sessions: map[string]session{}}
	b, e := os.ReadFile(filepath.Join(c.StateDir, "enrollments.json"))
	if e == nil {
		if len(b) > 2<<20 || json.Unmarshal(b, &s.enrollments) != nil || len(s.enrollments) > 500 {
			return nil, errors.New("invalid enrollment store")
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	for _, entry := range s.enrollments {
		if !validOrganizationAccess(entry.OrganizationAccess) || !validOrganizationAccess(entry.AppliedOrganizationAccess) || entry.Account.Type != "Organization" && (entry.OrganizationAccess != nil || entry.AppliedOrganizationAccess != nil) {
			return nil, errors.New("invalid stored organization access")
		}
	}
	s.mux.HandleFunc("GET /{$}", s.home)
	s.mux.HandleFunc("GET /login", s.login)
	s.mux.HandleFunc("GET /auth/github/callback", s.callback)
	s.mux.HandleFunc("GET /install", s.install)
	s.mux.HandleFunc("GET /setup", s.setup)
	if c.AdminUserID < 0 {
		return nil, errors.New("invalid admin user ID")
	}
	if e = s.loadTelemetry(); e != nil {
		return nil, e
	}
	if e = s.loadAccountUsage(); e != nil {
		return nil, e
	}
	s.mux.HandleFunc("GET /dashboard/admin", s.adminDashboard)
	s.mux.HandleFunc("GET /dashboard", s.dashboard)
	if len(c.AgentTokenKey) != 0 {
		if !s.ready() {
			return nil, errors.New("agent access requires configured GitHub OAuth")
		}
		s.agentVault, e = newAgentVault(c.AgentTokenKey, c.StateDir)
		if e != nil {
			return nil, e
		}
	}
	s.agentRoutes()
	s.mux.HandleFunc("POST /enroll", s.enroll)
	s.mux.HandleFunc("POST /logout", s.logout)
	s.mux.HandleFunc("POST /manage", s.manage)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	files, _ := fs.Sub(assets, "assets")
	s.mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(files))))
	return s, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' https://analytics.plover.digital; connect-src 'self' https://analytics.plover.digital; style-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Link", `</llms.txt>; rel="describedby"`)
	s.mux.ServeHTTP(w, r)
}
func random() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("random source unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func (s *Server) ready() bool { return s.cfg.ClientSecret != "" }
func (s *Server) cookieName(name string) string {
	if strings.HasPrefix(s.cfg.PublicURL, "https://") {
		return "__Host-" + name
	}
	return name
}
func (s *Server) cookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(name), Value: value, Path: "/", MaxAge: age, HttpOnly: true, Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"), SameSite: http.SameSiteLaxMode})
}
func (s *Server) pruneLocked() {
	now := time.Now()
	for k, v := range s.states {
		if now.After(v.Expires) {
			delete(s.states, k)
		}
	}
	for k, v := range s.sessions {
		if now.After(v.Expires) {
			delete(s.sessions, k)
		}
	}
}
func (s *Server) current(r *http.Request) (string, session, bool) {
	c, e := r.Cookie(s.cookieName("chickadee-session"))
	if e != nil {
		return "", session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	v, ok := s.sessions[c.Value]
	return c.Value, v, ok
}
func (s *Server) render(w http.ResponseWriter, p page) {
	p.PublicURL = s.cfg.PublicURL
	p.IsAdmin = p.User != nil && s.cfg.AdminUserID > 0 && p.User.ID == s.cfg.AdminUserID
	p.LoginReady = s.ready()
	p.AutomaticActivation = s.cfg.AutomaticActivation
	p.MacOSPreview = s.cfg.MacOSPreview
	var b bytes.Buffer
	if e := s.templates.ExecuteTemplate(&b, "page.html", p); e != nil {
		http.Error(w, "Page unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	_, v, ok := s.current(r)
	p := page{Title: "Ephemeral GitHub Actions runners"}
	if ok {
		p.User = &v.User
		p.CSRF = v.CSRF
	}
	s.render(w, p)
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.ready() {
		s.render(w, page{Title: "GitHub sign-in", Message: "GitHub sign-in is being configured. You can install the App when GitHub makes it publicly available; runner activation is a separate beta step."})
		return
	}
	state, nonce, verifier := random(), random(), random()
	s.mu.Lock()
	s.pruneLocked()
	if len(s.states) >= 1024 {
		s.mu.Unlock()
		http.Error(w, "Try again shortly", 503)
		return
	}
	s.states[state] = oauthState{nonce, verifier, time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	s.cookie(w, "chickadee-oauth", nonce, 600)
	digest := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {s.cfg.ClientID}, "redirect_uri": {s.cfg.PublicURL + "/auth/github/callback"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, s.oauth+"/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}
func (s *Server) fetch(ctx context.Context, path, token string, out any) error {
	req, e := http.NewRequestWithContext(ctx, "GET", s.api+path, nil)
	if e != nil {
		return errors.New("GitHub request unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "chickadee-onboarding")
	res, e := s.http.Do(req)
	if e != nil {
		return errors.New("GitHub unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode == 429 || res.StatusCode == 403 && (res.Header.Get("Retry-After") != "" || res.Header.Get("X-RateLimit-Remaining") == "0") {
		return errors.New("GitHub verification temporarily rate limited")
	}
	if res.StatusCode != 200 {
		if res.StatusCode == 403 {
			var problem struct {
				Message string `json:"message"`
			}
			if json.NewDecoder(io.LimitReader(res.Body, 1024)).Decode(&problem) == nil && strings.Contains(strings.ToLower(problem.Message), "rate limit") {
				return errors.New("GitHub verification temporarily rate limited")
			}
		}
		return githubStatusError(res.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if e != nil || len(b) > 2<<20 || json.Unmarshal(b, out) != nil {
		return errors.New("GitHub response invalid")
	}
	return nil
}
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	cookie, e := r.Cookie(s.cookieName("chickadee-oauth"))
	s.mu.Lock()
	s.pruneLocked()
	flow, ok := s.states[state]
	delete(s.states, state)
	s.mu.Unlock()
	s.cookie(w, "chickadee-oauth", "", -1)
	if !s.ready() || !ok || e != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(flow.Cookie)) != 1 || code == "" || len(code) > 1024 {
		http.Error(w, "Sign-in expired or invalid. Start again from the website.", 400)
		return
	}
	data := url.Values{"client_id": {s.cfg.ClientID}, "client_secret": {s.cfg.ClientSecret}, "code": {code}, "redirect_uri": {s.cfg.PublicURL + "/auth/github/callback"}, "code_verifier": {flow.Verifier}}
	req, _ := http.NewRequestWithContext(r.Context(), "POST", s.oauth+"/login/oauth/access_token", strings.NewReader(data.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, e := s.http.Do(req)
	if e != nil {
		http.Error(w, "GitHub sign-in unavailable", 502)
		return
	}
	defer res.Body.Close()
	var result struct {
		Token            string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
		Error            string `json:"error"`
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, 65537))
	if e != nil || res.StatusCode != 200 || len(b) > 65536 || json.Unmarshal(b, &result) != nil || result.Token == "" || result.Error != "" {
		http.Error(w, "GitHub sign-in failed. Start again.", 502)
		return
	}
	var user User
	if s.fetch(r.Context(), "/user", result.Token, &user) != nil || user.ID <= 0 || user.Login == "" {
		http.Error(w, "GitHub identity unavailable", 502)
		return
	}
	sid := random()
	s.mu.Lock()
	s.pruneLocked()
	if len(s.sessions) >= 1024 {
		s.mu.Unlock()
		http.Error(w, "Try again shortly", 503)
		return
	}
	v := session{User: user, Token: result.Token, CSRF: random(), Expires: time.Now().Add(time.Hour)}
	if s.agentVault != nil && result.RefreshToken != "" && result.ExpiresIn > 0 && result.ExpiresIn <= 86400 && result.RefreshExpiresIn > 0 && result.RefreshExpiresIn <= 366*86400 {
		v.RefreshToken = result.RefreshToken
		v.GitHubExpires = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
		v.RefreshExpires = time.Now().Add(time.Duration(result.RefreshExpiresIn) * time.Second)
	}
	s.sessions[sid] = v
	s.mu.Unlock()
	s.cookie(w, "chickadee-session", sid, 3600)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}
func (s *Server) install(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "https://github.com/apps/"+s.cfg.AppSlug+"/installations/new", http.StatusFound)
}
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	// A callback query is not proof of installation or ownership. The dashboard
	// checks installation access through GitHub using the authenticated user token.
	if _, _, ok := s.current(r); ok {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.render(w, page{Title: "Connect your repositories", Message: "Continue with GitHub to verify your identity and see the repositories available to this App. Installing the App does not automatically activate a runner pool during the beta."})
}
func (s *Server) choices(ctx context.Context, v session) ([]Choice, error) {
	var list struct {
		Total         int            `json:"total_count"`
		Installations []Installation `json:"installations"`
	}
	if e := s.fetch(ctx, "/user/installations?per_page=100", v.Token, &list); e != nil {
		return nil, e
	}
	if list.Total > 100 {
		return nil, errors.New("installation list exceeds beta limit")
	}
	var out []Choice
	for _, i := range list.Installations {
		if i.AppID != s.cfg.AppID || i.ID <= 0 || i.Account.ID <= 0 || (i.Account.Type != "User" && i.Account.Type != "Organization") || (i.Account.Type == "User" && i.Account.ID != v.User.ID) {
			continue
		}
		var repos struct {
			Total        int          `json:"total_count"`
			Repositories []Repository `json:"repositories"`
		}
		if e := s.fetch(ctx, fmt.Sprintf("/user/installations/%d/repositories?per_page=100", i.ID), v.Token, &repos); e != nil {
			return nil, e
		}
		if repos.Total > 100 || len(out)+len(repos.Repositories) > 500 {
			return nil, errors.New("repository list exceeds beta limit")
		}
		orgAdmin := false
		if i.Account.Type == "Organization" && (i.Permissions["members"] == "read" || i.Permissions["members"] == "write") {
			var err error
			orgAdmin, err = s.organizationAdmin(ctx, v, i.Account)
			if err != nil {
				return nil, err
			}
		}
		for _, repo := range repos.Repositories {
			if repo.ID > 0 && repo.Permissions["admin"] && (v.AgentRepositories == nil || v.AgentRepositories[repo.ID]) {
				out = append(out, Choice{Installation: i, Repository: repo, OrganizationAdmin: orgAdmin, RepositoryOptions: repos.Repositories})
			}
		}
	}
	return out, nil
}
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	_, v, ok := s.current(r)
	if !ok {
		http.Redirect(w, r, "/login", 303)
		return
	}
	choices, e := s.choices(r.Context(), v)
	p := page{Title: "Your repositories", User: &v.User, CSRF: v.CSRF, Choices: choices, ChoicesUnavailable: e != nil, ExtraQueues: queueCatalog(s.cfg.MacOSPreview), MacOSPreview: s.cfg.MacOSPreview}
	if e != nil {
		p.Message = "GitHub access could not be verified. Sign in again, or check your App installation permissions."
	}
	s.mu.Lock()
	for i := range p.Choices {
		p.Choices[i].EnrollmentNotice = organizationConstraint(s.enrollments, v.User, p.Choices[i])
		for _, entry := range s.enrollments {
			if p.Choices[i].OrganizationAdmin {
				current := canonicalOrganization(s.enrollments, p.Choices[i].Installation.Account.ID)
				if current != nil && entry.ID != current.ID {
					continue
				}
			}
			if (entry.User.ID == v.User.ID || p.Choices[i].OrganizationAdmin && entry.Account.ID == p.Choices[i].Installation.Account.ID) && entry.InstallationID == p.Choices[i].Installation.ID && (entry.Repository.ID == p.Choices[i].Repository.ID || p.Choices[i].OrganizationAdmin && entry.Account.ID == p.Choices[i].Installation.Account.ID) {
				p.Choices[i].SelectedQueues = append([]string(nil), entry.Queues...)
				if len(p.Choices[i].SelectedQueues) == 0 {
					p.Choices[i].SelectedQueues = []string{"chickadee"}
				}
				p.Choices[i].WorkflowPath = entry.WorkflowPath
				p.Choices[i].EnabledWorkflowAccess = entry.EnabledWorkflowAccess
				p.Choices[i].EnabledWorkflowPath = entry.EnabledWorkflowPath
				p.Choices[i].OrganizationAccess = entry.OrganizationAccess
				p.Choices[i].LegacyRepository = entry.Repository
				break
			}
		}
		if p.Choices[i].Installation.Account.Type == "Organization" && !p.Choices[i].OrganizationAdmin && len(p.Choices[i].SelectedQueues) == 0 && p.Choices[i].EnrollmentNotice == "" {
			p.Choices[i].EnrollmentNotice = "Organization setup requires an organization owner and the App’s Members read permission. Approve that permission on GitHub and sign in as an owner."
		}
	}
	for _, entry := range s.enrollments {
		if entry.User.ID == v.User.ID || organizationOwner(choices, entry) {
			authorized := false
			if e == nil {
				for _, choice := range choices {
					if choice.authorizes(entry) {
						authorized = true
						break
					}
				}
			}
			disconnected := entry.DesiredState == "disconnected" || entry.Status == "disconnected"
			if !authorized && e == nil && disconnected {
				continue
			}
			if !authorized {
				entry.Usage = nil
				entry.EnabledQueues = nil
				entry.EnabledWorkflowAccess = ""
				entry.EnabledWorkflowPath = ""
				if disconnected {
					entry.Status = "disconnected"
					entry.Message = "GitHub access could not be verified. This disconnected service remains stored for cleanup; no runner availability is inferred."
				} else {
					entry.Status = "permission-required"
					if e != nil {
						entry.Message = "GitHub verification is unavailable. Retry the lookup or sign in again; the previous service state has not been rechecked."
					} else {
						entry.Message = "Current GitHub administration access could not be verified. Restore access or sign in again to view usage and manage queues."
					}
				}
			}
			p.Enrollments = append(p.Enrollments, entry)
		}
	}
	if e == nil {
		p.AccountUsage = append([]AccountUsage(nil), s.accountUsage...)
	}
	s.mu.Unlock()
	s.render(w, p)
}
func (s *Server) postSession(w http.ResponseWriter, r *http.Request) (string, session, bool) {
	sid, v, ok := s.current(r)
	if !ok {
		http.Error(w, "Sign in again", 401)
		return "", v, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, postBodyLimit(r.URL.Path))
	if r.ParseForm() != nil || subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(v.CSRF)) != 1 || r.Header.Get("Origin") != s.cfg.PublicURL {
		http.Error(w, "Invalid request", 403)
		return "", v, false
	}
	return sid, v, true
}

var extraQueues = []string{"chickadee-small-rocky-102", "chickadee-medium-rocky-102", "chickadee-small-ubuntu-2404", "chickadee-medium-ubuntu-2404", "chickadee-small-ubuntu-2604", "chickadee-medium-ubuntu-2604"}

func requestedQueues(values []string) ([]string, error) {
	return requestedQueuesFor(values, false)
}
func queueCatalog(mac bool) []string {
	result := append([]string(nil), extraQueues...)
	if mac {
		result = append(result, "chickadee-small-macos-26")
	}
	return result
}
func knownQueue(q string) bool {
	if q == "chickadee" || q == "chickadee-small-macos-26" {
		return true
	}
	for _, label := range extraQueues {
		if q == label {
			return true
		}
	}
	return false
}
func requestedQueuesFor(values []string, mac bool) ([]string, error) {
	catalog := queueCatalog(mac)
	if len(values) > len(catalog)+1 {
		return nil, errors.New("too many queues")
	}
	queues := []string{"chickadee"}
	for _, q := range values {
		if q == "chickadee" {
			continue
		}
		valid := false
		for _, allowed := range catalog {
			if q == allowed {
				valid = true
				break
			}
		}
		if !valid {
			return nil, errors.New("unknown queue")
		}
		duplicate := false
		for _, existing := range queues {
			if existing == q {
				duplicate = true
				break
			}
		}
		if !duplicate {
			queues = append(queues, q)
		}
	}
	return queues, nil
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	_, v, ok := s.postSession(w, r)
	if !ok {
		return
	}
	s.enrollRequest(w, r, v)
}

func (s *Server) enrollRequest(w http.ResponseWriter, r *http.Request, v session) {
	queues, err := requestedQueuesFor(r.PostForm["queue"], s.cfg.MacOSPreview)
	if err != nil {
		operationError(w, r, "Invalid queue selection", 400)
		return
	}
	iid, e1 := strconv.ParseInt(r.FormValue("installation_id"), 10, 64)
	rid, e2 := strconv.ParseInt(r.FormValue("repository_id"), 10, 64)
	if e1 != nil || e2 != nil || iid <= 0 || rid <= 0 {
		operationError(w, r, "Invalid repository selection", 400)
		return
	}
	choices, e := s.choices(r.Context(), v)
	if e != nil {
		operationError(w, r, "GitHub access could not be verified", 502)
		return
	}
	var choice *Choice
	for _, c := range choices {
		if c.Installation.ID == iid && c.Repository.ID == rid {
			copy := c
			choice = &copy
			break
		}
	}
	if choice == nil {
		operationError(w, r, "Repository not authorized", 403)
		return
	}
	access, accessErr := requestedOrganizationAccess(r.PostForm, *choice)
	if accessErr != nil {
		operationError(w, r, accessErr.Error(), http.StatusForbidden)
		return
	}
	s.mu.Lock()
	constraint := organizationRequestConstraint(s.enrollments, v.User, *choice, access)
	if v.AgentRepositories != nil {
		constraint = agentOrganizationConstraint(s.enrollments, v.User, *choice)
	}
	s.mu.Unlock()
	if constraint != "" {
		status := http.StatusConflict
		if constraint == organizationOwnerSetupMessage || constraint == agentOrganizationSetupMessage {
			status = http.StatusForbidden
		}
		operationError(w, r, constraint, status)
		return
	}
	workflowPath := strings.TrimSpace(r.FormValue("workflow_path"))
	repositoryAccess := false
	s.mu.Lock()
	for _, entry := range s.enrollments {
		if entry.User.ID == v.User.ID && entry.InstallationID == iid && entry.Repository.ID == rid && entry.EnabledWorkflowAccess == "repository" {
			repositoryAccess = true
			break
		}
	}
	s.mu.Unlock()
	if access == nil && choice.Installation.Account.Type == "Organization" && !repositoryAccess && !s.cfg.AutomaticActivation && !validWorkflowPath(workflowPath) || workflowPath != "" && !validWorkflowPath(workflowPath) {
		operationError(w, r, "Enter an exact main-branch workflow path, such as .github/workflows/build.yml.", 400)
		return
	}
	permissions := choice.Installation.Permissions
	if choice.Installation.Account.Type == "User" && permissions["administration"] != "write" || choice.Installation.Account.Type == "Organization" && permissions["organization_self_hosted_runners"] != "write" {
		operationError(w, r, "App runner-management permission needs approval", 403)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	constraint = organizationRequestConstraint(s.enrollments, v.User, *choice, access)
	if v.AgentRepositories != nil {
		constraint = agentOrganizationConstraint(s.enrollments, v.User, *choice)
	}
	if constraint != "" {
		status := http.StatusConflict
		if constraint == organizationOwnerSetupMessage || constraint == agentOrganizationSetupMessage {
			status = http.StatusForbidden
		}
		operationError(w, r, constraint, status)
		return
	}
	for i, entry := range s.enrollments {
		if access != nil {
			current := canonicalOrganization(s.enrollments, choice.Installation.Account.ID)
			if current != nil && current.ID != entry.ID {
				continue
			}
		}
		if entry.InstallationID == iid && (entry.Repository.ID == rid || access != nil && entry.Account.ID == choice.Installation.Account.ID) && (entry.User.ID == v.User.ID || access != nil && choice.OrganizationAdmin) {
			if v.AgentRepositories != nil && !agentEnrollmentAllowed(v, entry, s.enrollments) {
				operationError(w, r, agentOrganizationSetupMessage, http.StatusForbidden)
				return
			}
			if access != nil && len(access.Workflows) == 0 && entry.OrganizationAccess == nil && entry.EnabledWorkflowAccess != "repository" && entry.WorkflowPath == "" && entry.EnabledWorkflowPath == "" && r.FormValue("confirm_all_workflows") != "1" {
				operationError(w, r, "Existing workflow access is unknown. Enter allowed workflow references or explicitly confirm all workflows.", 400)
				return
			}
			entries := append([]Enrollment(nil), s.enrollments...)
			entries[i].Authenticated = true
			if entry.OrganizationAccess != nil && (access == nil || !choice.OrganizationAdmin) {
				operationError(w, r, "Organization owner verification is required to change this service", 403)
				return
			}
			entries[i].OrganizationAccess = access
			if access != nil {
				entries[i].User = v.User
			}
			if access != nil {
				entries[i].Repository = choice.Repository
				entries[i].WorkflowPath = ""
			}
			entries[i].Queues = queues
			if access == nil {
				entries[i].WorkflowPath = workflowPath
			}
			entries[i].Updated = time.Now().UTC()
			if s.cfg.AutomaticActivation && entries[i].Status == "pending" {
				entries[i].Status = "approved"
			}
			if access != nil {
				entries = replaceOrganizationEnrollment(entries, entries[i])
			}
			if e = s.saveLocked(entries); e != nil {
				operationError(w, r, "Request could not be saved", 500)
				return
			}
			s.enrollments = entries
			operationAccepted(w, r)
			return
		}
	}
	if len(s.enrollments) >= 500 {
		operationError(w, r, "Beta capacity reached", 503)
		return
	}
	scope := "repository"
	if choice.Installation.Account.Type == "Organization" {
		scope = "organization"
	}
	initialStatus := "pending"
	if s.cfg.AutomaticActivation {
		initialStatus = "approved"
	}
	entries := append(append([]Enrollment(nil), s.enrollments...), Enrollment{OrganizationAccess: access, ID: random(), User: v.User, Authenticated: true, InstallationID: iid, Account: choice.Installation.Account, Repository: choice.Repository, Scope: scope, Status: initialStatus, Queues: queues, WorkflowPath: workflowPath, DesiredState: "active", Created: time.Now().UTC()})
	if e = s.saveLocked(entries); e != nil {
		operationError(w, r, "Request could not be saved", 500)
		return
	}
	s.enrollments = entries
	operationAccepted(w, r)
}
func (s *Server) saveLocked(entries []Enrollment) error {
	b, e := json.MarshalIndent(entries, "", "  ")
	if e != nil {
		return e
	}
	if len(b) > 2<<20 {
		return errors.New("enrollment store exceeds limit")
	}
	f, e := os.CreateTemp(s.cfg.StateDir, ".enrollments-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(f.Name(), filepath.Join(s.cfg.StateDir, "enrollments.json")); e != nil {
		return e
	}
	dir, e := os.Open(s.cfg.StateDir)
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	sid, _, ok := s.postSession(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	delete(s.sessions, sid)
	s.mu.Unlock()
	s.cookie(w, "chickadee-session", "", -1)
	http.Redirect(w, r, "/", 303)
}

// Organization enrollment has bounded repository IDs and workflow refs. URL
// encoding those validated fields needs more room than the small manage forms.
func postBodyLimit(path string) int64 {
	if path == "/settings/tokens" {
		return 16 << 10 // bounded token selection of up to 500 repository IDs
	}
	if path == "/enroll" {
		return 64 << 10
	}
	return 4096
}
