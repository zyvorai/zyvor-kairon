// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package mcp is a minimal Model Context Protocol server over stdio:
// newline-delimited JSON-RPC 2.0 with the tools capability only.
package mcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

// LatestProtocolVersion is answered when the client asks for a version
// this server does not know.
const LatestProtocolVersion = "2025-06-18"

var supportedVersions = map[string]bool{
	"2024-11-05":          true,
	"2025-03-26":          true,
	LatestProtocolVersion: true,
}

// MaxOutput caps a tool result so one call cannot flood the model's context.
const MaxOutput = 64 << 10

// MaxImage caps one image block's raw bytes.
const MaxImage = 4 << 20

// Block is one tool-result content block; see TextBlock and ImageBlock.
type Block map[string]any

func TextBlock(text string) Block { return Block{"type": "text", "text": text} }

// ImageBlock carries data (e.g. a PNG) base64-encoded.
func ImageBlock(mimeType string, data []byte) Block {
	return Block{"type": "image", "mimeType": mimeType, "data": base64.StdEncoding.EncodeToString(data)}
}

// Tool is one callable tool. Write tools change state and are only
// listed and callable when the server allows writes.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Write       bool
	// Approval marks a Write tool that also needs a human's per-call
	// approval when the server has an Approve hook.
	Approval bool
	Call     func(ctx context.Context, args json.RawMessage) (string, error)
	// CallBlocks, when set, is used instead of Call for tools that answer
	// with more than text (images).
	CallBlocks func(ctx context.Context, args json.RawMessage) ([]Block, error)
}

// ApprovalError refuses a call pending (or after a failed) approval.
// Outcome is what the audit log records, e.g. "approval-required".
type ApprovalError struct {
	Outcome string
	Message string
}

func (e *ApprovalError) Error() string { return e.Message }

// Server dispatches JSON-RPC requests to registered tools.
type Server struct {
	Name       string
	Version    string
	AllowWrite bool
	// Audit, when set, is called before every Write tool runs (outcome
	// "intent") and after it returns ("ok" or the error text). An error
	// on the intent call refuses the write.
	Audit func(ctx context.Context, tool string, args json.RawMessage, outcome string) error
	// Approve, when set, runs before every Approval tool. It returns who
	// approved this exact call ("" when this call needs no approval), or an
	// error (an *ApprovalError names the audit outcome) that refuses it.
	Approve func(ctx context.Context, tool string, args json.RawMessage) (approver string, err error)

	tools map[string]Tool
}

// NewServer returns a server with no tools.
func NewServer(name, version string, allowWrite bool) *Server {
	return &Server{Name: name, Version: version, AllowWrite: allowWrite, tools: map[string]Tool{}}
}

// Add registers tools; a later tool with the same name replaces an earlier one.
func (s *Server) Add(tools ...Tool) {
	for _, t := range tools {
		s.tools[t.Name] = t
	}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Serve reads requests from r and writes responses to w until r ends or
// ctx is cancelled. Requests run concurrently; responses are written
// whole, one per line.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	var mu sync.Mutex
	var wg sync.WaitGroup
	write := func(resp response) {
		b, err := json.Marshal(resp)
		if err != nil {
			b, _ = json.Marshal(response{JSONRPC: "2.0", ID: resp.ID, Error: &rpcError{Code: -32603, Message: err.Error()}})
		}
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write(append(b, '\n'))
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			write(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: codeParse, Message: "parse error: " + err.Error()}})
			continue
		}
		if req.Method == "" {
			if len(req.ID) > 0 {
				write(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: codeInvalidRequest, Message: "missing method"}})
			}
			continue
		}
		if len(req.ID) == 0 {
			continue // notification; nothing to answer
		}
		wg.Add(1)
		go func(req request) {
			defer wg.Done()
			result, rerr := s.handle(ctx, req)
			resp := response{JSONRPC: "2.0", ID: req.ID}
			if rerr != nil {
				resp.Error = rerr
			} else {
				resp.Result = result
			}
			write(resp)
		}(req)
	}
	wg.Wait()
	if err := sc.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *Server) handle(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := LatestProtocolVersion
		if supportedVersions[p.ProtocolVersion] {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.listed()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call needs a name"}
		}
		t, ok := s.tools[p.Name]
		if !ok || (t.Write && !s.AllowWrite) {
			if ok {
				return toolResult(fmt.Sprintf("%s changes state; start the server with --allow-write to use it", p.Name), true), nil
			}
			return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool " + p.Name}
		}
		args := p.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		if t.Write && t.Approval && s.Approve != nil {
			approver, err := s.Approve(ctx, t.Name, args)
			if err != nil {
				outcome := "approval-error"
				var ae *ApprovalError
				if errors.As(err, &ae) {
					outcome = ae.Outcome
				}
				if s.Audit != nil {
					_ = s.Audit(ctx, t.Name, args, outcome)
				}
				return toolResult(err.Error(), true), nil
			}
			if s.Audit != nil && approver != "" {
				if err := s.Audit(ctx, t.Name, args, "approved:"+approver); err != nil {
					return toolResult("audit log unavailable; write refused: "+err.Error(), true), nil
				}
			}
		}
		if t.Write && s.Audit != nil {
			if err := s.Audit(ctx, t.Name, args, "intent"); err != nil {
				return toolResult("audit log unavailable; write refused: "+err.Error(), true), nil
			}
		}
		var blocks []Block
		var err error
		if t.CallBlocks != nil {
			blocks, err = t.CallBlocks(ctx, args)
		} else {
			var out string
			out, err = t.Call(ctx, args)
			blocks = []Block{TextBlock(out)}
		}
		if t.Write && s.Audit != nil {
			outcome := "ok"
			if err != nil {
				outcome = err.Error()
			}
			_ = s.Audit(ctx, t.Name, args, outcome)
		}
		if err != nil {
			return toolResult(err.Error(), true), nil
		}
		return blocksResult(blocks), nil
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

func (s *Server) listed() []map[string]any {
	names := make([]string, 0, len(s.tools))
	for name, t := range s.tools {
		if t.Write && !s.AllowWrite {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		t := s.tools[name]
		schema := t.Schema
		if schema == nil {
			schema = Object(nil)
		}
		entry := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema}
		switch {
		case !t.Write:
			entry["annotations"] = map[string]any{"readOnlyHint": true}
		case t.Approval:
			entry["annotations"] = map[string]any{"destructiveHint": true}
		}
		out = append(out, entry)
	}
	return out
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []Block{TextBlock(capText(text))},
		"isError": isError,
	}
}

func capText(text string) string {
	if len(text) > MaxOutput {
		text = text[:MaxOutput] + fmt.Sprintf("\n[truncated: output exceeded %d bytes; narrow the request, e.g. with limit]", MaxOutput)
	}
	return text
}

// blocksResult caps text blocks like toolResult and refuses an image over
// MaxImage rather than cutting it.
func blocksResult(blocks []Block) map[string]any {
	out := make([]Block, 0, len(blocks))
	for _, b := range blocks {
		switch b["type"] {
		case "text":
			text, _ := b["text"].(string)
			out = append(out, TextBlock(capText(text)))
		case "image":
			if data, _ := b["data"].(string); base64.StdEncoding.DecodedLen(len(data)) > MaxImage {
				return toolResult(fmt.Sprintf("image exceeds %d bytes; ask for a smaller one", MaxImage), true)
			}
			out = append(out, b)
		default:
			out = append(out, b)
		}
	}
	return map[string]any{"content": out, "isError": false}
}

// Object builds a JSON Schema object from property schemas; required
// names are listed after the properties.
func Object(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// String is a string property; enum values, if any, restrict it.
func String(desc string, enum ...string) map[string]any {
	s := map[string]any{"type": "string", "description": desc}
	if len(enum) > 0 {
		s["enum"] = enum
	}
	return s
}

// Integer is an integer property bounded by min and max.
func Integer(desc string, minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": minimum, "maximum": maximum}
}

// JSON renders v indented for a tool result.
func JSON(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
