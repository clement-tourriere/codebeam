package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	codebeam "github.com/ctourriere/codebeam"
	"github.com/ctourriere/codebeam/internal/codehost"
	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/indexer"
	"github.com/ctourriere/codebeam/internal/oidc"
	codesearch "github.com/ctourriere/codebeam/internal/search"
	"github.com/ctourriere/codebeam/internal/store"
	"github.com/ctourriere/codebeam/internal/structural"
)

const (
	sessionCookie     = "codebeam_session"
	stateCookie       = "codebeam_oauth_state"
	loginNextCookie   = "codebeam_login_next"
	loginNextTTL      = 10 * time.Minute
	maxRepoRows       = 60
	defaultRepoStatus = "all"
)

type Server struct {
	cfg        config.Config
	store      *store.Store
	indexer    *indexer.Indexer
	search     codesearch.Engine
	structural *structural.Engine
	templates  *template.Template
	secret     []byte
	hosts      map[string]*codehost.Client
	oidc       *oidc.Client
	chromaCSS  string
}

type LoginData struct {
	User             *store.User
	Error            string
	DevLogin         bool
	GitHubConfigured bool
	GitLabConfigured bool
	GitLabBaseURL    string
	OIDCConfigured   bool
	OIDCName         string
}

type RepoPageData struct {
	User                  *store.User
	Error                 string
	Notice                string
	Repos                 []RepoView
	Filter                RepoFilter
	Stats                 RepoStats
	Sources               []SourceSummary
	SourceOptions         []RepoSourceOption
	FilteredTotal         int
	DisplayedTotal        int
	HasMoreRepos          bool
	HasActiveIndexJobs    bool
	Jobs                  []store.IndexJob
	Identities            []store.Identity
	GitHubConfigured      bool
	GitLabOAuthConfigured bool
	GitLabBaseURL         string
	GitLabCallbackURL     string
}

type RepoFilter struct {
	Query  string
	Source string
	// Status is the active status chip: all|indexed|indexing|stale|failed|off.
	Status string
}

type RepoStats struct {
	Total     int
	Selected  int
	Available int
	Indexed   int
	Active    int
	Stale     int
	Failed    int
	Off       int
}

type RepoSourceOption struct {
	Value string
	Label string
	Count int
}

type RepoView struct {
	Repo           store.Repo
	Title          string
	Subtitle       string
	SourceLabel    string
	Status         string
	StatusLabel    string
	StatusClass    string
	StatusMessage  string
	IndexedLabel   string
	FreshnessLabel string
	Stale          bool
	BranchLabel    string
	BranchInput    string
	LatestJob      *store.IndexJob
	SearchText     string
}

type BrowsePageData struct {
	User   *store.User
	Repos  []store.Repo
	Notice string
	Error  string
}

type SourcesPageData struct {
	User                  *store.User
	Error                 string
	Notice                string
	Sources               []SourceSummary
	GitHubIdentity        *store.Identity
	GitLabIdentity        *store.Identity
	SelfManagedGitLab     []store.Identity
	GitHubConfigured      bool
	GitLabOAuthConfigured bool
	GitLabBaseURL         string
	GitLabCallbackURL     string
}

type SourceSummary struct {
	Provider       string
	Label          string
	Description    string
	Username       string
	Connected      bool
	CanSync        bool
	IsLocal        bool
	RepoCount      int
	InstalledCount int
	IndexedCount   int
	FailedCount    int
}

type SearchParams struct {
	Query      string
	Mode       string // "" (lexical/Zoekt) or "structural" (AST)
	Repo       string
	Repos      []string
	Branch     string
	Path       string
	TopPath    string
	Ext        string
	Lang       string
	Source     string
	Provider   string
	Dirty      string
	SymbolKind string
	Freshness  string
	Sort       string
	Normalized bool
	Symbols    bool
}

// Structural reports whether the params select structural (AST) search.
func (p SearchParams) Structural() bool { return p.Mode == "structural" }

type SearchPageData struct {
	User     *store.User
	Error    string
	Repos    []store.Repo
	Params   SearchParams
	BasePath string
	Result   codesearch.Result
	// StructuralLangs lists the languages available to structural search.
	StructuralLangs []string
}

type CodeLine struct {
	Number    int
	HTML      template.HTML
	Highlight bool
}

func New(cfg config.Config, st *store.Store, ix *indexer.Indexer) (*Server, error) {
	hosts := map[string]*codehost.Client{
		string(codehost.GitHub): codehost.New(codehost.OAuthConfig{
			Provider:     codehost.GitHub,
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
		}),
		string(codehost.GitLab): codehost.New(codehost.OAuthConfig{
			Provider:     codehost.GitLab,
			BaseURL:      cfg.GitLabBaseURL,
			ClientID:     cfg.GitLabClientID,
			ClientSecret: cfg.GitLabClientSecret,
		}),
	}
	s := &Server{
		cfg:        cfg,
		store:      st,
		indexer:    ix,
		search:     codesearch.Engine{IndexDir: cfg.IndexDir},
		structural: structural.NewEngine(cfg, ix),
		secret:     []byte(cfg.SessionSecret),
		hosts:      hosts,
		oidc: oidc.New(oidc.Config{
			Issuer:       cfg.OIDCIssuer,
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			Scopes:       cfg.OIDCScopes,
		}),
		chromaCSS: buildChromaCSS(),
	}

	tmpl := template.New("").Funcs(template.FuncMap{
		"formatUnix":              formatUnix,
		"sinceUnix":               sinceUnix,
		"codeURL":                 codeURL,
		"statusClass":             statusClass,
		"providerName":            providerName,
		"canSyncProvider":         canSyncProvider,
		"isGitLabProvider":        codehost.IsGitLabProvider,
		"displayUserName":         displayUserName,
		"repoTitle":               repoTitle,
		"repoSubtitle":            repoSubtitle,
		"repoOptionLabel":         repoOptionLabel,
		"filterQuery":             filterQuery,
		"chipURL":                 chipURL,
		"sourceFilterURL":         sourceFilterURL,
		"chipLabel":               chipLabel,
		"providerSourceValue":     providerSourceValue,
		"referencesURL":           referencesURL,
		"repoReferencesURL":       repoReferencesURL,
		"sourceManageURL":         sourceManageURL,
		"facetURL":                facetURL,
		"clearSearchFiltersURL":   clearSearchFiltersURL,
		"activeSearchFilterCount": activeSearchFilterCount,
		"activeSearchFilters":     activeSearchFilters,
		"showFacetGroup":          showFacetGroup,
		"searchSortLabel":         searchSortLabel,
		"searchSortDescription":   searchSortDescription,
		"branchPrimary":           branchPrimary,
		"branchExtraCount":        branchExtraCount,
		"branchTitle":             branchTitle,
		"shortCommit":             shortCommit,
	})
	var err error
	// Disk templates win when present (development, Docker image); a released
	// binary run outside the repo falls back to the embedded copies.
	if matches, globErr := filepath.Glob(cfg.TemplateGlob); globErr == nil && len(matches) > 0 {
		tmpl, err = tmpl.ParseGlob(cfg.TemplateGlob)
	} else {
		tmpl, err = tmpl.ParseFS(codebeam.Templates, "templates/*.html")
	}
	if err != nil {
		return nil, err
	}
	s.templates = tmpl
	return s, nil
}

// staticFS mirrors the template lookup: serve from the configured directory
// when it exists, otherwise from the assets embedded in the binary.
func (s *Server) staticFS() http.FileSystem {
	if info, err := os.Stat(s.cfg.StaticDir); err == nil && info.IsDir() {
		return http.Dir(s.cfg.StaticDir)
	}
	sub, err := fs.Sub(codebeam.Static, "static")
	if err != nil {
		return http.Dir(s.cfg.StaticDir)
	}
	return http.FS(sub)
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(s.staticFS())))
	mux.HandleFunc("/assets/chroma.css", s.handleChromaCSS)
	mux.HandleFunc("/", s.route)
	return mux
}

func (s *Server) handleChromaCSS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = io.WriteString(w, s.chromaCSS)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if !s.allowCrossOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	switch {
	case r.URL.Path == "/":
		if _, ok := s.currentUser(r); ok {
			http.Redirect(w, r, "/search", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case r.URL.Path == "/login":
		s.handleLogin(w, r)
	case r.URL.Path == "/dev-login":
		s.handleDevLogin(w, r)
	case r.URL.Path == "/.well-known/oauth-protected-resource",
		r.URL.Path == protectedResourceMetadataPath:
		s.handleProtectedResourceMetadata(w, r)
	case r.URL.Path == "/.well-known/oauth-authorization-server":
		s.handleAuthServerMetadata(w, r)
	case r.URL.Path == "/oauth/register":
		s.handleOAuthRegister(w, r)
	case r.URL.Path == "/oauth/authorize":
		s.handleOAuthAuthorize(w, r)
	case r.URL.Path == "/oauth/token":
		s.handleOAuthToken(w, r)
	case r.URL.Path == "/mcp":
		s.handleMCP(w, r)
	case strings.HasPrefix(r.URL.Path, "/auth/"):
		s.handleAuth(w, r)
	case r.URL.Path == "/logout":
		s.handleLogout(w, r)
	case r.URL.Path == "/repos":
		s.handleRepos(w, r)
	case r.URL.Path == "/repos/manage":
		s.handleManage(w, r)
	case r.URL.Path == "/repos/list":
		s.handleReposList(w, r)
	case r.URL.Path == "/repos/status":
		s.handleReposStatus(w, r)
	case r.URL.Path == "/repos/jobs":
		s.handleReposJobs(w, r)
	case r.URL.Path == "/repos/bulk":
		s.handleRepoBulkAction(w, r)
	case r.URL.Path == "/repos/sync":
		s.handleRepoSync(w, r)
	case r.URL.Path == "/repos/github-token":
		s.handleGitHubToken(w, r)
	case r.URL.Path == "/repos/gitlab-token":
		s.handleGitLabToken(w, r)
	case r.URL.Path == "/repos/github-public":
		s.handleGitHubPublicRepo(w, r)
	case r.URL.Path == "/repos/local":
		s.handleLocalRepo(w, r)
	case r.URL.Path == "/sources/remove":
		s.handleSourceRemove(w, r)
	case r.URL.Path == "/sources/toggle":
		s.handleSourceBulk(w, r)
	case strings.HasPrefix(r.URL.Path, "/repos/"):
		// GET /repos/{id}/panel renders the manage drawer; POST /repos/{id}/...
		// are the per-repo actions (select/reindex/branches/cancel-index).
		if r.Method == http.MethodGet {
			s.handleRepoPanel(w, r)
			return
		}
		s.handleRepoAction(w, r)
	case strings.HasPrefix(r.URL.Path, "/repo/"):
		s.handleRepoView(w, r)
	case r.URL.Path == "/sources":
		s.handleSources(w, r)
	case r.URL.Path == "/settings":
		s.handleSettings(w, r)
	case r.URL.Path == "/settings/tokens":
		s.handleSettingsTokenCreate(w, r)
	case r.URL.Path == "/settings/tokens/revoke":
		s.handleSettingsTokenRevoke(w, r)
	case r.URL.Path == "/settings/grants/revoke":
		s.handleSettingsGrantRevoke(w, r)
	case r.URL.Path == "/settings/users/role":
		s.handleSettingsUserRole(w, r)
	case r.URL.Path == "/api/search":
		s.handleAPISearch(w, r)
	case r.URL.Path == "/api/read":
		s.handleAPIRead(w, r)
	case r.URL.Path == "/search":
		s.handleSearch(w, r)
	case r.URL.Path == "/search/results":
		s.handleSearchResults(w, r)
	case strings.HasPrefix(r.URL.Path, "/code/"):
		s.handleCode(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if user, ok := s.currentUser(r); ok && user != nil {
		http.Redirect(w, r, "/search", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "page_login", LoginData{
		Error:            r.URL.Query().Get("error"),
		DevLogin:         s.cfg.DevLogin,
		GitHubConfigured: s.hosts["github"].Configured(),
		GitLabConfigured: s.hosts["gitlab"].Configured(),
		GitLabBaseURL:    s.cfg.GitLabBaseURL,
		OIDCConfigured:   s.oidc.Configured(),
		OIDCName:         s.cfg.OIDCName,
	})
}

func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.DevLogin {
		http.NotFound(w, r)
		return
	}
	// POST-only: a passwordless login must not be reachable by navigating to a
	// URL (a drive-by GET would silently create a session, and the first user
	// becomes admin).
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, err := s.store.CreateDevUser(r.Context())
	if err != nil {
		s.render(w, http.StatusInternalServerError, "page_login", LoginData{Error: err.Error(), DevLogin: s.cfg.DevLogin})
		return
	}
	s.setSession(w, user.ID)
	http.Redirect(w, r, s.consumeLoginNext(w, r, "/repos"), http.StatusSeeOther)
}

// crossOriginExemptPaths are the endpoints that are meant to be called from a
// different origin: they authenticate with a bearer token or PKCE, not the
// ambient session cookie, so browser CSRF does not apply to them.
var crossOriginExemptPaths = map[string]bool{
	"/oauth/register": true,
	"/oauth/token":    true,
	"/mcp":            true,
}

// allowCrossOrigin is the CSRF guard for cookie-authenticated requests. For an
// unsafe method it rejects a request whose Origin header is present and does
// not match this instance's own origin. Cross-site fetches and top-level form
// POSTs always send Origin, so this blocks classic CSRF (including the
// SameSite=Lax POST window and sibling-subdomain attacks) without breaking
// non-browser clients, which omit Origin.
func (s *Server) allowCrossOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if crossOriginExemptPaths[r.URL.Path] {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return true // non-browser client, or a same-origin navigation with no Origin
	}
	return origin == s.selfOrigin()
}

// selfOrigin is this instance's scheme://host[:port], derived from BaseURL.
func (s *Server) selfOrigin() string {
	if u, err := url.Parse(s.cfg.BaseURL); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return s.cfg.BaseURL
}

// consumeLoginNext pops the post-login redirect target set before an
// authentication round-trip (e.g. by /oauth/authorize), falling back when none
// is stored. Only same-origin paths are honored.
func (s *Server) consumeLoginNext(w http.ResponseWriter, r *http.Request, fallback string) string {
	next, ok := s.readSignedCookie(r, loginNextCookie)
	if !ok {
		return fallback
	}
	http.SetCookie(w, &http.Cookie{Name: loginNextCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	// Must be a site-relative path with no host. Reject "//host" and "/\host",
	// both of which browsers can normalize to a protocol-relative URL and use
	// for an open redirect.
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.HasPrefix(next, "/\\") {
		return next
	}
	return fallback
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/auth/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	provider := parts[0]
	if provider == oidcProvider {
		// SSO login has its own flow (PKCE + nonce + ID token) and no code-host
		// API behind it, so it does not go through the codehost clients.
		switch {
		case len(parts) == 1:
			s.handleOIDCStart(w, r)
		case len(parts) == 2 && parts[1] == "callback":
			s.handleOIDCCallback(w, r)
		default:
			http.NotFound(w, r)
		}
		return
	}
	if len(parts) == 1 {
		s.handleAuthStart(w, r, provider)
		return
	}
	if len(parts) == 2 && parts[1] == "callback" {
		s.handleAuthCallback(w, r, provider)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleAuthStart(w http.ResponseWriter, r *http.Request, provider string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	client, ok := s.hosts[provider]
	if !ok {
		http.NotFound(w, r)
		return
	}
	state := randomToken()
	redirectURI := s.oauthRedirectURI(provider)
	authURL, err := client.AuthCodeURL(state, redirectURI)
	if err != nil {
		redirectError(w, r, s.authErrorTarget(r), err)
		return
	}
	s.setSignedCookie(w, stateCookie, provider+":"+state, 10*time.Minute)
	http.Redirect(w, r, authURL, http.StatusFound)
}

func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request, provider string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	client, ok := s.hosts[provider]
	if !ok {
		http.NotFound(w, r)
		return
	}
	expected, ok := s.readSignedCookie(r, stateCookie)
	if !ok || expected != provider+":"+r.URL.Query().Get("state") {
		redirectError(w, r, s.authErrorTarget(r), errors.New("invalid OAuth state"))
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		redirectError(w, r, s.authErrorTarget(r), errors.New("OAuth callback did not include a code"))
		return
	}
	token, err := client.ExchangeCode(r.Context(), code, r.URL.Query().Get("state"), s.oauthRedirectURI(provider))
	if err != nil {
		redirectError(w, r, s.authErrorTarget(r), err)
		return
	}
	info, err := client.FetchUser(r.Context(), token)
	if err != nil {
		redirectError(w, r, s.authErrorTarget(r), err)
		return
	}
	var user *store.User
	connected := false
	if current, ok := s.currentUser(r); ok {
		user = current
		connected = true
		err = s.store.UpsertIdentityForUser(r.Context(), current.ID, provider, info.ProviderUserID, info.Username, token)
	} else {
		user, err = s.store.UpsertUserIdentity(
			r.Context(),
			provider,
			info.ProviderUserID,
			info.Username,
			info.Email,
			info.Name,
			info.AvatarURL,
			token,
		)
	}
	if err != nil {
		redirectError(w, r, s.authErrorTarget(r), err)
		return
	}
	s.setSession(w, user.ID)
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	if connected {
		http.Redirect(w, r, "/sources?notice="+url.QueryEscape("Connected "+providerName(provider)+"."), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.consumeLoginNext(w, r, "/repos"), http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleRepos renders the browse directory: a grid of repositories whose source
// is on disk (indexed remotes or local repos), each linking to the tree view.
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	repos, err := s.store.ListReposForUser(r.Context(), user.ID)
	if err != nil {
		s.render(w, http.StatusInternalServerError, "page_repos", BrowsePageData{User: user, Error: err.Error()})
		return
	}
	s.render(w, http.StatusOK, "page_repos", BrowsePageData{
		User:   user,
		Repos:  browsableRepos(repos),
		Notice: r.URL.Query().Get("notice"),
		Error:  r.URL.Query().Get("error"),
	})
}

// handleManage renders the admin page for installing/removing and indexing repos.
func (s *Server) handleManage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	data, err := s.repoPageData(r.Context(), user, repoFilterFromRequest(r))
	if err != nil {
		s.render(w, http.StatusInternalServerError, "page_manage", RepoPageData{User: user, Error: err.Error()})
		return
	}
	data.Error = r.URL.Query().Get("error")
	data.Notice = r.URL.Query().Get("notice")
	s.render(w, http.StatusOK, "page_manage", data)
}

// handleSources renders the page for connecting code hosts and adding local repos.
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	identities, err := s.store.ListIdentities(r.Context(), user.ID)
	if err != nil {
		s.render(w, http.StatusInternalServerError, "page_sources", SourcesPageData{User: user, Error: err.Error()})
		return
	}
	repos, err := s.store.ListReposForUser(r.Context(), user.ID)
	if err != nil {
		s.render(w, http.StatusInternalServerError, "page_sources", SourcesPageData{User: user, Error: err.Error()})
		return
	}
	data := SourcesPageData{
		User:                  user,
		Notice:                r.URL.Query().Get("notice"),
		Error:                 r.URL.Query().Get("error"),
		Sources:               buildSourceSummaries(repos, identities),
		GitHubConfigured:      s.hosts["github"].Configured(),
		GitLabOAuthConfigured: s.hosts["gitlab"].Configured(),
		GitLabBaseURL:         s.cfg.GitLabBaseURL,
		GitLabCallbackURL:     s.oauthRedirectURI("gitlab"),
	}
	for i := range identities {
		identity := &identities[i]
		switch {
		case identity.Provider == string(codehost.GitHub):
			data.GitHubIdentity = identity
		case identity.Provider == string(codehost.GitLab):
			data.GitLabIdentity = identity
		case codehost.IsGitLabProvider(identity.Provider):
			data.SelfManagedGitLab = append(data.SelfManagedGitLab, *identity)
		}
	}
	s.render(w, http.StatusOK, "page_sources", data)
}

func (s *Server) handleSourceRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	provider := strings.TrimSpace(r.FormValue("provider"))
	if provider == "" || provider == "all" || strings.HasPrefix(provider, "family:") {
		redirectError(w, r, "/sources", errors.New("choose a source to remove"))
		return
	}
	result, err := s.store.DeleteSourceForUser(r.Context(), user.ID, provider)
	if err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	if !result.IdentityDeleted && len(result.Repos) == 0 {
		redirectError(w, r, "/sources", fmt.Errorf("%s source was not found", providerName(provider)))
		return
	}
	if err := s.removeDeletedRepoArtifacts(r.Context(), result.DeletedRepos); err != nil {
		redirectError(w, r, "/sources", fmt.Errorf("source removed, but cleanup failed: %w", err))
		return
	}
	http.Redirect(w, r, "/sources?notice="+url.QueryEscape(sourceRemovedNotice(result)), http.StatusSeeOther)
}

// handleSourceBulk turns indexing on or off for every repository under a source
// in one action (the rail's per-source master toggle).
func (s *Server) handleSourceBulk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	provider := strings.TrimSpace(r.FormValue("provider"))
	if provider == "" || provider == "all" {
		redirectError(w, r, "/repos/manage", errors.New("choose a source"))
		return
	}
	enable := r.FormValue("enable") == "1" || r.FormValue("enable") == "on"
	repoIDs, err := s.repoIDsMatchingFilter(r.Context(), user.ID, RepoFilter{Source: normalizeRepoSourceParam(provider), Status: defaultRepoStatus})
	if err != nil {
		redirectError(w, r, "/repos/manage", err)
		return
	}
	var changed, skipped int
	verb := "Deactivated"
	if enable {
		changed, skipped = s.bulkIndex(r.Context(), user.ID, repoIDs)
		verb = "Activated"
	} else {
		changed, skipped = s.bulkRemove(r.Context(), user.ID, repoIDs)
	}
	message := bulkNotice(verb, "matching", changed, skipped)
	if s.isHX(r) {
		s.renderRepoListMessage(w, r, user, repoFilterFromRequest(r), message)
		return
	}
	redirect := "/repos/manage?source=" + url.QueryEscape(normalizeRepoSourceParam(provider)) + "&notice=" + url.QueryEscape(message)
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

func (s *Server) removeDeletedRepoArtifacts(ctx context.Context, repos []store.Repo) error {
	var failures []string
	for _, repo := range repos {
		if s.indexer != nil {
			s.indexer.CancelReindex(repo.ID)
			if err := s.indexer.RemoveRepository(ctx, repo.ID); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", repo.FullName, err))
			}
			continue
		}
		if s.cfg.RepoDir != "" {
			if err := os.RemoveAll(filepath.Join(s.cfg.RepoDir, strconv.FormatInt(repo.ID, 10))); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", repo.FullName, err))
			}
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func sourceRemovedNotice(result store.SourceDeleteResult) string {
	parts := []string{fmt.Sprintf("Removed %s source", providerName(result.Provider))}
	if result.IdentityDeleted {
		parts = append(parts, "connection credentials")
	}
	if len(result.Repos) > 0 {
		parts = append(parts, fmt.Sprintf("%d related %s", len(result.Repos), repoNoun(len(result.Repos))))
	}
	message := strings.Join(parts, ", ") + "."
	if len(result.SharedRepos) > 0 {
		message += fmt.Sprintf(" Detached %d shared %s without deleting shared index data.", len(result.SharedRepos), repoNoun(len(result.SharedRepos)))
	}
	if result.Provider == "local" {
		message += " Original local files were not deleted."
	}
	return message
}

// browsableRepos keeps repos whose source exists on disk (indexed remotes or
// local), sorted most-recently-indexed first, then by name.
func browsableRepos(repos []store.Repo) []store.Repo {
	out := make([]store.Repo, 0, len(repos))
	for _, repo := range repos {
		if repo.IndexedAt > 0 || repo.HostProvider == "local" {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IndexedAt != out[j].IndexedAt {
			return out[i].IndexedAt > out[j].IndexedAt
		}
		return strings.ToLower(repoTitle(out[i])) < strings.ToLower(repoTitle(out[j]))
	})
	return out
}

func (s *Server) handleReposList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	data, err := s.repoPageData(r.Context(), user, repoFilterFromRequest(r))
	if err != nil {
		s.render(w, http.StatusInternalServerError, "partial_repo_list", RepoPageData{User: user, Error: err.Error()})
		return
	}
	s.render(w, http.StatusOK, "partial_repo_list", data)
}

func (s *Server) handleReposStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	data, err := s.repoPageData(r.Context(), user, repoFilterFromRequest(r))
	if err != nil {
		s.render(w, http.StatusInternalServerError, "partial_repo_status", RepoPageData{User: user, Error: err.Error()})
		return
	}
	s.render(w, http.StatusOK, "partial_repo_status", data)
}

func (s *Server) handleReposJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	data, err := s.repoPageData(r.Context(), user, repoFilterFromRequest(r))
	if err != nil {
		s.render(w, http.StatusInternalServerError, "partial_repo_jobs", RepoPageData{User: user, Error: err.Error()})
		return
	}
	s.render(w, http.StatusOK, "partial_repo_jobs", data)
}

func (s *Server) handleRepoSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	provider := r.FormValue("provider")
	count, err := s.syncProviderRepos(r.Context(), user, provider)
	if err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	http.Redirect(w, r, "/repos/manage?notice="+url.QueryEscape(fmt.Sprintf("Synced %d repositories from %s.", count, providerName(provider))), http.StatusSeeOther)
}

func (s *Server) handleGitHubToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		redirectError(w, r, "/sources", errors.New("GitHub access token is required"))
		return
	}
	client := codehost.New(codehost.OAuthConfig{Provider: codehost.GitHub})
	info, err := client.FetchUser(r.Context(), token)
	if err != nil {
		redirectError(w, r, "/sources", fmt.Errorf("connect GitHub: %w", err))
		return
	}
	provider := string(codehost.GitHub)
	if err := s.store.UpsertIdentityForUser(r.Context(), user.ID, provider, info.ProviderUserID, info.Username, token); err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	count, err := s.syncProviderRepos(r.Context(), user, provider)
	if err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	http.Redirect(w, r, "/repos/manage?notice="+url.QueryEscape(fmt.Sprintf("Connected and synced %d repositories from GitHub.", count)), http.StatusSeeOther)
}

func (s *Server) handleGitLabToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	baseURL, err := codehost.NormalizeBaseURL(r.FormValue("base_url"))
	if err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		redirectError(w, r, "/sources", errors.New("GitLab access token is required"))
		return
	}
	provider, err := codehost.GitLabProviderKey(baseURL)
	if err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	client := codehost.New(codehost.OAuthConfig{Provider: codehost.GitLab, BaseURL: baseURL})
	info, err := client.FetchUser(r.Context(), token)
	if err != nil {
		redirectError(w, r, "/sources", fmt.Errorf("connect GitLab: %w", err))
		return
	}
	if err := s.store.UpsertIdentityForUser(r.Context(), user.ID, provider, info.ProviderUserID, info.Username, token); err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	count, err := s.syncProviderRepos(r.Context(), user, provider)
	if err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	http.Redirect(w, r, "/repos/manage?notice="+url.QueryEscape(fmt.Sprintf("Connected and synced %d repositories from %s.", count, providerName(provider))), http.StatusSeeOther)
}

func (s *Server) handleGitHubPublicRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	repoSpec := strings.TrimSpace(r.FormValue("repo"))
	client := codehost.New(codehost.OAuthConfig{Provider: codehost.GitHub})
	ghRepo, err := client.FetchGitHubPublicRepo(r.Context(), repoSpec)
	if err != nil {
		redirectError(w, r, "/sources", fmt.Errorf("add public GitHub repo: %w", err))
		return
	}
	indexedBranches := store.NormalizeBranchPolicy(r.FormValue("branches"))
	if strings.TrimSpace(r.FormValue("branches")) != "" && indexedBranches == "" {
		redirectError(w, r, "/sources", errors.New("branch list is empty"))
		return
	}
	repo, err := s.store.UpsertRepo(r.Context(), store.Repo{
		HostProvider:    string(codehost.GitHub),
		HostRepoID:      ghRepo.ProviderID,
		Name:            ghRepo.Name,
		FullName:        ghRepo.FullName,
		CloneURL:        ghRepo.CloneURL,
		WebURL:          ghRepo.WebURL,
		DefaultBranch:   ghRepo.DefaultBranch,
		IndexedBranches: indexedBranches,
		Private:         false,
		Selected:        true,
	}, user.ID)
	if err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	if err := s.store.SelectRepoForUser(r.Context(), user.ID, repo.ID); err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	notice := "Added " + ghRepo.FullName + "."
	if s.indexer != nil {
		if _, err := s.indexer.EnqueueReindex(r.Context(), repo.ID, user.ID); err != nil {
			if strings.Contains(err.Error(), "already indexing") {
				notice += " It is already indexing."
			} else {
				redirectError(w, r, "/sources", err)
				return
			}
		} else {
			notice += " Indexing started."
		}
	}
	http.Redirect(w, r, "/repos/manage?source="+url.QueryEscape(providerSourceValue(string(codehost.GitHub)))+"&notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

func (s *Server) syncProviderRepos(ctx context.Context, user *store.User, provider string) (int, error) {
	client, ok := s.clientForProvider(provider)
	if !ok {
		return 0, fmt.Errorf("unknown provider %q", provider)
	}
	token, err := s.store.GetAccessToken(ctx, user.ID, provider)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("connect %s first", providerName(provider))
	}
	if err != nil {
		// A decrypt failure (e.g. a rotated or lost encryption key) is not a
		// "not connected" case. Keep the token out of the message and the logs.
		slog.Error("read code-host token", "provider", provider, "user", user.ID, "error", err)
		return 0, fmt.Errorf("could not read %s credentials", providerName(provider))
	}
	repos, complete, err := client.ListRepos(ctx, token)
	if err != nil {
		return 0, err
	}
	// The provider stored on repo rows is the connection's provider key
	// (e.g. "github" or "gitlab_gitlab.example.com"); GitHub normalizes to the
	// bare "github" key. accessibleIDs is the set the host says this user can
	// reach, used below to prune permissions the user has lost.
	hostProvider := provider
	accessibleIDs := make([]string, 0, len(repos))
	for _, repo := range repos {
		if repo.Provider == codehost.GitHub {
			hostProvider = string(codehost.GitHub)
		}
		accessibleIDs = append(accessibleIDs, repo.ProviderID)
		if _, err := s.store.UpsertRepo(ctx, store.Repo{
			HostProvider:  hostProvider,
			HostRepoID:    repo.ProviderID,
			Name:          repo.Name,
			FullName:      repo.FullName,
			CloneURL:      repo.CloneURL,
			WebURL:        repo.WebURL,
			DefaultBranch: repo.DefaultBranch,
			Private:       repo.Private,
		}, user.ID); err != nil {
			return 0, err
		}
	}
	// Permission sync: mirror the host's access control by pruning permissions
	// for private repos the user can no longer reach. Only safe on a complete
	// listing — a truncated one would revoke access to repos that simply fell
	// past the pagination cap.
	if complete {
		if revoked, err := s.store.RevokeStalePermissions(ctx, user.ID, hostProvider, accessibleIDs); err != nil {
			return 0, err
		} else if revoked > 0 {
			slog.Info("pruned stale repo permissions", "user", user.ID, "provider", hostProvider, "revoked", revoked)
		}
	}
	return len(repos), nil
}

// ReconcileRepoPermissions mirrors a code host's current answer for one user
// onto Codebeam's permissions WITHOUT importing repositories: it grants access
// the user newly gained on repos already in Codebeam and revokes access they
// lost. It backs the background PermissionSyncer. Unknown providers and users
// with no stored token are silently skipped (nothing to reconcile).
func (s *Server) ReconcileRepoPermissions(ctx context.Context, userID int64, provider string) error {
	client, ok := s.clientForProvider(provider)
	if !ok {
		return nil
	}
	token, err := s.store.GetAccessToken(ctx, userID, provider)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no identity for this provider: nothing to reconcile
	}
	if err != nil {
		// A decrypt failure (rotated/lost key): report it so the syncer logs and
		// moves to the next identity, rather than silently skipping this user.
		return fmt.Errorf("read %s token for user %d: %w", provider, userID, err)
	}
	if token == "" {
		return nil // identity exists but carries no token (dev/OIDC login)
	}
	repos, complete, err := client.ListRepos(ctx, token)
	if err != nil {
		return err
	}
	hostProvider := provider
	ids := make([]string, 0, len(repos))
	for _, repo := range repos {
		if repo.Provider == codehost.GitHub {
			hostProvider = string(codehost.GitHub)
		}
		ids = append(ids, repo.ProviderID)
	}
	if _, err := s.store.GrantExistingRepoPermissions(ctx, userID, hostProvider, ids); err != nil {
		return err
	}
	// Revoke only on a complete listing, or repos past the pagination cap would
	// be wrongly treated as inaccessible.
	if complete {
		if revoked, err := s.store.RevokeStalePermissions(ctx, userID, hostProvider, ids); err != nil {
			return err
		} else if revoked > 0 {
			slog.Info("pruned stale repo permissions", "user", userID, "provider", hostProvider, "revoked", revoked)
		}
	}
	return nil
}

func (s *Server) clientForProvider(provider string) (*codehost.Client, bool) {
	if client, ok := s.hosts[provider]; ok {
		return client, true
	}
	if codehost.IsGitLabProvider(provider) {
		baseURL := codehost.GitLabBaseURLFromProvider(provider, s.cfg.GitLabBaseURL)
		return codehost.New(codehost.OAuthConfig{Provider: codehost.GitLab, BaseURL: baseURL}), true
	}
	return nil, false
}

func (s *Server) handleLocalRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	// Local paths read the server's own disk, so in a multi-user deployment
	// only admins may register them.
	if !user.IsAdmin() {
		forbidden(w)
		return
	}
	localPath := strings.TrimSpace(r.FormValue("path"))
	if localPath == "" {
		redirectError(w, r, "/sources", errors.New("local repository path is required"))
		return
	}
	abs, err := filepath.Abs(localPath)
	if err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("%s is not a directory", abs)
		}
		redirectError(w, r, "/sources", err)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = filepath.Base(abs)
	}
	fullName := localRepoFullName(name)
	repo, err := s.store.UpsertRepo(r.Context(), store.Repo{
		HostProvider:  "local",
		HostRepoID:    abs,
		Name:          name,
		FullName:      fullName,
		CloneURL:      abs,
		WebURL:        "",
		DefaultBranch: "HEAD",
		LocalPath:     abs,
		Selected:      true,
	}, user.ID)
	if err != nil {
		redirectError(w, r, "/sources", err)
		return
	}
	_ = s.store.SelectRepoForUser(r.Context(), user.ID, repo.ID)
	http.Redirect(w, r, "/repos/manage?notice="+url.QueryEscape("Local repository added."), http.StatusSeeOther)
}

func (s *Server) handleRepoAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/repos/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	repoID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	canAccess, err := s.store.UserCanAccessRepo(r.Context(), user.ID, repoID)
	if err != nil || !canAccess {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "select":
		// The On/Off switch unifies "installed" and "indexed": turning a repo on
		// selects it and immediately queues indexing (and the scheduler/watcher
		// keep it fresh); turning it off removes it from search and deletes shards.
		selected := r.FormValue("selected") == "on" || r.FormValue("selected") == "1"
		if selected {
			if err := s.store.SelectRepoForUser(r.Context(), user.ID, repoID); err != nil {
				redirectError(w, r, "/repos/manage", err)
				return
			}
			if s.indexer != nil {
				if _, err := s.indexer.EnqueueReindex(r.Context(), repoID, user.ID); err != nil && !strings.Contains(err.Error(), "already indexing") {
					redirectError(w, r, "/repos/manage", err)
					return
				}
			}
		} else if changed, skipped := s.bulkRemove(r.Context(), user.ID, []int64{repoID}); changed == 0 && skipped > 0 {
			redirectError(w, r, "/repos/manage", errors.New("remove repository from search"))
			return
		}
		if s.isHX(r) {
			s.renderRepoList(w, r, user)
			return
		}
		http.Redirect(w, r, "/repos/manage", http.StatusSeeOther)
	case "reindex":
		if _, err := s.indexer.EnqueueReindex(r.Context(), repoID, user.ID); err != nil {
			redirectError(w, r, "/repos/manage", err)
			return
		}
		if s.isHX(r) {
			s.renderRepoList(w, r, user)
			return
		}
		http.Redirect(w, r, "/repos/manage?notice="+url.QueryEscape("Indexing started."), http.StatusSeeOther)
	case "branches":
		repo, err := s.store.GetRepoForUser(r.Context(), repoID, user.ID)
		if err != nil {
			redirectError(w, r, "/repos/manage", err)
			return
		}
		if repo.HostProvider == "local" {
			redirectError(w, r, "/repos/manage", errors.New("local repository branches follow the checked-out worktree"))
			return
		}
		branches := store.NormalizeBranchPolicy(r.FormValue("branches"))
		if strings.TrimSpace(r.FormValue("branches")) != "" && branches == "" {
			redirectError(w, r, "/repos/manage", errors.New("branch list is empty"))
			return
		}
		if err := s.store.SetRepoIndexedBranches(r.Context(), repoID, branches); err != nil {
			redirectError(w, r, "/repos/manage", err)
			return
		}
		notice := "Branch selection saved."
		if repo.Selected && s.indexer != nil {
			if _, err := s.indexer.EnqueueReindex(r.Context(), repoID, user.ID); err != nil {
				if strings.Contains(err.Error(), "already indexing") {
					notice += " Repository is already indexing."
				} else {
					redirectError(w, r, "/repos/manage", err)
					return
				}
			} else {
				notice += " Reindexing started."
			}
		}
		if s.isHX(r) {
			s.renderRepoListMessage(w, r, user, repoFilterFromRequest(r), notice)
			return
		}
		http.Redirect(w, r, "/repos/manage?notice="+url.QueryEscape(notice), http.StatusSeeOther)
	case "cancel-index":
		cancelled := s.indexer.CancelReindex(repoID)
		message := "Indexing cancelled."
		if !cancelled {
			message = "Indexing marked cancelled."
		}
		if err := s.store.CancelLatestIndexJobForRepo(r.Context(), repoID, message); err != nil {
			redirectError(w, r, "/repos/manage", err)
			return
		}
		if s.isHX(r) {
			s.renderRepoList(w, r, user)
			return
		}
		http.Redirect(w, r, "/repos/manage?notice="+url.QueryEscape(message), http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

type RepoPanelData struct {
	User        *store.User
	Repo        store.Repo
	View        RepoView
	Branches    []string
	BranchInput string
	IsLocal     bool
	CanBrowse   bool
	Jobs        []store.IndexJob
}

// handleRepoPanel renders the slide-over management drawer for one repository:
// status + freshness, indexed branches, branch policy, actions, and recent jobs.
func (s *Server) handleRepoPanel(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/repos/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[1] != "panel" {
		http.NotFound(w, r)
		return
	}
	repoID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	repo, err := s.store.GetRepoForUser(r.Context(), repoID, user.ID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	latestJobs, err := s.store.LatestIndexJobsForUser(r.Context(), user.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, interval, err := s.store.AutoIndexSettings(r.Context(), s.cfg.AutoIndexRemote, s.cfg.RemoteRefreshInterval)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jobs, err := s.store.ListIndexJobsForRepo(r.Context(), user.ID, repoID, 8)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view := buildRepoView(*repo, latestJobs[repo.ID], interval, store.UnixNow())
	s.render(w, http.StatusOK, "partial_repo_panel", RepoPanelData{
		User:        user,
		Repo:        *repo,
		View:        view,
		Branches:    repo.IndexedBranchList(),
		BranchInput: view.BranchInput,
		IsLocal:     repo.HostProvider == "local",
		CanBrowse:   repo.IndexedAt > 0 || repo.HostProvider == "local",
		Jobs:        jobs,
	})
}

func (s *Server) handleRepoBulkAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	filter := repoFilterFromRequest(r)
	action := strings.TrimSpace(r.FormValue("bulk_action"))
	// match_all=1 acts on every repository matching the current filters (the
	// "select all N" escalation); otherwise it acts on the checked rows.
	allMatching := r.FormValue("match_all") == "1"

	var (
		repoIDs []int64
		err     error
	)
	if allMatching {
		repoIDs, err = s.repoIDsMatchingFilter(r.Context(), user.ID, filter)
	} else {
		repoIDs, err = repoIDsFromRequest(r)
	}
	if err != nil {
		s.renderRepoListMessage(w, r, user, filter, err.Error())
		return
	}
	if len(repoIDs) == 0 {
		s.renderRepoListMessage(w, r, user, filter, "Select repositories first, or use a source's Activate all / Deactivate all.")
		return
	}

	scope := "selected"
	if allMatching {
		scope = "matching"
	}
	switch action {
	case "activate":
		queued, skipped := s.bulkIndex(r.Context(), user.ID, repoIDs)
		s.renderRepoListMessage(w, r, user, filter, bulkNotice("Activated", scope, queued, skipped))
	case "deactivate":
		changed, skipped := s.bulkRemove(r.Context(), user.ID, repoIDs)
		s.renderRepoListMessage(w, r, user, filter, bulkNotice("Deactivated", scope, changed, skipped))
	default:
		s.renderRepoListMessage(w, r, user, filter, "Choose an action.")
	}
}

func (s *Server) bulkRemove(ctx context.Context, userID int64, repoIDs []int64) (int, int) {
	var changed, skipped int
	for _, repoID := range repoIDs {
		repo, err := s.store.GetRepoForUser(ctx, repoID, userID)
		if err != nil {
			skipped++
			continue
		}
		wasSearchable := repo.Selected || repo.IndexedAt > 0 || repo.LastIndexError != ""
		// Drop this user's selection first. If another permitted user still
		// wants the repo indexed, we must NOT touch the shared index or its
		// in-flight job — just detach this user.
		stillSelected, err := s.store.DeselectRepoForUser(ctx, userID, repoID)
		if err != nil {
			skipped++
			continue
		}
		if stillSelected {
			if wasSearchable {
				changed++
			}
			continue
		}
		if s.indexer != nil && s.indexer.CancelReindex(repoID) {
			wasSearchable = true
			_ = s.store.CancelLatestIndexJobForRepo(context.Background(), repoID, "Indexing cancelled after repository removal.")
		}
		if s.indexer != nil {
			if err := s.indexer.RemoveIndex(ctx, repoID); err != nil {
				skipped++
				continue
			}
		} else if err := s.store.MarkRepoUnindexed(ctx, repoID); err != nil {
			skipped++
			continue
		}
		if wasSearchable {
			changed++
		}
	}
	return changed, skipped
}

func (s *Server) bulkIndex(ctx context.Context, userID int64, repoIDs []int64) (int, int) {
	var queued, skipped int
	for _, repoID := range repoIDs {
		ok, err := s.store.UserCanAccessRepo(ctx, userID, repoID)
		if err != nil || !ok {
			skipped++
			continue
		}
		if err := s.store.SelectRepoForUser(ctx, userID, repoID); err != nil {
			skipped++
			continue
		}
		if _, err := s.indexer.EnqueueReindex(ctx, repoID, userID); err != nil {
			skipped++
			continue
		}
		queued++
	}
	return queued, skipped
}

func (s *Server) repoIDsMatchingFilter(ctx context.Context, userID int64, filter RepoFilter) ([]int64, error) {
	repos, err := s.store.ListReposForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	latestJobs, err := s.store.LatestIndexJobsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	_, interval, err := s.store.AutoIndexSettings(ctx, s.cfg.AutoIndexRemote, s.cfg.RemoteRefreshInterval)
	if err != nil {
		return nil, err
	}
	now := store.UnixNow()
	query := strings.ToLower(filter.Query)
	repoIDs := make([]int64, 0, len(repos))
	for _, repo := range repos {
		view := buildRepoView(repo, latestJobs[repo.ID], interval, now)
		if query != "" && !strings.Contains(view.SearchText, query) {
			continue
		}
		if !sourceMatches(view.Repo.HostProvider, filter.Source) {
			continue
		}
		if !chipMatches(view, filter.Status) {
			continue
		}
		repoIDs = append(repoIDs, repo.ID)
	}
	return repoIDs, nil
}

func (s *Server) renderRepoList(w http.ResponseWriter, r *http.Request, user *store.User) {
	s.renderRepoListMessage(w, r, user, repoFilterFromRequest(r), "")
}

func (s *Server) renderRepoListMessage(w http.ResponseWriter, r *http.Request, user *store.User, filter RepoFilter, message string) {
	data, err := s.repoPageData(r.Context(), user, filter)
	if err != nil {
		s.render(w, http.StatusInternalServerError, "partial_repo_list", RepoPageData{User: user, Error: err.Error()})
		return
	}
	data.Notice = message
	s.render(w, http.StatusOK, "partial_repo_list", data)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	data := s.searchPageData(r.Context(), user, r)
	s.render(w, http.StatusOK, "page_search", data)
}

func (s *Server) handleSearchResults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	data := s.searchPageData(r.Context(), user, r)
	if !s.isHX(r) {
		// HTMX pushes /search/results?... into history after an in-page search. A
		// browser reload of that URL must return the full shell (CSS, nav, search
		// form), not only the fragment intended for an HTMX swap.
		s.render(w, http.StatusOK, "page_search", data)
		return
	}
	s.render(w, http.StatusOK, "partial_search_results", data)
}

func (s *Server) handleCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/code/")
	idPart, relPath, ok := strings.Cut(rest, "/")
	if !ok || idPart == "" || relPath == "" {
		http.NotFound(w, r)
		return
	}
	repoID, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	repo, err := s.store.GetRepoForUser(r.Context(), repoID, user.ID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	root := s.indexer.SourceRoot(*repo)
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	filePath, err := safeJoin(root, relPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var content []byte
	var symbols []indexer.FileSymbol
	if branch != "" && repo.HostProvider != "local" {
		if !repoHasIndexedBranch(*repo, branch) {
			http.NotFound(w, r)
			return
		}
		content, err = readGitBranchFile(r.Context(), root, branch, relPath)
		if err != nil || len(content) > 4<<20 {
			http.NotFound(w, r)
			return
		}
	} else {
		info, err := os.Stat(filePath)
		if err != nil || info.IsDir() || info.Size() > 4<<20 {
			http.NotFound(w, r)
			return
		}
		content, err = os.ReadFile(filePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		symbols = s.indexer.FileSymbols(r.Context(), filePath)
	}
	focusLine, _ := strconv.Atoi(r.URL.Query().Get("line"))
	slashPath := filepath.ToSlash(relPath)

	page := s.repoPage(*repo, user)
	page.View = "file"
	page.Path = slashPath
	page.Crumbs = pathCrumbs(slashPath)
	page.Symbols = symbols
	page.Lines = codeLines(slashPath, content, focusLine, symbolDefLines(page.Symbols))
	page.FocusLine = focusLine

	// HTMX navigation from the tree swaps only the main pane; a full load
	// renders the whole two-pane shell with the tree expanded to this file.
	if s.isHX(r) {
		s.render(w, http.StatusOK, "partial_code", page)
		return
	}
	if tree, err := s.buildTree(*repo, slashPath); err == nil {
		page.Tree = tree
	}
	s.render(w, http.StatusOK, "page_repo", page)
}

func (s *Server) repoPageData(ctx context.Context, user *store.User, filter RepoFilter) (RepoPageData, error) {
	repos, err := s.store.ListReposForUser(ctx, user.ID)
	if err != nil {
		return RepoPageData{}, err
	}
	latestJobs, err := s.store.LatestIndexJobsForUser(ctx, user.ID)
	if err != nil {
		return RepoPageData{}, err
	}
	jobs, err := s.store.ListIndexJobsForUser(ctx, user.ID, 20)
	if err != nil {
		return RepoPageData{}, err
	}
	identities, err := s.store.ListIdentities(ctx, user.ID)
	if err != nil {
		return RepoPageData{}, err
	}
	_, interval, err := s.store.AutoIndexSettings(ctx, s.cfg.AutoIndexRemote, s.cfg.RemoteRefreshInterval)
	if err != nil {
		return RepoPageData{}, err
	}
	views, stats, filteredTotal, hasMore, hasActive := buildRepoViews(repos, latestJobs, filter, interval, store.UnixNow())
	return RepoPageData{
		User:                  user,
		Repos:                 views,
		Filter:                filter,
		Stats:                 stats,
		Sources:               buildSourceSummaries(repos, identities),
		SourceOptions:         repoSourceOptions(repos),
		FilteredTotal:         filteredTotal,
		DisplayedTotal:        len(views),
		HasMoreRepos:          hasMore,
		HasActiveIndexJobs:    hasActive,
		Jobs:                  jobs,
		Identities:            identities,
		GitHubConfigured:      s.hosts["github"].Configured(),
		GitLabOAuthConfigured: s.hosts["gitlab"].Configured(),
		GitLabBaseURL:         s.cfg.GitLabBaseURL,
		GitLabCallbackURL:     s.oauthRedirectURI("gitlab"),
	}, nil
}

func repoFilterFromRequest(r *http.Request) RepoFilter {
	status := strings.TrimSpace(r.FormValue("status"))
	if status == "" {
		status = defaultRepoStatus
	}
	return RepoFilter{
		Query:  strings.TrimSpace(r.FormValue("q")),
		Source: normalizeRepoSourceParam(r.FormValue("source")),
		Status: status,
	}
}

func normalizeRepoSourceParam(value string) string {
	value = strings.TrimSpace(value)
	switch value {
	case "", "all":
		return "all"
	case "github", "local":
		return providerSourceValue(value)
	case "gitlab":
		return providerSourceValue(value)
	}
	if strings.HasPrefix(value, "provider:") || strings.HasPrefix(value, "family:") {
		return value
	}
	return providerSourceValue(value)
}

func repoIDsFromRequest(r *http.Request) ([]int64, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	seen := map[int64]struct{}{}
	var repoIDs []int64
	for _, raw := range r.Form["repo_id"] {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		repoID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || repoID <= 0 {
			return nil, fmt.Errorf("invalid repository id %q", raw)
		}
		if _, ok := seen[repoID]; ok {
			continue
		}
		seen[repoID] = struct{}{}
		repoIDs = append(repoIDs, repoID)
	}
	return repoIDs, nil
}

func bulkNotice(verb, scope string, changed, skipped int) string {
	if changed == 0 && skipped == 0 {
		return "Select at least one repository, or use an all matching action."
	}
	message := fmt.Sprintf("%s %d %s", verb, changed, repoNoun(changed))
	if scope == "matching" {
		message += " matching the current filters"
	}
	message += "."
	if skipped > 0 {
		message += fmt.Sprintf(" Skipped %d.", skipped)
	}
	return message
}

func repoNoun(count int) string {
	if count == 1 {
		return "repository"
	}
	return "repositories"
}

func buildRepoViews(repos []store.Repo, latestJobs map[int64]store.IndexJob, filter RepoFilter, interval time.Duration, now int64) ([]RepoView, RepoStats, int, bool, bool) {
	stats := RepoStats{Total: len(repos)}
	var all []RepoView
	var hasActive bool
	for _, repo := range repos {
		view := buildRepoView(repo, latestJobs[repo.ID], interval, now)
		all = append(all, view)
		if repo.Selected {
			stats.Selected++
		}
		// Counts mirror the status chips so each chip's badge matches what it filters to.
		if chipMatches(view, "indexed") {
			stats.Indexed++
		}
		if chipMatches(view, "indexing") {
			stats.Active++
			hasActive = true
		}
		if chipMatches(view, "stale") {
			stats.Stale++
		}
		if chipMatches(view, "failed") {
			stats.Failed++
		}
		if chipMatches(view, "off") {
			stats.Off++
		}
	}
	stats.Available = stats.Off

	var filtered []RepoView
	query := strings.ToLower(filter.Query)
	for _, view := range all {
		if query != "" && !strings.Contains(view.SearchText, query) {
			continue
		}
		if !sourceMatches(view.Repo.HostProvider, filter.Source) {
			continue
		}
		if !chipMatches(view, filter.Status) {
			continue
		}
		filtered = append(filtered, view)
	}
	filteredTotal := len(filtered)
	hasMore := filteredTotal > maxRepoRows
	if hasMore {
		filtered = filtered[:maxRepoRows]
	}
	return filtered, stats, filteredTotal, hasMore, hasActive
}

func buildRepoView(repo store.Repo, latestJob store.IndexJob, interval time.Duration, now int64) RepoView {
	status := "not-indexed"
	statusLabel := "Off"
	statusClass := "badge-ghost"
	statusMessage := "Turn this repository on to index it and make it searchable."
	indexedLabel := "Never indexed"
	if repo.Selected {
		statusLabel = "Needs index"
		statusClass = "badge-warning"
		statusMessage = "Queued for indexing."
	}

	if repo.IndexedAt > 0 {
		status = "indexed"
		statusLabel = "Indexed"
		statusClass = "badge-success"
		statusMessage = "Ready for search."
		indexedLabel = sinceUnix(repo.IndexedAt)
	}
	if repo.LastIndexError != "" {
		status = "needs-index"
		statusLabel = "Needs index"
		statusClass = "badge-warning"
		statusMessage = repo.LastIndexError
	}

	var jobPtr *store.IndexJob
	if latestJob.ID != 0 {
		job := latestJob
		jobPtr = &job
		if repo.Selected {
			switch latestJob.Status {
			case "queued":
				status = "queued"
				statusLabel = "Queued"
				statusClass = "badge-info"
				statusMessage = latestJob.Message
			case "running":
				status = "running"
				statusLabel = "Indexing"
				statusClass = "badge-warning"
				statusMessage = latestJob.Message
			case "failed":
				status = "failed"
				statusLabel = "Failed"
				statusClass = "badge-error"
				statusMessage = latestJob.Message
			case "cancelled":
				status = "cancelled"
				statusLabel = "Cancelled"
				statusClass = "badge-ghost"
				statusMessage = latestJob.Message
			}
		}
	}

	// Staleness is display-only and derived from existing columns plus the
	// remote auto-refresh interval: a selected, successfully-indexed remote repo
	// whose last index is older than the interval is "stale". Local repos refresh
	// via the filesystem watcher, so they are never soft-stale.
	stale := false
	freshnessLabel := ""
	if repo.Selected && status == "indexed" {
		freshnessLabel = "Fresh"
		if repo.HostProvider != "local" && interval > 0 && repo.IndexedAt > 0 && now > repo.IndexedAt {
			if overdue := time.Duration(now-repo.IndexedAt)*time.Second - interval; overdue > 0 {
				stale = true
				statusLabel = "Stale"
				statusClass = "badge-warning"
				statusMessage = fmt.Sprintf("Indexed %s; auto-refresh overdue.", sinceUnix(repo.IndexedAt))
				freshnessLabel = "Refresh due " + humanizeDuration(overdue) + " ago"
			}
		}
	}

	title := repoTitle(repo)
	subtitle := repoSubtitle(repo)
	source := providerName(repo.HostProvider)
	branchLabel := repo.BranchLabel()
	branchInput := strings.Join(store.BranchPolicyList(repo.IndexedBranches), ", ")
	searchText := strings.ToLower(strings.Join([]string{
		title,
		subtitle,
		source,
		branchLabel,
		repo.FullName,
		repo.CloneURL,
		repo.WebURL,
		status,
		statusLabel,
		statusMessage,
	}, " "))
	return RepoView{
		Repo:           repo,
		Title:          title,
		Subtitle:       subtitle,
		SourceLabel:    source,
		Status:         status,
		StatusLabel:    statusLabel,
		StatusClass:    statusClass,
		StatusMessage:  statusMessage,
		IndexedLabel:   indexedLabel,
		FreshnessLabel: freshnessLabel,
		Stale:          stale,
		BranchLabel:    branchLabel,
		BranchInput:    branchInput,
		LatestJob:      jobPtr,
		SearchText:     searchText,
	}
}

func buildSourceSummaries(repos []store.Repo, identities []store.Identity) []SourceSummary {
	byProvider := map[string]*SourceSummary{}
	ensure := func(provider string) *SourceSummary {
		provider = strings.TrimSpace(provider)
		if provider == "" {
			provider = "unknown"
		}
		if summary, ok := byProvider[provider]; ok {
			return summary
		}
		summary := &SourceSummary{
			Provider:    provider,
			Label:       providerName(provider),
			Description: sourceDescription(provider),
			CanSync:     canSyncProvider(provider),
			IsLocal:     provider == "local",
		}
		byProvider[provider] = summary
		return summary
	}

	for _, repo := range repos {
		summary := ensure(repo.HostProvider)
		summary.RepoCount++
		if repo.Selected {
			summary.InstalledCount++
		}
		if repo.IndexedAt > 0 && repo.LastIndexError == "" {
			summary.IndexedCount++
		}
		if repo.LastIndexError != "" {
			summary.FailedCount++
		}
	}
	for _, identity := range identities {
		summary := ensure(identity.Provider)
		summary.Connected = true
		summary.Username = identity.Username
		summary.CanSync = canSyncProvider(identity.Provider)
	}

	out := make([]SourceSummary, 0, len(byProvider))
	for _, summary := range byProvider {
		// The dev-login identity is not a real code source; only surface it if
		// repos somehow ended up attached to it.
		if summary.Provider == "dev" && summary.RepoCount == 0 {
			continue
		}
		if !summary.Connected {
			summary.CanSync = false
		}
		out = append(out, *summary)
	}
	sort.Slice(out, func(i, j int) bool {
		if sourceSortRank(out[i].Provider) != sourceSortRank(out[j].Provider) {
			return sourceSortRank(out[i].Provider) < sourceSortRank(out[j].Provider)
		}
		return strings.ToLower(out[i].Label) < strings.ToLower(out[j].Label)
	})
	return out
}

func sourceDescription(provider string) string {
	switch provider {
	case string(codehost.GitHub):
		return "github.com repositories"
	case string(codehost.GitLab):
		return "gitlab.com repositories"
	case "local":
		return "Repository paths on this machine"
	}
	if codehost.IsGitLabProvider(provider) {
		baseURL := codehost.GitLabBaseURLFromProvider(provider, "")
		if host := codehost.HostFromBaseURL(baseURL); host != "" {
			return host + " repositories"
		}
	}
	return "Repository source"
}

func sourceSortRank(provider string) int {
	switch provider {
	case string(codehost.GitHub):
		return 0
	case string(codehost.GitLab):
		return 1
	case "local":
		return 3
	}
	if codehost.IsGitLabProvider(provider) {
		return 2
	}
	return 4
}

func repoSourceOptions(repos []store.Repo) []RepoSourceOption {
	counts := map[string]int{}
	labels := map[string]string{}
	gitLabCount := 0
	gitLabSources := map[string]struct{}{}
	for _, repo := range repos {
		value := providerSourceValue(repo.HostProvider)
		counts[value]++
		labels[value] = providerName(repo.HostProvider)
		if codehost.IsGitLabProvider(repo.HostProvider) {
			gitLabCount++
			gitLabSources[value] = struct{}{}
		}
	}

	var options []RepoSourceOption
	if len(gitLabSources) > 1 {
		options = append(options, RepoSourceOption{Value: "family:gitlab", Label: "All GitLab", Count: gitLabCount})
	}
	for value, count := range counts {
		options = append(options, RepoSourceOption{Value: value, Label: labels[value], Count: count})
	}
	sort.Slice(options, func(i, j int) bool {
		if options[i].Value == "family:gitlab" {
			return true
		}
		if options[j].Value == "family:gitlab" {
			return false
		}
		return strings.ToLower(options[i].Label) < strings.ToLower(options[j].Label)
	})
	return options
}

func providerSourceValue(provider string) string {
	return "provider:" + strings.TrimSpace(provider)
}

func sourceMatches(provider, source string) bool {
	source = strings.TrimSpace(source)
	switch source {
	case "", "all":
		return true
	case "family:gitlab", "gitlab":
		return codehost.IsGitLabProvider(provider)
	case "github":
		return provider == string(codehost.GitHub)
	case "local":
		return provider == "local"
	}
	if exact, ok := strings.CutPrefix(source, "provider:"); ok {
		return provider == exact
	}
	return provider == source
}

// chipMatches reports whether a repo view belongs under the given status chip.
// Chips collapse the old tab+status filters into one practical set:
// all | indexed | indexing | stale | failed | off.
func chipMatches(view RepoView, chip string) bool {
	switch chip {
	case "", "all":
		return true
	case "indexed":
		return view.Status == "indexed" && !view.Stale
	case "indexing":
		return view.Status == "queued" || view.Status == "running"
	case "stale":
		return view.Stale ||
			view.Status == "needs-index" ||
			(view.Status == "not-indexed" && view.Repo.Selected)
	case "failed":
		return view.Status == "failed"
	case "off":
		return !view.Repo.Selected
	default:
		return true
	}
}

// chipLabel is the human label for a status chip id.
func chipLabel(chip string) string {
	switch chip {
	case "indexed":
		return "Indexed"
	case "indexing":
		return "Indexing"
	case "stale":
		return "Stale"
	case "failed":
		return "Failed"
	case "off":
		return "Off"
	default:
		return "All"
	}
}

func (s *Server) searchPageData(ctx context.Context, user *store.User, r *http.Request) SearchPageData {
	return s.searchPageDataForParams(ctx, user, searchParamsFromQuery(r.URL.Query()), "/search")
}

func (s *Server) repoSearchPageData(ctx context.Context, user *store.User, repo store.Repo, r *http.Request) SearchPageData {
	params := searchParamsFromQuery(r.URL.Query())
	params.Repo = repo.FullName
	params.Repos = []string{repo.FullName}
	return s.searchPageDataForParams(ctx, user, params, fmt.Sprintf("/repo/%d", repo.ID))
}

func (s *Server) searchPageDataForParams(ctx context.Context, user *store.User, params SearchParams, basePath string) SearchPageData {
	repos, err := s.store.ListIndexedReposForUser(ctx, user.ID)
	if err != nil {
		return SearchPageData{User: user, Error: err.Error(), Params: params, BasePath: basePath}
	}
	data := SearchPageData{User: user, Repos: repos, Params: params, BasePath: basePath, StructuralLangs: structural.SupportedLanguages()}
	if strings.TrimSpace(params.Query) == "" {
		return data
	}
	if params.Structural() {
		result, err := s.structural.Search(ctx, structural.Request{
			Pattern:         params.Query,
			Lang:            params.Lang,
			RepoFilter:      params.Repo,
			RepoFilters:     params.Repos,
			BranchFilter:    params.Branch,
			PathFilter:      params.Path,
			TopPathFilter:   params.TopPath,
			ExtFilter:       params.Ext,
			SourceFilter:    params.Source,
			ProviderFilter:  params.Provider,
			FreshnessFilter: params.Freshness,
			DirtyFilter:     params.Dirty,
			Allowed:         repos,
		})
		if err != nil {
			data.Error = err.Error()
			return data
		}
		data.Result = result
		return data
	}
	result, err := s.search.Search(ctx, codesearch.Request{
		Query:            params.Query,
		RepoFilter:       params.Repo,
		RepoFilters:      params.Repos,
		BranchFilter:     params.Branch,
		PathFilter:       params.Path,
		TopPathFilter:    params.TopPath,
		ExtFilter:        params.Ext,
		LangFilter:       params.Lang,
		SourceFilter:     params.Source,
		ProviderFilter:   params.Provider,
		DirtyFilter:      params.Dirty,
		SymbolKindFilter: params.SymbolKind,
		FreshnessFilter:  params.Freshness,
		Sort:             params.Sort,
		Normalized:       params.Normalized,
		Symbols:          params.Symbols,
		Allowed:          repos,
	})
	if err != nil {
		data.Error = err.Error()
		return data
	}
	data.Result = result
	return data
}

func searchParamsFromQuery(values url.Values) SearchParams {
	repos := normalizeRepoParams(values["repo"])
	return SearchParams{
		Query:      values.Get("q"),
		Mode:       normalizeModeParam(values.Get("mode")),
		Repo:       firstString(repos),
		Repos:      repos,
		Branch:     values.Get("branch"),
		Path:       values.Get("path"),
		TopPath:    values.Get("top"),
		Ext:        normalizeExtParam(values.Get("ext")),
		Lang:       values.Get("lang"),
		Source:     normalizeSourceParam(values.Get("source")),
		Provider:   values.Get("provider"),
		Dirty:      normalizeDirtyParam(values.Get("dirty")),
		SymbolKind: values.Get("symbol_kind"),
		Freshness:  normalizeFreshnessParam(values.Get("freshness")),
		Sort:       normalizeSortParam(values.Get("sort")),
		Normalized: isTrueParam(values.Get("norm")) || isTrueParam(values.Get("normalized")),
		Symbols:    isTrueParam(values.Get("sym")),
	}
}

func normalizeRepoParams(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func repoFiltersFromParams(params SearchParams) []string {
	values := params.Repos
	if len(values) == 0 && strings.TrimSpace(params.Repo) != "" {
		values = []string{params.Repo}
	}
	return normalizeRepoParams(values)
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func toggleString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	out := make([]string, 0, len(values)+1)
	removed := false
	for _, existing := range normalizeRepoParams(values) {
		if existing == value {
			removed = true
			continue
		}
		out = append(out, existing)
	}
	if !removed {
		out = append(out, value)
	}
	return out
}

func repoSearchActive(r *http.Request) bool {
	return strings.TrimSpace(r.URL.Query().Get("q")) != ""
}

func normalizeExtParam(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == "__none__" {
		return value
	}
	if !strings.HasPrefix(value, ".") {
		value = "." + value
	}
	return value
}

func normalizeDirtyParam(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "dirty", "1", "true", "yes":
		return "dirty"
	case "clean", "0", "false", "no":
		return "clean"
	default:
		return ""
	}
}

func normalizeSourceParam(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "/"))
}

func normalizeModeParam(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "structural", "ast":
		return "structural"
	default:
		return ""
	}
}

func normalizeSortParam(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return ""
	case "relevance":
		return "relevance"
	case "repo", "repository":
		return "repo"
	case "path", "file":
		return "path"
	case "indexed_desc", "freshness", "newest", "newest_indexed":
		return "indexed_desc"
	case "indexed_asc", "oldest", "oldest_indexed":
		return "indexed_asc"
	case "match_count", "matches", "matches_desc":
		return "match_count"
	default:
		return ""
	}
}

func normalizeFreshnessParam(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "hour", "day", "week", "month", "older", "unknown":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	user, ok := s.currentUser(r)
	if ok {
		return user, true
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
	return nil, false
}

func (s *Server) currentUser(r *http.Request) (*store.User, bool) {
	raw, ok := s.readSignedCookie(r, sessionCookie)
	if !ok {
		return nil, false
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 2 {
		return nil, false
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > expires {
		return nil, false
	}
	userID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, false
	}
	user, err := s.store.GetUser(r.Context(), userID)
	return user, err == nil
}

func (s *Server) setSession(w http.ResponseWriter, userID int64) {
	expires := time.Now().Add(30 * 24 * time.Hour)
	value := fmt.Sprintf("%d:%d", userID, expires.Unix())
	s.setSignedCookie(w, sessionCookie, value, time.Until(expires))
}

func (s *Server) setSignedCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    s.sign(value),
		Path:     "/",
		HttpOnly: true,
		// Mark cookies Secure when the instance is served over TLS so they are
		// never replayed over plaintext.
		Secure:   strings.HasPrefix(s.cfg.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
	})
}

func (s *Server) readSignedCookie(r *http.Request, name string) (string, bool) {
	cookie, err := r.Cookie(name)
	if err != nil {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return "", false
	}
	raw, sig, ok := strings.Cut(string(payload), "|")
	if !ok {
		return "", false
	}
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(raw))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", false
	}
	return raw, true
}

func (s *Server) sign(value string) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(value))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(value + "|" + sig))
}

func (s *Server) oauthRedirectURI(provider string) string {
	return s.cfg.BaseURL + "/auth/" + provider + "/callback"
}

func (s *Server) authErrorTarget(r *http.Request) string {
	if _, ok := s.currentUser(r); ok {
		return "/sources"
	}
	return "/login"
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render template", "template", name, "error", err)
	}
}

func (s *Server) isHX(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("HX-Request"), "true")
}

func redirectError(w http.ResponseWriter, r *http.Request, target string, err error) {
	http.Redirect(w, r, target+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func randomToken() string {
	var buf [32]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:])
}

func repoHasIndexedBranch(repo store.Repo, branch string) bool {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return true
	}
	indexedBranches := repo.IndexedBranchList()
	if len(indexedBranches) == 0 {
		indexedBranches = repo.BranchesToIndex()
	}
	for _, indexed := range indexedBranches {
		if indexed == branch {
			return true
		}
	}
	return false
}

func readGitBranchFile(ctx context.Context, root, branch, relPath string) ([]byte, error) {
	if strings.TrimSpace(branch) == "" {
		branch = "HEAD"
	}
	ref := branch
	if branch != "HEAD" {
		ref = "refs/remotes/origin/" + branch
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "show", ref+":"+filepath.ToSlash(relPath))
	return cmd.Output()
}

func branchPrimary(branches []string) string {
	branches = compactBranches(branches)
	if len(branches) == 0 {
		return ""
	}
	return branches[0]
}

func branchExtraCount(branches []string) int {
	count := len(compactBranches(branches))
	if count <= 1 {
		return 0
	}
	return count - 1
}

func branchTitle(branches []string) string {
	return strings.Join(compactBranches(branches), ", ")
}

// shortCommit abbreviates a commit hash for the file:line@commit provenance
// badge; returns "" for an empty value.
func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) >= 8 {
		return commit[:8]
	}
	return commit
}

func compactBranches(branches []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(branches))
	for _, branch := range branches {
		branch = strings.TrimSpace(branch)
		if branch == "" {
			continue
		}
		if _, ok := seen[branch]; ok {
			continue
		}
		seen[branch] = struct{}{}
		out = append(out, branch)
	}
	return out
}

func safeJoin(root, rel string) (string, error) {
	if root == "" {
		return "", errors.New("root is empty")
	}
	if filepath.IsAbs(rel) {
		return "", errors.New("absolute paths are not allowed")
	}
	cleanRel := filepath.Clean(filepath.FromSlash(rel))
	if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes repository")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(rootAbs, cleanRel)
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(filepath.Separator)) {
		return "", errors.New("path escapes repository")
	}
	// The lexical check above cannot see symlinks: a cloned repo containing
	// `evil -> /etc` would pass it and let a read escape the repository. Resolve
	// symlinks and re-check containment against the resolved root.
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(fullAbs)
	if errors.Is(err, fs.ErrNotExist) {
		// Nothing exists at the resolved target, so nothing can be read through
		// it; callers stat the returned path and surface their own not-found.
		return fullAbs, nil
	}
	if err != nil {
		return "", err
	}
	if resolved != rootResolved && !strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
		return "", errors.New("path escapes repository")
	}
	return resolved, nil
}

func codeLines(path string, content []byte, focusLine int, symbolLines map[string]int) []CodeLine {
	htmls := highlightLines(path, content, symbolLines)
	out := make([]CodeLine, 0, len(htmls))
	for i, h := range htmls {
		number := i + 1
		out = append(out, CodeLine{Number: number, HTML: h, Highlight: number == focusLine})
	}
	return out
}

// symbolDefLines maps each symbol name to the line of its first definition in the
// file, so in-body identifiers can link to where they are defined. Names defined
// more than once (e.g. methods sharing a name) resolve to the earliest line.
func symbolDefLines(symbols []indexer.FileSymbol) map[string]int {
	if len(symbols) == 0 {
		return nil
	}
	lines := make(map[string]int, len(symbols))
	for _, sym := range symbols {
		// The package/module clause is not a useful go-to-definition target and
		// would otherwise shadow a same-named function (e.g. Go's `main`).
		if sym.Name == "" || sym.Line <= 0 || sym.Kind == "package" {
			continue
		}
		if existing, ok := lines[sym.Name]; !ok || sym.Line < existing {
			lines[sym.Name] = sym.Line
		}
	}
	return lines
}

func codeURL(repoID int64, path string, line int, branchSets ...[]string) string {
	segments := strings.Split(filepath.ToSlash(path), "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	u := fmt.Sprintf("/code/%d/%s", repoID, strings.Join(segments, "/"))
	values := url.Values{}
	if len(branchSets) > 0 {
		for _, branch := range branchSets[0] {
			branch = strings.TrimSpace(branch)
			if branch != "" && branch != "HEAD" {
				values.Set("branch", branch)
				break
			}
		}
	}
	if line > 0 {
		values.Set("line", strconv.Itoa(line))
	}
	if encoded := values.Encode(); encoded != "" {
		u += "?" + encoded
	}
	return u
}

func filterValues(filter RepoFilter) url.Values {
	values := url.Values{}
	if filter.Query != "" {
		values.Set("q", filter.Query)
	}
	if filter.Source != "" && filter.Source != "all" {
		values.Set("source", filter.Source)
	}
	if filter.Status != "" && filter.Status != defaultRepoStatus {
		values.Set("status", filter.Status)
	}
	return values
}

func filterQuery(filter RepoFilter) string {
	encoded := filterValues(filter).Encode()
	if encoded == "" {
		return ""
	}
	return "?" + encoded
}

func manageFilterURL(filter RepoFilter) string {
	encoded := filterValues(filter).Encode()
	if encoded == "" {
		return "/repos/manage"
	}
	return "/repos/manage?" + encoded
}

// chipURL builds a manage-page URL that switches the active status chip while
// preserving the current search query and source scope.
func chipURL(filter RepoFilter, chip string) template.URL {
	filter.Status = chip
	return template.URL(manageFilterURL(filter)) // #nosec G203 -- constant path; query values are URL-encoded
}

// sourceFilterURL builds a manage-page URL that scopes to a source value while
// preserving the current search query and status chip.
func sourceFilterURL(filter RepoFilter, source string) template.URL {
	filter.Source = source
	return template.URL(manageFilterURL(filter)) // #nosec G203 -- constant path; query values are URL-encoded
}

// referencesURL builds a repo-scoped, word-boundary find-references URL for the
// file-viewer symbol outline. It returns template.URL so html/template trusts the
// already-encoded query string instead of percent-escaping it a second time.
func referencesURL(base, repoFullName, symbol string) template.URL {
	values := url.Values{}
	values.Set("repo", repoFullName)
	values.Set("q", `\b`+symbol+`\b`)
	return template.URL(base + "?" + values.Encode()) // #nosec G203 -- base is a constant path; values are URL-encoded
}

func sourceManageURL(provider string) template.URL {
	values := url.Values{}
	values.Set("source", providerSourceValue(provider))
	return template.URL("/repos/manage?" + values.Encode()) // #nosec G203 -- path is constant; query values are URL-encoded
}

func repoSearchURL(repoID int64, params SearchParams) string {
	return searchURL(fmt.Sprintf("/repo/%d", repoID), params)
}

func searchURL(base string, params SearchParams) string {
	values := searchValues(params)
	if encoded := values.Encode(); encoded != "" {
		return base + "?" + encoded
	}
	return base
}

func searchValues(params SearchParams) url.Values {
	values := url.Values{}
	if params.Query != "" {
		values.Set("q", params.Query)
	}
	if params.Mode != "" {
		values.Set("mode", params.Mode)
	}
	for _, repo := range repoFiltersFromParams(params) {
		values.Add("repo", repo)
	}
	if params.Branch != "" {
		values.Set("branch", params.Branch)
	}
	if params.Path != "" {
		values.Set("path", params.Path)
	}
	if params.TopPath != "" {
		values.Set("top", params.TopPath)
	}
	if params.Ext != "" {
		values.Set("ext", params.Ext)
	}
	if params.Lang != "" {
		values.Set("lang", params.Lang)
	}
	if params.Source != "" {
		values.Set("source", params.Source)
	}
	if params.Provider != "" {
		values.Set("provider", params.Provider)
	}
	if params.Dirty != "" {
		values.Set("dirty", params.Dirty)
	}
	if params.SymbolKind != "" {
		values.Set("symbol_kind", params.SymbolKind)
	}
	if params.Freshness != "" {
		values.Set("freshness", params.Freshness)
	}
	if params.Sort != "" && params.Sort != "relevance" {
		values.Set("sort", params.Sort)
	}
	if params.Normalized {
		values.Set("norm", "1")
	}
	if params.Symbols {
		values.Set("sym", "1")
	}
	return values
}

func showFacetGroup(base string, group codesearch.FacetGroup) bool {
	return !(strings.HasPrefix(base, "/repo/") && group.Field == "repo")
}

func searchSortLabel(sort string) string {
	switch normalizeSortParam(sort) {
	case "repo":
		return "repository"
	case "path":
		return "path"
	case "indexed_desc":
		return "newest index"
	case "indexed_asc":
		return "oldest index"
	case "match_count":
		return "most matches"
	default:
		return "relevance"
	}
}

func searchSortDescription(sort string) string {
	switch normalizeSortParam(sort) {
	case "repo":
		return "Grouped by source and repository, then file path."
	case "path":
		return "Alphabetical by file path, then repository."
	case "indexed_desc":
		return "Repositories indexed most recently first."
	case "indexed_asc":
		return "Repositories indexed least recently first; never-indexed timestamps last."
	case "match_count":
		return "Files with the most matching lines first."
	default:
		return "Zoekt relevance score, including file ranking and result diversity."
	}
}

func facetURL(base string, params SearchParams, field, value string) template.URL {
	updated := params
	if field == "repo" {
		updated.Repos = toggleString(repoFiltersFromParams(params), value)
		updated.Repo = firstString(updated.Repos)
		return template.URL(searchURL(base, updated)) // #nosec G203 -- base is generated internally; query values are URL-encoded
	}
	if activeFacetValue(params, field) == value {
		setFacetValue(&updated, field, "")
	} else {
		setFacetValue(&updated, field, value)
	}
	return template.URL(searchURL(base, updated)) // #nosec G203 -- base is generated internally; query values are URL-encoded
}

func clearSearchFiltersURL(base string, params SearchParams) template.URL {
	params.Repo = ""
	params.Repos = nil
	params.Branch = ""
	params.Path = ""
	params.TopPath = ""
	params.Ext = ""
	if !params.Structural() {
		// Structural search requires a language; clearing filters must not
		// break the query itself.
		params.Lang = ""
	}
	params.Source = ""
	params.Provider = ""
	params.Dirty = ""
	params.SymbolKind = ""
	params.Freshness = ""
	return template.URL(searchURL(base, params)) // #nosec G203 -- base is generated internally; query values are URL-encoded
}

// SearchFilterChip is one active search filter rendered as a removable chip.
type SearchFilterChip struct {
	Field     string
	Value     string
	RemoveURL template.URL
}

// activeSearchFilters lists every filter narrowing the current search, each
// with a URL that removes just that filter. This is what makes facet-applied
// filters (branch, language, …) visible and individually clearable instead of
// riding along as hidden form inputs.
func activeSearchFilters(base string, params SearchParams) []SearchFilterChip {
	chips := make([]SearchFilterChip, 0, 8)
	if !strings.HasPrefix(base, "/repo/") {
		for _, repo := range repoFiltersFromParams(params) {
			chips = append(chips, SearchFilterChip{
				Field:     "repo",
				Value:     repo,
				RemoveURL: facetURL(base, params, "repo", repo),
			})
		}
	}
	if strings.TrimSpace(params.Path) != "" {
		updated := params
		updated.Path = ""
		chips = append(chips, SearchFilterChip{
			Field:     "path",
			Value:     params.Path,
			RemoveURL: template.URL(searchURL(base, updated)), // #nosec G203 -- base is generated internally; query values are URL-encoded
		})
	}
	fields := []struct{ field, label, value string }{
		{"branch", "branch", params.Branch},
		{"top_path", "folder", params.TopPath},
		{"extension", "extension", params.Ext},
		{"language", "language", params.Lang},
		{"source", "source", params.Source},
		{"provider", "provider", params.Provider},
		{"dirty", "working tree", params.Dirty},
		{"symbol_kind", "symbol", params.SymbolKind},
		{"freshness", "freshness", params.Freshness},
	}
	for _, item := range fields {
		if strings.TrimSpace(item.value) == "" {
			continue
		}
		// In structural mode the language is part of the query, not a
		// removable filter.
		if item.field == "language" && params.Structural() {
			continue
		}
		updated := params
		setFacetValue(&updated, item.field, "")
		chips = append(chips, SearchFilterChip{
			Field:     item.label,
			Value:     codesearch.FacetValueLabel(item.field, item.value),
			RemoveURL: template.URL(searchURL(base, updated)), // #nosec G203 -- base is generated internally; query values are URL-encoded
		})
	}
	return chips
}

func activeSearchFilterCount(base string, params SearchParams) int {
	count := 0
	lang := params.Lang
	if params.Structural() {
		lang = ""
	}
	values := []string{params.Branch, params.Path, params.TopPath, params.Ext, lang, params.Source, params.Provider, params.Dirty, params.SymbolKind, params.Freshness}
	if !strings.HasPrefix(base, "/repo/") && len(repoFiltersFromParams(params)) > 0 {
		count++
	}
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			count++
		}
	}
	return count
}

func activeFacetValue(params SearchParams, field string) string {
	switch field {
	case "source":
		return params.Source
	case "repo":
		return params.Repo
	case "branch":
		return params.Branch
	case "language":
		return params.Lang
	case "top_path":
		return params.TopPath
	case "extension":
		return params.Ext
	case "provider":
		return params.Provider
	case "dirty":
		return params.Dirty
	case "symbol_kind":
		return params.SymbolKind
	case "freshness":
		return params.Freshness
	default:
		return ""
	}
}

func setFacetValue(params *SearchParams, field, value string) {
	switch field {
	case "source":
		params.Source = value
	case "repo":
		params.Repo = value
		if value == "" {
			params.Repos = nil
		} else {
			params.Repos = []string{value}
		}
	case "branch":
		params.Branch = value
	case "language":
		params.Lang = value
	case "top_path":
		params.TopPath = value
	case "extension":
		params.Ext = value
	case "provider":
		params.Provider = value
	case "dirty":
		params.Dirty = value
	case "symbol_kind":
		params.SymbolKind = value
	case "freshness":
		params.Freshness = value
	}
}

func repoReferencesURL(repoID int64, symbol string) template.URL {
	return template.URL(repoSearchURL(repoID, SearchParams{Query: `\b` + symbol + `\b`})) // #nosec G203 -- repoID is numeric; query values are URL-encoded
}

func localRepoFullName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "repository"
	}
	name = strings.Trim(name, "/")
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.ReplaceAll(name, "/", "-")
	return "local/" + name
}

func displayUserName(user *store.User) string {
	if user == nil {
		return ""
	}
	if user.Name == "Development User" || user.Name == "" {
		return "Local admin"
	}
	return user.Name
}

func repoTitle(repo store.Repo) string {
	if repo.Name != "" {
		return repo.Name
	}
	return repo.FullName
}

func repoSubtitle(repo store.Repo) string {
	if repo.HostProvider == "local" {
		if repo.LocalPath == "" {
			return "Local repository"
		}
		return "Local path: " + shortPath(repo.LocalPath)
	}
	return repo.FullName
}

func repoOptionLabel(repo store.Repo) string {
	if repo.HostProvider == "local" {
		return repoTitle(repo) + " (local)"
	}
	return repo.FullName
}

func shortPath(path string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if path == home {
			return "~"
		}
		if strings.HasPrefix(path, home+string(filepath.Separator)) {
			return "~" + strings.TrimPrefix(path, home)
		}
	}
	return path
}

func formatUnix(ts int64) string {
	if ts <= 0 {
		return "never"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

func sinceUnix(ts int64) string {
	if ts <= 0 {
		return "never"
	}
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func humanizeDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func statusClass(status string) string {
	switch status {
	case "succeeded":
		return "badge-success"
	case "failed":
		return "badge-error"
	case "running":
		return "badge-warning"
	case "queued":
		return "badge-info"
	case "cancelled":
		return "badge-ghost"
	default:
		return "badge-ghost"
	}
}

func providerName(provider string) string {
	switch provider {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	case "local":
		return "Local"
	case "dev":
		return "Development"
	default:
		if codehost.IsGitLabProvider(provider) {
			baseURL := codehost.GitLabBaseURLFromProvider(provider, "")
			return "GitLab " + codehost.HostFromBaseURL(baseURL)
		}
		return provider
	}
}

func canSyncProvider(provider string) bool {
	return provider == string(codehost.GitHub) || codehost.IsGitLabProvider(provider)
}
