package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// refreshSkew renews an access token slightly before its recorded expiry so a
// request never leaves with a token that dies in flight.
const refreshSkew = 30 * time.Second

// client calls tools on a Codebeam server over the MCP streamable-HTTP
// transport: one JSON-RPC message per POST to /mcp, one JSON body back. It is
// the whole of cb's server protocol — the CLI is just another MCP client.
type client struct {
	server string
	creds  *credentials
	hc     *http.Client
	// persist saves rotated credentials after a refresh; nil skips saving
	// (e.g. when the token came from the environment).
	persist func(*credentials) error
}

var errNotLoggedIn = errors.New("not logged in")

// callTool invokes one MCP tool and returns its text result.
func (c *client) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	if err != nil {
		return "", err
	}
	raw, hasResponse, err := c.forward(ctx, body)
	if err != nil {
		return "", err
	}
	if !hasResponse {
		return "", errors.New("server returned no response")
	}
	return decodeToolResult(raw)
}

// forward relays one raw JSON-RPC message to the server's /mcp endpoint.
// Expired OAuth access tokens are refreshed transparently — before the
// request when the recorded expiry has passed, and once more on a 401 in
// case the server revoked the token early. hasResponse is false for
// notifications (202 Accepted, no body per the transport spec).
func (c *client) forward(ctx context.Context, body []byte) (raw []byte, hasResponse bool, err error) {
	if c.creds == nil {
		return nil, false, errNotLoggedIn
	}
	if c.staleOAuth() {
		if err := c.refresh(ctx); err != nil {
			return nil, false, err
		}
	}
	resp, raw, err := c.post(ctx, body)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusUnauthorized && c.creds.Kind == "oauth" && c.creds.RefreshToken != "" {
		if err := c.refresh(ctx); err != nil {
			return nil, false, err
		}
		if resp, raw, err = c.post(ctx, body); err != nil {
			return nil, false, err
		}
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return nil, false, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, false, fmt.Errorf("%s rejected the credentials — the token may be expired or revoked; sign in again with `cb login %s`", c.server, c.server)
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Errorf("%s answered %s: %s", c.server, resp.Status, apiErrorMessage(raw))
	}
	return raw, true, nil
}

func (c *client) post(ctx context.Context, body []byte) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.creds.bearer())
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach %s: %w", c.server, err)
	}
	defer resp.Body.Close() // nolint:errcheck
	if blockedByCFAccess(resp) {
		return nil, nil, fmt.Errorf("Cloudflare Access intercepted the request to %s (Access session missing or expired) — run `cb login %s`", c.server, c.server)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, nil, err
	}
	return resp, raw, nil
}

func (c *client) staleOAuth() bool {
	return c.creds.Kind == "oauth" && c.creds.RefreshToken != "" &&
		c.creds.ExpiresAt > 0 && time.Now().Add(refreshSkew).Unix() >= c.creds.ExpiresAt
}

func (c *client) refresh(ctx context.Context) error {
	if err := refreshCredentials(ctx, c.hc, c.creds); err != nil {
		return fmt.Errorf("session expired and refresh failed (%v) — run `cb login %s`", err, c.server)
	}
	if c.persist != nil {
		if err := c.persist(c.creds); err != nil {
			return fmt.Errorf("cannot save refreshed credentials: %w", err)
		}
	}
	return nil
}

// decodeToolResult unwraps a JSON-RPC tool response into its text content.
// Tool failures arrive as results flagged isError (per MCP) and become plain
// errors here.
func decodeToolResult(raw []byte) (string, error) {
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", errors.New("server returned an unreadable response")
	}
	if resp.Error != nil {
		return "", errors.New(resp.Error.Message)
	}
	var b strings.Builder
	for _, part := range resp.Result.Content {
		if part.Type == "text" {
			b.WriteString(part.Text)
		}
	}
	if resp.Result.IsError {
		return "", errors.New(strings.TrimSpace(b.String()))
	}
	return b.String(), nil
}

// apiErrorMessage extracts {"error": ...} from a non-200 API body, falling
// back to a trimmed slice of the raw body.
func apiErrorMessage(raw []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		return body.Error
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	if msg == "" {
		msg = "(empty body)"
	}
	return msg
}
