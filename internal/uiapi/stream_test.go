// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestStreamEmitsMachineChange(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm-1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm-1", Namespace: "default"},
		Spec:     model.MachineSpec{NodeName: "worker-1"},
		Status:   model.MachineStatus{Phase: "Running"},
	}
	s := newTestServer(t, fk, "")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before a change arrived")
			}
			if strings.HasPrefix(l, "data: ") {
				for _, want := range []string{`"type":"ADDED"`, `"kind":"Machine"`, `"name":"vm-1"`, `"phase":"Running"`, `"nodeName":"worker-1"`} {
					if !strings.Contains(l, want) {
						t.Errorf("payload %s missing %s", l, want)
					}
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for a change event")
		}
	}
}

func TestSummarizeWatchEventSkipsBookmarkAndError(t *testing.T) {
	for _, typ := range []string{"BOOKMARK", "ERROR"} {
		if _, ok := summarizeWatchEvent("Machine", watchEvent(typ, `{"metadata":{"name":"x"}}`)); ok {
			t.Errorf("%s should be skipped", typ)
		}
	}
}

func watchEvent(typ, obj string) kube.WatchEvent {
	return kube.WatchEvent{Type: typ, Object: json.RawMessage(obj)}
}
