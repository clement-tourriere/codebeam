package codehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Provider string

const (
	GitHub               Provider = "github"
	GitLab               Provider = "gitlab"
	gitLabProviderPrefix          = "gitlab:"
)

type OAuthConfig struct {
	Provider     Provider
	BaseURL      string
	ClientID     string
	ClientSecret string
}

// OAuthTokens is the complete credential returned by a code host. GitLab
// access tokens last two hours and rotates both tokens on refresh, so retaining
// only AccessToken is not sufficient for unattended indexing.
type OAuthTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
}

type UserInfo struct {
	ProviderUserID string
	Username       string
	Email          string
	Name           string
	AvatarURL      string
}

type Repo struct {
	Provider      Provider
	ProviderID    string
	Name          string
	FullName      string
	CloneURL      string
	WebURL        string
	DefaultBranch string
	Private       bool
}

type Client struct {
	config OAuthConfig
	http   *http.Client
}

func New(config OAuthConfig) *Client {
	if config.Provider == GitLab && config.BaseURL == "" {
		config.BaseURL = "https://gitlab.com"
	}
	return &Client{
		config: config,
		http:   &http.Client{Timeout: 20 * time.Second},
	}
}

func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("base URL is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("base URL must use http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("base URL must include a host")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func GitLabProviderKey(baseURL string) (string, error) {
	normalized, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return "", err
	}
	return gitLabProviderPrefix + normalized, nil
}

func IsGitLabProvider(provider string) bool {
	return provider == string(GitLab) || strings.HasPrefix(provider, gitLabProviderPrefix)
}

func GitLabBaseURLFromProvider(provider, fallback string) string {
	if strings.HasPrefix(provider, gitLabProviderPrefix) {
		return strings.TrimPrefix(provider, gitLabProviderPrefix)
	}
	if fallback == "" {
		return "https://gitlab.com"
	}
	return fallback
}

func HostFromBaseURL(baseURL string) string {
	normalized, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return strings.TrimSpace(baseURL)
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Host == "" {
		return normalized
	}
	if parsed.Path == "" || parsed.Path == "/" {
		return parsed.Host
	}
	return parsed.Host + strings.TrimRight(parsed.Path, "/")
}

func (c *Client) Configured() bool {
	return c.config.ClientID != "" && c.config.ClientSecret != ""
}

func (c *Client) Provider() Provider {
	return c.config.Provider
}

func (c *Client) AuthCodeURL(state, redirectURI string) (string, error) {
	if !c.Configured() {
		return "", fmt.Errorf("%s OAuth is not configured", c.config.Provider)
	}

	values := url.Values{}
	values.Set("client_id", c.config.ClientID)
	values.Set("redirect_uri", redirectURI)
	values.Set("state", state)

	switch c.config.Provider {
	case GitHub:
		values.Set("scope", "read:user user:email repo")
		return "https://github.com/login/oauth/authorize?" + values.Encode(), nil
	case GitLab:
		values.Set("response_type", "code")
		values.Set("scope", "read_user read_api read_repository")
		return strings.TrimRight(c.config.BaseURL, "/") + "/oauth/authorize?" + values.Encode(), nil
	default:
		return "", fmt.Errorf("unsupported OAuth provider %q", c.config.Provider)
	}
}

// ExchangeCode is retained for callers that only need the access token. New
// server code should use ExchangeCodeTokens so refreshability is not discarded.
func (c *Client) ExchangeCode(ctx context.Context, code, state, redirectURI string) (string, error) {
	tokens, err := c.ExchangeCodeTokens(ctx, code, state, redirectURI)
	if err != nil {
		return "", err
	}
	return tokens.AccessToken, nil
}

func (c *Client) ExchangeCodeTokens(ctx context.Context, code, state, redirectURI string) (OAuthTokens, error) {
	if !c.Configured() {
		return OAuthTokens{}, fmt.Errorf("%s OAuth is not configured", c.config.Provider)
	}

	values := url.Values{}
	values.Set("client_id", c.config.ClientID)
	values.Set("client_secret", c.config.ClientSecret)
	values.Set("code", code)
	values.Set("redirect_uri", redirectURI)

	endpoint, err := c.oauthTokenEndpoint()
	if err != nil {
		return OAuthTokens{}, err
	}
	switch c.config.Provider {
	case GitHub:
		values.Set("state", state)
	case GitLab:
		values.Set("grant_type", "authorization_code")
	}
	return c.postOAuthToken(ctx, endpoint, values)
}

// RefreshOAuthToken rotates an expiring provider credential. GitLab requires
// the same redirect URI as the original authorization and invalidates both old
// tokens after a successful exchange.
func (c *Client) RefreshOAuthToken(ctx context.Context, refreshToken, redirectURI string) (OAuthTokens, error) {
	if !c.Configured() {
		return OAuthTokens{}, fmt.Errorf("%s OAuth is not configured", c.config.Provider)
	}
	if strings.TrimSpace(refreshToken) == "" {
		return OAuthTokens{}, errors.New("OAuth refresh token is required")
	}
	endpoint, err := c.oauthTokenEndpoint()
	if err != nil {
		return OAuthTokens{}, err
	}
	values := url.Values{
		"client_id":     {c.config.ClientID},
		"client_secret": {c.config.ClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	if redirectURI != "" {
		values.Set("redirect_uri", redirectURI)
	}
	return c.postOAuthToken(ctx, endpoint, values)
}

func (c *Client) oauthTokenEndpoint() (string, error) {
	switch c.config.Provider {
	case GitHub:
		return "https://github.com/login/oauth/access_token", nil
	case GitLab:
		return strings.TrimRight(c.config.BaseURL, "/") + "/oauth/token", nil
	default:
		return "", fmt.Errorf("unsupported OAuth provider %q", c.config.Provider)
	}
}

func (c *Client) postOAuthToken(ctx context.Context, endpoint string, values url.Values) (OAuthTokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return OAuthTokens{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		CreatedAt    int64  `json:"created_at"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	if err := c.doJSON(req, &out); err != nil {
		return OAuthTokens{}, err
	}
	if out.Error != "" {
		if out.Description != "" {
			return OAuthTokens{}, fmt.Errorf("%s OAuth error: %s", c.config.Provider, out.Description)
		}
		return OAuthTokens{}, fmt.Errorf("%s OAuth error: %s", c.config.Provider, out.Error)
	}
	if out.AccessToken == "" {
		return OAuthTokens{}, errors.New("OAuth response did not include an access token")
	}
	expiresAt := int64(0)
	if out.ExpiresIn > 0 {
		createdAt := out.CreatedAt
		if createdAt == 0 {
			createdAt = time.Now().Unix()
		}
		expiresAt = createdAt + out.ExpiresIn
	}
	return OAuthTokens{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, ExpiresAt: expiresAt}, nil
}

func (c *Client) FetchUser(ctx context.Context, token string) (UserInfo, error) {
	switch c.config.Provider {
	case GitHub:
		var out struct {
			ID        int64  `json:"id"`
			Login     string `json:"login"`
			Email     string `json:"email"`
			Name      string `json:"name"`
			AvatarURL string `json:"avatar_url"`
		}
		req, err := c.apiRequest(ctx, http.MethodGet, "https://api.github.com/user", token, nil)
		if err != nil {
			return UserInfo{}, err
		}
		if err := c.doJSON(req, &out); err != nil {
			return UserInfo{}, err
		}
		email := out.Email
		if email == "" {
			email = fmt.Sprintf("%s@github.local", out.Login)
		}
		return UserInfo{
			ProviderUserID: strconv.FormatInt(out.ID, 10),
			Username:       out.Login,
			Email:          email,
			Name:           out.Name,
			AvatarURL:      out.AvatarURL,
		}, nil
	case GitLab:
		var out struct {
			ID        int64  `json:"id"`
			Username  string `json:"username"`
			Email     string `json:"email"`
			Name      string `json:"name"`
			AvatarURL string `json:"avatar_url"`
		}
		req, err := c.apiRequest(ctx, http.MethodGet, strings.TrimRight(c.config.BaseURL, "/")+"/api/v4/user", token, nil)
		if err != nil {
			return UserInfo{}, err
		}
		if err := c.doJSON(req, &out); err != nil {
			return UserInfo{}, err
		}
		return UserInfo{
			ProviderUserID: strconv.FormatInt(out.ID, 10),
			Username:       out.Username,
			Email:          out.Email,
			Name:           out.Name,
			AvatarURL:      out.AvatarURL,
		}, nil
	default:
		return UserInfo{}, fmt.Errorf("unsupported provider %q", c.config.Provider)
	}
}

// ListRepos returns the repositories the token's owner can access. The second
// result reports whether the listing is complete: it is false when the provider
// has more repositories than the pagination cap could fetch, so callers must not
// treat an absent repo as "access revoked" in that case.
func (c *Client) ListRepos(ctx context.Context, token string) ([]Repo, bool, error) {
	switch c.config.Provider {
	case GitHub:
		return c.listGitHubRepos(ctx, token)
	case GitLab:
		return c.listGitLabRepos(ctx, token)
	default:
		return nil, false, fmt.Errorf("unsupported provider %q", c.config.Provider)
	}
}

func (c *Client) FetchGitHubPublicRepo(ctx context.Context, spec string) (Repo, error) {
	owner, name, err := ParseGitHubRepoSpec(spec)
	if err != nil {
		return Repo{}, err
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Repo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "codebeam")
	var out struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		FullName      string `json:"full_name"`
		CloneURL      string `json:"clone_url"`
		HTMLURL       string `json:"html_url"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
	}
	if err := c.doJSON(req, &out); err != nil {
		return Repo{}, err
	}
	if out.ID == 0 || out.FullName == "" {
		return Repo{}, errors.New("GitHub repository response was incomplete")
	}
	if out.Private {
		return Repo{}, errors.New("that GitHub repository is private; connect GitHub OAuth instead")
	}
	return Repo{
		Provider:      GitHub,
		ProviderID:    strconv.FormatInt(out.ID, 10),
		Name:          out.Name,
		FullName:      "github.com/" + out.FullName,
		CloneURL:      out.CloneURL,
		WebURL:        out.HTMLURL,
		DefaultBranch: out.DefaultBranch,
		Private:       false,
	}, nil
}

func ParseGitHubRepoSpec(raw string) (owner, repo string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("GitHub repository URL is required")
	}
	if strings.HasPrefix(raw, "git@github.com:") {
		raw = strings.TrimPrefix(raw, "git@github.com:")
	}
	if strings.HasPrefix(raw, "github.com/") {
		raw = "https://" + raw
	}
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", "", err
		}
		if !strings.EqualFold(parsed.Host, "github.com") {
			return "", "", errors.New("GitHub repository must be hosted on github.com")
		}
		raw = parsed.Path
	}
	raw = strings.Trim(raw, "/")
	parts := strings.Split(raw, "/")
	if len(parts) < 2 {
		return "", "", errors.New("enter a GitHub repository as owner/name or https://github.com/owner/name")
	}
	owner = strings.TrimSpace(parts[0])
	repo = strings.TrimSuffix(strings.TrimSpace(parts[1]), ".git")
	if owner == "" || repo == "" || strings.HasPrefix(owner, ".") || strings.HasPrefix(repo, ".") {
		return "", "", errors.New("invalid GitHub repository name")
	}
	return owner, repo, nil
}

func (c *Client) listGitHubRepos(ctx context.Context, token string) ([]Repo, bool, error) {
	var repos []Repo
	complete := false
	for page := 1; page <= 20; page++ {
		endpoint := fmt.Sprintf("https://api.github.com/user/repos?per_page=100&page=%d&affiliation=owner,collaborator,organization_member&sort=full_name", page)
		req, err := c.apiRequest(ctx, http.MethodGet, endpoint, token, nil)
		if err != nil {
			return nil, false, err
		}
		var out []struct {
			ID            int64  `json:"id"`
			Name          string `json:"name"`
			FullName      string `json:"full_name"`
			CloneURL      string `json:"clone_url"`
			HTMLURL       string `json:"html_url"`
			DefaultBranch string `json:"default_branch"`
			Private       bool   `json:"private"`
		}
		if err := c.doJSON(req, &out); err != nil {
			return nil, false, err
		}
		for _, repo := range out {
			repos = append(repos, Repo{
				Provider:      GitHub,
				ProviderID:    strconv.FormatInt(repo.ID, 10),
				Name:          repo.Name,
				FullName:      "github.com/" + repo.FullName,
				CloneURL:      repo.CloneURL,
				WebURL:        repo.HTMLURL,
				DefaultBranch: repo.DefaultBranch,
				Private:       repo.Private,
			})
		}
		if len(out) < 100 {
			complete = true
			break
		}
	}
	return repos, complete, nil
}

func (c *Client) listGitLabRepos(ctx context.Context, token string) ([]Repo, bool, error) {
	var repos []Repo
	complete := false
	for page := 1; page <= 20; page++ {
		endpoint := fmt.Sprintf("%s/api/v4/projects?membership=true&simple=true&per_page=100&page=%d&order_by=path&sort=asc", strings.TrimRight(c.config.BaseURL, "/"), page)
		req, err := c.apiRequest(ctx, http.MethodGet, endpoint, token, nil)
		if err != nil {
			return nil, false, err
		}
		var out []struct {
			ID                int64  `json:"id"`
			Name              string `json:"name"`
			PathWithNamespace string `json:"path_with_namespace"`
			HTTPURLToRepo     string `json:"http_url_to_repo"`
			WebURL            string `json:"web_url"`
			DefaultBranch     string `json:"default_branch"`
			Visibility        string `json:"visibility"`
		}
		if err := c.doJSON(req, &out); err != nil {
			return nil, false, err
		}
		host := strings.TrimPrefix(strings.TrimRight(c.config.BaseURL, "/"), "https://")
		host = strings.TrimPrefix(host, "http://")
		for _, repo := range out {
			repos = append(repos, Repo{
				Provider:      GitLab,
				ProviderID:    strconv.FormatInt(repo.ID, 10),
				Name:          repo.Name,
				FullName:      host + "/" + repo.PathWithNamespace,
				CloneURL:      repo.HTTPURLToRepo,
				WebURL:        repo.WebURL,
				DefaultBranch: repo.DefaultBranch,
				Private:       repo.Visibility != "public",
			})
		}
		if len(out) < 100 {
			complete = true
			break
		}
	}
	return repos, complete, nil
}

func (c *Client) apiRequest(ctx context.Context, method, endpoint, token string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

func (c *Client) doJSON(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s returned %s: %s", req.Method, req.URL.String(), resp.Status, strings.TrimSpace(string(body)))
	}
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode JSON from %s: %w", req.URL.String(), err)
	}
	return nil
}
