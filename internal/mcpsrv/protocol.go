// Package mcpsrv serves tmon's read-only tools over the Model Context
// Protocol.
//
// The protocol is implemented directly rather than through an SDK. MCP over
// stdio is JSON-RPC 2.0 with one object per line, and the surface tmon needs
// is four methods, so a dependency would add version risk without removing
// much code.
//
// Two properties of this server matter more than any of its plumbing:
//
// It is strictly demand-driven. Nothing here runs on a timer, subscribes to
// anything, or pushes notifications. A tool executes when the model calls it
// because a person asked a question, and at no other time.
//
// Every tool is a reader. There is no tool that writes a file, runs a command
// of the caller's choosing, or changes a host. For terminal history the
// server only opens buffers on the local disk. For host facts it sends a verb
// from a fixed list to a probe reached through a key that sshd pins to a
// forced command, so even a tool call constructed by a prompt injection has
// nowhere to go.
package mcpsrv

import (
	"encoding/json"
	"fmt"
)

// protocolVersion is the MCP revision tmon implements. When a client asks for
// a different one, the server echoes the client's version back if it is a
// revision tmon understands, since MCP revisions have so far been additive
// for a server this small.
const protocolVersion = "2025-06-18"

var knownProtocolVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

// JSON-RPC 2.0 error codes, plus the MCP convention of returning tool
// failures as results rather than protocol errors.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the peer expects no reply. JSON-RPC
// notifications carry no id, and replying to one is a protocol violation.
func (r rpcRequest) isNotification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func newResult(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func newError(id json.RawMessage, code int, format string, args ...any) *rpcResponse {
	return &rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: fmt.Sprintf(format, args...)},
	}
}

// initializeParams is the subset of the handshake tmon reads.
type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      serverInfo     `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

// toolDef is one tool as advertised to the client.
type toolDef struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// Annotations tell a client how a tool behaves. readOnlyHint is set on
	// every tool tmon exposes, because every tool tmon exposes is a reader.
	Annotations map[string]any `json:"annotations,omitempty"`
}

type listToolsResult struct {
	Tools []toolDef `json:"tools"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callToolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

func textResult(text string) *callToolResult {
	return &callToolResult{Content: []textContent{{Type: "text", Text: text}}}
}

// errorResult reports a tool failure to the model rather than to the
// transport, so the model can read what went wrong and adjust instead of the
// call simply vanishing.
func errorResult(format string, args ...any) *callToolResult {
	return &callToolResult{
		Content: []textContent{{Type: "text", Text: fmt.Sprintf(format, args...)}},
		IsError: true,
	}
}

// readOnlyAnnotations is attached to every tool.
func readOnlyAnnotations(title string) map[string]any {
	return map[string]any{
		"title":           title,
		"readOnlyHint":    true,
		"destructiveHint": false,
		"idempotentHint":  true,
		"openWorldHint":   false,
	}
}
