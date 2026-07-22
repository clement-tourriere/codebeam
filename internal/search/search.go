package search

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ctourriere/codebeam/internal/store"
	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
	zsearch "github.com/sourcegraph/zoekt/search"
)

const (
	// facetFileBudget / facetMatchBudget cap how many matched files and matches we
	// pull back from Zoekt for faceting, so counts are exact for realistic searches
	// (well past Zoekt's per-shard processing cap) while bounding memory for huge
	// result sets. displayFileLimit caps how many files actually get rendered.
	facetFileBudget  = 20000
	facetMatchBudget = 200000
	displayFileLimit = 100

	topPathRootValue = "__root__"
	noExtensionValue = "__none__"
	unknownValue     = "__unknown__"

	dirtyValue = "dirty"
	cleanValue = "clean"

	freshnessHour    = "hour"
	freshnessDay     = "day"
	freshnessWeek    = "week"
	freshnessMonth   = "month"
	freshnessOlder   = "older"
	freshnessUnknown = "unknown"

	sortRelevance   = "relevance"
	sortRepo        = "repo"
	sortPath        = "path"
	sortIndexedDesc = "indexed_desc"
	sortIndexedAsc  = "indexed_asc"
	sortMatchCount  = "match_count"
)

type collectedFileMatch struct {
	match FileMatch
	raw   zoekt.FileMatch
}

type Engine struct {
	IndexDir string
}

var errIndexChanging = errors.New("search index changed during an update; please retry")

// openDirectorySearcher tolerates a shard being atomically replaced between a
// directory scan and Zoekt opening it. Reindexing publishes with renames, so an
// ENOENT here is transient; retrying prevents an internal filesystem race from
// leaking into the search UI.
func openDirectorySearcher(ctx context.Context, indexDir string) (zoekt.Streamer, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		searcher, err := zsearch.NewDirectorySearcher(indexDir)
		if err == nil {
			return searcher, nil
		}
		lastErr = err
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if attempt == 3 {
			break
		}
		delay := time.Duration(5*(1<<attempt)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if errors.Is(lastErr, os.ErrNotExist) {
		return nil, errIndexChanging
	}
	return nil, lastErr
}

func indexedCommit(version, fallback string) string {
	version = strings.TrimSpace(version)
	if version == "" || version == "WORKTREE" {
		return strings.TrimSpace(fallback)
	}
	return version
}

func commitComesFromShard(version string) bool {
	version = strings.TrimSpace(version)
	return version != "" && version != "WORKTREE"
}

// FacetFilters is a set of values to exclude from a search. Values within one
// facet are ORed (exclude any matching value); different facets are ANDed.
// Keeping this typed and shared lets lexical, structural, web, API, CLI, and MCP
// searches use exactly the same exclusion semantics.
type FacetFilters struct {
	Sources     []string
	Repos       []string
	Branches    []string
	Languages   []string
	TopPaths    []string
	Extensions  []string
	Providers   []string
	Dirty       []string
	SymbolKinds []string
	Freshness   []string
}

type Request struct {
	Query            string
	RepoFilter       string
	RepoFilters      []string
	BranchFilter     string
	PathFilter       string
	TopPathFilter    string
	ExtFilter        string
	LangFilter       string
	SourceFilter     string
	ProviderFilter   string
	DirtyFilter      string
	SymbolKindFilter string
	FreshnessFilter  string
	// Exclude removes any result matching one of these facet values. Unlike the
	// single-value positive facets, each exclusion facet accepts multiple values.
	Exclude FacetFilters
	Sort    string
	// Normalized enables user-friendly matching that ignores case and Latin
	// accents for search atoms. When false, Codebeam keeps Zoekt's default
	// syntax and case:auto semantics.
	Normalized bool
	// Symbols restricts matches to indexed symbol definitions (Zoekt's sym:
	// atom, populated by ctags at index time) instead of full file content.
	Symbols bool
	Allowed []store.Repo
}

type Result struct {
	Query       string
	Files       []FileMatch
	Facets      []FacetGroup
	FileCount   int
	MatchCount  int
	Duration    time.Duration
	ZoektQuery  string
	EmptyReason string
	// FacetedFileCount is how many files the facet counts are computed over (the
	// matched set, capped at facetFileBudget). FacetsTruncated is true when that
	// budget cut off some matched files.
	FacetedFileCount int
	FacetsTruncated  bool
	// Truncated reports that Files does not contain every matched file: the
	// search stopped early (a file/match cap or the timeout was reached) or the
	// display window cut the list. Consumers paging through results should
	// narrow the query instead of assuming completeness.
	Truncated bool
}

type FileMatch struct {
	RepoID     int64
	Repository string
	RepoName   string
	Provider   string
	Branches   []string
	Path       string
	Language   string
	Score      float64
	IndexedAt  int64
	// Commit is the indexed commit hash for file:line@commit provenance; empty
	// for non-git sources. A Dirty match may differ from this commit.
	Commit string
	// CommitFromShard means Commit came from this match's Zoekt shard rather
	// than repository-level fallback metadata. Remote details links can safely
	// pin that immutable revision while an update is being published.
	CommitFromShard bool
	Dirty           bool
	Lines           []LineMatch
}

type LineMatch struct {
	Number      int
	Before      []ContextLine
	After       []ContextLine
	Segments    []Segment
	FileName    bool
	SymbolKinds []string
	// MetaVars holds structural-search metavariable captures ($VAR → matched
	// text). Nil for lexical (Zoekt) results.
	MetaVars map[string]string
}

type ContextLine struct {
	Number int
	Text   string
}

type Segment struct {
	Text  string
	Match bool
}

type FacetGroup struct {
	Field  string
	Label  string
	Values []FacetValue
}

type FacetValue struct {
	Value    string
	Label    string
	Count    int
	Active   bool
	Excluded bool
}

func (e Engine) Search(ctx context.Context, req Request) (Result, error) {
	raw := strings.TrimSpace(req.Query)
	if raw == "" {
		return Result{EmptyReason: "Enter a query to search the selected indexes."}, nil
	}
	zoektQuery, err := BuildZoektQuery(req)
	if err != nil {
		return Result{}, err
	}
	if zoektQuery == "" {
		return Result{
			Query:       raw,
			Facets:      buildFacets(nil, req, time.Now()),
			EmptyReason: "No indexed repositories are available for this search.",
		}, nil
	}

	searcher, err := openDirectorySearcher(ctx, e.IndexDir)
	if err != nil {
		if errors.Is(err, errIndexChanging) {
			return Result{}, err
		}
		return Result{}, fmt.Errorf("open Zoekt index: %w", err)
	}
	defer searcher.Close()

	q, err := query.Parse(zoektQuery)
	if err != nil {
		return Result{}, fmt.Errorf("parse query: %w", err)
	}
	if req.Normalized {
		q = normalizeQuery(q)
	}
	q = query.Map(q, query.ExpandFileContent)
	q = query.Simplify(q)

	opts := zoekt.SearchOptions{
		// Return every matched file (not just the rendered page) so facet counts
		// are exact for realistic searches; snippets are only built for the first
		// displayFileLimit files, so the extra files are cheap. We keep Zoekt's
		// default per-shard match cap (applied by SetDefaults) as a safety ceiling
		// so a pathological query like a single letter can't exhaust memory — such
		// a result set is reported as truncated rather than counted exactly.
		MaxDocDisplayCount:   facetFileBudget,
		MaxMatchDisplayCount: facetMatchBudget,
		NumContextLines:      2,
		MaxWallTime:          8 * time.Second,
	}
	opts.SetDefaults()

	start := time.Now()
	res, err := searcher.Search(ctx, q, &opts)
	if err != nil {
		return Result{}, fmt.Errorf("search Zoekt index: %w", err)
	}
	result := Result{
		Query:      raw,
		FileCount:  res.Stats.FileCount,
		MatchCount: res.Stats.MatchCount,
		Duration:   time.Since(start),
		ZoektQuery: zoektQuery,
	}

	repoByName := make(map[string]store.Repo, len(req.Allowed))
	for _, repo := range req.Allowed {
		repoByName[repo.FullName] = repo
	}
	// DirtyPaths shells out to `git status`, so compute it lazily and at most
	// once per repository that actually appears in the result set.
	dirtyCache := map[string]map[string]struct{}{}
	dirtyFor := func(fullName string) map[string]struct{} {
		if cached, ok := dirtyCache[fullName]; ok {
			return cached
		}
		var paths map[string]struct{}
		if repo := repoByName[fullName]; repo.HostProvider == "local" && repo.LocalPath != "" {
			paths = DirtyPaths(ctx, repo.LocalPath)
		}
		dirtyCache[fullName] = paths
		return paths
	}

	dirtyFilter := normalizeDirtyFilter(req.DirtyFilter)
	languageFilter := strings.TrimSpace(req.LangFilter)
	symbolKindFilter := strings.TrimSpace(req.SymbolKindFilter)
	excluded := NormalizeFacetFilters(req.Exclude)
	facetNow := time.Now()
	// collected holds every matched file (up to the budget) and backs facet
	// counts. We deduplicate identical branch hits, sort this full set first, then
	// render snippets only for the display window so alternate sort orders do not
	// show snippet-less files.
	collected := make([]collectedFileMatch, 0, len(res.Files))
	for _, file := range res.Files {
		repo := repoByName[file.Repository]
		repoName := repo.Name
		if repoName == "" {
			repoName = file.Repository
		}
		_, dirty := dirtyFor(file.Repository)[file.FileName]
		if dirtyFilter == dirtyValue && !dirty {
			continue
		}
		if dirtyFilter == cleanValue && dirty {
			continue
		}
		if languageFilter == unknownValue && strings.TrimSpace(file.Language) != "" {
			continue
		}
		match := FileMatch{
			RepoID:          repo.ID,
			Repository:      file.Repository,
			RepoName:        repoName,
			Provider:        repo.HostProvider,
			Branches:        normalizedBranches(file.Branches, repo.DefaultBranch),
			Path:            file.FileName,
			Language:        file.Language,
			Score:           file.Score,
			IndexedAt:       repo.IndexedAt,
			Commit:          indexedCommit(file.Version, repo.IndexedCommit),
			CommitFromShard: commitComesFromShard(file.Version),
			Dirty:           dirty,
			Lines:           lineMatches(file.LineMatches, symbolKindFilter, excluded.SymbolKinds, false),
		}
		if len(match.Lines) == 0 || fileMatchesExcludedFacet(match, excluded, facetNow) {
			continue
		}
		collected = append(collected, collectedFileMatch{match: match, raw: file})
	}
	rawCollectedCount := len(collected)
	collected = deduplicateCollectedFiles(collected)
	sortCollectedFiles(collected, req.Sort)

	faceted := make([]FileMatch, 0, len(collected))
	for i, item := range collected {
		match := item.match
		if i < displayFileLimit {
			match.Lines = lineMatches(item.raw.LineMatches, symbolKindFilter, excluded.SymbolKinds, true)
		}
		faceted = append(faceted, match)
	}
	if dirtyFilter != "" || languageFilter == unknownValue || symbolKindFilter != "" || exclusionsNeedPostFilter(excluded) || rawCollectedCount != len(faceted) {
		result.FileCount, result.MatchCount = displayedCounts(faceted)
	}
	result.Facets = buildFacets(faceted, req, facetNow)
	result.FacetedFileCount = len(faceted)
	result.FacetsTruncated = result.FileCount > len(faceted)
	if len(faceted) > displayFileLimit {
		result.Files = faceted[:displayFileLimit]
	} else {
		result.Files = faceted
	}
	// Flag partial results whether the display window cut the list or Zoekt's
	// own budgets stopped collecting matched files (FacetsTruncated).
	result.Truncated = len(faceted) > displayFileLimit || result.FacetsTruncated
	return result, nil
}

func BuildZoektQuery(req Request) (string, error) {
	raw := strings.TrimSpace(req.Query)
	if raw == "" {
		return "", nil
	}

	allowed, err := selectedReposForQuery(req)
	if err != nil {
		return "", err
	}
	if len(allowed) == 0 {
		return "", nil
	}

	repoTerms := make([]string, 0, len(allowed))
	for _, repo := range allowed {
		repoTerms = append(repoTerms, "repo:"+anchorRegexp(repo.FullName))
	}

	var terms []string
	if req.Symbols {
		// sym:(...) must not be wrapped in an extra group: Zoekt would then read
		// the literal "sym:" as regex content rather than the symbol atom.
		terms = append(terms, "sym:("+raw+")")
	} else {
		terms = append(terms, "("+raw+")")
	}
	if len(repoTerms) == 1 {
		terms = append(terms, repoTerms[0])
	} else {
		terms = append(terms, "("+strings.Join(repoTerms, " or ")+")")
	}
	if branch := strings.TrimSpace(req.BranchFilter); branch != "" {
		terms = append(terms, fieldTerm("branch", branch))
	}
	if path := strings.TrimSpace(req.PathFilter); path != "" {
		terms = append(terms, fieldTerm("file", path))
	}
	if top := strings.TrimSpace(req.TopPathFilter); top != "" {
		if pattern := topPathPattern(top); pattern != "" {
			terms = append(terms, fieldTerm("file", pattern))
		}
	}
	if ext := normalizeExtFilter(req.ExtFilter); ext != "" {
		terms = append(terms, fieldTerm("file", extensionPattern(ext)))
	}
	if lang := strings.TrimSpace(req.LangFilter); lang != "" && lang != unknownValue {
		terms = append(terms, fieldTerm("lang", lang))
	}

	// Apply exclusions inside Zoekt whenever the facet maps to a native field.
	// Result-side checks below remain as a correctness backstop and cover values
	// Zoekt cannot express, such as an unknown language or working-tree state.
	excluded := NormalizeFacetFilters(req.Exclude)
	for _, branch := range excluded.Branches {
		terms = append(terms, negatedFieldTerm("branch", branch))
	}
	for _, top := range excluded.TopPaths {
		if pattern := topPathPattern(top); pattern != "" {
			terms = append(terms, negatedFieldTerm("file", pattern))
		}
	}
	for _, ext := range excluded.Extensions {
		terms = append(terms, negatedFieldTerm("file", extensionPattern(ext)))
	}
	for _, lang := range excluded.Languages {
		if lang != unknownValue {
			terms = append(terms, negatedFieldTerm("lang", lang))
		}
	}
	if req.Normalized {
		// Normalized search should make uppercase input match lowercase code.
		// Users can still scope exact matching inside their query with case:yes,
		// e.g. `case:yes MySymbol`.
		terms = append(terms, "case:no")
	}
	return strings.Join(terms, " "), nil
}

func selectedReposForQuery(req Request) ([]store.Repo, error) {
	allowed := req.Allowed
	if len(allowed) == 0 {
		return nil, nil
	}

	repoFilters := normalizedRepoFilters(req)
	if len(repoFilters) > 0 {
		wanted := make(map[string]struct{}, len(repoFilters))
		for _, repoFilter := range repoFilters {
			wanted[repoFilter] = struct{}{}
		}
		selected := make([]store.Repo, 0, len(repoFilters))
		for _, repo := range allowed {
			if _, ok := wanted[repo.FullName]; ok {
				selected = append(selected, repo)
			}
		}
		if len(selected) != len(wanted) {
			return nil, errors.New("one or more selected repositories are not available to this user")
		}
		allowed = selected
	}
	if source := normalizeSourceFilter(req.SourceFilter); source != "" {
		allowed = filterRepos(allowed, func(repo store.Repo) bool {
			value, _ := repoSourceFacetValue(repo.FullName, repo.HostProvider)
			return value == source
		})
	}
	if provider := strings.TrimSpace(req.ProviderFilter); provider != "" {
		allowed = filterRepos(allowed, func(repo store.Repo) bool { return repo.HostProvider == provider })
	}
	if freshness := normalizeFreshnessFilter(req.FreshnessFilter); freshness != "" {
		now := time.Now()
		allowed = filterRepos(allowed, func(repo store.Repo) bool {
			value, _ := freshnessFacetValue(repo.IndexedAt, now)
			return value == freshness
		})
	}

	excluded := NormalizeFacetFilters(req.Exclude)
	if len(excluded.Repos) > 0 {
		allowed = filterRepos(allowed, func(repo store.Repo) bool { return !containsString(excluded.Repos, repo.FullName) })
	}
	if len(excluded.Sources) > 0 {
		allowed = filterRepos(allowed, func(repo store.Repo) bool {
			value, _ := repoSourceFacetValue(repo.FullName, repo.HostProvider)
			return !containsString(excluded.Sources, value)
		})
	}
	if len(excluded.Providers) > 0 {
		allowed = filterRepos(allowed, func(repo store.Repo) bool { return !containsString(excluded.Providers, repo.HostProvider) })
	}
	if len(excluded.Freshness) > 0 {
		now := time.Now()
		allowed = filterRepos(allowed, func(repo store.Repo) bool {
			value, _ := freshnessFacetValue(repo.IndexedAt, now)
			return !containsString(excluded.Freshness, value)
		})
	}
	return allowed, nil
}

func normalizedRepoFilters(req Request) []string {
	values := req.RepoFilters
	if len(values) == 0 && strings.TrimSpace(req.RepoFilter) != "" {
		values = []string{req.RepoFilter}
	}
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

func filterRepos(repos []store.Repo, keep func(store.Repo) bool) []store.Repo {
	out := make([]store.Repo, 0, len(repos))
	for _, repo := range repos {
		if keep(repo) {
			out = append(out, repo)
		}
	}
	return out
}

func anchorRegexp(value string) string {
	return "^" + regexp.QuoteMeta(value) + "$"
}

func fieldTerm(field, value string) string {
	if strings.ContainsAny(value, " \t()") {
		return field + ":" + strconv.Quote(value)
	}
	return field + ":" + value
}

func negatedFieldTerm(field, value string) string {
	return "-" + fieldTerm(field, value)
}

func topPathPattern(value string) string {
	if value == topPathRootValue {
		return "^[^/]+$"
	}
	trimmed := strings.Trim(strings.TrimSpace(value), "/")
	if trimmed == "" {
		return ""
	}
	return "^" + regexp.QuoteMeta(trimmed) + "/"
}

func extensionPattern(value string) string {
	if value == noExtensionValue {
		return `(^|/)[^/\.]+$`
	}
	return regexp.QuoteMeta(normalizeExtFilter(value)) + "$"
}

func lineMatches(lines []zoekt.LineMatch, symbolKindFilter string, excludedSymbolKinds []string, withSnippets bool) []LineMatch {
	out := make([]LineMatch, 0, len(lines))
	for _, line := range lines {
		kinds := symbolKinds(line.LineFragments)
		if symbolKindFilter != "" && !containsString(kinds, symbolKindFilter) {
			continue
		}
		if intersectsFold(kinds, excludedSymbolKinds) {
			continue
		}
		lineMatch := LineMatch{
			Number:      line.LineNumber,
			FileName:    line.FileName,
			SymbolKinds: kinds,
		}
		if withSnippets {
			lineMatch.Before = contextLines(line.Before, line.LineNumber-lineCount(line.Before))
			lineMatch.After = contextLines(line.After, line.LineNumber+1)
			lineMatch.Segments = lineSegments(line.Line, line.LineFragments)
		}
		out = append(out, lineMatch)
	}
	return out
}

func lineCount(raw []byte) int {
	text := strings.TrimSuffix(string(raw), "\n")
	if text == "" {
		return 0
	}
	return len(strings.Split(text, "\n"))
}

func contextLines(raw []byte, start int) []ContextLine {
	text := strings.TrimSuffix(string(raw), "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	out := make([]ContextLine, 0, len(lines))
	for i, line := range lines {
		out = append(out, ContextLine{Number: start + i, Text: line})
	}
	return out
}

// DirtyPaths returns the repo-relative paths with uncommitted changes (staged,
// unstaged, or untracked) in the git working tree at root, or nil when the tree
// is clean or git fails. Both the lexical and structural engines use it to badge
// working-tree matches as dirty.
func DirtyPaths(ctx context.Context, root string) map[string]struct{} {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseGitStatusPorcelainZ(out)
}

func parseGitStatusPorcelainZ(out []byte) map[string]struct{} {
	dirty := map[string]struct{}{}
	entries := strings.Split(string(out), "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		path := strings.TrimSpace(entry[3:])
		if path == "" {
			continue
		}
		dirty[path] = struct{}{}
		status := entry[:2]
		if (status[0] == 'R' || status[0] == 'C' || status[1] == 'R' || status[1] == 'C') && i+1 < len(entries) {
			// Porcelain -z includes a second path for rename/copy records. Mark it
			// too so either side can be badged if it appears in the current index.
			if other := strings.TrimSpace(entries[i+1]); other != "" {
				dirty[other] = struct{}{}
			}
			i++
		}
	}
	if len(dirty) == 0 {
		return nil
	}
	return dirty
}

func lineSegments(line []byte, fragments []zoekt.LineFragmentMatch) []Segment {
	text := strings.TrimSuffix(string(line), "\n")
	if len(fragments) == 0 {
		return []Segment{{Text: text}}
	}

	sort.Slice(fragments, func(i, j int) bool {
		return fragments[i].LineOffset < fragments[j].LineOffset
	})

	var out []Segment
	cursor := 0
	for _, fragment := range fragments {
		start := clamp(fragment.LineOffset, 0, len(line))
		end := clamp(fragment.LineOffset+fragment.MatchLength, start, len(line))
		if start > cursor {
			out = append(out, Segment{Text: strings.TrimSuffix(string(line[cursor:start]), "\n")})
		}
		if end > start {
			out = append(out, Segment{Text: strings.TrimSuffix(string(line[start:end]), "\n"), Match: true})
		}
		cursor = max(cursor, end)
	}
	if cursor < len(line) {
		out = append(out, Segment{Text: strings.TrimSuffix(string(line[cursor:]), "\n")})
	}
	return out
}

func symbolKinds(fragments []zoekt.LineFragmentMatch) []string {
	seen := map[string]struct{}{}
	var kinds []string
	for _, fragment := range fragments {
		if fragment.SymbolInfo == nil {
			continue
		}
		kind := strings.TrimSpace(fragment.SymbolInfo.Kind)
		if kind == "" {
			continue
		}
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

func normalizedBranches(branches []string, _ string) []string {
	seen := map[string]struct{}{}
	var out []string
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

func deduplicateCollectedFiles(files []collectedFileMatch) []collectedFileMatch {
	if len(files) < 2 {
		return files
	}
	out := make([]collectedFileMatch, 0, len(files))
	byKey := make(map[string]int, len(files))
	for _, file := range files {
		key := collectedDedupKey(file)
		if existing, ok := byKey[key]; ok {
			existingFile := &out[existing]
			existingFile.match.Branches = mergeBranches(existingFile.match.Branches, file.match.Branches)
			if file.match.Score > existingFile.match.Score {
				existingFile.match.Score = file.match.Score
			}
			existingFile.match.Dirty = existingFile.match.Dirty || file.match.Dirty
			continue
		}
		byKey[key] = len(out)
		out = append(out, file)
	}
	return out
}

func collectedDedupKey(file collectedFileMatch) string {
	h := fnv.New64a()
	writeHashString(h, file.match.Repository)
	writeHashString(h, file.match.Path)
	writeHashString(h, file.match.Language)
	for _, raw := range file.raw.LineMatches {
		if !rawLineIncluded(file.match.Lines, raw) {
			continue
		}
		_, _ = fmt.Fprintf(h, "%d:%t:", raw.LineNumber, raw.FileName)
		_, _ = h.Write(raw.Before)
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(raw.Line)
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(raw.After)
		_, _ = h.Write([]byte{0})
		for _, fragment := range raw.LineFragments {
			kind := ""
			if fragment.SymbolInfo != nil {
				kind = fragment.SymbolInfo.Kind
			}
			_, _ = fmt.Fprintf(h, "%d:%d:%s;", fragment.LineOffset, fragment.MatchLength, kind)
		}
		_, _ = h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

func writeHashString(h interface{ Write([]byte) (int, error) }, value string) {
	_, _ = h.Write([]byte(value))
	_, _ = h.Write([]byte{0})
}

func rawLineIncluded(lines []LineMatch, raw zoekt.LineMatch) bool {
	for _, line := range lines {
		if line.Number == raw.LineNumber && line.FileName == raw.FileName {
			return true
		}
	}
	return false
}

func mergeBranches(left, right []string) []string {
	seen := make(map[string]struct{}, len(left)+len(right))
	out := make([]string, 0, len(left)+len(right))
	for _, branch := range append(left, right...) {
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

func displayedCounts(files []FileMatch) (int, int) {
	matches := 0
	for _, file := range files {
		matches += len(file.Lines)
	}
	return len(files), matches
}

func sortCollectedFiles(files []collectedFileMatch, sortBy string) {
	switch normalizeSort(sortBy) {
	case sortRepo:
		sort.SliceStable(files, func(i, j int) bool {
			left, right := files[i].match, files[j].match
			if c := compareFolded(repoSourceSortKey(left), repoSourceSortKey(right)); c != 0 {
				return c < 0
			}
			if c := compareFolded(repoFacetLabel(left.Repository, left.RepoName), repoFacetLabel(right.Repository, right.RepoName)); c != 0 {
				return c < 0
			}
			if c := compareFolded(left.Path, right.Path); c != 0 {
				return c < 0
			}
			return left.Score > right.Score
		})
	case sortPath:
		sort.SliceStable(files, func(i, j int) bool {
			left, right := files[i].match, files[j].match
			if c := compareFolded(left.Path, right.Path); c != 0 {
				return c < 0
			}
			if c := compareFolded(left.Repository, right.Repository); c != 0 {
				return c < 0
			}
			return left.Score > right.Score
		})
	case sortIndexedDesc:
		sort.SliceStable(files, func(i, j int) bool {
			left, right := files[i].match, files[j].match
			if left.IndexedAt != right.IndexedAt {
				return left.IndexedAt > right.IndexedAt
			}
			return fallbackLess(left, right)
		})
	case sortIndexedAsc:
		sort.SliceStable(files, func(i, j int) bool {
			left, right := files[i].match, files[j].match
			if left.IndexedAt != right.IndexedAt {
				if left.IndexedAt == 0 {
					return false
				}
				if right.IndexedAt == 0 {
					return true
				}
				return left.IndexedAt < right.IndexedAt
			}
			return fallbackLess(left, right)
		})
	case sortMatchCount:
		sort.SliceStable(files, func(i, j int) bool {
			left, right := files[i].match, files[j].match
			if len(left.Lines) != len(right.Lines) {
				return len(left.Lines) > len(right.Lines)
			}
			return fallbackLess(left, right)
		})
	}
}

func fallbackLess(left, right FileMatch) bool {
	if left.Score != right.Score {
		return left.Score > right.Score
	}
	if c := compareFolded(left.Repository, right.Repository); c != 0 {
		return c < 0
	}
	return compareFolded(left.Path, right.Path) < 0
}

func repoSourceSortKey(file FileMatch) string {
	value, label := repoSourceFacetValue(file.Repository, file.Provider)
	if label != "" {
		return label
	}
	return value
}

func compareFolded(left, right string) int {
	return strings.Compare(strings.ToLower(left), strings.ToLower(right))
}

// BuildFacets aggregates facet groups over an arbitrary matched-file set.
// Exported so the structural engine shares the facet UI with lexical search.
func BuildFacets(files []FileMatch, req Request, now time.Time) []FacetGroup {
	return buildFacets(files, req, now)
}

// SelectedRepos narrows req.Allowed by the repo/source/provider/freshness
// filters with the same semantics and errors as lexical search.
func SelectedRepos(req Request) ([]store.Repo, error) {
	return selectedReposForQuery(req)
}

// MatchTopPath reports whether a repo-relative path satisfies a top_path
// facet filter. Empty matches everything; the special root value matches
// files with no directory.
func MatchTopPath(relPath, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	value, _ := topPathFacetValue(relPath)
	return value == filter
}

// MatchExtension reports whether a path satisfies an extension facet filter,
// including the special no-extension value.
func MatchExtension(relPath, filter string) bool {
	filter = normalizeExtFilter(filter)
	if filter == "" {
		return true
	}
	value, _ := extensionFacetValue(relPath)
	return value == filter
}

// NormalizeFacetFilters trims, canonicalizes, and deduplicates exclusion
// values. It is exported so non-Zoekt engines can apply the same semantics.
func NormalizeFacetFilters(in FacetFilters) FacetFilters {
	return FacetFilters{
		Sources:     normalizeFacetValues(in.Sources, normalizeSourceFilter),
		Repos:       normalizeFacetValues(in.Repos, strings.TrimSpace),
		Branches:    normalizeFacetValues(in.Branches, strings.TrimSpace),
		Languages:   normalizeFacetValues(in.Languages, strings.TrimSpace),
		TopPaths:    normalizeFacetValues(in.TopPaths, strings.TrimSpace),
		Extensions:  normalizeFacetValues(in.Extensions, normalizeExtFilter),
		Providers:   normalizeFacetValues(in.Providers, strings.TrimSpace),
		Dirty:       normalizeFacetValues(in.Dirty, normalizeDirtyFilter),
		SymbolKinds: normalizeFacetValues(in.SymbolKinds, strings.TrimSpace),
		Freshness:   normalizeFacetValues(in.Freshness, normalizeFreshnessFilter),
	}
}

func normalizeFacetValues(values []string, normalize func(string) string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = normalize(value)
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

func exclusionsNeedPostFilter(excluded FacetFilters) bool {
	return len(excluded.Dirty) > 0 || len(excluded.SymbolKinds) > 0 || containsString(excluded.Languages, unknownValue)
}

func fileMatchesExcludedFacet(file FileMatch, excluded FacetFilters, now time.Time) bool {
	if containsString(excluded.Repos, file.Repository) {
		return true
	}
	source, _ := repoSourceFacetValue(file.Repository, file.Provider)
	if containsString(excluded.Sources, source) || containsString(excluded.Providers, file.Provider) {
		return true
	}
	if intersectsString(file.Branches, excluded.Branches) || containsFold(excluded.Languages, file.Language) ||
		(strings.TrimSpace(file.Language) == "" && containsString(excluded.Languages, unknownValue)) {
		return true
	}
	for _, value := range excluded.TopPaths {
		if MatchTopPath(file.Path, value) {
			return true
		}
	}
	for _, value := range excluded.Extensions {
		if MatchExtension(file.Path, value) {
			return true
		}
	}
	dirty := cleanValue
	if file.Dirty {
		dirty = dirtyValue
	}
	if containsString(excluded.Dirty, dirty) {
		return true
	}
	freshness, _ := freshnessFacetValue(file.IndexedAt, now)
	return containsString(excluded.Freshness, freshness)
}

func buildFacets(files []FileMatch, req Request, now time.Time) []FacetGroup {
	type bucket struct {
		counts map[string]int
		labels map[string]string
	}
	newBucket := func() bucket { return bucket{counts: map[string]int{}, labels: map[string]string{}} }
	inc := func(b bucket, value, label string) {
		if value == "" {
			value = unknownValue
		}
		b.counts[value]++
		if label != "" {
			b.labels[value] = label
		}
	}

	sources := newBucket()
	repos := newBucket()
	branches := newBucket()
	languages := newBucket()
	topPaths := newBucket()
	extensions := newBucket()
	providers := newBucket()
	dirty := newBucket()
	symbolKindsBucket := newBucket()
	freshness := newBucket()

	for _, file := range files {
		sourceValue, sourceLabel := repoSourceFacetValue(file.Repository, file.Provider)
		inc(sources, sourceValue, sourceLabel)
		inc(repos, file.Repository, repoFacetLabel(file.Repository, file.RepoName))
		for _, branch := range file.Branches {
			inc(branches, branch, branch)
		}
		lang := file.Language
		if strings.TrimSpace(lang) == "" {
			lang = unknownValue
		}
		inc(languages, lang, defaultFacetLabel("language", lang))
		topValue, topLabel := topPathFacetValue(file.Path)
		inc(topPaths, topValue, topLabel)
		extValue, extLabel := extensionFacetValue(file.Path)
		inc(extensions, extValue, extLabel)
		inc(providers, file.Provider, providerFacetLabel(file.Provider))
		if file.Dirty {
			inc(dirty, dirtyValue, "Dirty / uncommitted")
		} else {
			inc(dirty, cleanValue, "Clean / indexed")
		}
		freshValue, freshLabel := freshnessFacetValue(file.IndexedAt, now)
		inc(freshness, freshValue, freshLabel)
		for _, line := range file.Lines {
			seenLineKinds := map[string]struct{}{}
			for _, kind := range line.SymbolKinds {
				if _, ok := seenLineKinds[kind]; ok {
					continue
				}
				seenLineKinds[kind] = struct{}{}
				inc(symbolKindsBucket, kind, symbolKindLabel(kind))
			}
		}
	}

	selected := map[string][]string{
		"source":      {normalizeSourceFilter(req.SourceFilter)},
		"repo":        normalizedRepoFilters(req),
		"branch":      {strings.TrimSpace(req.BranchFilter)},
		"language":    {strings.TrimSpace(req.LangFilter)},
		"top_path":    {strings.TrimSpace(req.TopPathFilter)},
		"extension":   {normalizeExtFilter(req.ExtFilter)},
		"provider":    {strings.TrimSpace(req.ProviderFilter)},
		"dirty":       {normalizeDirtyFilter(req.DirtyFilter)},
		"symbol_kind": {strings.TrimSpace(req.SymbolKindFilter)},
		"freshness":   {normalizeFreshnessFilter(req.FreshnessFilter)},
	}
	exclude := NormalizeFacetFilters(req.Exclude)
	excluded := map[string][]string{
		"source":      exclude.Sources,
		"repo":        exclude.Repos,
		"branch":      exclude.Branches,
		"language":    exclude.Languages,
		"top_path":    exclude.TopPaths,
		"extension":   exclude.Extensions,
		"provider":    exclude.Providers,
		"dirty":       exclude.Dirty,
		"symbol_kind": exclude.SymbolKinds,
		"freshness":   exclude.Freshness,
	}

	groups := make([]FacetGroup, 0, 10)
	appendGroup := func(field, label string, b bucket, fixedOrder []string) {
		group, ok := makeFacetGroup(field, label, b.counts, b.labels, selected[field], excluded[field], fixedOrder)
		if ok {
			groups = append(groups, group)
		}
	}
	appendGroup("source", "Source", sources, nil)
	appendGroup("repo", "Repository", repos, nil)
	appendGroup("branch", "Branch", branches, nil)
	appendGroup("language", "Language", languages, nil)
	appendGroup("top_path", "Top-level path", topPaths, nil)
	appendGroup("extension", "File extension", extensions, nil)
	appendGroup("provider", "Provider", providers, nil)
	appendGroup("dirty", "Working tree", dirty, []string{dirtyValue, cleanValue})
	appendGroup("symbol_kind", "Symbol kind", symbolKindsBucket, nil)
	appendGroup("freshness", "Freshness", freshness, []string{freshnessHour, freshnessDay, freshnessWeek, freshnessMonth, freshnessOlder, freshnessUnknown})
	return groups
}

func makeFacetGroup(field, label string, counts map[string]int, labels map[string]string, selected, excluded []string, fixedOrder []string) (FacetGroup, bool) {
	selectedSet := map[string]struct{}{}
	excludedSet := map[string]struct{}{}
	ensureValue := func(value string, set map[string]struct{}) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		set[value] = struct{}{}
		if _, ok := counts[value]; !ok {
			counts[value] = 0
		}
		if labels[value] == "" {
			labels[value] = defaultFacetLabel(field, value)
		}
	}
	for _, value := range selected {
		ensureValue(value, selectedSet)
	}
	for _, value := range excluded {
		ensureValue(value, excludedSet)
	}
	if len(counts) == 0 {
		return FacetGroup{}, false
	}

	values := make([]FacetValue, 0, len(counts))
	appendValue := func(value string) {
		count, ok := counts[value]
		if !ok {
			return
		}
		facetLabel := labels[value]
		if facetLabel == "" {
			facetLabel = defaultFacetLabel(field, value)
		}
		_, excluded := excludedSet[value]
		_, active := selectedSet[value]
		// A direct API caller can submit contradictory include and exclude
		// values. Exclusion wins, matching the actual result set.
		active = active && !excluded
		values = append(values, FacetValue{Value: value, Label: facetLabel, Count: count, Active: active, Excluded: excluded})
	}
	seen := map[string]struct{}{}
	for _, value := range fixedOrder {
		appendValue(value)
		seen[value] = struct{}{}
	}
	var rest []string
	for value := range counts {
		if _, ok := seen[value]; ok {
			continue
		}
		rest = append(rest, value)
	}
	sort.Slice(rest, func(i, j int) bool {
		ci, cj := counts[rest[i]], counts[rest[j]]
		if ci != cj {
			return ci > cj
		}
		return defaultFacetLabel(field, rest[i]) < defaultFacetLabel(field, rest[j])
	})
	for _, value := range rest {
		appendValue(value)
	}
	if len(fixedOrder) == 0 {
		sort.SliceStable(values, func(i, j int) bool {
			state := func(v FacetValue) int {
				if v.Active {
					return 0
				}
				if v.Excluded {
					return 1
				}
				return 2
			}
			if left, right := state(values[i]), state(values[j]); left != right {
				return left < right
			}
			if values[i].Count != values[j].Count {
				return values[i].Count > values[j].Count
			}
			return strings.ToLower(values[i].Label) < strings.ToLower(values[j].Label)
		})
	}
	return FacetGroup{Field: field, Label: label, Values: values}, true
}

func repoSourceFacetValue(fullName, provider string) (string, string) {
	trimmed := strings.Trim(strings.TrimSpace(fullName), "/")
	if provider == "local" || trimmed == "local" || strings.HasPrefix(trimmed, "local/") {
		return "local", "Local"
	}
	if trimmed == "" {
		return unknownValue, "Unknown"
	}
	source, _, ok := strings.Cut(trimmed, "/")
	if !ok {
		source = trimmed
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return unknownValue, "Unknown"
	}
	return strings.ToLower(source), source
}

func sourceFacetLabel(value string) string {
	switch value {
	case "", unknownValue:
		return "Unknown"
	case "local":
		return "Local"
	default:
		return value
	}
}

func repoFacetLabel(fullName, repoName string) string {
	trimmed := strings.Trim(strings.TrimSpace(fullName), "/")
	if trimmed != "" {
		if source, rest, ok := strings.Cut(trimmed, "/"); ok && rest != "" {
			if source == "local" && strings.TrimSpace(repoName) != "" {
				return repoName
			}
			return rest
		}
	}
	if strings.TrimSpace(repoName) != "" {
		return repoName
	}
	if trimmed != "" {
		return trimmed
	}
	return "Unknown"
}

func topPathFacetValue(filePath string) (string, string) {
	trimmed := strings.Trim(filePath, "/")
	if trimmed == "" || !strings.Contains(trimmed, "/") {
		return topPathRootValue, "Repository root"
	}
	first, _, _ := strings.Cut(trimmed, "/")
	return first, first + "/"
}

func extensionFacetValue(filePath string) (string, string) {
	ext := strings.ToLower(path.Ext(filePath))
	if ext == "" {
		return noExtensionValue, "No extension"
	}
	return ext, ext
}

func freshnessFacetValue(indexedAt int64, now time.Time) (string, string) {
	if indexedAt <= 0 {
		return freshnessUnknown, "Unknown"
	}
	age := now.Sub(time.Unix(indexedAt, 0))
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Hour:
		return freshnessHour, "Last hour"
	case age < 24*time.Hour:
		return freshnessDay, "Last 24 hours"
	case age < 7*24*time.Hour:
		return freshnessWeek, "Last 7 days"
	case age < 30*24*time.Hour:
		return freshnessMonth, "Last 30 days"
	default:
		return freshnessOlder, "Older"
	}
}

// FacetValueLabel returns the human label for a raw facet filter value, e.g.
// "dirty" → "Dirty / uncommitted". Used by the web UI to render active filter
// chips with the same wording as the facet lists.
func FacetValueLabel(field, value string) string {
	return defaultFacetLabel(field, value)
}

func defaultFacetLabel(field, value string) string {
	switch field {
	case "source":
		return sourceFacetLabel(value)
	case "repo":
		return repoFacetLabel(value, "")
	case "top_path":
		if value == topPathRootValue {
			return "Repository root"
		}
		return value + "/"
	case "extension":
		if value == noExtensionValue {
			return "No extension"
		}
		return value
	case "provider":
		return providerFacetLabel(value)
	case "dirty":
		if value == dirtyValue {
			return "Dirty / uncommitted"
		}
		if value == cleanValue {
			return "Clean / indexed"
		}
	case "freshness":
		switch value {
		case freshnessHour:
			return "Last hour"
		case freshnessDay:
			return "Last 24 hours"
		case freshnessWeek:
			return "Last 7 days"
		case freshnessMonth:
			return "Last 30 days"
		case freshnessOlder:
			return "Older"
		case freshnessUnknown:
			return "Unknown"
		}
	case "symbol_kind":
		return symbolKindLabel(value)
	case "language":
		if value == unknownValue || value == "" {
			return "Unknown"
		}
	}
	if value == unknownValue || value == "" {
		return "Unknown"
	}
	return value
}

func providerFacetLabel(provider string) string {
	switch provider {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	case "local":
		return "Local"
	case "dev":
		return "Development"
	case "":
		return "Unknown"
	default:
		if strings.HasPrefix(provider, "gitlab:") {
			return "GitLab"
		}
		return provider
	}
}

func symbolKindLabel(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "Unknown"
	}
	kind = strings.ReplaceAll(kind, "_", " ")
	kind = strings.ReplaceAll(kind, "-", " ")
	parts := strings.Fields(kind)
	for i, part := range parts {
		if len(part) == 1 {
			parts[i] = strings.ToUpper(part)
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func normalizeExtFilter(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	if value == noExtensionValue {
		return value
	}
	if !strings.HasPrefix(value, ".") {
		value = "." + value
	}
	return value
}

// NormalizeDirtyFilter maps user-supplied dirty-filter values ("1", "true",
// "dirty", ...) onto the canonical "dirty"/"clean" facet values; anything else
// normalizes to "" (no filter). Shared with the structural engine.
func NormalizeDirtyFilter(value string) string {
	return normalizeDirtyFilter(value)
}

func normalizeDirtyFilter(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case dirtyValue, "1", "true", "yes":
		return dirtyValue
	case cleanValue, "0", "false", "no":
		return cleanValue
	default:
		return ""
	}
}

func normalizeSourceFilter(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "/"))
}

func normalizeSort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", sortRelevance:
		return sortRelevance
	case sortRepo, "repository":
		return sortRepo
	case sortPath, "file":
		return sortPath
	case sortIndexedDesc, "freshness", "newest", "newest_indexed":
		return sortIndexedDesc
	case sortIndexedAsc, "oldest", "oldest_indexed":
		return sortIndexedAsc
	case sortMatchCount, "matches", "matches_desc":
		return sortMatchCount
	default:
		return sortRelevance
	}
}

func normalizeFreshnessFilter(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case freshnessHour, freshnessDay, freshnessWeek, freshnessMonth, freshnessOlder, freshnessUnknown:
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

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

func intersectsString(left, right []string) bool {
	for _, value := range left {
		if containsString(right, value) {
			return true
		}
	}
	return false
}

func intersectsFold(left, right []string) bool {
	for _, value := range left {
		if containsFold(right, value) {
			return true
		}
	}
	return false
}

func clamp(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
