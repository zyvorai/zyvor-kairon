// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const sessionsPrefix = "/api/v1/agentsessions/default/demo/"

func TestSessionExecApprovalRequired(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["approval_id"] == nil {
			w.WriteHeader(http.StatusPreconditionRequired)
			_, _ = io.WriteString(w, `{"approval_required":true,"approval_id":"a1b2","kind":"exec","summary":"git push","matched":"git push*","expires_at":"2026-10-14T12:00:00Z","error":"x"}`)
			return
		}
		_, _ = io.WriteString(w, `{"exit_code":0,"stdout":"pushed\n","stderr":""}`)
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "t")

	_, err := sessionExec(t.Context(), "default", "demo", "git push", 0, approvalFlags{})
	var ar *approvalRequired
	if !errors.As(err, &ar) || ar.ID != "a1b2" || ar.Matched != "git push*" || !strings.Contains(err.Error(), "kaironctl session approve demo a1b2") {
		t.Fatalf("err = %v", err)
	}
	res, err := sessionExec(t.Context(), "default", "demo", "git push", 0, approvalFlags{id: "a1b2", wait: 90 * time.Second})
	if err != nil || res.Stdout != "pushed\n" {
		t.Fatalf("approved exec = %+v, %v", res, err)
	}
	if got["approval_id"] != "a1b2" || got["approval_wait_seconds"] != float64(90) {
		t.Fatalf("body = %v", got)
	}
	if err := (approvalFlags{wait: 3 * time.Minute}).apply(map[string]any{}); err == nil {
		t.Fatal("--approval-wait over 2m accepted")
	}
}

func TestSessionUploadChunksAndResumes(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), (9<<20)/16+7)
	local := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(local, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	var (
		mu        sync.Mutex
		received  bytes.Buffer
		conflicts int
		created   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == sessionsPrefix+"uploads":
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"u1","received":0}`)
		case r.Method == http.MethodPut && r.URL.Path == sessionsPrefix+"uploads/u1":
			off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			chunk, _ := io.ReadAll(r.Body)
			if len(chunk) > 4<<20 {
				t.Errorf("chunk of %d bytes", len(chunk))
			}
			if off == 4<<20 && conflicts == 0 {
				conflicts++
				w.WriteHeader(http.StatusConflict)
				return
			}
			if off != received.Len() {
				w.WriteHeader(http.StatusConflict)
				return
			}
			received.Write(chunk)
			_, _ = io.WriteString(w, `{"received":`+strconv.Itoa(received.Len())+`}`)
		case r.Method == http.MethodGet && r.URL.Path == sessionsPrefix+"uploads/u1":
			_, _ = io.WriteString(w, `{"id":"u1","state":"open","received":`+strconv.Itoa(received.Len())+`}`)
		case r.Method == http.MethodPost && r.URL.Path == sessionsPrefix+"uploads/u1/complete":
			_, _ = io.WriteString(w, `{"id":"u1","state":"complete","path":"/workspace/big.bin"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "t")

	raw, err := sessionUpload(t.Context(), "default", "demo", local, "/workspace/big.bin", nil, true, nil)
	if err != nil || !strings.Contains(string(raw), `"complete"`) {
		t.Fatalf("upload = %s, %v", raw, err)
	}
	if !bytes.Equal(received.Bytes(), data) || conflicts != 1 {
		t.Fatalf("received %d of %d bytes, conflicts %d", received.Len(), len(data), conflicts)
	}
	if created["sha256"] != hex.EncodeToString(sum[:]) || created["size"] != float64(len(data)) || created["path"] != "/workspace/big.bin" {
		t.Fatalf("create body = %v", created)
	}
}

func TestSessionUploadAbortsOnFailure(t *testing.T) {
	local := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(local, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	aborted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == sessionsPrefix+"uploads":
			_, _ = io.WriteString(w, `{"id":"u2"}`)
		case r.Method == http.MethodPut:
			_, _ = io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"the checksum does not match"}`)
		case r.Method == http.MethodDelete && r.URL.Path == sessionsPrefix+"uploads/u2":
			aborted = true
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "t")
	if _, err := sessionUpload(t.Context(), "default", "demo", local, "/x", nil, true, nil); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
	if !aborted {
		t.Fatal("failed upload was not aborted")
	}
}

func TestPtyQuery(t *testing.T) {
	env := map[string]string{"TERM": "xterm-ghostty", "TERM_PROGRAM": "ghostty"}
	q := ptyQuery(900, 0, "htop", true, func(k string) string { return env[k] })
	if q.Get("cols") != "500" || q.Get("rows") != "1" || q.Get("command") != "htop" || q.Get("term") != "xterm-ghostty" ||
		q.Get("term_program") != "ghostty" || q.Get("plain") != "true" || q.Has("colorterm") {
		t.Fatalf("query = %v", q)
	}
}

func TestRunSessionPtyExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_, resize, _ := c.Read(r.Context())
		_, in, _ := c.Read(r.Context())
		_ = c.Write(r.Context(), websocket.MessageBinary, append(resize, in...))
		_ = c.Close(websocket.StatusNormalClosure, "exit=3")
	}))
	defer srv.Close()
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil) //nolint:bodyclose // websocket upgrade response body is hijacked
	if err != nil {
		t.Fatal(err)
	}
	resize := make(chan [2]int, 1)
	resize <- [2]int{120, 40}
	pr, pw := io.Pipe()
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = pw.Write([]byte("exit 3\n"))
	}()
	var out bytes.Buffer
	if code := runSessionPty(t.Context(), conn, pr, &out, resize); code != 3 {
		t.Fatalf("exit code = %d", code)
	}
	if out.String() != `{"resize":{"cols":120,"rows":40}}exit 3`+"\n" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestProbeSummary(t *testing.T) {
	got := probeSummary(map[string]string{"git": "/usr/bin/git", "go": "/usr/bin/go"}, map[string]string{"arch": "aarch64", "kernel": "6.8"},
		map[string]bool{"git": true, "build_node": false})
	want := "system: aarch64 6.8\ntools: git go\nready for: git\nnot ready for: build_node\n"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestPrintApprovalsPendingFirst(t *testing.T) {
	var b bytes.Buffer
	printApprovals(&b, []sessionApproval{
		{ID: "old", State: "consumed", RequestedAt: "2026-10-14T10:00:00Z", Summary: "deploy a"},
		{ID: "new", State: "pending", RequestedAt: "2026-10-14T11:00:00Z", Summary: "deploy\nb", Matched: "deploy *"},
	}, false)
	if strings.Contains(b.String(), "old") || !strings.Contains(b.String(), "new\tpending\t-\tdeploy *\tdeploy b") {
		t.Fatalf("table = %q", b.String())
	}
}
