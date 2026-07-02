package web

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	codesearch "github.com/ctourriere/codebeam/internal/search"
	"github.com/ctourriere/codebeam/internal/store"
	"github.com/ctourriere/codebeam/internal/structural"
)

const maxAPIReadLines = 500

type apiSearchResponse struct {
	Query string `json:"query"`
	// Engine identifies which backend produced the results: "zoekt"
	// (lexical/regex/symbols) or "structural" (ast-grep AST patterns).
	Engine      string          `json:"engine"`
	EngineQuery string          `json:"engine_query,omitempty"`
	ZoektQuery  string          `json:"zoekt_query,omitempty"`
	Sort        string          `json:"sort"`
	Truncated   bool            `json:"truncated,omitempty"`
	Stats       apiSearchStats  `json:"stats"`
	Facets      []apiFacetGroup `json:"facets,omitempty"`
	Files       []apiSearchFile `json:"files"`
}

type apiSearchStats struct {
	FileCount  int   `json:"file_count"`
	MatchCount int   `json:"match_count"`
	DurationMS int64 `json:"duration_ms"`
}

type apiSearchFile struct {
	RepoID     int64           `json:"repo_id"`
	Repository string          `json:"repository"`
	RepoName   string          `json:"repo_name"`
	Provider   string          `json:"provider,omitempty"`
	Branches   []string        `json:"branches,omitempty"`
	Path       string          `json:"path"`
	Language   string          `json:"language,omitempty"`
	IndexedAt  int64           `json:"indexed_at,omitempty"`
	Commit     string          `json:"commit,omitempty"`
	Dirty      bool            `json:"dirty,omitempty"`
	Lines      []apiSearchLine `json:"lines"`
}

type apiSearchLine struct {
	Number      int              `json:"number"`
	Before      []apiContextLine `json:"before,omitempty"`
	After       []apiContextLine `json:"after,omitempty"`
	Segments    []apiSegment     `json:"segments"`
	FileName    bool             `json:"file_name,omitempty"`
	SymbolKinds []string         `json:"symbol_kinds,omitempty"`
	// MetaVars holds structural-search captures ($VAR → matched text).
	MetaVars map[string]string `json:"meta_vars,omitempty"`
}

type apiContextLine struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}

type apiSegment struct {
	Text  string `json:"text"`
	Match bool   `json:"match"`
}

type apiFacetGroup struct {
	Field  string          `json:"field"`
	Label  string          `json:"label"`
	Values []apiFacetValue `json:"values"`
}

type apiFacetValue struct {
	Value  string `json:"value"`
	Label  string `json:"label"`
	Count  int    `json:"count"`
	Active bool   `json:"active,omitempty"`
}

type apiReadResponse struct {
	RepoID     int64         `json:"repo_id"`
	Repository string        `json:"repository"`
	Path       string        `json:"path"`
	StartLine  int           `json:"start_line"`
	EndLine    int           `json:"end_line"`
	TotalLines int           `json:"total_lines"`
	Lines      []apiReadLine `json:"lines"`
}

type apiReadLine struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}

type apiErrorResponse struct {
	Error string `json:"error"`
}

func (s *Server) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user, ok := s.requireAPIUser(w, r)
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		apiError(w, http.StatusBadRequest, "query parameter q is required")
		return
	}
	repos, err := s.store.ListIndexedReposForUser(r.Context(), user.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	params := searchParamsFromQuery(r.URL.Query())
	if params.Structural() {
		result, err := s.structural.Search(r.Context(), structural.Request{
			Pattern:         query,
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
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		apiJSON(w, http.StatusOK, apiSearchResponse{
			Query:       result.Query,
			Engine:      "structural",
			EngineQuery: result.Query,
			Sort:        "path",
			Truncated:   result.Truncated,
			Stats: apiSearchStats{
				FileCount:  result.FileCount,
				MatchCount: result.MatchCount,
				DurationMS: result.Duration.Milliseconds(),
			},
			Facets: apiFacetGroups(result.Facets),
			Files:  apiSearchFiles(result.Files),
		})
		return
	}
	result, err := s.search.Search(r.Context(), codesearch.Request{
		Query:            query,
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
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	sortValue := params.Sort
	if sortValue == "" {
		sortValue = "relevance"
	}
	apiJSON(w, http.StatusOK, apiSearchResponse{
		Query:       result.Query,
		Engine:      "zoekt",
		EngineQuery: result.ZoektQuery,
		ZoektQuery:  result.ZoektQuery,
		Sort:        sortValue,
		Truncated:   result.Truncated,
		Stats: apiSearchStats{
			FileCount:  result.FileCount,
			MatchCount: result.MatchCount,
			DurationMS: result.Duration.Milliseconds(),
		},
		Facets: apiFacetGroups(result.Facets),
		Files:  apiSearchFiles(result.Files),
	})
}

func (s *Server) handleAPIRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user, ok := s.requireAPIUser(w, r)
	if !ok {
		return
	}
	repo, err := s.repoFromAPIParam(r, user.ID)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	relPath := strings.TrimSpace(r.URL.Query().Get("path"))
	if relPath == "" {
		apiError(w, http.StatusBadRequest, "path parameter is required")
		return
	}
	root := s.indexer.SourceRoot(*repo)
	filePath, err := safeJoin(root, relPath)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	var content []byte
	if branch != "" && repo.HostProvider != "local" {
		if !repoHasIndexedBranch(*repo, branch) {
			apiError(w, http.StatusBadRequest, "branch is not indexed for this repository")
			return
		}
		content, err = readGitBranchFile(r.Context(), root, branch, relPath)
		if err != nil || len(content) > 4<<20 {
			apiError(w, http.StatusNotFound, "file not found")
			return
		}
	} else {
		info, err := os.Stat(filePath)
		if err != nil || info.IsDir() || info.Size() > 4<<20 {
			apiError(w, http.StatusNotFound, "file not found")
			return
		}
		content, err = os.ReadFile(filePath)
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	allLines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(allLines) == 1 && allLines[0] == "" {
		allLines = nil
	}
	start, end, err := apiLineRange(r, len(allLines))
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	lines := make([]apiReadLine, 0, max(0, end-start+1))
	for i := start; i <= end && i > 0; i++ {
		lines = append(lines, apiReadLine{Number: i, Text: allLines[i-1]})
	}
	apiJSON(w, http.StatusOK, apiReadResponse{
		RepoID:     repo.ID,
		Repository: repo.FullName,
		Path:       relPath,
		StartLine:  start,
		EndLine:    end,
		TotalLines: len(allLines),
		Lines:      lines,
	})
}

func (s *Server) requireAPIUser(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	user, ok := s.apiUser(r)
	if !ok {
		// RFC 9728 §5.1: point unauthenticated callers at the protected-resource
		// metadata so OAuth-capable clients (MCP agents) can discover the flow.
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+s.cfg.BaseURL+protectedResourceMetadataPath+`"`)
		apiError(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	return user, true
}

// apiUser authenticates a programmatic request: an Authorization: Bearer
// credential (personal access token or OAuth access token) takes precedence,
// with the browser session cookie as the same-origin fallback.
func (s *Server) apiUser(r *http.Request) (*store.User, bool) {
	if token, ok := bearerToken(r); ok {
		usage := store.TokenUsage{IP: clientIP(r), UserAgent: r.UserAgent()}
		user, err := s.store.UserForBearerToken(r.Context(), token, usage)
		return user, err == nil
	}
	return s.currentUser(r)
}

// clientIP extracts the caller's address for usage provenance. The leftmost
// X-Forwarded-For entry is preferred so deployments behind a reverse proxy see
// the real client; it is display-only information, never an access control
// input, so a forged header cannot grant anything.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := strings.TrimSpace(first); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func bearerToken(r *http.Request) (string, bool) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	const scheme = "bearer "
	if len(auth) > len(scheme) && strings.EqualFold(auth[:len(scheme)], scheme) {
		return strings.TrimSpace(auth[len(scheme):]), true
	}
	return "", false
}

func (s *Server) repoFromAPIParam(r *http.Request, userID int64) (*store.Repo, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("repo"))
	if raw == "" {
		return nil, errors.New("repo parameter is required")
	}
	repos, err := s.store.ListReposForUser(r.Context(), userID)
	if err != nil {
		return nil, err
	}
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
		for i := range repos {
			if repos[i].ID == id {
				return &repos[i], nil
			}
		}
	}
	for i := range repos {
		if repos[i].FullName == raw {
			return &repos[i], nil
		}
	}
	return nil, errors.New("repository is not available to this user")
}

func apiLineRange(r *http.Request, total int) (int, int, error) {
	if total == 0 {
		return 0, 0, nil
	}
	start := 1
	end := min(total, maxAPIReadLines)
	var err error
	if raw := strings.TrimSpace(r.URL.Query().Get("start")); raw != "" {
		start, err = strconv.Atoi(raw)
		if err != nil || start < 1 {
			return 0, 0, errors.New("start must be a positive line number")
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("end")); raw != "" {
		end, err = strconv.Atoi(raw)
		if err != nil || end < 1 {
			return 0, 0, errors.New("end must be a positive line number")
		}
	} else {
		end = min(total, start+maxAPIReadLines-1)
	}
	if start > total {
		return 0, 0, errors.New("start is beyond end of file")
	}
	if end > total {
		end = total
	}
	if end < start {
		return 0, 0, errors.New("end must be greater than or equal to start")
	}
	if end-start+1 > maxAPIReadLines {
		return 0, 0, errors.New("requested range exceeds maximum of 500 lines")
	}
	return start, end, nil
}

func apiSearchFiles(files []codesearch.FileMatch) []apiSearchFile {
	out := make([]apiSearchFile, 0, len(files))
	for _, file := range files {
		lines := make([]apiSearchLine, 0, len(file.Lines))
		for _, line := range file.Lines {
			lines = append(lines, apiSearchLine{
				Number:      line.Number,
				Before:      apiContextLines(line.Before),
				After:       apiContextLines(line.After),
				Segments:    apiSegments(line.Segments),
				FileName:    line.FileName,
				SymbolKinds: line.SymbolKinds,
				MetaVars:    line.MetaVars,
			})
		}
		out = append(out, apiSearchFile{
			RepoID:     file.RepoID,
			Repository: file.Repository,
			RepoName:   file.RepoName,
			Provider:   file.Provider,
			Branches:   file.Branches,
			Path:       file.Path,
			Language:   file.Language,
			IndexedAt:  file.IndexedAt,
			Commit:     file.Commit,
			Dirty:      file.Dirty,
			Lines:      lines,
		})
	}
	return out
}

func apiFacetGroups(groups []codesearch.FacetGroup) []apiFacetGroup {
	out := make([]apiFacetGroup, 0, len(groups))
	for _, group := range groups {
		values := make([]apiFacetValue, 0, len(group.Values))
		for _, value := range group.Values {
			values = append(values, apiFacetValue{Value: value.Value, Label: value.Label, Count: value.Count, Active: value.Active})
		}
		out = append(out, apiFacetGroup{Field: group.Field, Label: group.Label, Values: values})
	}
	return out
}

func apiContextLines(lines []codesearch.ContextLine) []apiContextLine {
	out := make([]apiContextLine, 0, len(lines))
	for _, line := range lines {
		out = append(out, apiContextLine{Number: line.Number, Text: line.Text})
	}
	return out
}

func apiSegments(segments []codesearch.Segment) []apiSegment {
	out := make([]apiSegment, 0, len(segments))
	for _, segment := range segments {
		out = append(out, apiSegment{Text: segment.Text, Match: segment.Match})
	}
	return out
}

func apiJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func apiError(w http.ResponseWriter, status int, message string) {
	apiJSON(w, status, apiErrorResponse{Error: message})
}

// isTrueParam parses a boolean-ish query parameter (1/true/yes/on). It lives here
// because the JSON API was the first surface to need it; the HTML handlers reuse it.
func isTrueParam(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
