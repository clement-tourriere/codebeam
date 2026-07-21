package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// cb mcp serves the MCP stdio transport and relays every message to the
// server's /mcp endpoint, reusing cb's stored credentials — OAuth refresh and
// Cloudflare Access included. Agents that cannot walk a browser flow (or
// reach a gateway-protected instance) register `cb mcp` as a local stdio
// server and inherit whatever `cb login` set up.

// maxMCPMessage caps one stdio message; matches the server's /mcp body limit
// plus slack for framing.
const maxMCPMessage = 5 << 20

func (a *app) cmdMCP(args []string) error {
	flags := a.flagSet("mcp", `cb mcp [flags]`)
	server := flags.String("server", "", "Codebeam server URL")
	if _, err := parseArgs(flags, args); err != nil {
		return err
	}
	c, err := a.toolClient(*server)
	if err != nil {
		return err
	}
	return runMCPProxy(a.ctx, c, a.stdin, a.stdout, a.stderr)
}

// runMCPProxy pumps newline-delimited JSON-RPC messages from the agent to the
// server and back. Transport failures answer the offending request as a
// JSON-RPC error instead of killing the session, so one flaky call (or an
// expired login) surfaces inside the agent rather than as a dead server.
func runMCPProxy(ctx context.Context, c *client, in io.Reader, out, errw io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64<<10), maxMCPMessage)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		raw, hasResponse, err := c.forward(ctx, line)
		if err != nil {
			if id := messageID(line); id != nil {
				writeMessage(out, errorResponse(id, err))
			} else {
				fmt.Fprintln(errw, "cb mcp: "+err.Error())
			}
			continue
		}
		if !hasResponse {
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			if id := messageID(line); id != nil {
				writeMessage(out, errorResponse(id, fmt.Errorf("server returned an unreadable response")))
			}
			continue
		}
		compact.WriteByte('\n')
		out.Write(compact.Bytes()) // nolint:errcheck
	}
	return scanner.Err()
}

// messageID extracts a request's id, or nil for notifications (and for the
// null id the spec forbids requests to use).
func messageID(raw []byte) json.RawMessage {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &probe) != nil || string(probe.ID) == "null" {
		return nil
	}
	return probe.ID
}

func errorResponse(id json.RawMessage, err error) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32000, "message": err.Error()},
	}
}

func writeMessage(out io.Writer, message map[string]any) {
	raw, err := json.Marshal(message)
	if err != nil {
		return
	}
	out.Write(append(raw, '\n')) // nolint:errcheck
}
