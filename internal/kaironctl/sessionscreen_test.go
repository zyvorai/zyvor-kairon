// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/mcp"
)

func TestInputAction(t *testing.T) {
	ok := []struct {
		words []string
		mods  []string
		width int
		want  map[string]any
	}{
		{[]string{"type", "make", "test"}, nil, 0, map[string]any{"action": "type", "text": "make test"}},
		{[]string{"key", "q"}, []string{"command"}, 0, map[string]any{"action": "key", "key": "q", "modifiers": []string{"command"}}},
		{[]string{"double-click", "10", "20.5"}, nil, 1280, map[string]any{"action": "double_click", "x": 10.0, "y": 20.5, "screen_width": 1280}},
		{[]string{"drag", "1", "2", "3", "4"}, nil, 0, map[string]any{"action": "drag", "x": 1.0, "y": 2.0, "to_x": 3.0, "to_y": 4.0}},
		{[]string{"scroll", "-3"}, nil, 0, map[string]any{"action": "scroll", "dy": -3}},
		{[]string{"scroll", "2", "1"}, nil, 0, map[string]any{"action": "scroll", "dy": 2, "dx": 1}},
	}
	for _, c := range ok {
		got, err := inputAction(c.words, c.mods, c.width)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("inputAction(%v) = %v, %v; want %v", c.words, got, err, c.want)
		}
	}
	bad := [][]string{{}, {"tap", "1", "2"}, {"type"}, {"key"}, {"click", "1"}, {"click", "-1", "2"}, {"drag", "1", "2"}, {"scroll", "x"}, {"type", strings.Repeat("a", maxTypeChars+1)}}
	for _, words := range bad {
		if _, err := inputAction(words, nil, 0); err == nil {
			t.Errorf("inputAction(%v) accepted", words)
		}
	}
	if _, err := inputAction([]string{"move", "1", "2"}, []string{"shift"}, 0); err == nil {
		t.Error("modifier on move accepted")
	}
	if _, err := inputAction([]string{"key", "a"}, []string{"hyper"}, 0); err == nil {
		t.Error("unknown modifier accepted")
	}
}

func TestSessionInputBody(t *testing.T) {
	decode := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	got, err := sessionInputBody(decode(`{"name":"w","action":"drag","x":1,"y":2,"toX":3,"toY":4,"screenWidth":800,"screenshot":true}`))
	want := map[string]any{"action": "drag", "x": 1.0, "y": 2.0, "to_x": 3.0, "to_y": 4.0, "screen_width": 800.0}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("single = %v, %v", got, err)
	}
	got, err = sessionInputBody(decode(`{"name":"w","screenWidth":800,"actions":[{"action":"click","x":5,"y":6},{"action":"type","text":"hi"}]}`))
	want = map[string]any{"actions": []map[string]any{{"action": "click", "x": 5.0, "y": 6.0, "screen_width": 800.0}, {"action": "type", "text": "hi"}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %v, %v", got, err)
	}
	for _, bad := range []string{
		`{"name":"w"}`,
		`{"name":"w","action":"click","actions":[{"action":"click"}]}`,
		`{"name":"w","action":"tap"}`,
		`{"name":"w","action":"click","bogus":1}`,
		`{"name":"w","actions":[]}`,
		`{"name":"w","actions":["click"]}`,
	} {
		if _, err := sessionInputBody(decode(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestRedactTypedText(t *testing.T) {
	got := string(redactTypedText(json.RawMessage(`{"name":"w","action":"type","text":"hunter2","actions":[{"action":"type","text":"secret!"}]}`)))
	if strings.Contains(got, "hunter2") || strings.Contains(got, "secret!") || !strings.Contains(got, "[7 characters]") {
		t.Fatalf("redacted = %s", got)
	}
}

func TestSessionScreenMCP(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake")
	var inputBody map[string]any
	var shotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agentsessions/dev/work/screenshot":
			shotQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("X-Fluxvm-Screen-Width", "1280")
			w.Header().Set("X-Fluxvm-Screen-Height", "800")
			_, _ = w.Write(png)
		case "/api/v1/agentsessions/dev/work/input":
			_ = json.NewDecoder(r.Body).Decode(&inputBody)
			_, _ = io.WriteString(w, `{"ok":true,"actions":1}`)
		case "/api/v1/agentsessions/dev/text/screenshot":
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "t")
	tools := map[string]mcp.Tool{}
	for _, tool := range sessionScreenTools("dev") {
		tools[tool.Name] = tool
	}
	blocks, err := tools["session_screenshot"].CallBlocks(context.Background(), json.RawMessage(`{"name":"work"}`))
	if err != nil || len(blocks) != 2 || blocks[0]["type"] != "image" || blocks[1]["text"] != "1280x800 PNG" || shotQuery != "max_width=1280" {
		t.Fatalf("screenshot = %v, %v (query %q)", blocks, err, shotQuery)
	}
	if _, err := tools["session_screenshot"].CallBlocks(context.Background(), json.RawMessage(`{"name":"text"}`)); err == nil {
		t.Fatal("a non-PNG answer was accepted")
	}
	blocks, err = tools["session_input"].CallBlocks(context.Background(), json.RawMessage(`{"name":"work","action":"key","key":"enter","screenshot":true}`))
	if err != nil || len(blocks) != 3 || blocks[0]["type"] != "image" || inputBody["key"] != "enter" {
		t.Fatalf("input = %v, %v (body %v)", blocks, err, inputBody)
	}
	if !tools["session_input"].Write || tools["session_screenshot"].Write {
		t.Fatal("session_input must be a write tool and session_screenshot a read tool")
	}
}
