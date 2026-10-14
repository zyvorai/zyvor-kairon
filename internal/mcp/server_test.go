// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testServer(allowWrite bool) *Server {
	s := NewServer("test", "v0", allowWrite)
	s.Add(
		Tool{Name: "echo", Description: "echo", Schema: Object(map[string]any{"msg": String("m")}, "msg"),
			Call: func(_ context.Context, args json.RawMessage) (string, error) {
				var a struct{ Msg string }
				if err := json.Unmarshal(args, &a); err != nil {
					return "", err
				}
				return a.Msg, nil
			}},
		Tool{Name: "fail", Call: func(context.Context, json.RawMessage) (string, error) { return "", errors.New("boom") }},
		Tool{Name: "big", Call: func(context.Context, json.RawMessage) (string, error) {
			return strings.Repeat("x", MaxOutput+10), nil
		}},
		Tool{Name: "stop", Write: true, Call: func(context.Context, json.RawMessage) (string, error) { return "stopped", nil }},
	)
	return s
}

// run sends lines to the server and returns responses keyed by id.
func run(t *testing.T, s *Server, lines ...string) map[string]map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad response %q: %v", l, err)
		}
		id, _ := json.Marshal(m["id"])
		got[string(id)] = m
	}
	return got
}

func resultText(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	content := res["content"].([]any)[0].(map[string]any)
	return content["text"].(string), res["isError"].(bool)
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	got := run(t, testServer(false),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	)
	if len(got) != 3 {
		t.Fatalf("notification must not be answered: %v", got)
	}
	if v := got["1"]["result"].(map[string]any)["protocolVersion"]; v != "2024-11-05" {
		t.Fatalf("version %v", v)
	}
	if v := got["2"]["result"].(map[string]any)["protocolVersion"]; v != LatestProtocolVersion {
		t.Fatalf("fallback version %v", v)
	}
	if got["3"]["error"] != nil {
		t.Fatalf("ping: %v", got["3"])
	}
}

func TestToolsListHidesWriteTools(t *testing.T) {
	names := func(allow bool) []string {
		got := run(t, testServer(allow), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		tools := got["1"]["result"].(map[string]any)["tools"].([]any)
		out := make([]string, 0, len(tools))
		for _, tl := range tools {
			out = append(out, tl.(map[string]any)["name"].(string))
		}
		return out
	}
	if n := strings.Join(names(false), ","); n != "big,echo,fail" {
		t.Fatalf("read-only list %s", n)
	}
	if n := strings.Join(names(true), ","); n != "big,echo,fail,stop" {
		t.Fatalf("write list %s", n)
	}
}

func TestToolsCall(t *testing.T) {
	got := run(t, testServer(false),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"hi"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fail"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"stop"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"big"}}`,
	)
	if txt, isErr := resultText(t, got["1"]); txt != "hi" || isErr {
		t.Fatalf("echo %q %v", txt, isErr)
	}
	if txt, isErr := resultText(t, got["2"]); txt != "boom" || !isErr {
		t.Fatalf("fail %q %v", txt, isErr)
	}
	if txt, isErr := resultText(t, got["3"]); !isErr || !strings.Contains(txt, "--allow-write") {
		t.Fatalf("write gate %q %v", txt, isErr)
	}
	if got["4"]["error"] == nil {
		t.Fatalf("unknown tool should be a protocol error: %v", got["4"])
	}
	if txt, _ := resultText(t, got["5"]); !strings.Contains(txt, "[truncated") || len(txt) > MaxOutput+200 {
		t.Fatalf("big output not truncated: %d", len(txt))
	}
}

func TestProtocolErrors(t *testing.T) {
	got := run(t, testServer(false),
		`not json`,
		`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`,
	)
	if e := got["null"]["error"].(map[string]any); e["code"].(float64) != codeParse {
		t.Fatalf("parse error %v", e)
	}
	if e := got["7"]["error"].(map[string]any); e["code"].(float64) != codeMethodNotFound {
		t.Fatalf("method error %v", e)
	}
}

func TestAuditWrapsWriteToolsAndRefusesWhenUnavailable(t *testing.T) {
	s := testServer(true)
	var calls []string
	s.Audit = func(_ context.Context, tool string, _ json.RawMessage, outcome string) error {
		calls = append(calls, tool+":"+outcome)
		return nil
	}
	got := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"hi"}}}`,
	)
	if txt, isErr := resultText(t, got["1"]); txt != "stopped" || isErr {
		t.Fatalf("stop %q %v", txt, isErr)
	}
	if strings.Join(calls, ",") != "stop:intent,stop:ok" {
		t.Fatalf("audit calls = %v; read tools must not be audited", calls)
	}

	ran := false
	s = NewServer("test", "v0", true)
	s.Add(Tool{Name: "stop", Write: true, Call: func(context.Context, json.RawMessage) (string, error) { ran = true; return "", nil }})
	s.Audit = func(context.Context, string, json.RawMessage, string) error { return errors.New("disk full") }
	got = run(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop"}}`)
	if txt, isErr := resultText(t, got["1"]); !isErr || !strings.Contains(txt, "write refused") || ran {
		t.Fatalf("refusal %q %v ran=%v", txt, isErr, ran)
	}
}

func TestApprovalGateRefusesThenAllows(t *testing.T) {
	s := NewServer("test", "v0", true)
	ran := 0
	s.Add(Tool{Name: "rm", Write: true, Approval: true, Call: func(context.Context, json.RawMessage) (string, error) {
		ran++
		return "removed", nil
	}})
	var outcomes []string
	s.Audit = func(_ context.Context, _ string, _ json.RawMessage, outcome string) error {
		outcomes = append(outcomes, outcome)
		return nil
	}
	approved := false
	s.Approve = func(context.Context, string, json.RawMessage) (string, error) {
		if !approved {
			return "", &ApprovalError{Outcome: "approval-required", Message: "ask a human (id abc)"}
		}
		return "alice", nil
	}
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rm","arguments":{}}}`
	text, isErr := resultText(t, run(t, s, call)["1"])
	if !isErr || !strings.Contains(text, "id abc") || ran != 0 {
		t.Fatalf("expected a refusal naming the id without running the tool, got %q err=%v ran=%d", text, isErr, ran)
	}
	approved = true
	text, isErr = resultText(t, run(t, s, call)["1"])
	if isErr || text != "removed" || ran != 1 {
		t.Fatalf("expected the approved call to run, got %q err=%v ran=%d", text, isErr, ran)
	}
	want := []string{"approval-required", "approved:alice", "intent", "ok"}
	if strings.Join(outcomes, ",") != strings.Join(want, ",") {
		t.Fatalf("audit outcomes = %v, want %v", outcomes, want)
	}
}

func TestApprovalGateSkipsToolsWithoutApproval(t *testing.T) {
	s := testServer(true)
	s.Approve = func(context.Context, string, json.RawMessage) (string, error) {
		t.Fatal("Approve must not run for a tool without Approval")
		return "", nil
	}
	text, isErr := resultText(t, run(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stop"}}`)["1"])
	if isErr || text != "stopped" {
		t.Fatalf("got %q err=%v", text, isErr)
	}
}

func TestImageBlocks(t *testing.T) {
	s := NewServer("test", "v0", false)
	s.Add(
		Tool{Name: "shot", CallBlocks: func(context.Context, json.RawMessage) ([]Block, error) {
			return []Block{ImageBlock("image/png", []byte{0x89, 'P', 'N', 'G'}), TextBlock(strings.Repeat("x", MaxOutput+10))}, nil
		}},
		Tool{Name: "huge", CallBlocks: func(context.Context, json.RawMessage) ([]Block, error) {
			return []Block{ImageBlock("image/png", make([]byte, MaxImage+1))}, nil
		}},
	)
	got := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shot"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"huge"}}`,
	)
	content := got["1"]["result"].(map[string]any)["content"].([]any)
	img := content[0].(map[string]any)
	if img["type"] != "image" || img["mimeType"] != "image/png" || img["data"] != "iVBORw==" {
		t.Fatalf("image block %v", img)
	}
	if text := content[1].(map[string]any)["text"].(string); !strings.Contains(text, "[truncated") {
		t.Fatal("text block not capped")
	}
	if text, isErr := resultText(t, got["2"]); !isErr || !strings.Contains(text, "image exceeds") {
		t.Fatalf("oversized image: %q %v", text, isErr)
	}
}
