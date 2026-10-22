// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

func studioServer(t *testing.T, flux http.Handler, fluxToken string) *httptest.Server {
	t.Helper()
	fs := httptest.NewServer(flux)
	t.Cleanup(fs.Close)
	srv := httptest.NewServer((&Server{Flux: fluxvm.New(fs.URL, fluxToken), Token: "relay-secret"}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestStudioConsoleServesStaticFilesWithoutABearer(t *testing.T) {
	var gotPath, gotINM, gotAuth string
	srv := studioServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotINM, gotAuth = r.URL.Path, r.Header.Get("If-None-Match"), r.Header.Get("Authorization")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Header().Set("ETag", `"abc"`)
		_, _ = w.Write([]byte("console.log(1)"))
	}), "flux-token")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/studio/console/assets/app.js", nil)
	req.Header.Set("If-None-Match", `"abc"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "console.log(1)" {
		t.Fatalf("unexpected response %d %q", resp.StatusCode, body)
	}
	if gotPath != "/console/assets/app.js" || gotINM != `"abc"` {
		t.Fatalf("upstream saw path=%q if-none-match=%q", gotPath, gotINM)
	}
	if gotAuth != "" {
		t.Fatalf("static files must not carry FluxVM's token, got %q", gotAuth)
	}
	if resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("ETag") == "" {
		t.Fatalf("security and cache headers must pass through: %v", resp.Header)
	}
}

func TestStudioConsoleRedirectsToTheSlashFormAndKeepsTheQuery(t *testing.T) {
	srv := studioServer(t, http.NotFoundHandler(), "")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(srv.URL + "/studio/console?x=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "/studio/console/?x=1" {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestStudioConsoleRejectsPathTraversal(t *testing.T) {
	srv := studioServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("FluxVM must not be reached: %s", r.URL.Path)
	}), "")
	for _, p := range []string{"/studio/console/..%2fv1%2fvms", "/studio/console/a/%2e%2e/b"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatalf("%s: traversal served", p)
		}
	}
}

func TestStudioAPINeedsTheRelayBearerAndSwapsInFluxVMsOwn(t *testing.T) {
	var gotAuth, gotPath, gotMethod, gotBody string
	srv := studioServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.RequestURI(), r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
	}), "flux-token")

	resp, err := http.Post(srv.URL+"/studio/v1/vms", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || gotPath != "" {
		t.Fatalf("an unauthenticated API call must stop at the relay: %d, upstream saw %q", resp.StatusCode, gotPath)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/studio/v1/vms?namespace=all", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Authorization", "Bearer relay-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if gotAuth != "Bearer flux-token" || gotPath != "/v1/vms?namespace=all" || gotMethod != http.MethodPost || gotBody != `{"name":"x"}` {
		t.Fatalf("upstream saw auth=%q path=%q method=%q body=%q", gotAuth, gotPath, gotMethod, gotBody)
	}
}

func TestStudioAPIStreamsServerSentEventsAsTheyArrive(t *testing.T) {
	release := make(chan struct{})
	srv := studioServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: vm.started\ndata: {}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}), "")
	defer close(release)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/studio/v1/events/stream", nil)
	req.Header.Set("Authorization", "Bearer relay-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		if !strings.Contains(s, "vm.started") {
			t.Fatalf("got %q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first event was held back: the relay is buffering the stream")
	}
}

func TestStudioConsoleRelaysAWebSocketTicketWithoutABearer(t *testing.T) {
	var gotAuth, gotPath string
	srv := studioServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		_, msg, err := c.Read(r.Context())
		if err == nil {
			_ = c.Write(r.Context(), websocket.MessageText, append([]byte("echo:"), msg...))
		}
	}), "flux-token")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/studio/console/ws/ticket-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	if err := c.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.Read(ctx)
	if err != nil || string(msg) != "echo:hi" {
		t.Fatalf("got %q, %v", msg, err)
	}
	if gotPath != "/console/ws/ticket-1" || gotAuth != "" {
		t.Fatalf("upstream saw path=%q auth=%q (the ticket is the credential)", gotPath, gotAuth)
	}
}

func TestStudioConsoleReportsAnUnknownTicketAsFluxVMDoes(t *testing.T) {
	srv := studioServer(t, http.NotFoundHandler(), "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/studio/console/ws/nope", nil)
	if err == nil {
		t.Fatal("expected the dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown ticket should be a 404, got %+v", resp)
	}
}
