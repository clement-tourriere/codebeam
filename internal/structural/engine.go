package structural

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ctourriere/codebeam/internal/indexer"
	codesearch "github.com/ctourriere/codebeam/internal/search"
	"github.com/ctourriere/codebeam/internal/store"
)

const (
	// maxFileSize mirrors the indexer's per-file cap; larger files are
	// skipped, matching what lexical search can see anyway.
	maxFileSize = 2 << 20
	// contextLines is how many lines of context surround a match line.
	contextLines = 2
	// maxMatchBodyLines bounds how much of a multi-line match body is shown
	// as after-context.
	maxMatchBodyLines = 8
	// maxMetaVarText bounds a single displayed metavariable capture.
	maxMetaVarText = 200
)

// Request describes one structural search. Allowed is the permission gate:
// only repos in it are searchable, exactly like codesearch.Request.
type Request struct {
	Pattern      string
	Lang         string
	RepoFilter   string
	RepoFilters  []string
	BranchFilter string
	PathFilter   string
	// Facet-driven filters, sharing lexical search's semantics.
	TopPathFilter   string
	ExtFilter       string
	SourceFilter    string
	ProviderFilter  string
	FreshnessFilter string
	// DirtyFilter restricts worktree scans to files with ("dirty") or without
	// ("clean") uncommitted changes, like lexical search's dirty facet.
	DirtyFilter string
	// Exclude removes matching facet values with the same semantics as lexical
	// search (repositories, branches, paths, providers, working-tree state, …).
	Exclude    codesearch.FacetFilters
	MaxMatches int // optional per-request override of the engine cap
	Allowed    []store.Repo
}

// facetRequest maps the structural request onto the shared codesearch
// request shape used for repo selection, facet building, and active-value
// highlighting.
func (r Request) facetRequest(displayLang string) codesearch.Request {
	return codesearch.Request{
		RepoFilter:      r.RepoFilter,
		RepoFilters:     r.RepoFilters,
		BranchFilter:    r.BranchFilter,
		TopPathFilter:   r.TopPathFilter,
		ExtFilter:       r.ExtFilter,
		SourceFilter:    r.SourceFilter,
		ProviderFilter:  r.ProviderFilter,
		FreshnessFilter: r.FreshnessFilter,
		DirtyFilter:     r.DirtyFilter,
		LangFilter:      displayLang,
		Exclude:         r.Exclude,
		Allowed:         r.Allowed,
	}
}

// Engine runs structural searches over repository files using the in-process
// WASM matcher. Results are adapted into codesearch.Result so the existing
// web/API/MCP rendering is reused unchanged.
type Engine struct {
	Matcher *Matcher
	Indexer *indexer.Indexer
	// Caps; zero values fall back to the defaults below.
	MaxFiles   int
	MaxMatches int
	Timeout    time.Duration
}

func (e *Engine) maxFiles() int {
	if e.MaxFiles > 0 {
		return e.MaxFiles
	}
	return 5000
}

func (e *Engine) maxMatches(req Request) int {
	if req.MaxMatches > 0 {
		return req.MaxMatches
	}
	if e.MaxMatches > 0 {
		return e.MaxMatches
	}
	return 1000
}

func (e *Engine) timeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return 15 * time.Second
}

// sourceFile is one candidate file: its repo-relative path and contents.
type sourceFile struct {
	relPath string
	data    []byte
}

// Search validates the pattern, scans the selected repos, and returns
// matches in the shared codesearch result shape.
func (e *Engine) Search(ctx context.Context, req Request) (codesearch.Result, error) {
	start := time.Now()
	pattern := strings.TrimSpace(req.Pattern)
	if pattern == "" {
		return codesearch.Result{EmptyReason: "Type a structural pattern to search."}, nil
	}
	lang, err := CanonicalLang(req.Lang)
	if err != nil {
		if strings.TrimSpace(req.Lang) == "" {
			return codesearch.Result{}, errors.New("structural search requires a language (e.g. lang=go)")
		}
		return codesearch.Result{}, err
	}
	var pathRe *regexp.Regexp
	if p := strings.TrimSpace(req.PathFilter); p != "" {
		pathRe, err = regexp.Compile(p)
		if err != nil {
			return codesearch.Result{}, fmt.Errorf("invalid path filter: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()

	if err := e.Matcher.Validate(ctx, pattern, lang); err != nil {
		return codesearch.Result{}, err
	}

	facetReq := req.facetRequest(langDisplayNames[lang])
	repos, err := codesearch.SelectedRepos(facetReq)
	if err != nil {
		return codesearch.Result{}, err
	}

	excluded := codesearch.NormalizeFacetFilters(req.Exclude)
	scan := &scanState{
		engine:         e,
		pattern:        pattern,
		lang:           lang,
		exts:           extensionSet(lang),
		pathRe:         pathRe,
		topFilter:      req.TopPathFilter,
		extFilter:      req.ExtFilter,
		excludedTop:    excluded.TopPaths,
		excludedExt:    excluded.Extensions,
		excludedBranch: excluded.Branches,
		excludedDirty:  excluded.Dirty,
		dirtyFilter:    codesearch.NormalizeDirtyFilter(req.DirtyFilter),
		maxFiles:       e.maxFiles(),
		maxMatches:     e.maxMatches(req),
	}
	if containsFold(excluded.Languages, lang) || containsFold(excluded.Languages, langDisplayNames[lang]) {
		scan.skipAll = true
	}
	for _, repo := range repos {
		if scan.capped() || ctx.Err() != nil {
			scan.truncated = true
			break
		}
		if err := scan.searchRepo(ctx, repo, req.BranchFilter); err != nil {
			// Timeout mid-scan degrades to partial results, anything else
			// (bad branch, git failure) is a real error.
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				scan.truncated = true
				break
			}
			return codesearch.Result{}, err
		}
	}

	files := scan.files
	sort.Slice(files, func(i, j int) bool {
		if files[i].Repository != files[j].Repository {
			return files[i].Repository < files[j].Repository
		}
		return files[i].Path < files[j].Path
	})
	result := codesearch.Result{
		Query:      pattern,
		Files:      files,
		FileCount:  len(files),
		MatchCount: scan.matchCount,
		Duration:   time.Since(start),
		Truncated:  scan.truncated,
	}
	result.Facets = structuralFacetGroups(codesearch.BuildFacets(files, facetReq, time.Now()))
	result.FacetedFileCount = len(files)
	if len(files) == 0 && len(repos) == 0 {
		result.EmptyReason = "No indexed repositories are available to search."
	}
	return result, nil
}

// structuralFacetGroups drops facets that do not make sense for structural
// results: language is fixed by the query and AST matches have no ctags symbol
// kind metadata.
func structuralFacetGroups(groups []codesearch.FacetGroup) []codesearch.FacetGroup {
	out := groups[:0]
	for _, group := range groups {
		if group.Field == "language" || group.Field == "symbol_kind" {
			continue
		}
		out = append(out, group)
	}
	return out
}

type scanState struct {
	engine         *Engine
	pattern        string
	lang           string
	exts           map[string]bool
	pathRe         *regexp.Regexp
	topFilter      string
	extFilter      string
	excludedTop    []string
	excludedExt    []string
	excludedBranch []string
	excludedDirty  []string
	dirtyFilter    string // "dirty", "clean", or "" (normalized)
	skipAll        bool
	maxFiles       int
	maxMatches     int

	mu         sync.Mutex
	files      []codesearch.FileMatch
	matchCount int
	filesSeen  int
	truncated  bool
}

func (s *scanState) capped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.matchCount >= s.maxMatches || s.filesSeen >= s.maxFiles
}

// admitFile reserves one file slot, reporting whether scanning may continue.
func (s *scanState) admitFile() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.matchCount >= s.maxMatches || s.filesSeen >= s.maxFiles {
		s.truncated = true
		return false
	}
	s.filesSeen++
	return true
}

// remainingMatches returns the current global match budget.
func (s *scanState) remainingMatches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.maxMatches - s.matchCount
	if n < 1 {
		n = 1
	}
	return n
}

func (s *scanState) record(fm codesearch.FileMatch, matches int, truncated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files = append(s.files, fm)
	s.matchCount += matches
	if truncated {
		s.truncated = true
	}
}

func (s *scanState) wantsPath(rel string) bool {
	ext := strings.ToLower(filepath.Ext(rel))
	if !s.exts[ext] {
		return false
	}
	if s.pathRe != nil && !s.pathRe.MatchString(rel) {
		return false
	}
	if !codesearch.MatchTopPath(rel, s.topFilter) {
		return false
	}
	if !codesearch.MatchExtension(rel, s.extFilter) {
		return false
	}
	for _, value := range s.excludedTop {
		if codesearch.MatchTopPath(rel, value) {
			return false
		}
	}
	for _, value := range s.excludedExt {
		if codesearch.MatchExtension(rel, value) {
			return false
		}
	}
	return true
}

// searchRepo scans one repo. With no branch filter it scans the working tree
// (live state for local repos, the checked-out primary branch for remote
// clones). With a branch filter it reads committed blobs straight from the
// git object store — no worktree mutation, no temp checkout.
func (s *scanState) searchRepo(ctx context.Context, repo store.Repo, branch string) error {
	branch = strings.TrimSpace(branch)
	if s.skipAll || (branch != "" && containsString(s.excludedBranch, branch)) {
		return nil
	}
	// Branch scans read committed blobs, so dirtiness only applies to worktree
	// scans of local repos — the same rule lexical search uses.
	var dirty map[string]struct{}
	if branch == "" && repo.HostProvider == "local" && repo.LocalPath != "" {
		dirty = codesearch.DirtyPaths(ctx, repo.LocalPath)
	}
	if s.dirtyFilter == "dirty" && len(dirty) == 0 {
		return nil
	}
	out := make(chan sourceFile, 8)
	var produceErr error
	go func() {
		defer close(out)
		if branch == "" {
			produceErr = s.produceWorktree(ctx, repo, out)
		} else {
			produceErr = s.produceBranch(ctx, repo, branch, out)
		}
	}()

	workers := s.engine.Matcher.poolSize()
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for file := range out {
				s.matchFile(ctx, repo, branch, file, dirty)
			}
		}()
	}
	wg.Wait()
	return produceErr
}

func (s *scanState) matchFile(ctx context.Context, repo store.Repo, branch string, file sourceFile, dirty map[string]struct{}) {
	_, isDirty := dirty[file.relPath]
	dirtyValue := "clean"
	if isDirty {
		dirtyValue = "dirty"
	}
	if (s.dirtyFilter == "dirty" && !isDirty) || (s.dirtyFilter == "clean" && isDirty) || containsFold(s.excludedDirty, dirtyValue) {
		return
	}
	matches, truncated, err := s.engine.Matcher.MatchBytes(ctx, s.pattern, s.lang, file.data, s.remainingMatches())
	if err != nil || len(matches) == 0 {
		// Per-file shim errors (e.g. non-UTF-8 content) skip the file; the
		// pattern itself was validated before scanning started.
		return
	}
	fm := adaptFileMatch(repo, branch, file, matches)
	fm.Dirty = isDirty
	s.record(fm, len(matches), truncated)
}

func (s *scanState) produceWorktree(ctx context.Context, repo store.Repo, out chan<- sourceFile) error {
	root := s.engine.Indexer.SourceRoot(repo)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entries are skipped, not fatal: local repos are
			// live directories.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != root && indexer.ShouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !s.wantsPath(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxFileSize {
			return nil
		}
		if !s.admitFile() {
			return filepath.SkipAll
		}
		data, err := os.ReadFile(path)
		if err != nil || !searchableContent(data) {
			return nil
		}
		select {
		case out <- sourceFile{relPath: rel, data: data}:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
}

func (s *scanState) produceBranch(ctx context.Context, repo store.Repo, branch string, out chan<- sourceFile) error {
	ref, err := s.engine.Indexer.ResolveBranchRef(ctx, repo, branch)
	if err != nil {
		return err
	}
	entries, err := s.engine.Indexer.ListBranchFiles(ctx, repo, ref)
	if err != nil {
		return err
	}
	reader, err := s.engine.Indexer.OpenBlobReader(ctx, repo)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel := filepath.ToSlash(entry.Path)
		if !s.wantsPath(rel) || entry.Size > maxFileSize {
			continue
		}
		if !s.admitFile() {
			return nil
		}
		data, err := reader.Read(ctx, entry.Blob)
		if err != nil {
			return err
		}
		if !searchableContent(data) {
			continue
		}
		select {
		case out <- sourceFile{relPath: rel, data: data}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// searchableContent filters out binary and non-UTF-8 files, which cannot be
// parsed structurally.
func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func containsFold(values []string, needle string) bool {
	for _, value := range values {
		if strings.EqualFold(value, needle) {
			return true
		}
	}
	return false
}

func searchableContent(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return false
	}
	return utf8.Valid(data)
}

var langDisplayNames = map[string]string{
	"bash":       "Bash",
	"c":          "C",
	"go":         "Go",
	"java":       "Java",
	"javascript": "JavaScript",
	"json":       "JSON",
	"python":     "Python",
	"rust":       "Rust",
	"tsx":        "TSX",
	"typescript": "TypeScript",
	"yaml":       "YAML",
}

func adaptFileMatch(repo store.Repo, branch string, file sourceFile, matches []WasmMatch) codesearch.FileMatch {
	lines := newLineIndex(file.data)
	fm := codesearch.FileMatch{
		RepoID:     repo.ID,
		Repository: repo.FullName,
		RepoName:   repo.Name,
		Provider:   repo.HostProvider,
		Path:       file.relPath,
		IndexedAt:  repo.IndexedAt,
		Commit:     repo.IndexedCommit,
	}
	if branch != "" {
		fm.Branches = []string{branch}
	}
	if lang, err := CanonicalLang(langForFile(file.relPath)); err == nil {
		fm.Language = langDisplayNames[lang]
	}
	for _, m := range matches {
		fm.Lines = append(fm.Lines, adaptLineMatch(file.data, lines, m))
	}
	return fm
}

// langForFile guesses the display language from the file extension; falls
// back to empty (adaptFileMatch tolerates the error).
func langForFile(rel string) string {
	ext := strings.ToLower(filepath.Ext(rel))
	for lang, exts := range langExtensions {
		for _, e := range exts {
			if e == ext {
				return lang
			}
		}
	}
	return ""
}

// lineIndex maps zero-based line numbers to byte offsets.
type lineIndex struct {
	starts []int // starts[i] = byte offset of line i
	size   int
}

func newLineIndex(data []byte) lineIndex {
	starts := []int{0}
	for i, b := range data {
		if b == '\n' && i+1 < len(data) {
			starts = append(starts, i+1)
		}
	}
	return lineIndex{starts: starts, size: len(data)}
}

func (li lineIndex) lineCount() int { return len(li.starts) }

func adaptLineMatch(data []byte, li lineIndex, m WasmMatch) codesearch.LineMatch {
	lm := codesearch.LineMatch{Number: m.StartLine + 1}

	lineStart, lineEnd := lineBounds(data, li, m.StartLine)
	line := string(data[lineStart:lineEnd])
	segStart := clamp(m.ByteStart-lineStart, 0, len(line))
	segEnd := clamp(m.ByteEnd-lineStart, segStart, len(line))
	var segs []codesearch.Segment
	if segStart > 0 {
		segs = append(segs, codesearch.Segment{Text: line[:segStart]})
	}
	segs = append(segs, codesearch.Segment{Text: line[segStart:segEnd], Match: true})
	if segEnd < len(line) {
		segs = append(segs, codesearch.Segment{Text: line[segEnd:]})
	}
	lm.Segments = segs

	for i := m.StartLine - contextLines; i < m.StartLine; i++ {
		if i < 0 {
			continue
		}
		s, e := lineBounds(data, li, i)
		lm.Before = append(lm.Before, codesearch.ContextLine{Number: i + 1, Text: string(data[s:e])})
	}
	// After-context: the remainder of a multi-line match body (bounded),
	// then regular trailing context.
	afterEnd := m.EndLine
	if afterEnd > m.StartLine+maxMatchBodyLines {
		afterEnd = m.StartLine + maxMatchBodyLines
	}
	afterEnd += contextLines
	for i := m.StartLine + 1; i <= afterEnd && i < li.lineCount(); i++ {
		s, e := lineBounds(data, li, i)
		lm.After = append(lm.After, codesearch.ContextLine{Number: i + 1, Text: string(data[s:e])})
	}

	vars := make(map[string]string, len(m.Vars)+len(m.MultiVars))
	for name, span := range m.Vars {
		vars["$"+name] = metaVarText(data, span)
	}
	for name, span := range m.MultiVars {
		vars["$$$"+name] = metaVarText(data, span)
	}
	if len(vars) > 0 {
		lm.MetaVars = vars
	}
	return lm
}

func metaVarText(data []byte, span Span) string {
	s := clamp(span.Start, 0, len(data))
	e := clamp(span.End, s, len(data))
	text := string(data[s:e])
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maxMetaVarText {
		text = text[:maxMetaVarText] + "…"
	}
	return text
}

// lineBounds returns the [start, end) byte range of the zero-based line
// without its trailing newline.
func lineBounds(data []byte, li lineIndex, line int) (int, int) {
	if line < 0 || line >= len(li.starts) {
		return 0, 0
	}
	start := li.starts[line]
	var end int
	if line+1 < len(li.starts) {
		end = li.starts[line+1] - 1
	} else {
		end = len(data)
		for end > start && data[end-1] == '\n' {
			end--
		}
	}
	if end < start {
		end = start
	}
	return start, end
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
