// Package mcp implements a minimal Model Context Protocol server over stdio so
// AI coding agents (Claude Code, Cursor, ...) can use Codebeam's local index as a
// retrieval toolbox instead of grep. It speaks newline-delimited JSON-RPC 2.0 and
// exposes a small set of tools backed by the same search engine and store the web
// UI uses.
//
// There is no external MCP SDK: the protocol surface we need (initialize,
// tools/list, tools/call) is small and hand-rolled to keep Codebeam a single,
// dependency-light binary — the same approach the project already takes for OAuth
// and session signing.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ctourriere/codebeam/internal/codehost"
	"github.com/ctourriere/codebeam/internal/indexer"
	codesearch "github.com/ctourriere/codebeam/internal/search"
	"github.com/ctourriere/codebeam/internal/store"
	"github.com/ctourriere/codebeam/internal/structural"
)

const (
	serverName        = "codebeam"
	serverVersion     = "0.1.0"
	defaultMaxResults = 20
	maxReadLines      = 500
	maxReadBytes      = 4 << 20
)

// supportedProtocolVersions are the MCP revisions this server implements,
// newest first. Initialize echoes the client's requested version when it is
// supported and otherwise answers with the newest one, per the spec's version
// negotiation rules.
var supportedProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidParams  = -32602
	codeMethodNotFound = -32601
)

// Server answers MCP requests against a set of locally indexed repositories.
// Over stdio (solo mode) every indexed repository is visible; when UserID is
// set (the authenticated HTTP transport), every tool is scoped to the
// repositories that user is permitted to see.
type Server struct {
	Store      *store.Store
	Search     codesearch.Engine
	Structural *structural.Engine
	Indexer    *indexer.Indexer
	MaxResults int
	Logger     *slog.Logger
	// UserID scopes repository visibility; 0 means unscoped solo mode.
	UserID int64
}

// allowedRepos is the permission gate every tool goes through.
func (s *Server) allowedRepos(ctx context.Context) ([]store.Repo, error) {
	if s.UserID > 0 {
		return s.Store.ListIndexedReposForUser(ctx, s.UserID)
	}
	return s.Store.ListIndexedRepos(ctx)
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the stdio JSON-RPC loop until r is exhausted or ctx is cancelled.
// Each request is read as one newline-delimited JSON message and each response is
// written the same way. HTML escaping is disabled so code snippets stay readable.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.applyDefaults()
	reader := bufio.NewReader(r)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, readErr := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			if resp, ok := s.handle(ctx, trimmed); ok {
				if err := enc.Encode(resp); err != nil {
					return err
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func (s *Server) applyDefaults() {
	if s.MaxResults <= 0 {
		s.MaxResults = defaultMaxResults
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
}

// HandleMessage processes one JSON-RPC message and returns the response along
// with whether one should be sent (notifications expect none). It backs the
// streamable-HTTP transport; Serve wraps the same dispatch for stdio.
func (s *Server) HandleMessage(ctx context.Context, message []byte) (any, bool) {
	s.applyDefaults()
	resp, ok := s.handle(ctx, bytes.TrimSpace(message))
	return resp, ok
}

// handle dispatches one JSON-RPC message. The bool result reports whether a
// response should be written (notifications expect none).
func (s *Server) handle(ctx context.Context, line []byte) (response, bool) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(nil, codeParseError, "parse error"), true
	}
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		return resultResponse(req.ID, s.initializeResult(req.Params)), true
	case "notifications/initialized", "initialized", "notifications/cancelled":
		return response{}, false
	case "ping":
		if isNotification {
			return response{}, false
		}
		return resultResponse(req.ID, map[string]any{}), true
	case "tools/list":
		if isNotification {
			return response{}, false
		}
		return resultResponse(req.ID, map[string]any{"tools": toolDefinitions()}), true
	case "tools/call":
		if isNotification {
			return response{}, false
		}
		return s.handleToolCall(ctx, req.ID, req.Params), true
	default:
		if isNotification {
			return response{}, false
		}
		return errorResponse(req.ID, codeMethodNotFound, "method not found: "+req.Method), true
	}
}

func (s *Server) initializeResult(params json.RawMessage) map[string]any {
	version := supportedProtocolVersions[0]
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(params, &p); err == nil {
			for _, v := range supportedProtocolVersions {
				if p.ProtocolVersion == v {
					version = v
					break
				}
			}
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		"instructions": "Codebeam exposes your locally indexed repositories as a retrieval toolbox. " +
			"Use search_code for lexical/regex (Zoekt) search across all indexed repos, structural_search " +
			"to match code by AST shape with ast-grep patterns, symbol_search to find where a symbol is " +
			"defined, find_references for word-boundary usages of a symbol, read_file to read a bounded " +
			"line range with provenance, file_tree to list a repository's files, list_repos to see " +
			"what is indexed, and repo_stats for per-repository statistics (languages, size, freshness). " +
			"Results carry repo:path:line citations and a dirty flag for uncommitted working-tree matches.",
	}
}

func (s *Server) handleToolCall(ctx context.Context, id, params json.RawMessage) response {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return errorResponse(id, codeInvalidParams, "invalid tool call params")
	}
	var (
		text string
		err  error
	)
	switch call.Name {
	case "search_code":
		text, err = s.callSearch(ctx, call.Arguments)
	case "structural_search":
		text, err = s.callStructuralSearch(ctx, call.Arguments)
	case "symbol_search":
		text, err = s.callSymbolSearch(ctx, call.Arguments)
	case "find_references":
		text, err = s.callFindReferences(ctx, call.Arguments)
	case "read_file":
		text, err = s.callRead(ctx, call.Arguments)
	case "file_tree":
		text, err = s.callFileTree(ctx, call.Arguments)
	case "list_repos":
		text, err = s.callListRepos(ctx)
	case "repo_stats":
		text, err = s.callRepoStats(ctx)
	default:
		return errorResponse(id, codeInvalidParams, "unknown tool: "+call.Name)
	}
	if err != nil {
		// Per the MCP spec, tool execution failures are reported as a tool result
		// with isError set (not a protocol error) so the agent can read and react.
		return resultResponse(id, toolResult(err.Error(), true))
	}
	return resultResponse(id, toolResult(text, false))
}

func (s *Server) callSearch(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query      string `json:"query"`
		Repo       string `json:"repo"`
		Path       string `json:"path"`
		Lang       string `json:"lang"`
		MaxResults int    `json:"max_results"`
	}
	if err := unmarshalArgs(args, &in); err != nil {
		return "", err
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return "", errors.New("query is required")
	}
	return s.runSearch(ctx, "Search: "+backquote(query), codesearch.Request{
		Query:      query,
		RepoFilter: strings.TrimSpace(in.Repo),
		PathFilter: strings.TrimSpace(in.Path),
		LangFilter: strings.TrimSpace(in.Lang),
	}, in.MaxResults)
}

func (s *Server) callStructuralSearch(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Pattern    string `json:"pattern"`
		Lang       string `json:"lang"`
		Repo       string `json:"repo"`
		Branch     string `json:"branch"`
		Path       string `json:"path"`
		MaxResults int    `json:"max_results"`
	}
	if err := unmarshalArgs(args, &in); err != nil {
		return "", err
	}
	pattern := strings.TrimSpace(in.Pattern)
	if pattern == "" {
		return "", errors.New("pattern is required")
	}
	if strings.TrimSpace(in.Lang) == "" {
		return "", errors.New("lang is required (one of: " + strings.Join(structural.SupportedLanguages(), ", ") + ")")
	}
	maxFiles := in.MaxResults
	if maxFiles <= 0 {
		maxFiles = s.MaxResults
	}
	repos, err := s.allowedRepos(ctx)
	if err != nil {
		return "", err
	}
	result, err := s.Structural.Search(ctx, structural.Request{
		Pattern:      pattern,
		Lang:         in.Lang,
		RepoFilter:   strings.TrimSpace(in.Repo),
		BranchFilter: strings.TrimSpace(in.Branch),
		PathFilter:   strings.TrimSpace(in.Path),
		Allowed:      repos,
	})
	if err != nil {
		return "", err
	}
	heading := "Structural search: " + backquote(pattern) + " (" + strings.ToLower(strings.TrimSpace(in.Lang)) + ")"
	return formatSearchResult(heading, result, maxFiles), nil
}

func (s *Server) callSymbolSearch(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Symbol     string `json:"symbol"`
		Repo       string `json:"repo"`
		Lang       string `json:"lang"`
		MaxResults int    `json:"max_results"`
	}
	if err := unmarshalArgs(args, &in); err != nil {
		return "", err
	}
	symbol := strings.TrimSpace(in.Symbol)
	if symbol == "" {
		return "", errors.New("symbol is required")
	}
	return s.runSearch(ctx, "Symbol definitions: "+backquote(symbol), codesearch.Request{
		Query:      symbol,
		RepoFilter: strings.TrimSpace(in.Repo),
		LangFilter: strings.TrimSpace(in.Lang),
		Symbols:    true,
	}, in.MaxResults)
}

func (s *Server) callFindReferences(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Symbol     string `json:"symbol"`
		Repo       string `json:"repo"`
		Lang       string `json:"lang"`
		MaxResults int    `json:"max_results"`
	}
	if err := unmarshalArgs(args, &in); err != nil {
		return "", err
	}
	symbol := strings.TrimSpace(in.Symbol)
	if symbol == "" {
		return "", errors.New("symbol is required")
	}
	// Word-boundary anchor so "References to Get" doesn't also match "Getter" or
	// "forGetting" — the precision agents lose with a raw grep.
	query := `\b` + regexp.QuoteMeta(symbol) + `\b`
	return s.runSearch(ctx, "References to "+backquote(symbol), codesearch.Request{
		Query:      query,
		RepoFilter: strings.TrimSpace(in.Repo),
		LangFilter: strings.TrimSpace(in.Lang),
	}, in.MaxResults)
}

// runSearch scopes a request to every indexed repository and renders the result
// as a citation-tagged text bundle. It backs the content, symbol, and reference
// tools; heading labels the bundle for the calling tool.
func (s *Server) runSearch(ctx context.Context, heading string, req codesearch.Request, maxFiles int) (string, error) {
	if maxFiles <= 0 {
		maxFiles = s.MaxResults
	}
	repos, err := s.allowedRepos(ctx)
	if err != nil {
		return "", err
	}
	req.Allowed = repos
	result, err := s.Search.Search(ctx, req)
	if err != nil {
		return "", err
	}
	return formatSearchResult(heading, result, maxFiles), nil
}

func (s *Server) callRead(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Repo  string `json:"repo"`
		Path  string `json:"path"`
		Start int    `json:"start"`
		End   int    `json:"end"`
	}
	if err := unmarshalArgs(args, &in); err != nil {
		return "", err
	}
	repo, err := s.resolveRepo(ctx, strings.TrimSpace(in.Repo))
	if err != nil {
		return "", err
	}
	relPath := strings.TrimSpace(in.Path)
	if relPath == "" {
		return "", errors.New("path is required")
	}
	full, err := safeJoin(s.Indexer.SourceRoot(*repo), relPath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() || info.Size() > maxReadBytes {
		return "", errors.New("file not found")
	}
	content, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	lines := splitLines(content)
	start, end := readRange(in.Start, in.End, len(lines))
	var b strings.Builder
	provenance := repo.FullName + ":" + relPath
	if c := shortCommit(repo.IndexedCommit); c != "" {
		provenance += "@" + c
	}
	fmt.Fprintf(&b, "# %s lines %d-%d of %d\n\n", provenance, start, end, len(lines))
	for i := start; i <= end && i >= 1 && i <= len(lines); i++ {
		fmt.Fprintf(&b, "%6d  %s\n", i, lines[i-1])
	}
	return b.String(), nil
}

func (s *Server) callListRepos(ctx context.Context) (string, error) {
	repos, err := s.allowedRepos(ctx)
	if err != nil {
		return "", err
	}
	if len(repos) == 0 {
		return "No repositories are indexed yet. Add and index a repository in the Codebeam web UI first.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Indexed repositories (%d)\n\n", len(repos))
	for _, repo := range repos {
		fmt.Fprintf(&b, "- %s (%s)", repo.FullName, providerLabel(repo.HostProvider))
		if repo.IndexedAt > 0 {
			fmt.Fprintf(&b, " — indexed %s", relativeTime(repo.IndexedAt))
		}
		if repo.HostProvider == "local" && repo.LocalPath != "" {
			fmt.Fprintf(&b, " — %s", repo.LocalPath)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (s *Server) callRepoStats(ctx context.Context) (string, error) {
	repos, err := s.allowedRepos(ctx)
	if err != nil {
		return "", err
	}
	if len(repos) == 0 {
		return "No repositories are indexed yet. Add and index a repository in the Codebeam web UI first.", nil
	}

	var body strings.Builder
	var totalFiles, pending int
	var totalLines, totalBytes int64
	for _, repo := range repos {
		stats, ok := repo.DecodeContentStats()
		writeRepoStats(&body, repo, stats, ok)
		if !ok {
			pending++
			continue
		}
		totalFiles += stats.Files
		totalLines += stats.Lines
		totalBytes += stats.Bytes
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Repository stats (%d %s)\n\n", len(repos), plural(len(repos), "repository", "repositories"))
	fmt.Fprintf(&b, "Totals: %s %s · %s %s · %s.\n",
		groupDigits(int64(totalFiles)), plural(totalFiles, "file", "files"),
		groupDigits(totalLines), plural(int(totalLines), "line", "lines"), humanBytes(totalBytes))
	if pending > 0 {
		fmt.Fprintf(&b, "_%d repositor%s indexed before content stats existed; totals exclude %s. Reindex to fill in files/lines/languages._\n",
			pending, plural(pending, "y was", "ies were"), plural(pending, "it", "them"))
	}
	b.WriteString("\n")
	b.WriteString(body.String())
	return b.String(), nil
}

// writeRepoStats renders one repository's stats block. Repos indexed before
// content stats existed still get their branch/commit/freshness lines, with a
// note that a reindex will fill in the rest.
func writeRepoStats(b *strings.Builder, repo store.Repo, stats store.RepoContentStats, hasStats bool) {
	fmt.Fprintf(b, "## %s (%s)\n", repo.FullName, providerLabel(repo.HostProvider))
	branchLabel := strings.ReplaceAll(repo.IndexedBranchNames, ",", ", ")
	if branchLabel == "" {
		branchLabel = repo.DefaultBranch
	}
	if c := shortCommit(repo.IndexedCommit); c != "" {
		branchLabel += " @ " + c
	}
	fmt.Fprintf(b, "- branches: %s\n", branchLabel)
	if repo.LastCommitAt > 0 {
		fmt.Fprintf(b, "- last commit: %s (%s)\n", time.Unix(repo.LastCommitAt, 0).Format("2006-01-02"), relativeTime(repo.LastCommitAt))
	}
	fmt.Fprintf(b, "- indexed: %s\n", relativeTime(repo.IndexedAt))
	if hasStats {
		fmt.Fprintf(b, "- files: %s · lines: %s · size: %s\n", groupDigits(int64(stats.Files)), groupDigits(stats.Lines), humanBytes(stats.Bytes))
		if langs := formatLanguageBreakdown(stats); langs != "" {
			fmt.Fprintf(b, "- languages: %s\n", langs)
		}
	} else {
		b.WriteString("- files/lines/languages: unavailable (indexed before content stats existed; reindex to collect)\n")
	}
	if repo.HostProvider == "local" && repo.LocalPath != "" {
		fmt.Fprintf(b, "- path: %s\n", repo.LocalPath)
	} else if repo.WebURL != "" {
		fmt.Fprintf(b, "- url: %s\n", repo.WebURL)
	}
	b.WriteString("\n")
}

// formatLanguageBreakdown renders languages by descending byte share, keeping
// the largest entries and folding the tail — plus files with no detectable
// language — into "other".
func formatLanguageBreakdown(stats store.RepoContentStats) string {
	if stats.Bytes <= 0 || len(stats.Languages) == 0 {
		return ""
	}
	type share struct {
		name  string
		bytes int64
	}
	shares := make([]share, 0, len(stats.Languages))
	var other int64
	for name, lang := range stats.Languages {
		if name == "" {
			other += lang.Bytes
			continue
		}
		shares = append(shares, share{name: name, bytes: lang.Bytes})
	}
	sort.Slice(shares, func(i, j int) bool {
		if shares[i].bytes != shares[j].bytes {
			return shares[i].bytes > shares[j].bytes
		}
		return shares[i].name < shares[j].name
	})
	const maxLanguages = 6
	if len(shares) > maxLanguages {
		for _, s := range shares[maxLanguages:] {
			other += s.bytes
		}
		shares = shares[:maxLanguages]
	}
	parts := make([]string, 0, len(shares)+1)
	for _, s := range shares {
		parts = append(parts, s.name+" "+percentOf(s.bytes, stats.Bytes))
	}
	if other > 0 {
		parts = append(parts, "other "+percentOf(other, stats.Bytes))
	}
	return strings.Join(parts, ", ")
}

func percentOf(part, total int64) string {
	if total <= 0 {
		return "0%"
	}
	p := 100 * float64(part) / float64(total)
	if p > 0 && p < 0.1 {
		return "<0.1%"
	}
	return strconv.FormatFloat(p, 'f', 1, 64) + "%"
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// groupDigits inserts thousands separators (89214 → 89,214) so line and file
// counts stay readable.
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	start := 0
	if s[0] == '-' {
		start = 1
	}
	var b strings.Builder
	b.WriteString(s[:start])
	for i, digit := range s[start:] {
		if i > 0 && (len(s)-start-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return b.String()
}

func plural(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

func (s *Server) callFileTree(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Repo       string `json:"repo"`
		Path       string `json:"path"`
		MaxEntries int    `json:"max_entries"`
	}
	if err := unmarshalArgs(args, &in); err != nil {
		return "", err
	}
	repo, err := s.resolveRepo(ctx, strings.TrimSpace(in.Repo))
	if err != nil {
		return "", err
	}
	root := s.Indexer.SourceRoot(*repo)
	base := root
	subPath := strings.TrimSpace(in.Path)
	if subPath != "" {
		if base, err = safeJoin(root, subPath); err != nil {
			return "", err
		}
	}
	max := in.MaxEntries
	if max <= 0 {
		max = 500
	}

	var entries []string
	truncated := false
	walkErr := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == base {
			return nil
		}
		if d.IsDir() && indexer.ShouldSkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if len(entries) >= max {
			truncated = true
			return filepath.SkipAll
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			rel += "/"
		}
		entries = append(entries, rel)
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	sort.Strings(entries)

	var b strings.Builder
	scope := repo.FullName
	if subPath != "" {
		scope += "/" + strings.TrimSuffix(filepath.ToSlash(filepath.Clean(subPath)), "/")
	}
	fmt.Fprintf(&b, "# Files in %s (%d shown)\n\n", scope, len(entries))
	if len(entries) == 0 {
		b.WriteString("(empty)\n")
		return b.String(), nil
	}
	for _, entry := range entries {
		b.WriteString(entry + "\n")
	}
	if truncated {
		fmt.Fprintf(&b, "\n_Truncated at %d entries. Pass a narrower path or a larger max_entries._\n", max)
	}
	return b.String(), nil
}

// resolveRepo matches a repository among the indexed set by numeric id or by full
// name, so agents can pass whichever search_code returned.
func (s *Server) resolveRepo(ctx context.Context, raw string) (*store.Repo, error) {
	if raw == "" {
		return nil, errors.New("repo is required")
	}
	repos, err := s.allowedRepos(ctx)
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
	return nil, fmt.Errorf("repository %q is not indexed", raw)
}

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name": "search_code",
			"description": "Lexical/regex code search across every indexed repository, powered by Zoekt. " +
				"Returns ranked snippets with repo:path:line citations and a dirty flag for uncommitted " +
				"working-tree matches. The query supports Zoekt syntax (regex by default, plus file:, lang:, " +
				"sym:, case: and boolean operators).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":       map[string]any{"type": "string", "description": "Zoekt query (regex or literal)."},
					"repo":        map[string]any{"type": "string", "description": "Optional: restrict to one repository full name (e.g. local/myrepo)."},
					"path":        map[string]any{"type": "string", "description": "Optional: restrict to file paths matching this regex."},
					"lang":        map[string]any{"type": "string", "description": "Optional: restrict to a language (e.g. go, python, typescript)."},
					"max_results": map[string]any{"type": "integer", "description": "Optional: maximum number of files to return (default 20)."},
				},
				"required": []string{"query"},
			},
		},
		{
			"name": "structural_search",
			"description": "Structural (AST) code search across every indexed repository, powered by an embedded ast-grep engine. " +
				"Matches code by syntax-tree shape rather than text: `$VAR` captures one AST node, `$$$` captures any number of nodes " +
				"(e.g. `if $ERR != nil { $$$ }`, `console.log($$$)`, `useEffect($FN, [])`). The pattern must itself parse as valid code " +
				"in the given language. Captured metavariables are reported with each match. Prefer search_code for plain text/regex; " +
				"use this to find code shaped a certain way regardless of formatting or identifier names. " +
				"Slower than lexical search; narrow with repo/path when possible.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern":     map[string]any{"type": "string", "description": "ast-grep pattern (valid code plus $VAR / $$$ metavariables)."},
					"lang":        map[string]any{"type": "string", "description": "Language to parse pattern and files as: " + strings.Join(structural.SupportedLanguages(), ", ") + "."},
					"repo":        map[string]any{"type": "string", "description": "Optional: restrict to one repository full name (e.g. local/myrepo)."},
					"branch":      map[string]any{"type": "string", "description": "Optional: search a specific branch's committed state instead of the working tree/primary branch."},
					"path":        map[string]any{"type": "string", "description": "Optional: restrict to file paths matching this regex."},
					"max_results": map[string]any{"type": "integer", "description": "Optional: maximum number of files to return (default 20)."},
				},
				"required": []string{"pattern", "lang"},
			},
		},
		{
			"name": "symbol_search",
			"description": "Find where a symbol (function, type, method, constant, ...) is DEFINED across every indexed repository, " +
				"using Zoekt's ctags-backed symbol index. Prefer this over search_code when you want the definition rather than " +
				"every textual mention. The symbol argument is matched as a regex. Requires universal-ctags to have been present " +
				"when the repository was indexed; if it was not, this returns no matches.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol":      map[string]any{"type": "string", "description": "Symbol name or regex to locate (e.g. NewServer or ^Handle)."},
					"repo":        map[string]any{"type": "string", "description": "Optional: restrict to one repository full name."},
					"lang":        map[string]any{"type": "string", "description": "Optional: restrict to a language (e.g. go, python)."},
					"max_results": map[string]any{"type": "integer", "description": "Optional: maximum number of files to return (default 20)."},
				},
				"required": []string{"symbol"},
			},
		},
		{
			"name": "find_references",
			"description": "Find usages of a symbol across every indexed repository — a word-boundary search so \"Get\" does not also match \"Getter\". " +
				"Pair with symbol_search (which finds the definition) to navigate code. Accepts a literal symbol name.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol":      map[string]any{"type": "string", "description": "Literal symbol/identifier to find references to (e.g. NewServer)."},
					"repo":        map[string]any{"type": "string", "description": "Optional: restrict to one repository full name."},
					"lang":        map[string]any{"type": "string", "description": "Optional: restrict to a language (e.g. go, python)."},
					"max_results": map[string]any{"type": "integer", "description": "Optional: maximum number of files to return (default 20)."},
				},
				"required": []string{"symbol"},
			},
		},
		{
			"name":        "read_file",
			"description": "Read a bounded range of lines from a file in an indexed repository, with repo:path:line provenance. Use this to expand context around a search hit without loading the whole file.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo":  map[string]any{"type": "string", "description": "Repository full name (e.g. local/myrepo) or numeric id."},
					"path":  map[string]any{"type": "string", "description": "File path relative to the repository root."},
					"start": map[string]any{"type": "integer", "description": "Optional: first line, 1-based (default 1)."},
					"end":   map[string]any{"type": "integer", "description": "Optional: last line, inclusive (default start+200, capped at start+499)."},
				},
				"required": []string{"repo", "path"},
			},
		},
		{
			"name":        "file_tree",
			"description": "List the files and directories in an indexed repository (optionally under a sub-path), so an agent can orient itself before reading or searching. Skips vendor/build/VCS directories.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo":        map[string]any{"type": "string", "description": "Repository full name or numeric id."},
					"path":        map[string]any{"type": "string", "description": "Optional: list only under this sub-directory."},
					"max_entries": map[string]any{"type": "integer", "description": "Optional: maximum entries to return (default 500)."},
				},
				"required": []string{"repo"},
			},
		},
		{
			"name":        "list_repos",
			"description": "List every repository that is currently indexed and searchable, including how recently each was indexed.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name": "repo_stats",
			"description": "Per-repository statistics for every indexed repository: language breakdown (percentages by bytes), " +
				"file/line/size totals, last commit date, indexed branches and commit, plus fleet-wide totals. " +
				"Stats cover the indexed content of the primary branch (vendor/build/VCS directories and oversized " +
				"files are excluded, like search). Prefer list_repos for a quick inventory; use this for sizes, " +
				"languages, or freshness.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func formatSearchResult(heading string, result codesearch.Result, maxFiles int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", heading)
	if result.EmptyReason != "" {
		b.WriteString(result.EmptyReason + "\n")
		return b.String()
	}
	if result.MatchCount == 0 || len(result.Files) == 0 {
		b.WriteString("No matches.\n")
		return b.String()
	}
	if maxFiles <= 0 {
		maxFiles = defaultMaxResults
	}
	shown := len(result.Files)
	if shown > maxFiles {
		shown = maxFiles
	}
	fmt.Fprintf(&b, "%d match(es) in %d file(s)", result.MatchCount, result.FileCount)
	if shown < len(result.Files) {
		fmt.Fprintf(&b, ", showing %d", shown)
	}
	b.WriteString(".\n\n")
	for _, file := range result.Files[:shown] {
		writeFileMatch(&b, file)
	}
	if shown < len(result.Files) {
		fmt.Fprintf(&b, "_%d more file(s) omitted. Raise max_results or narrow the query._\n", len(result.Files)-shown)
	}
	if result.Truncated {
		b.WriteString("_Results are partial: the search hit a file/match cap or the timeout. Narrow with repo/path filters._\n")
	}
	return b.String()
}

func writeFileMatch(b *strings.Builder, file codesearch.FileMatch) {
	fmt.Fprintf(b, "## %s:%s", file.Repository, file.Path)
	if c := shortCommit(file.Commit); c != "" {
		fmt.Fprintf(b, "@%s", c)
	}
	var tags []string
	if file.Dirty {
		tags = append(tags, "dirty")
	}
	if file.Language != "" {
		tags = append(tags, strings.ToLower(file.Language))
	}
	if file.IndexedAt > 0 {
		tags = append(tags, "indexed "+relativeTime(file.IndexedAt))
	}
	if len(tags) > 0 {
		fmt.Fprintf(b, "  (%s)", strings.Join(tags, ", "))
	}
	b.WriteString("\n")
	for _, line := range file.Lines {
		if line.FileName {
			b.WriteString("  (matched in file name)\n")
			continue
		}
		for _, c := range line.Before {
			fmt.Fprintf(b, "  %6d  %s\n", c.Number, c.Text)
		}
		fmt.Fprintf(b, "> %6d  %s\n", line.Number, plainSegments(line.Segments))
		for _, c := range line.After {
			fmt.Fprintf(b, "  %6d  %s\n", c.Number, c.Text)
		}
		if len(line.MetaVars) > 0 {
			names := make([]string, 0, len(line.MetaVars))
			for name := range line.MetaVars {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Fprintf(b, "          %s = %s\n", name, line.MetaVars[name])
			}
		}
	}
	b.WriteString("\n")
}

func plainSegments(segments []codesearch.Segment) string {
	var b strings.Builder
	for _, segment := range segments {
		b.WriteString(segment.Text)
	}
	return b.String()
}

func resultResponse(id json.RawMessage, result any) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResponse(id json.RawMessage, code int, message string) response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func toolResult(text string, isError bool) map[string]any {
	out := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
	if isError {
		out["isError"] = true
	}
	return out
}

func unmarshalArgs(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, dst); err != nil {
		return errors.New("invalid tool arguments")
	}
	return nil
}

func splitLines(content []byte) []string {
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func readRange(start, end, total int) (int, int) {
	if total == 0 {
		return 0, 0
	}
	if start < 1 {
		start = 1
	}
	if start > total {
		start = total
	}
	if end <= 0 {
		end = start + 200
	}
	if end > total {
		end = total
	}
	if end < start {
		end = start
	}
	if end-start+1 > maxReadLines {
		end = start + maxReadLines - 1
	}
	return start, end
}

func safeJoin(root, rel string) (string, error) {
	if root == "" {
		return "", errors.New("repository source is unavailable")
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
	fullAbs, err := filepath.Abs(filepath.Join(rootAbs, cleanRel))
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

func providerLabel(provider string) string {
	switch provider {
	case "local":
		return "local"
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	default:
		if codehost.IsGitLabProvider(provider) {
			return "GitLab " + codehost.HostFromBaseURL(codehost.GitLabBaseURLFromProvider(provider, ""))
		}
		return provider
	}
}

func backquote(s string) string {
	return "`" + s + "`"
}

// shortCommit abbreviates a commit hash to its first 8 characters for compact
// file:line@commit citations; returns "" for a non-hash or empty value.
func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) >= 8 {
		return commit[:8]
	}
	return commit
}

func relativeTime(ts int64) string {
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
