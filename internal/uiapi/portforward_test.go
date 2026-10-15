// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zyvorai/kairon/internal/consoleproxy"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/model"
)

func listenEchoTCP(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// portForwardChain wires kairon-ui -> a real consoleproxy.Server -> a
// user-mode FluxVM record forwarding guest port 22 to a local echo server.
func portForwardChain(t *testing.T, m model.Machine) (*Server, string) {
	t.Helper()
	echoPort := listenEchoTCP(t)
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "runtime-1", "status": "Running",
			"request": map[string]any{"network": map[string]any{
				"mode":     "user",
				"forwards": []map[string]any{{"host_port": echoPort, "guest_port": 22, "protocol": "tcp"}},
			}},
		})
	}))
	t.Cleanup(fluxSrv.Close)

	const consoleToken = "shared-node-console-token"
	nodeSrv := httptest.NewServer((&consoleproxy.Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: consoleToken}).Handler())
	t.Cleanup(nodeSrv.Close)
	nodeURL, _ := url.Parse(nodeSrv.URL)
	nodeHost, nodePort, _ := net.SplitHostPort(nodeURL.Host)

	fk := newFakeKube()
	fk.machines[m.Metadata.Name] = m
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	t.Cleanup(kubeSrv.Close)

	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL), ConsoleToken: consoleToken, ConsolePort: nodePort, Metrics: metrics.NewUIRecorder()}
	uiSrv := httptest.NewServer(s.Handler())
	t.Cleanup(uiSrv.Close)
	return s, "ws" + strings.TrimPrefix(uiSrv.URL, "http")
}

func pfMachine(annotations map[string]string) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default", Annotations: annotations},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
}

func TestHandlePortForwardFullRelay(t *testing.T) {
	_, base := portForwardChain(t, pfMachine(nil))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialWS(ctx, base+"/api/v1/machines/default/vm1/portforward?port=22", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	nc := websocket.NetConn(ctx, conn, websocket.MessageBinary)
	if _, err := nc.Write([]byte("SSH-2.0-test\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 64)
	n, err := io.ReadAtLeast(nc, buf, len("SSH-2.0-test\r\n"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "SSH-2.0-test\r\n" {
		t.Fatalf("round trip = %q", got)
	}
}

func TestHandlePortForwardRejects(t *testing.T) {
	cases := []struct {
		name   string
		m      model.Machine
		port   string
		status int
		body   string
	}{
		{"bad port", pfMachine(nil), "0", http.StatusBadRequest, ""},
		{"not running", model.Machine{Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"}, Status: model.MachineStatus{Phase: "Stopped"}}, "22", http.StatusConflict, ""},
		{"allowlist without identity", pfMachine(map[string]string{model.AnnotationConsoleAllowedUsers: "alice"}), "22", http.StatusForbidden, ""},
		{"port not forwarded", pfMachine(nil), "80", http.StatusConflict, "guest port 80 is not forwarded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, base := portForwardChain(t, tc.m)
			_, resp, err := websocket.Dial(context.Background(), base+"/api/v1/machines/default/vm1/portforward?port="+tc.port, nil) //nolint:bodyclose // failed upgrade; the body is not used
			if err == nil {
				t.Fatal("expected the upgrade to fail")
			}
			if resp == nil || resp.StatusCode != tc.status {
				status := 0
				if resp != nil {
					status = resp.StatusCode
				}
				t.Fatalf("status = %d, want %d (%v)", status, tc.status, err)
			}
			if tc.body != "" {
				b, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(b), tc.body) {
					t.Fatalf("body = %q, want it to contain %q", b, tc.body)
				}
			}
		})
	}
}
