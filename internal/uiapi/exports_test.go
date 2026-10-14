// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/consoleproxy"
	"github.com/zyvorai/kairon/internal/diskexport"
	"github.com/zyvorai/kairon/internal/model"
)

func exportChain(t *testing.T, phase string) string {
	t.Helper()
	disk := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(disk, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := &diskexport.Registry{}
	reg.Put("default/web-export", diskexport.Entry{Path: disk, Format: "raw", Expires: time.Now().Add(time.Hour)})
	const consoleToken = "shared-node-console-token"
	nodeSrv := httptest.NewServer((&consoleproxy.Server{Token: consoleToken, Exports: reg}).Handler())
	t.Cleanup(nodeSrv.Close)
	nodeURL, _ := url.Parse(nodeSrv.URL)
	nodeHost, nodePort, _ := net.SplitHostPort(nodeURL.Host)

	fk := newFakeKube()
	fk.machines["web"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "web", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Stopped", NodeName: "worker-1"},
	}
	fk.exports = map[string]model.MachineExport{"web-export": {
		Metadata: model.ObjectMeta{Name: "web-export", Namespace: "default"},
		Spec:     model.MachineExportSpec{MachineName: "web"},
		Status:   model.MachineExportStatus{Phase: phase, NodeName: "worker-1", Format: "raw"},
	}}
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	t.Cleanup(kubeSrv.Close)
	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL), ConsoleToken: consoleToken, ConsolePort: nodePort}
	uiSrv := httptest.NewServer(s.Handler())
	t.Cleanup(uiSrv.Close)
	return uiSrv.URL
}

func TestExportDownloadStreamsWithRange(t *testing.T) {
	base := exportChain(t, "Ready")
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/exports/default/web-export/download", nil)
	req.Header.Set("Range", "bytes=2-5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "2345" {
		t.Fatalf("code=%d body=%q", resp.StatusCode, body)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="web.raw"` {
		t.Fatalf("Content-Disposition %q", cd)
	}
}

func TestExportLinkIsSingleUse(t *testing.T) {
	base := exportChain(t, "Ready")
	resp, err := http.Post(base+"/api/v1/exports/default/web-export/link", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var link struct {
		URL string `json:"url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&link)
	_ = resp.Body.Close()
	if link.URL == "" {
		t.Fatalf("no link, status %d", resp.StatusCode)
	}
	for i, want := range []int{http.StatusOK, http.StatusUnauthorized} {
		resp, err := http.Get(base + link.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("use %d: code=%d body=%q", i, resp.StatusCode, body)
		}
		if i == 0 && string(body) != "0123456789" {
			t.Fatalf("body %q", body)
		}
	}
}

func TestExportDownloadRefusesNotReady(t *testing.T) {
	base := exportChain(t, "Pending")
	resp, err := http.Get(base + "/api/v1/exports/default/web-export/download")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("code=%d", resp.StatusCode)
	}
}

func TestConsoleRejectsExportTicket(t *testing.T) {
	base := exportChain(t, "Ready")
	resp, err := http.Post(base+"/api/v1/exports/default/web-export/link", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var link struct {
		URL string `json:"url"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&link)
	_ = resp.Body.Close()
	ticket := link.URL[len("/api/v1/export-links/"):]
	resp, err = http.Get(base + "/api/v1/machines/default/export%2Fweb-export/console?ticket=" + ticket)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatalf("export ticket opened a console: %d", resp.StatusCode)
	}
}
