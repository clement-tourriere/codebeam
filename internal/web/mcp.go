package web

import (
	"encoding/json"
	"io"
	"net/http"

	mcpserver "github.com/ctourriere/codebeam/internal/mcp"
)

const maxMCPBody = 4 << 20

// handleMCP serves the MCP streamable-HTTP transport: one JSON-RPC message per
// POST, answered with a single JSON body (no SSE stream — every Codebeam tool
// responds in one shot). Authentication is a bearer credential (OAuth access
// token from the built-in authorization server, or a personal access token),
// and every tool call is scoped to the authenticated user's repositories —
// unlike the stdio transport, which is local and unscoped by design.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	// CORS mirrors the OAuth endpoints so browser-based MCP clients work.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version, Mcp-Session-Id")
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
	default:
		// No server-initiated streams: the spec allows a plain 405 for GET.
		methodNotAllowed(w)
		return
	}

	user, ok := s.apiUser(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+s.cfg.BaseURL+protectedResourceMetadataPath+`"`)
		apiError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBody))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	srv := &mcpserver.Server{
		Store:      s.store,
		Search:     s.search,
		Structural: s.structural,
		Indexer:    s.indexer,
		UserID:     user.ID,
	}
	resp, hasResponse := srv.HandleMessage(r.Context(), body)
	if !hasResponse {
		// Notifications get 202 Accepted with no body per the transport spec.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}
