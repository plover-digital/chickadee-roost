package site

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const agentStoreVersion = 1
const maxAgentTokens = 4096

type agentToken struct {
	ID            string    `json:"id"`
	GrantID       string    `json:"grant_id"`
	UserID        int64     `json:"user_id"`
	Name          string    `json:"name"`
	Scope         string    `json:"scope"`
	RepositoryIDs []int64   `json:"repository_ids"`
	Created       time.Time `json:"created_at"`
	Expires       time.Time `json:"expires_at"`
}
type agentGrant struct {
	User            User      `json:"user"`
	Token           string    `json:"access_token"`
	RefreshToken    string    `json:"refresh_token"`
	Expires         time.Time `json:"expires_at"`
	RefreshExpires  time.Time `json:"refresh_expires_at"`
	IdentityPending bool      `json:"identity_pending,omitempty"`
}
type agentState struct {
	Version int                   `json:"version"`
	Tokens  map[string]agentToken `json:"tokens"`
	Grants  map[string]agentGrant `json:"grants"`
}
type agentVault struct {
	mu    sync.Mutex
	state agentState
	aead  cipher.AEAD
	path  string
	dirty bool
}

func agentDigest(raw string) string {
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:])
}
func newAgentVault(key []byte, dir string) (*agentVault, error) {
	if len(key) != 32 {
		return nil, errors.New("agent token key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	v := &agentVault{state: agentState{Version: agentStoreVersion, Tokens: map[string]agentToken{}, Grants: map[string]agentGrant{}}, aead: aead, path: filepath.Join(dir, "agent-credentials.enc")}
	info, err := os.Lstat(v.path)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil || !ownedUsageFile(info) || info.Size() > 8<<20 {
		return nil, errors.New("agent credential store must be a bounded private regular file")
	}
	data, err := os.ReadFile(v.path)
	if err != nil {
		return nil, errors.New("agent credential store unavailable")
	}
	if len(data) < aead.NonceSize() {
		return nil, errors.New("invalid agent credential store")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte("chickadee-agent-v1"))
	if err != nil || json.Unmarshal(plain, &v.state) != nil || v.state.Version != agentStoreVersion || v.state.Tokens == nil || v.state.Grants == nil || len(v.state.Tokens) > maxAgentTokens || len(v.state.Grants) > maxAgentTokens {
		return nil, errors.New("agent credential store cannot be decrypted or has an unsupported format")
	}
	for digest, t := range v.state.Tokens {
		if len(digest) != 64 || t.UserID <= 0 || t.ID == "" || t.Name == "" || (t.Scope != "read" && t.Scope != "manage") || len(t.RepositoryIDs) == 0 || len(t.RepositoryIDs) > 500 || t.Expires.IsZero() || t.Created.IsZero() || t.Expires.After(t.Created.Add(90*24*time.Hour)) {
			return nil, errors.New("invalid agent token metadata")
		}
		grant, ok := v.state.Grants[t.GrantID]
		if !ok || grant.User.ID != t.UserID {
			return nil, errors.New("invalid agent grant identity")
		}
	}
	v.pruneLocked()
	return v, nil
}
func (v *agentVault) pruneLocked() {
	used := map[string]bool{}
	for hash, t := range v.state.Tokens {
		if !time.Now().Before(t.Expires) {
			delete(v.state.Tokens, hash)
			v.dirty = true
			continue
		}
		used[t.GrantID] = true
	}
	for id := range v.state.Grants {
		if !used[id] {
			delete(v.state.Grants, id)
			v.dirty = true
		}
	}
}
func (v *agentVault) saveLocked() error {
	plain, err := json.Marshal(v.state)
	if err != nil {
		return err
	}
	if len(plain) > 8<<20-v.aead.NonceSize()-v.aead.Overhead() {
		return errors.New("agent credential store full")
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	encrypted := v.aead.Seal(nonce, nonce, plain, []byte("chickadee-agent-v1"))
	f, err := os.CreateTemp(filepath.Dir(v.path), ".agent-credentials-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(encrypted); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), v.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(v.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return err
	}
	v.dirty = false
	return nil
}
func (v *agentVault) list(userID int64) []agentToken {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pruneLocked()
	out := []agentToken{}
	for _, t := range v.state.Tokens {
		if t.UserID == userID {
			out = append(out, t)
		}
	}
	return out
}
func (v *agentVault) create(sid string, user session, name, scope string, repositories []int64, lifetime time.Duration) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pruneLocked()
	count := 0
	for _, t := range v.state.Tokens {
		if t.UserID == user.User.ID {
			count++
		}
	}
	if count >= 32 || len(v.state.Tokens) >= maxAgentTokens {
		return "", errors.New("Revoke an existing token before creating another (32 per account)")
	}
	if user.RefreshToken == "" || !time.Now().Before(user.GitHubExpires) || !time.Now().Add(lifetime).Before(user.RefreshExpires) {
		return "", errors.New("Sign in with GitHub again to authorize refreshable agent access")
	}
	grantID := agentDigest(sid)
	raw := "chk_" + random()
	now := time.Now().UTC()
	token := agentToken{ID: random(), GrantID: grantID, UserID: user.User.ID, Name: name, Scope: scope, RepositoryIDs: repositories, Created: now, Expires: now.Add(lifetime)}
	_, existed := v.state.Grants[grantID]
	if !existed {
		v.state.Grants[grantID] = agentGrant{User: user.User, Token: user.Token, RefreshToken: user.RefreshToken, Expires: user.GitHubExpires, RefreshExpires: user.RefreshExpires}
	}
	v.state.Tokens[agentDigest(raw)] = token
	v.dirty = true
	if err := v.saveLocked(); err != nil {
		delete(v.state.Tokens, agentDigest(raw))
		if !existed {
			delete(v.state.Grants, grantID)
		}
		return "", errors.New("Token could not be saved")
	}
	return raw, nil
}
func (v *agentVault) revoke(userID int64, id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	for digest, t := range v.state.Tokens {
		if t.UserID == userID && t.ID == id {
			delete(v.state.Tokens, digest)
			v.dirty = true
		}
	}
	v.pruneLocked()
	return v.saveLocked()
}

// authorize serializes refresh per vault, separate from the website's mutex.
// A rotated grant is durably saved before it can authorize a request. If saving
// fails, a later request retries the save, never the consumed refresh token.
func (s *Server) authorizeAgent(r *http.Request, raw string) (agentToken, session, int, string) {
	vault := s.agentVault
	if vault == nil {
		return agentToken{}, session{}, 503, "Agent access is not configured"
	}
	vault.mu.Lock()
	defer vault.mu.Unlock()
	vault.pruneLocked()
	token, ok := vault.state.Tokens[agentDigest(raw)]
	if !ok {
		return token, session{}, 401, "Token is invalid, expired or revoked"
	}
	grant, ok := vault.state.Grants[token.GrantID]
	if !ok || grant.Token == "" || !time.Now().Before(grant.RefreshExpires) {
		return token, session{}, 401, "Sign in with GitHub again and create a replacement token"
	}
	if !time.Now().Before(grant.Expires) || !grant.IdentityPending && !time.Now().Add(time.Minute).Before(grant.Expires) {
		data := url.Values{"client_id": {s.cfg.ClientID}, "client_secret": {s.cfg.ClientSecret}, "grant_type": {"refresh_token"}, "refresh_token": {grant.RefreshToken}}
		req, err := http.NewRequestWithContext(r.Context(), "POST", s.oauth+"/login/oauth/access_token", strings.NewReader(data.Encode()))
		if err != nil {
			return token, session{}, 502, "GitHub refresh unavailable"
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		res, err := s.http.Do(req)
		if err != nil {
			return token, session{}, 502, "GitHub refresh unavailable"
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 65537))
		res.Body.Close()
		var refreshed struct {
			Token            string `json:"access_token"`
			RefreshToken     string `json:"refresh_token"`
			ExpiresIn        int64  `json:"expires_in"`
			RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
			Error            string `json:"error"`
		}
		if readErr != nil || len(body) > 65536 || res.StatusCode != 200 || json.Unmarshal(body, &refreshed) != nil || refreshed.Token == "" || refreshed.Error != "" || refreshed.RefreshToken == "" || refreshed.ExpiresIn <= 0 || refreshed.ExpiresIn > 86400 || refreshed.RefreshExpiresIn <= 0 || refreshed.RefreshExpiresIn > 366*86400 {
			// A transport/5xx failure may be transient. Invalid successful responses
			// or rejected grants require reconnection; never expose upstream bodies.
			if res.StatusCode == 400 || res.StatusCode == 401 || res.StatusCode == 200 {
				grant.Token = ""
				grant.RefreshToken = ""
				vault.state.Grants[token.GrantID] = grant
				vault.dirty = true
				_ = vault.saveLocked()
				return token, session{}, 401, "GitHub authorization expired; sign in again and create a replacement token"
			}
			return token, session{}, 502, "GitHub refresh unavailable"
		}
		grant.Token = refreshed.Token
		grant.RefreshToken = refreshed.RefreshToken
		grant.Expires = time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second)
		grant.RefreshExpires = time.Now().Add(time.Duration(refreshed.RefreshExpiresIn) * time.Second)
		// Rotation consumes the previous refresh token. Save the replacement
		// before querying identity, so cancellation or a transient GitHub failure
		// can retry verification without losing the grant or replaying refresh.
		grant.IdentityPending = true
		vault.state.Grants[token.GrantID] = grant
		vault.dirty = true
	}
	if vault.dirty {
		if err := vault.saveLocked(); err != nil {
			return token, session{}, 503, "Agent credentials could not be saved; retry later"
		}
	}
	if grant.IdentityPending {
		var user User
		if s.fetch(r.Context(), "/user", grant.Token, &user) != nil || user.ID <= 0 || user.Login == "" {
			return token, session{}, 502, "GitHub identity verification unavailable; retry later"
		}
		if user.ID != grant.User.ID {
			grant.Token = ""
			grant.RefreshToken = ""
			grant.IdentityPending = false
			vault.state.Grants[token.GrantID] = grant
			vault.dirty = true
			if err := vault.saveLocked(); err != nil {
				return token, session{}, 503, "Agent credentials could not be saved; retry later"
			}
			return token, session{}, 401, "GitHub identity could not be verified; reconnect agent access"
		}
		grant.IdentityPending = false
		vault.state.Grants[token.GrantID] = grant
		vault.dirty = true
		if err := vault.saveLocked(); err != nil {
			return token, session{}, 503, "Agent credentials could not be saved; retry later"
		}
	}
	allowed := map[int64]bool{}
	for _, id := range token.RepositoryIDs {
		allowed[id] = true
	}
	return token, session{User: grant.User, Token: grant.Token, Expires: token.Expires, AgentRepositories: allowed}, 0, ""
}
