package web

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/ctourriere/codebeam/internal/indexer"
	"github.com/ctourriere/codebeam/internal/store"
)

// RepoPage drives the two-pane repository view (file tree + main pane). The same
// struct renders the repo landing (View == "overview"), a file (View == "file"),
// and URL-backed repo search results (View == "search").
type RepoPage struct {
	User        *store.User
	Repo        store.Repo
	RepoTitle   string
	SourceLabel string
	StatusLabel string
	StatusClass string
	IndexedAt   string
	BranchLabel string
	Tree        []TreeNode
	View        string
	Path        string
	Crumbs      []Crumb
	Lines       []CodeLine
	Symbols     []indexer.FileSymbol
	FocusLine   int
	Search      SearchPageData
	Error       string
}

// Crumb is one segment of the file path breadcrumb.
type Crumb struct {
	Name string
	Last bool
}

// TreeEntry is an immediate child of a directory in the repository.
type TreeEntry struct {
	Name  string
	Path  string
	IsDir bool
}

// TreeNode is a rendered file-tree node. Directories are lazy: Children is only
// populated (Loaded == true) for ancestors of the active file; everything else
// is fetched on demand via /repo/{id}/tree.
type TreeNode struct {
	RepoID   int64
	Name     string
	Path     string
	IsDir    bool
	Active   bool
	Open     bool
	Loaded   bool
	Children []TreeNode
}

func (s *Server) handleRepoView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/repo/")
	idPart, sub, _ := strings.Cut(rest, "/")
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
	if sub == "tree" {
		s.handleRepoTree(w, r, *repo)
		return
	}
	if sub != "" {
		http.NotFound(w, r)
		return
	}

	page := s.repoPage(*repo, user)
	if repoSearchActive(r) {
		page.View = "search"
		page.Search = s.repoSearchPageData(r.Context(), user, *repo, r)
		if s.isHX(r) {
			s.render(w, http.StatusOK, "partial_search_results", page.Search)
			return
		}
	} else {
		page.View = "overview"
		if s.isHX(r) {
			w.Header().Set("HX-Push-Url", repoSearchURL(repo.ID, SearchParams{}))
			s.render(w, http.StatusOK, "partial_repo_overview", page)
			return
		}
	}
	tree, err := s.buildTree(*repo, "")
	if err != nil {
		page.Error = err.Error()
	} else {
		page.Tree = tree
	}
	s.render(w, http.StatusOK, "page_repo", page)
}

func (s *Server) handleRepoTree(w http.ResponseWriter, r *http.Request, repo store.Repo) {
	relPath := strings.TrimSpace(r.URL.Query().Get("path"))
	entries, err := s.readDir(repo, relPath)
	if err != nil {
		s.render(w, http.StatusOK, "tree_children", []TreeNode{})
		return
	}
	s.render(w, http.StatusOK, "tree_children", nodesFromEntries(repo.ID, entries))
}

// repoPage assembles the fields shared by every repo-view render.
func (s *Server) repoPage(repo store.Repo, user *store.User) RepoPage {
	statusLabel := "Not indexed"
	statusClass := "badge-ghost"
	indexedAt := "Never indexed"
	if repo.IndexedAt > 0 {
		statusLabel = "Indexed"
		statusClass = "badge-success"
		indexedAt = sinceUnix(repo.IndexedAt)
	}
	if repo.LastIndexError != "" {
		statusLabel = "Needs index"
		statusClass = "badge-warning"
	}
	return RepoPage{
		User:        user,
		Repo:        repo,
		RepoTitle:   repoTitle(repo),
		SourceLabel: providerName(repo.HostProvider),
		StatusLabel: statusLabel,
		StatusClass: statusClass,
		IndexedAt:   indexedAt,
		BranchLabel: repo.BranchLabel(),
	}
}

// readDir lists the immediate children of a repo-relative directory, hiding the
// same directories the indexer skips and dropping symlinks to avoid escaping the
// repo. relPath is "" for the repository root.
func (s *Server) readDir(repo store.Repo, relPath string) ([]TreeEntry, error) {
	root := s.indexer.SourceRoot(repo)
	if root == "" {
		return nil, errors.New("repository source is unavailable")
	}
	dir := root
	if relPath != "" {
		joined, err := safeJoin(root, relPath)
		if err != nil {
			return nil, err
		}
		dir = joined
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]TreeEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		isDir := entry.IsDir()
		if isDir && indexer.ShouldSkipDir(name) {
			continue
		}
		childPath := name
		if relPath != "" {
			childPath = path.Join(relPath, name)
		}
		out = append(out, TreeEntry{Name: name, Path: childPath, IsDir: isDir})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func nodesFromEntries(repoID int64, entries []TreeEntry) []TreeNode {
	nodes := make([]TreeNode, len(entries))
	for i, entry := range entries {
		nodes[i] = TreeNode{RepoID: repoID, Name: entry.Name, Path: entry.Path, IsDir: entry.IsDir}
	}
	return nodes
}

// buildTree returns the root listing with the ancestor directories of activePath
// pre-expanded so a deep-linked file is revealed in the tree. activePath is ""
// for the landing view.
func (s *Server) buildTree(repo store.Repo, activePath string) ([]TreeNode, error) {
	return s.expandTree(repo, "", activePath)
}

func (s *Server) expandTree(repo store.Repo, dirPath, activePath string) ([]TreeNode, error) {
	entries, err := s.readDir(repo, dirPath)
	if err != nil {
		return nil, err
	}
	nodes := nodesFromEntries(repo.ID, entries)
	if activePath == "" {
		return nodes, nil
	}
	for i := range nodes {
		node := &nodes[i]
		if node.IsDir {
			if activePath == node.Path || strings.HasPrefix(activePath, node.Path+"/") {
				children, err := s.expandTree(repo, node.Path, activePath)
				if err == nil {
					node.Children = children
					node.Open = true
					node.Loaded = true
				}
			}
		} else if node.Path == activePath {
			node.Active = true
		}
	}
	return nodes, nil
}

func pathCrumbs(slashPath string) []Crumb {
	parts := strings.Split(slashPath, "/")
	crumbs := make([]Crumb, 0, len(parts))
	for i, part := range parts {
		crumbs = append(crumbs, Crumb{Name: part, Last: i == len(parts)-1})
	}
	return crumbs
}
