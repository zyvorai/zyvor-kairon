// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

var auditNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func auditMig(ns, name, machine, phase string, age time.Duration) model.MachineMigration {
	return model.MachineMigration{
		Metadata: model.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: auditNow.Add(-age)},
		Spec:     model.MachineMigrationSpec{MachineName: machine, Strategy: "auto"},
		Status:   model.MachineMigrationStatus{Phase: phase, SourceNode: "n1", TargetNode: "n2", EffectiveStrategy: "live", DowntimeMs: 120, Message: "ok"},
	}
}

func TestBuildAudit(t *testing.T) {
	migs := []model.MachineMigration{
		auditMig("a", "old", "vm-1", "Succeeded", 72*time.Hour),
		auditMig("a", "mid", "vm-1", "Failed", 3*time.Hour),
		auditMig("b", "new", "vm-2", "Succeeded", 10*time.Minute),
		auditMig("b", "pend", "vm-3", "", time.Minute),
	}
	rep := buildAudit(migs, auditNow.Add(-24*time.Hour), "", 0)
	if rep.Total != 3 || rep.ByPhase["Succeeded"] != 1 || rep.ByPhase["Failed"] != 1 || rep.ByPhase["Pending"] != 1 {
		t.Fatalf("counts: %+v", rep)
	}
	if got := []string{rep.Entries[0].Name, rep.Entries[1].Name, rep.Entries[2].Name}; strings.Join(got, ",") != "pend,new,mid" {
		t.Fatalf("order (newest first): %v", got)
	}
	if rep = buildAudit(migs, time.Time{}, "vm-1", 0); rep.Total != 2 {
		t.Fatalf("machine filter: %+v", rep)
	}
	if rep = buildAudit(migs, time.Time{}, "", 1); rep.Total != 4 || len(rep.Entries) != 1 {
		t.Fatalf("limit must trim entries but not counts: %+v", rep)
	}
}

func TestWriteAuditTableAndJSON(t *testing.T) {
	rep := buildAudit([]model.MachineMigration{auditMig("a", "m1", "vm-1", "Succeeded", 2*time.Hour)}, time.Time{}, "", 0)
	var buf bytes.Buffer
	if err := writeAudit(&buf, rep, "table", auditNow); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 migration(s): Succeeded=1", "AGE", "2h", "vm-1", "live", "n1", "n2", "120ms"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("table missing %q:\n%s", want, buf.String())
		}
	}
	buf.Reset()
	if err := writeAudit(&buf, rep, "json", auditNow); err != nil {
		t.Fatal(err)
	}
	var got auditReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || got.Total != 1 || got.Entries[0].Machine != "vm-1" {
		t.Fatalf("json: %v %+v", err, got)
	}
	buf.Reset()
	_ = writeAudit(&buf, auditReport{}, "table", auditNow)
	if !strings.Contains(buf.String(), "no migrations") {
		t.Errorf("empty output: %q", buf.String())
	}
	if err := writeAudit(&buf, rep, "xml", auditNow); err == nil {
		t.Error("want error for unknown format")
	}
}

func TestRunAuditScopesNamespaceAgainstFakeServer(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{auditMig("prod", "m1", "db", "Succeeded", time.Hour)}})
	}))
	defer srv.Close()
	kc := &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	var buf bytes.Buffer
	if err := runAudit(context.Background(), kc, &buf, "", "", 0, 50, "table", auditNow); err != nil {
		t.Fatal(err)
	}
	if err := runAudit(context.Background(), kc, &buf, "prod", "", 0, 50, "table", auditNow); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/apis/kairon.zyvor.dev/v1/machinemigrations" || paths[1] != "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations" {
		t.Fatalf("paths: %v", paths)
	}
}

func TestAuditAge(t *testing.T) {
	for d, want := range map[time.Duration]string{30 * time.Second: "30s", 5 * time.Minute: "5m", 5 * time.Hour: "5h", 72 * time.Hour: "3d", -time.Hour: "0s"} {
		if got := auditAge(auditNow, auditNow.Add(-d)); got != want {
			t.Errorf("auditAge(%v) = %s, want %s", d, got, want)
		}
	}
}
