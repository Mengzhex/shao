package mcpsrv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
)

// maxMessageBytes bounds a single JSON-RPC message. Requests to shao are
// small; responses can be large, but those are written, not read.
const maxMessageBytes = 8 << 20

// ServeStdio runs the MCP protocol over a pipe, one JSON object per line.
//
// This is the transport an AI client uses when it launches shao itself, which
// means there is no long-lived process, no port and no token to manage: the
// client starts `shao mcp`, reads the answer, and the process goes away.
//
// Nothing may be written to out except protocol messages, so every diagnostic
// in this mode has to go to stderr.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64<<10), maxMessageBytes)

	enc := json.NewEncoder(out)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		// A batch arrives as a JSON array. Batches were removed in later
		// protocol revisions but older clients still send them.
		if line[0] == '[' {
			for _, resp := range s.handleBatch(line) {
				if err := enc.Encode(resp); err != nil {
					return err
				}
			}
			continue
		}
		resp := s.handle(line)
		if resp == nil {
			continue // notification: no reply by definition
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// handleBatch processes a JSON-RPC batch, dropping the replies to any
// notifications it contained.
func (s *Server) handleBatch(line []byte) []*rpcResponse {
	var msgs []json.RawMessage
	if err := json.Unmarshal(line, &msgs); err != nil {
		return []*rpcResponse{newError(nil, codeParseError, "invalid JSON batch: %v", err)}
	}
	var out []*rpcResponse
	for _, m := range msgs {
		if resp := s.handle(m); resp != nil {
			out = append(out, resp)
		}
	}
	return out
}
