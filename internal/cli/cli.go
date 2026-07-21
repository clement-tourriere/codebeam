package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ctourriere/codebeam/internal/version"
)

const usage = `cb — search your Codebeam-indexed repositories from the command line

cb talks to a Codebeam server over the same authenticated endpoint MCP
agents use, so results are identical: ranked snippets with
repo:path:line citations.

Usage:
  cb <command> [flags] [args]

Search commands:
  search <query>            Lexical/regex search (Zoekt syntax: file:, lang:, sym:, ...)
  ast <pattern> --lang <l>  Structural (AST) search with an ast-grep pattern
  def <symbol>              Find where a symbol is defined
  refs <symbol>             Find references to a symbol (word-boundary match)
  read <repo:path[:N-M]>    Read a line range from a file
  tree <repo> [path]        List a repository's files
  repos                     List indexed repositories
  stats                     Per-repository statistics (languages, size, freshness)

Agent commands:
  mcp                       Serve MCP over stdio, proxying to the server with
                            cb's stored credentials (Cloudflare Access included):
                            claude mcp add codebeam -- cb mcp

Account commands:
  login [server]            Sign in via the browser, or --token <pat> for headless use
  logout [server]           Forget stored credentials
  status                    Show server, credentials, and connectivity
  version                   Print the cb version

Server selection: --server flag > CODEBEAM_URL > last login > http://localhost:8080
Headless auth:    set CODEBEAM_TOKEN to a personal access token
                  (web UI: Settings -> API tokens) — no login needed.
Cloudflare Access: detected automatically; login uses the cloudflared CLI,
                  or set CF_ACCESS_CLIENT_ID / CF_ACCESS_CLIENT_SECRET
                  to a service token for headless use.

Examples:
  cb login codebeam.acme.dev
  cb search "func NewServer" --lang go
  cb ast 'if $ERR != nil { $$$ }' --lang go --repo local/myrepo
  cb read local/myrepo:internal/web/api.go:100-160
  CODEBEAM_TOKEN=cbp_... cb search "retry backoff" -n 5

Run 'cb <command> -h' for the flags of one command.
`

// errParse marks a flag-parsing failure the flag package has already reported
// on stderr, so run() sets the exit code without printing twice.
var errParse = errors.New("usage error")

type app struct {
	ctx    context.Context
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	hc     *http.Client
	// openURL launches the browser during login; injectable for tests.
	openURL func(string) error
	// cfCredentials obtains Cloudflare Access headers for a protected server;
	// injectable for tests.
	cfCredentials func(ctx context.Context, server string, interactive bool, out io.Writer) (http.Header, error)
}

// Run executes one cb invocation and returns its process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := &app{
		ctx:    ctx,
		stdin:  os.Stdin,
		stdout: stdout,
		stderr: stderr,
		// Generous timeout: structural search walks whole repositories.
		hc:            &http.Client{Timeout: 120 * time.Second},
		openURL:       openBrowser,
		cfCredentials: cfAccessCredentials,
	}
	return a.run(args)
}

func (a *app) run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, usage)
		return 2
	}
	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "help", "-h", "--help":
		fmt.Fprint(a.stdout, usage)
	case "version", "--version", "-v":
		fmt.Fprintln(a.stdout, "cb "+version.Version)
	case "login":
		err = a.cmdLogin(rest)
	case "logout":
		err = a.cmdLogout(rest)
	case "status":
		err = a.cmdStatus(rest)
	case "search":
		err = a.cmdSearch(rest)
	case "ast":
		err = a.cmdAST(rest)
	case "def", "sym", "symbol":
		err = a.cmdDef(rest)
	case "refs", "references":
		err = a.cmdRefs(rest)
	case "read":
		err = a.cmdRead(rest)
	case "tree":
		err = a.cmdTree(rest)
	case "repos":
		err = a.cmdRepos(rest)
	case "stats":
		err = a.cmdStats(rest)
	case "mcp":
		err = a.cmdMCP(rest)
	default:
		fmt.Fprintf(a.stderr, "cb: unknown command %q — run `cb help`\n", cmd)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errParse):
		return 2
	default:
		fmt.Fprintln(a.stderr, "cb: "+err.Error())
		return 1
	}
}

func (a *app) flagSet(name, synopsis string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(a.stderr)
	flags.Usage = func() {
		fmt.Fprintf(a.stderr, "usage: %s\n", synopsis)
		flags.PrintDefaults()
	}
	return flags
}

// parseArgs parses flags that may appear before or after positional arguments
// (Go's flag package stops at the first positional) and honors a literal "--"
// as the end of flags. Returns the positional arguments in order.
func parseArgs(flags *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, arg := range args {
		if arg == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, flag.ErrHelp
			}
			return nil, errParse
		}
		rest := flags.Args()
		if len(rest) == 0 {
			return append(positional, tail...), nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func (a *app) badUsage(flags *flag.FlagSet, message string) error {
	fmt.Fprintf(a.stderr, "cb %s: %s\n", flags.Name(), message)
	flags.Usage()
	return errParse
}

// toolClient builds the /mcp client for one invocation, resolving the server
// and credentials and wiring refreshed tokens back to the credentials file.
func (a *app) toolClient(flagServer string) (*client, error) {
	cf, err := loadConfig()
	if err != nil {
		return nil, err
	}
	server, _, err := resolveServer(flagServer, cf)
	if err != nil {
		return nil, err
	}
	creds := resolveCredentials(cf, server)
	if creds == nil {
		return nil, fmt.Errorf("not logged in to %s — run `cb login %s`, or set %s to a personal access token", server, server, envToken)
	}
	hc := a.hc
	// The stored login remembers when the server sits behind Cloudflare
	// Access; a service token in the environment covers pure-env (CI) use
	// where nothing is stored.
	if stored := cf.Servers[server]; creds.CFAccess || cfServiceTokenSet() || (stored != nil && stored.CFAccess) {
		headers, err := a.cfCredentials(a.ctx, server, false, io.Discard)
		if err != nil {
			return nil, err
		}
		hc = withExtraHeaders(a.hc, server, headers)
	}
	persist := func(c *credentials) error {
		cf.Servers[server] = c
		return saveConfig(cf)
	}
	return &client{server: server, creds: creds, hc: hc, persist: persist}, nil
}

// printTool calls one tool and writes its text to stdout, guaranteeing a
// trailing newline so shell pipelines behave.
func (a *app) printTool(server, tool string, args map[string]any) error {
	c, err := a.toolClient(server)
	if err != nil {
		return err
	}
	text, err := c.callTool(a.ctx, tool, args)
	if err != nil {
		return err
	}
	fmt.Fprint(a.stdout, text)
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(a.stdout)
	}
	return nil
}

// toolArgs drops unset values so the wire payload only carries what the user
// actually asked for.
func toolArgs(pairs map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range pairs {
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				out[key] = v
			}
		case int:
			if v > 0 {
				out[key] = v
			}
		}
	}
	return out
}

// --- search commands ---

func (a *app) cmdSearch(args []string) error {
	flags := a.flagSet("search", `cb search [flags] <query>`)
	server := flags.String("server", "", "Codebeam server URL")
	repo := flags.String("repo", "", "restrict to one repository full name (e.g. local/myrepo)")
	path := flags.String("path", "", "restrict to file paths matching this regex")
	lang := flags.String("lang", "", "restrict to a language (e.g. go, python)")
	maxFiles := flags.Int("n", 0, "maximum files to return (server default 20)")
	pos, err := parseArgs(flags, args)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(pos, " "))
	if query == "" {
		return a.badUsage(flags, "query is required")
	}
	return a.printTool(*server, "search_code", toolArgs(map[string]any{
		"query": query, "repo": *repo, "path": *path, "lang": *lang, "max_results": *maxFiles,
	}))
}

func (a *app) cmdAST(args []string) error {
	flags := a.flagSet("ast", `cb ast [flags] --lang <lang> '<pattern>'`)
	server := flags.String("server", "", "Codebeam server URL")
	lang := flags.String("lang", "", "language to parse pattern and files as (required)")
	repo := flags.String("repo", "", "restrict to one repository full name")
	branch := flags.String("branch", "", "search a specific branch's committed state")
	path := flags.String("path", "", "restrict to file paths matching this regex")
	maxFiles := flags.Int("n", 0, "maximum files to return (server default 20)")
	pos, err := parseArgs(flags, args)
	if err != nil {
		return err
	}
	pattern := strings.TrimSpace(strings.Join(pos, " "))
	if pattern == "" {
		return a.badUsage(flags, "pattern is required (e.g. 'if $ERR != nil { $$$ }')")
	}
	return a.printTool(*server, "structural_search", toolArgs(map[string]any{
		"pattern": pattern, "lang": *lang, "repo": *repo, "branch": *branch, "path": *path, "max_results": *maxFiles,
	}))
}

func (a *app) cmdDef(args []string) error {
	flags := a.flagSet("def", `cb def [flags] <symbol>`)
	server := flags.String("server", "", "Codebeam server URL")
	repo := flags.String("repo", "", "restrict to one repository full name")
	lang := flags.String("lang", "", "restrict to a language")
	maxFiles := flags.Int("n", 0, "maximum files to return (server default 20)")
	pos, err := parseArgs(flags, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		return a.badUsage(flags, "exactly one symbol is required")
	}
	return a.printTool(*server, "symbol_search", toolArgs(map[string]any{
		"symbol": pos[0], "repo": *repo, "lang": *lang, "max_results": *maxFiles,
	}))
}

func (a *app) cmdRefs(args []string) error {
	flags := a.flagSet("refs", `cb refs [flags] <symbol>`)
	server := flags.String("server", "", "Codebeam server URL")
	repo := flags.String("repo", "", "restrict to one repository full name")
	lang := flags.String("lang", "", "restrict to a language")
	maxFiles := flags.Int("n", 0, "maximum files to return (server default 20)")
	pos, err := parseArgs(flags, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		return a.badUsage(flags, "exactly one symbol is required")
	}
	return a.printTool(*server, "find_references", toolArgs(map[string]any{
		"symbol": pos[0], "repo": *repo, "lang": *lang, "max_results": *maxFiles,
	}))
}

func (a *app) cmdRead(args []string) error {
	flags := a.flagSet("read", `cb read [flags] <repo:path[:N[-M]]>  |  cb read <repo> <path> [N[-M]]`)
	server := flags.String("server", "", "Codebeam server URL")
	start := flags.Int("start", 0, "first line, 1-based")
	end := flags.Int("end", 0, "last line, inclusive")
	pos, err := parseArgs(flags, args)
	if err != nil {
		return err
	}
	target, err := parseReadTarget(pos)
	if err != nil {
		return a.badUsage(flags, err.Error())
	}
	if *start > 0 {
		target.start = *start
	}
	if *end > 0 {
		target.end = *end
	}
	return a.printTool(*server, "read_file", toolArgs(map[string]any{
		"repo": target.repo, "path": target.path, "start": target.start, "end": target.end,
	}))
}

func (a *app) cmdTree(args []string) error {
	flags := a.flagSet("tree", `cb tree [flags] <repo> [path]`)
	server := flags.String("server", "", "Codebeam server URL")
	maxEntries := flags.Int("n", 0, "maximum entries to return (server default 500)")
	pos, err := parseArgs(flags, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return a.badUsage(flags, "a repository is required (e.g. cb tree local/myrepo internal/web)")
	}
	callArgs := map[string]any{"repo": pos[0], "max_entries": *maxEntries}
	if len(pos) == 2 {
		callArgs["path"] = pos[1]
	}
	return a.printTool(*server, "file_tree", toolArgs(callArgs))
}

func (a *app) cmdRepos(args []string) error {
	flags := a.flagSet("repos", `cb repos [flags]`)
	server := flags.String("server", "", "Codebeam server URL")
	if _, err := parseArgs(flags, args); err != nil {
		return err
	}
	return a.printTool(*server, "list_repos", map[string]any{})
}

func (a *app) cmdStats(args []string) error {
	flags := a.flagSet("stats", `cb stats [flags]`)
	server := flags.String("server", "", "Codebeam server URL")
	if _, err := parseArgs(flags, args); err != nil {
		return err
	}
	return a.printTool(*server, "repo_stats", map[string]any{})
}

// readTarget is a parsed `cb read` destination.
type readTarget struct {
	repo, path string
	start, end int
}

// commitSuffix matches the "@1a2b3c4d" provenance search headings append to a
// path, so a copied citation can be passed to cb read unchanged.
var commitSuffix = regexp.MustCompile(`@[0-9a-fA-F]{6,40}$`)

// parseReadTarget accepts "repo:path[:N[-M]]" as one argument — the shape
// search results print — or repo, path, and an optional range as separate
// arguments.
func parseReadTarget(pos []string) (readTarget, error) {
	var t readTarget
	switch len(pos) {
	case 1:
		parts := strings.Split(pos[0], ":")
		if len(parts) < 2 {
			return t, errors.New("target must look like repo:path (e.g. local/myrepo:cmd/main.go:10-40)")
		}
		t.repo = parts[0]
		rest := parts[1:]
		if len(rest) > 1 {
			if start, end, ok := parseLineRange(rest[len(rest)-1]); ok {
				t.start, t.end = start, end
				rest = rest[:len(rest)-1]
			}
		}
		t.path = strings.Join(rest, ":")
	case 2, 3:
		t.repo, t.path = pos[0], pos[1]
		if len(pos) == 3 {
			start, end, ok := parseLineRange(pos[2])
			if !ok {
				return t, errors.New("line range must be N or N-M (e.g. 10-40)")
			}
			t.start, t.end = start, end
		}
	default:
		return t, errors.New("a file is required (e.g. cb read local/myrepo:cmd/main.go:10-40)")
	}
	t.path = commitSuffix.ReplaceAllString(t.path, "")
	if strings.TrimSpace(t.repo) == "" || strings.TrimSpace(t.path) == "" {
		return t, errors.New("both a repository and a file path are required")
	}
	return t, nil
}

// parseLineRange parses "12" or "12-40" into a start/end pair.
func parseLineRange(s string) (start, end int, ok bool) {
	first, second, hasDash := strings.Cut(s, "-")
	start, err := strconv.Atoi(first)
	if err != nil || start < 1 {
		return 0, 0, false
	}
	if !hasDash {
		return start, 0, true
	}
	end, err = strconv.Atoi(second)
	if err != nil || end < start {
		return 0, 0, false
	}
	return start, end, true
}

// --- account commands ---

func (a *app) cmdLogin(args []string) error {
	flags := a.flagSet("login", `cb login [flags] [server]`)
	server := flags.String("server", "", "Codebeam server URL")
	token := flags.String("token", "", "personal access token (skips the browser flow)")
	pos, err := parseArgs(flags, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return a.badUsage(flags, "at most one server argument is allowed")
	}
	target := *server
	if len(pos) == 1 {
		if target != "" {
			return a.badUsage(flags, "pass the server either as an argument or with --server, not both")
		}
		target = pos[0]
	}
	cf, err := loadConfig()
	if err != nil {
		return err
	}
	resolved, _, err := resolveServer(target, cf)
	if err != nil {
		return err
	}

	hc, cfProtected, err := a.accessAwareClient(a.ctx, resolved, true, a.stdout)
	if err != nil {
		return err
	}

	var creds *credentials
	if strings.TrimSpace(*token) != "" {
		creds = &credentials{Kind: "token", Token: strings.TrimSpace(*token)}
	} else if creds, err = loginBrowser(a.ctx, hc, resolved, a.openURL, a.stdout); err != nil {
		return err
	}
	creds.CFAccess = cfProtected

	// Verify before saving, so a typoed token or wrong server fails loudly now
	// rather than on the first real search.
	probe := &client{server: resolved, creds: creds, hc: hc}
	summary, err := probe.callTool(a.ctx, "list_repos", map[string]any{})
	if err != nil {
		return fmt.Errorf("login verification against %s failed: %w", resolved, err)
	}

	cf.Servers[resolved] = creds
	cf.DefaultServer = resolved
	if err := saveConfig(cf); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Logged in to %s — it is now the default server.\n", resolved)
	if first := firstLine(summary); first != "" {
		fmt.Fprintln(a.stdout, first)
	}
	return nil
}

func (a *app) cmdLogout(args []string) error {
	flags := a.flagSet("logout", `cb logout [flags] [server]`)
	server := flags.String("server", "", "Codebeam server URL")
	pos, err := parseArgs(flags, args)
	if err != nil {
		return err
	}
	target := *server
	if len(pos) == 1 && target == "" {
		target = pos[0]
	} else if len(pos) > 0 {
		return a.badUsage(flags, "at most one server argument is allowed")
	}
	cf, err := loadConfig()
	if err != nil {
		return err
	}
	resolved, _, err := resolveServer(target, cf)
	if err != nil {
		return err
	}
	if _, ok := cf.Servers[resolved]; !ok {
		fmt.Fprintf(a.stdout, "No stored credentials for %s.\n", resolved)
		return nil
	}
	delete(cf.Servers, resolved)
	if cf.DefaultServer == resolved {
		cf.DefaultServer = ""
	}
	if err := saveConfig(cf); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Logged out of %s.\n", resolved)
	return nil
}

func (a *app) cmdStatus(args []string) error {
	flags := a.flagSet("status", `cb status [flags]`)
	server := flags.String("server", "", "Codebeam server URL")
	if _, err := parseArgs(flags, args); err != nil {
		return err
	}
	cf, err := loadConfig()
	if err != nil {
		return err
	}
	resolved, source, err := resolveServer(*server, cf)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Server: %s (from %s)\n", resolved, source)

	creds := resolveCredentials(cf, resolved)
	switch {
	case creds == nil:
		fmt.Fprintf(a.stdout, "Auth:   none — run `cb login %s` or set %s\n", resolved, envToken)
		return nil
	case os.Getenv(envToken) != "":
		fmt.Fprintf(a.stdout, "Auth:   personal access token (from %s)\n", envToken)
	case creds.Kind == "token":
		fmt.Fprintln(a.stdout, "Auth:   personal access token (stored)")
	default:
		fmt.Fprintf(a.stdout, "Auth:   OAuth (%s)\n", oauthExpiryLabel(creds))
	}
	if stored := cf.Servers[resolved]; stored != nil && stored.CFAccess {
		fmt.Fprintln(a.stdout, "Gate:   Cloudflare Access (token attached automatically)")
	}

	c, err := a.toolClient(*server)
	if err != nil {
		return err
	}
	summary, err := c.callTool(a.ctx, "list_repos", map[string]any{})
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	fmt.Fprintf(a.stdout, "OK:     connected — %s\n", strings.TrimSuffix(firstLine(summary), "."))
	return nil
}

func oauthExpiryLabel(creds *credentials) string {
	if creds.ExpiresAt <= 0 {
		return "access token lifetime unknown"
	}
	left := time.Until(time.Unix(creds.ExpiresAt, 0)).Round(time.Minute)
	if left <= 0 {
		return "access token expired; will refresh on next use"
	}
	return "access token valid for " + left.String()
}

// firstLine returns the first non-empty line of a tool result, stripped of
// markdown heading markers — a compact human summary.
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "# "))
		if line != "" {
			return line
		}
	}
	return ""
}
