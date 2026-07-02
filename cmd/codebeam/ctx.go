package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ctourriere/codebeam/internal/config"
	codesearch "github.com/ctourriere/codebeam/internal/search"
)

const defaultCtxMaxChars = 12000

func runCtxCommand(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("ctx", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", "", "restrict to a repository full name")
	path := flags.String("path", "", "restrict to paths matching a Zoekt file filter")
	lang := flags.String("lang", "", "restrict to a language")
	maxChars := flags.Int("max-chars", defaultCtxMaxChars, "maximum approximate output characters")
	maxFiles := flags.Int("max-files", 10, "maximum files to include")
	if err := flags.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if query == "" {
		_, _ = fmt.Fprintln(stderr, "usage: codebeam ctx [--repo full/name] [--path regex] [--lang go] [--max-chars n] <query>")
		return errors.New("query is required")
	}
	if *maxChars <= 0 {
		return errors.New("max-chars must be positive")
	}
	if *maxFiles <= 0 {
		return errors.New("max-files must be positive")
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close() // nolint:errcheck
	repos, err := st.ListIndexedRepos(ctx)
	if err != nil {
		return err
	}
	result, err := (codesearch.Engine{IndexDir: cfg.IndexDir}).Search(ctx, codesearch.Request{
		Query:      query,
		RepoFilter: *repo,
		PathFilter: *path,
		LangFilter: *lang,
		Allowed:    repos,
	})
	if err != nil {
		return err
	}
	writeContextBundle(stdout, result, *maxChars, *maxFiles)
	return nil
}

func writeContextBundle(w io.Writer, result codesearch.Result, maxChars, maxFiles int) {
	used := 0
	write := func(format string, args ...any) bool {
		text := fmt.Sprintf(format, args...)
		if used+len(text) > maxChars {
			return false
		}
		_, _ = io.WriteString(w, text)
		used += len(text)
		return true
	}

	if !write("# Codebeam context\n\nQuery: `%s`\nMatches: %d in %d files\n\n", result.Query, result.MatchCount, result.FileCount) {
		return
	}
	if result.EmptyReason != "" {
		_, _ = fmt.Fprintf(w, "%s\n", result.EmptyReason)
		return
	}

	filesWritten := 0
	for _, file := range result.Files {
		if filesWritten >= maxFiles {
			break
		}
		heading := file.Repository + ":" + file.Path
		if c := shortCommit(file.Commit); c != "" {
			heading += "@" + c
		}
		if !write("## %s\n\n", heading) {
			break
		}
		filesWritten++
		for _, line := range file.Lines {
			for _, ctxLine := range line.Before {
				if !write("%d: %s\n", ctxLine.Number, ctxLine.Text) {
					writeTruncated(w, &used, maxChars)
					return
				}
			}
			if !write("%d: %s\n", line.Number, plainSegments(line.Segments)) {
				writeTruncated(w, &used, maxChars)
				return
			}
			for _, ctxLine := range line.After {
				if !write("%d: %s\n", ctxLine.Number, ctxLine.Text) {
					writeTruncated(w, &used, maxChars)
					return
				}
			}
			if !write("\n") {
				writeTruncated(w, &used, maxChars)
				return
			}
		}
	}
	if len(result.Files) > filesWritten {
		_, _ = fmt.Fprintf(w, "_Omitted %d additional files. Increase --max-files or --max-chars for more._\n", len(result.Files)-filesWritten)
	}
}

func plainSegments(segments []codesearch.Segment) string {
	var b strings.Builder
	for _, segment := range segments {
		b.WriteString(segment.Text)
	}
	return b.String()
}

// shortCommit abbreviates a commit hash for compact file:line@commit citations.
func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) >= 8 {
		return commit[:8]
	}
	return commit
}

func writeTruncated(w io.Writer, used *int, maxChars int) {
	message := "\n_Truncated by --max-chars._\n"
	if *used+len(message) <= maxChars {
		_, _ = io.WriteString(w, message)
		*used += len(message)
	}
}
