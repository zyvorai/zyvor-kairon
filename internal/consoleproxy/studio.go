// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// FluxVM Studio (the node daemon's web UI, FluxVM `/console/`) reached through this relay, so a Kairon node's
// daemon -- which listens on loopback -- has a UI without a port-forward:
//
//	/studio/console/...   the SPA's static files and its single-use WebSocket tickets. No bearer: the files hold no
//	                      data and a ticket is the credential FluxVM itself checks, exactly as on the daemon.
//	/studio/v1/...        FluxVM's REST API. Needs the relay's shared bearer, which the SPA sends as its sign-in token
//	                      (the SPA derives its API base from the `/console` segment of its own URL, so it lands here).
//	                      FluxVM's own token replaces it upstream, so whoever holds the relay token has the daemon's
//	                      authority -- the same blast radius as the exec and files routes next to it.
//
// The relay port must still not be exposed to the internet (docs/security.md).

func (s *Server) registerStudio(mux *http.ServeMux) {
	mux.HandleFunc("GET /studio/console", func(w http.ResponseWriter, r *http.Request) {
		target := "/studio/console/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
	mux.HandleFunc("GET /studio/console/{rest...}", s.handleStudioConsole)
	mux.HandleFunc("/studio/v1/{rest...}", s.handleStudioAPI)
}

// cleanRest rejects anything that could climb out of the prefix it is appended to.
func cleanRest(rest string) (string, bool) {
	for _, seg := range strings.Split(rest, "/") {
		if seg == ".." || strings.ContainsAny(seg, "\\\x00") {
			return "", false
		}
	}
	return rest, true
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func (s *Server) handleStudioConsole(w http.ResponseWriter, r *http.Request) {
	rest, ok := cleanRest(r.PathValue("rest"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	upstream := "/console/" + rest
	if isWebSocketUpgrade(r) {
		s.relayStudioWebSocket(w, r, upstream)
		return
	}
	s.proxyStudio(w, r, upstream, false)
}

func (s *Server) handleStudioAPI(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	rest, ok := cleanRest(r.PathValue("rest"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.proxyStudio(w, r, "/v1/"+rest, true)
}

// proxyStudio relays one HTTP request and streams the response back, flushing as it goes so server-sent events
// (`/v1/events/stream`) and followed logs arrive live. The relay's own bearer is never sent upstream.
func (s *Server) proxyStudio(w http.ResponseWriter, r *http.Request, upstreamPath string, authed bool) {
	upstreamURL := s.Flux.BaseURL + upstreamPath
	if q := r.URL.RawQuery; q != "" {
		upstreamURL += "?" + q
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for k, vv := range r.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Authorization", "Connection", "Upgrade", "Cookie", "Host":
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	if authed && s.Flux.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Flux.Token)
	}
	resp, err := s.Flux.HTTP.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial fluxvm: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return // io.EOF is the normal end; anything else means the stream broke and the client sees a short body
		}
	}
}

// relayStudioWebSocket bridges a browser WebSocket to FluxVM's ticketed endpoint (`/console/ws/{ticket}`,
// `/console/pty/{ticket}`). No bearer is added: FluxVM redeems the single-use ticket itself.
func (s *Server) relayStudioWebSocket(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	upstreamURL := fluxWebSocketURL(s.Flux, upstreamPath, r.URL.RawQuery)
	dialCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	upstream, resp, err := websocket.Dial(dialCtx, upstreamURL, &websocket.DialOptions{HTTPClient: s.Flux.HTTP})
	cancel()
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		code := http.StatusBadGateway
		if resp != nil && resp.StatusCode >= 400 {
			code = resp.StatusCode // an unknown or used ticket is FluxVM's 404, not a gateway fault
		}
		http.Error(w, fmt.Sprintf("dial fluxvm: %v", err), code)
		return
	}
	defer func() { _ = upstream.CloseNow() }()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(4 << 20)
	upstream.SetReadLimit(4 << 20)
	// Frames are copied with their type: a terminal sends binary, but the daemon's JSON control messages (resize)
	// and agent-session PTY use text frames, and NetConn would coerce them all to one type.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	done := make(chan struct{}, 2)
	pump := func(dst, src *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			typ, data, err := src.Read(ctx)
			if err != nil {
				return
			}
			if err := dst.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}
	go pump(upstream, conn)
	go pump(conn, upstream)
	<-done
	cancel()
	_ = conn.Close(websocket.StatusNormalClosure, "")
}
