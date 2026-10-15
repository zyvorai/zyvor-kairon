// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package incidentwork

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/rehearsal"
)

func incidentPlan() rehearsal.Plan {
	step := func(name string) rehearsal.Step {
		return rehearsal.Step{Name: name, Machine: "db", Path: "/bin/true", TimeoutSeconds: 5}
	}
	baseline := step("reproduce")
	baseline.ExpectedExitCode = 1
	return rehearsal.Plan{APIVersion: "rehearsal.kairon.zyvor.dev/v1alpha1", Name: "incident", Namespace: "default", Node: "lab", Bridge: "krh-demo", TimeoutSeconds: 30, TTLSeconds: 180, Consistency: "existing", Members: []rehearsal.Member{{Name: "db", SourceSnapshot: "saved"}}, Baseline: []rehearsal.Step{baseline}, Changes: []rehearsal.Step{step("fix")}, Verify: []rehearsal.Step{step("verify")}}
}
func archive(t *testing.T, m Manifest, extra string, kind byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	b, _ := json.Marshal(m)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(b); err != nil {
		t.Fatal(err)
	}
	if extra != "" {
		if err := tw.WriteHeader(&tar.Header{Name: extra, Mode: 0600, Typeflag: kind, Linkname: "/etc/passwd"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func goodManifest() Manifest {
	return Manifest{Kind: "KaironIncidentBundle", Namespace: "default", Machine: "db", UID: "source-uid", CollectedAt: time.Now().UTC(), Redacted: true}
}
func TestIncidentBundleBinding(t *testing.T) {
	b := archive(t, goodManifest(), "cluster/machine.json", tar.TypeReg)
	p, err := Bind(bytes.NewReader(b), incidentPlan())
	if err != nil {
		t.Fatal(err)
	}
	if p.Purpose != "incident" || p.Incident.SourceUID != "source-uid" || len(p.Incident.SHA256) != 64 {
		t.Fatal(p)
	}
	again, err := Bind(bytes.NewReader(b), incidentPlan())
	if err != nil || again.Incident.SHA256 != p.Incident.SHA256 {
		t.Fatal("unstable incident digest")
	}
}
func TestIncidentArchiveRefusals(t *testing.T) {
	for _, test := range []string{"unredacted", "wrong-namespace", "missing-uid", "wrong-kind", "traversal", "absolute", "symlink", "duplicate-manifest", "broken-gzip", "not-reproducing"} {
		t.Run(test, func(t *testing.T) {
			m := goodManifest()
			extra := ""
			kind := byte(tar.TypeReg)
			p := incidentPlan()
			switch test {
			case "unredacted":
				m.Redacted = false
			case "wrong-namespace":
				m.Namespace = "other"
			case "missing-uid":
				m.UID = ""
			case "wrong-kind":
				m.Kind = "other"
			case "traversal":
				extra = "../escape"
			case "absolute":
				extra = "/etc/passwd"
			case "symlink":
				extra = "link"
				kind = tar.TypeSymlink
			case "duplicate-manifest":
				extra = "manifest.json"
			case "not-reproducing":
				p.Baseline[0].ExpectedExitCode = 0
			}
			b := archive(t, m, extra, kind)
			if test == "broken-gzip" {
				b = []byte("bad")
			}
			if _, err := Bind(bytes.NewReader(b), p); err == nil {
				t.Fatal("accepted unsafe incident input")
			}
		})
	}
}
func TestIncidentCannotExecuteArchiveCommands(t *testing.T) {
	p := incidentPlan()
	b := archive(t, goodManifest(), "commands/run-me.sh", tar.TypeReg)
	out, err := Bind(bytes.NewReader(b), p)
	if err != nil {
		t.Fatal(err)
	}
	if out.Changes[0].Name != "fix" || strings.Contains(out.Changes[0].Path, "run-me") {
		t.Fatal("bundle altered executable plan")
	}
}
