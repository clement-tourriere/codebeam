package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/store"
)

const minRefreshInterval = time.Minute

type SettingsPageData struct {
	User                    *store.User
	Notice                  string
	Error                   string
	AutoIndexRemote         bool
	RemoteRefreshInterval   string
	MaxIndexedBranches      int
	AutoExcludeInaccessible bool
	Tokens                  []store.APIToken
	Grants                  []store.OAuthGrantInfo
	Users                   []store.User // admin only
	// NewTokenSecret is shown exactly once, in the response to the create form.
	NewTokenSecret string
	NewTokenName   string
	MCPEndpoint    string
	// ActiveTab selects which settings tab is shown (tokens/agents/users/indexing),
	// preserved across form round-trips via the ?tab= query parameter.
	ActiveTab string
}

// settingsTabFromQuery returns a valid tab name, defaulting to "tokens".
// Admin-only tabs fall back to the default for members — their radio is not
// rendered, and checking none would show an empty page.
func settingsTabFromQuery(r *http.Request, admin bool) string {
	switch tab := r.URL.Query().Get("tab"); tab {
	case "agents":
		return tab
	case "users", "indexing":
		if admin {
			return tab
		}
		return "tokens"
	default:
		return "tokens"
	}
}

// handleSettings renders and saves application settings: instance-wide
// configuration and user administration for admins, plus every user's own API
// tokens and connected OAuth agents.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		data := s.settingsData(r, user)
		data.ActiveTab = settingsTabFromQuery(r, user.IsAdmin())
		data.Notice = r.URL.Query().Get("notice")
		if data.Error == "" {
			data.Error = r.URL.Query().Get("error")
		}
		s.render(w, http.StatusOK, "page_settings", data)
	case http.MethodPost:
		// The bare POST /settings saves instance-wide indexing configuration.
		if !user.IsAdmin() {
			forbidden(w)
			return
		}
		enabled := isTrueParam(r.FormValue("auto_index_remote"))
		interval, err := time.ParseDuration(strings.TrimSpace(r.FormValue("remote_refresh_interval")))
		if err != nil || interval <= 0 {
			redirectError(w, r, "/settings", errors.New("refresh interval must be a duration like 30m or 1h"))
			return
		}
		if interval < minRefreshInterval {
			interval = minRefreshInterval
		}
		maxBranches, err := strconv.Atoi(strings.TrimSpace(r.FormValue("max_indexed_branches")))
		if err != nil || maxBranches < 0 {
			redirectError(w, r, "/settings", errors.New("max branches must be a whole number (0 = the maximum)"))
			return
		}
		// Zoekt shards cannot hold more than 64 branches, so anything above the
		// ceiling would silently behave as 64 — store the truth instead.
		if maxBranches > config.ZoektMaxBranches {
			maxBranches = config.ZoektMaxBranches
		}
		autoExclude := isTrueParam(r.FormValue("auto_exclude_inaccessible"))
		if err := s.store.SetAutoIndexSettings(r.Context(), enabled, interval); err != nil {
			redirectError(w, r, "/settings", err)
			return
		}
		if err := s.store.SetMaxIndexedBranches(r.Context(), maxBranches); err != nil {
			redirectError(w, r, "/settings", err)
			return
		}
		if err := s.store.SetAutoExcludeInaccessible(r.Context(), autoExclude); err != nil {
			redirectError(w, r, "/settings", err)
			return
		}
		http.Redirect(w, r, "/settings?tab=indexing&notice="+url.QueryEscape("Settings saved."), http.StatusSeeOther)
	default:
		methodNotAllowed(w)
	}
}

// settingsData assembles everything the settings page shows for this user.
func (s *Server) settingsData(r *http.Request, user *store.User) SettingsPageData {
	data := SettingsPageData{User: user, MCPEndpoint: s.cfg.BaseURL + "/mcp"}
	enabled, interval, err := s.store.AutoIndexSettings(r.Context(), s.cfg.AutoIndexRemote, s.cfg.RemoteRefreshInterval)
	if err != nil {
		data.Error = err.Error()
	}
	data.AutoIndexRemote = enabled
	data.RemoteRefreshInterval = formatInterval(interval)
	if maxBranches, err := s.store.MaxIndexedBranches(r.Context(), s.cfg.MaxIndexedBranches); err == nil {
		// Show the effective value: stored settings (or env defaults) that predate
		// the Zoekt 64-branch ceiling may exceed it, but the indexer clamps.
		data.MaxIndexedBranches = min(maxBranches, config.ZoektMaxBranches)
	} else {
		data.Error = err.Error()
	}
	if autoExclude, err := s.store.AutoExcludeInaccessible(r.Context(), s.cfg.AutoExcludeInaccessible); err == nil {
		data.AutoExcludeInaccessible = autoExclude
	} else {
		data.Error = err.Error()
	}
	if tokens, err := s.store.ListAPITokens(r.Context(), user.ID); err == nil {
		data.Tokens = tokens
	} else {
		data.Error = err.Error()
	}
	if grants, err := s.store.ListOAuthGrants(r.Context(), user.ID); err == nil {
		data.Grants = grants
	} else {
		data.Error = err.Error()
	}
	if user.IsAdmin() {
		if users, err := s.store.ListUsers(r.Context()); err == nil {
			data.Users = users
		} else {
			data.Error = err.Error()
		}
	}
	return data
}

// handleSettingsTokenCreate mints a PAT and renders the page with the secret,
// which is shown exactly once and never stored in the clear.
func (s *Server) handleSettingsTokenCreate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	var ttl time.Duration
	if raw := strings.TrimSpace(r.FormValue("expires_days")); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days < 0 {
			redirectError(w, r, "/settings", errors.New("expiry must be a number of days (0 = never)"))
			return
		}
		ttl = time.Duration(days) * 24 * time.Hour
	}
	secret, token, err := s.store.CreateAPIToken(r.Context(), user.ID, name, ttl)
	if err != nil {
		redirectError(w, r, "/settings", err)
		return
	}
	data := s.settingsData(r, user)
	data.ActiveTab = "tokens"
	data.NewTokenSecret = secret
	data.NewTokenName = token.Name
	data.Notice = "Token created. Copy it now — it will not be shown again."
	s.render(w, http.StatusOK, "page_settings", data)
}

func (s *Server) handleSettingsTokenRevoke(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	tokenID, err := strconv.ParseInt(r.FormValue("token_id"), 10, 64)
	if err != nil {
		redirectError(w, r, "/settings", errors.New("invalid token id"))
		return
	}
	if err := s.store.RevokeAPIToken(r.Context(), user.ID, tokenID); err != nil {
		redirectError(w, r, "/settings", errors.New("token not found"))
		return
	}
	http.Redirect(w, r, "/settings?tab=tokens&notice="+url.QueryEscape("Token revoked."), http.StatusSeeOther)
}

func (s *Server) handleSettingsGrantRevoke(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	grantID, err := strconv.ParseInt(r.FormValue("grant_id"), 10, 64)
	if err != nil {
		redirectError(w, r, "/settings", errors.New("invalid grant id"))
		return
	}
	if err := s.store.RevokeOAuthGrant(r.Context(), user.ID, grantID); err != nil {
		redirectError(w, r, "/settings", errors.New("grant not found"))
		return
	}
	http.Redirect(w, r, "/settings?tab=agents&notice="+url.QueryEscape("Agent access revoked."), http.StatusSeeOther)
}

// handleSettingsUserRole lets an admin change another user's role. The store
// refuses to demote the last admin.
func (s *Server) handleSettingsUserRole(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin() {
		forbidden(w)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	targetID, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		redirectError(w, r, "/settings", errors.New("invalid user id"))
		return
	}
	if err := s.store.SetUserRole(r.Context(), targetID, r.FormValue("role")); err != nil {
		redirectError(w, r, "/settings", err)
		return
	}
	http.Redirect(w, r, "/settings?tab=users&notice="+url.QueryEscape("Role updated."), http.StatusSeeOther)
}

func forbidden(w http.ResponseWriter) {
	http.Error(w, "admin access required", http.StatusForbidden)
}

// formatInterval drops the trailing "0s" Go adds to whole-minute durations so the
// settings field shows "30m" rather than "30m0s".
func formatInterval(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		return strings.TrimSuffix(s, "0s")
	}
	return s
}
