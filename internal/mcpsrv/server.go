package mcpsrv

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/Mengzhex/shao/internal/config"
	"github.com/Mengzhex/shao/internal/redact"
)

// Version is reported to clients during the handshake.
const Version = "1.0.0"

// instructions are shown to the model once, at connect time. They exist to
// prevent two specific mistakes: treating shao as something that watches and
// alerts, and treating a deployment suggestion as something shao will carry
// out.
const instructions = `shao gives you read-only access to terminal history recorded on this machine, and to read-only facts about configured remote hosts.

Use it when the user asks a question about something that already happened in a terminal, or about the state of a host they are planning to deploy to. Call these tools in response to a question; there is nothing to monitor and nothing will be pushed to you.

Every tool here reads. None of them can run a command chosen by you, edit a file, or change a host: remote access is pinned by the target's SSH configuration to a single read-only probe. When a user asks how to deploy something, inspect the host and then write out the commands for them to run themselves. Do not present them as commands you have executed or will execute.

Terminal buffers can contain sensitive text that redaction did not catch. Quote from them only as far as answering the question requires.`

// handler runs one tool call.
type handler func(args json.RawMessage) *callToolResult

type tool struct {
	def toolDef
	run handler
}

// Server holds the tool registry and the configuration it reads through. It
// keeps no session state beyond the handshake, so a client reconnecting loses
// nothing.
type Server struct {
	cfg      *config.Config
	redactor *redact.Redactor

	mu          sync.RWMutex
	tools       map[string]tool
	initialized bool
	negotiated  string
}

// New builds a server exposing shao's read-only tools.
func New(cfg *config.Config) (*Server, error) {
	s := &Server{cfg: cfg, tools: map[string]tool{}, negotiated: protocolVersion}

	// The read-time pass is what protects sessions recorded before a
	// redaction rule existed, and covers anything the capture-side filter
	// missed.
	if cfg.Redact.ReadTimeSecondPass {
		r, err := redact.New(cfg.Redact)
		if err != nil {
			return nil, err
		}
		s.redactor = r
	}

	s.registerTerminalTools()
	s.registerHostTools()
	return s, nil
}

func (s *Server) register(def toolDef, run handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[def.Name] = tool{def: def, run: run}
}

// scrub applies the read-time redaction pass. Everything leaving this server
// that came from a terminal buffer goes through here.
func (s *Server) scrub(text string) string {
	if s.redactor == nil {
		return text
	}
	return string(s.redactor.All([]byte(text)))
}

// handle dispatches one JSON-RPC message and returns the reply, or nil when
// the message was a notification.
func (s *Server) handle(raw []byte) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return newError(nil, codeParseError, "invalid JSON: %v", err)
	}
	if req.JSONRPC != "2.0" && req.JSONRPC != "" {
		return newError(req.ID, codeInvalidRequest, "unsupported jsonrpc version %q", req.JSONRPC)
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)

	case "notifications/initialized", "notifications/cancelled":
		// Notifications get no reply, by definition.
		return nil

	case "ping":
		if req.isNotification() {
			return nil
		}
		return newResult(req.ID, map[string]any{})

	case "tools/list":
		if req.isNotification() {
			return nil
		}
		return newResult(req.ID, listToolsResult{Tools: s.toolDefs()})

	case "tools/call":
		if req.isNotification() {
			return nil
		}
		return s.handleCall(req)

	default:
		if req.isNotification() {
			return nil
		}
		return newError(req.ID, codeMethodNotFound, "method %q is not supported", req.Method)
	}
}

func (s *Server) handleInitialize(req rpcRequest) *rpcResponse {
	var params initializeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return newError(req.ID, codeInvalidParams, "invalid initialize params: %v", err)
		}
	}

	version := protocolVersion
	if params.ProtocolVersion != "" && knownProtocolVersions[params.ProtocolVersion] {
		version = params.ProtocolVersion
	}

	s.mu.Lock()
	s.initialized = true
	s.negotiated = version
	s.mu.Unlock()

	return newResult(req.ID, initializeResult{
		ProtocolVersion: version,
		Capabilities: map[string]any{
			// Only tools. No resources, no prompts, and specifically no
			// subscriptions: nothing about shao is push-driven.
			"tools": map[string]any{"listChanged": false},
		},
		ServerInfo:   serverInfo{Name: "shao", Version: Version},
		Instructions: instructions,
	})
}

func (s *Server) toolDefs() []toolDef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	defs := make([]toolDef, 0, len(s.tools))
	for _, t := range s.tools {
		defs = append(defs, t.def)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

func (s *Server) handleCall(req rpcRequest) *rpcResponse {
	var params callToolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return newError(req.ID, codeInvalidParams, "invalid tools/call params: %v", err)
	}

	s.mu.RLock()
	t, ok := s.tools[params.Name]
	s.mu.RUnlock()
	if !ok {
		return newError(req.ID, codeInvalidParams, "unknown tool %q", params.Name)
	}

	result := func() (res *callToolResult) {
		// A panic in one tool must not take down a server the user's editor
		// is talking to.
		defer func() {
			if r := recover(); r != nil {
				res = errorResult("internal error in tool %s: %v", params.Name, r)
			}
		}()
		return t.run(params.Arguments)
	}()

	return newResult(req.ID, result)
}

// decodeArgs unmarshals tool arguments, tolerating an absent object.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// schema is a small helper for writing JSON Schema objects readably.
func schema(properties map[string]any, required ...string) map[string]any {
	s := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

func propEnum(description string, values []string, def string) map[string]any {
	m := map[string]any{"type": "string", "description": description, "enum": values}
	if def != "" {
		m["default"] = def
	}
	return m
}

func propInt(description string, def int) map[string]any {
	return map[string]any{"type": "integer", "description": description, "default": def}
}
