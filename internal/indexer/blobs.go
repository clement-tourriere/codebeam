package indexer

import (
	"context"
	"fmt"
	"strings"

	"github.com/ctourriere/codebeam/internal/store"
)

// BranchFile is one file at a branch ref, addressed by blob hash. It lets
// callers read committed file contents without materializing a working tree.
type BranchFile struct {
	Path string
	Blob string
	Size int64
}

// ResolveBranchRef maps a user-facing branch name to the git ref that holds
// it inside the repo's clone (or the local repo itself), verifying that the
// ref exists. Remote clones only materialize the primary branch as a working
// tree; other indexed branches live under refs/remotes/origin/*.
func (i *Indexer) ResolveBranchRef(ctx context.Context, repo store.Repo, branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "HEAD", nil
	}
	root := i.SourceRoot(repo)
	candidates := []string{branch}
	if repo.HostProvider != "local" {
		candidates = []string{remoteBranchRef(branch), branch}
	}
	for _, ref := range candidates {
		if _, err := gitOutput(ctx, root, "", "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return ref, nil
		}
	}
	return "", fmt.Errorf("branch %q not found in repository %s", branch, repo.FullName)
}

// ListBranchFiles enumerates the files reachable at ref in the repo's git
// object store, without touching the working tree.
func (i *Indexer) ListBranchFiles(ctx context.Context, repo store.Repo, ref string) ([]BranchFile, error) {
	entries, err := listGitTree(ctx, i.SourceRoot(repo), ref)
	if err != nil {
		return nil, err
	}
	files := make([]BranchFile, 0, len(entries))
	for _, entry := range entries {
		files = append(files, BranchFile{Path: entry.Path, Blob: entry.Blob, Size: entry.Size})
	}
	return files, nil
}

// BlobReader streams blob contents out of a repo's git object store using a
// single long-lived `git cat-file --batch` process. Not safe for concurrent
// use. Close must be called when done.
type BlobReader struct {
	batch *gitCatFileBatch
}

// OpenBlobReader opens a blob reader for the repo's clone (or local path).
func (i *Indexer) OpenBlobReader(ctx context.Context, repo store.Repo) (*BlobReader, error) {
	batch, err := newGitCatFileBatch(ctx, i.SourceRoot(repo))
	if err != nil {
		return nil, err
	}
	return &BlobReader{batch: batch}, nil
}

// Read returns the contents of one blob.
func (r *BlobReader) Read(ctx context.Context, blob string) ([]byte, error) {
	return r.batch.Cat(ctx, blob)
}

// Close terminates the underlying git process.
func (r *BlobReader) Close() error {
	return r.batch.Close()
}
