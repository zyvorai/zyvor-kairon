// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// machineSetStore is a one-object fake apiserver: GET returns the stored
// MachineSet, PATCH applies an RFC 7386 merge patch to it (honoring a
// metadata.resourceVersion precondition like the real API server).
type machineSetStore struct {
	mu      sync.Mutex
	obj     map[string]any
	patches []map[string]any
}

func applyMerge(dst, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		pm, pIsMap := v.(map[string]any)
		dm, dIsMap := dst[k].(map[string]any)
		if pIsMap && dIsMap {
			applyMerge(dm, pm)
			continue
		}
		if pIsMap {
			fresh := map[string]any{}
			applyMerge(fresh, pm)
			dst[k] = fresh
			continue
		}
		dst[k] = v
	}
}

func newMachineSetStore(t *testing.T, ms model.MachineSet) (*kube.Client, *machineSetStore) {
	t.Helper()
	raw, _ := json.Marshal(ms)
	store := &machineSetStore{obj: map[string]any{}}
	_ = json.Unmarshal(raw, &store.obj)
	const path = "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesets/web"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store.mu.Lock()
		defer store.mu.Unlock()
		if r.URL.Path != path {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		meta := store.obj["metadata"].(map[string]any)
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(store.obj)
		case http.MethodPatch:
			var patch map[string]any
			_ = json.NewDecoder(r.Body).Decode(&patch)
			if pm, ok := patch["metadata"].(map[string]any); ok {
				if rv, ok := pm["resourceVersion"]; ok && rv != meta["resourceVersion"] {
					http.Error(w, "conflict", http.StatusConflict)
					return
				}
			}
			store.patches = append(store.patches, patch)
			applyMerge(store.obj, patch)
			meta["resourceVersion"] = meta["resourceVersion"].(string) + "+"
			_ = json.NewEncoder(w).Encode(store.obj)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}, store
}

func (s *machineSetStore) machineSet(t *testing.T) model.MachineSet {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.obj)
	var ms model.MachineSet
	if err := json.Unmarshal(raw, &ms); err != nil {
		t.Fatal(err)
	}
	return ms
}

// rolledOutMachineSet is web after kairon-controller observed template v1
// then v2: v2 is live and recorded as current, v1 recorded as previous.
func rolledOutMachineSet() (ms model.MachineSet, v1, v2 model.MachineTemplate) {
	v1 = model.MachineTemplate{
		Labels: map[string]string{"tier": "web", "canary": "true"},
		Spec:   model.MachineSpec{Image: model.ImageSpec{Path: "/img-v1.qcow2"}, Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	v2 = model.MachineTemplate{
		Labels: map[string]string{"tier": "web"},
		Spec:   model.MachineSpec{Image: model.ImageSpec{Path: "/img-v2.qcow2"}, Resources: model.ResourceSpec{CPU: "2", Memory: "1Gi"}},
	}
	b1, _ := json.Marshal(v1)
	b2, _ := json.Marshal(v2)
	ms = model.MachineSet{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineSet},
		Metadata: model.ObjectMeta{Name: "web", Namespace: "prod", ResourceVersion: "7", Annotations: map[string]string{
			model.AnnotationMachineSetCurrentTemplateHash:  model.MachineSetTemplateHash(v2),
			model.AnnotationMachineSetCurrentTemplate:      string(b2),
			model.AnnotationMachineSetPreviousTemplateHash: model.MachineSetTemplateHash(v1),
			model.AnnotationMachineSetPreviousTemplate:     string(b1),
		}},
		Spec: model.MachineSetSpec{Replicas: 3, Template: v2},
	}
	return ms, v1, v2
}

// TestRollbackMachineSetRestoresAndGoesForwardAgain: rollback restores v1
// (including the "canary" label v2 dropped), swaps the annotation pairs,
// and a second rollback restores v2 -- with the stored object staying
// exactly what the controller would consider "in step" each time.
func TestRollbackMachineSetRestoresAndGoesForwardAgain(t *testing.T) {
	ms, v1, v2 := rolledOutMachineSet()
	kc, store := newMachineSetStore(t, ms)
	h1, h2 := model.MachineSetTemplateHash(v1), model.MachineSetTemplateHash(v2)

	from, to, err := rollbackMachineSet(context.Background(), kc, "prod", "web", false)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if from != h2 || to != h1 {
		t.Fatalf("from/to = %s/%s, want %s/%s", from, to, h2, h1)
	}
	if rv := store.patches[0]["metadata"].(map[string]any)["resourceVersion"]; rv != "7" {
		t.Errorf("patch resourceVersion = %v, want 7 (optimistic concurrency)", rv)
	}
	got := store.machineSet(t)
	if model.MachineSetTemplateHash(got.Spec.Template) != h1 {
		t.Fatalf("live template after rollback = %+v, want v1 %+v", got.Spec.Template, v1)
	}
	ann := got.Metadata.Annotations
	if ann[model.AnnotationMachineSetCurrentTemplateHash] != h1 || ann[model.AnnotationMachineSetPreviousTemplateHash] != h2 {
		t.Fatalf("annotations not swapped: %v", ann)
	}

	if _, _, err := rollbackMachineSet(context.Background(), kc, "prod", "web", false); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	got = store.machineSet(t)
	if model.MachineSetTemplateHash(got.Spec.Template) != h2 {
		t.Fatalf("second rollback should go forward to v2, got %+v", got.Spec.Template)
	}
	if _, stale := got.Spec.Template.Labels["canary"]; stale {
		t.Errorf("canary label should be removed again on going forward, got %v", got.Spec.Template.Labels)
	}
	if got.Metadata.Annotations[model.AnnotationMachineSetCurrentTemplateHash] != h2 {
		t.Errorf("current hash = %s, want %s", got.Metadata.Annotations[model.AnnotationMachineSetCurrentTemplateHash], h2)
	}
}

func TestRollbackMachineSetDryRunChangesNothing(t *testing.T) {
	ms, v1, v2 := rolledOutMachineSet()
	kc, store := newMachineSetStore(t, ms)
	from, to, err := rollbackMachineSet(context.Background(), kc, "prod", "web", true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if from != model.MachineSetTemplateHash(v2) || to != model.MachineSetTemplateHash(v1) {
		t.Errorf("dry-run from/to = %s/%s", from, to)
	}
	if len(store.patches) != 0 {
		t.Fatalf("dry-run sent %d PATCH(es)", len(store.patches))
	}
}

func TestRollbackMachineSetRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*model.MachineSet)
		want   string
	}{
		"no previous recorded": {func(ms *model.MachineSet) {
			delete(ms.Metadata.Annotations, model.AnnotationMachineSetPreviousTemplateHash)
			delete(ms.Metadata.Annotations, model.AnnotationMachineSetPreviousTemplate)
		}, "no previous template"},
		"previous recorded by hash only": {func(ms *model.MachineSet) {
			delete(ms.Metadata.Annotations, model.AnnotationMachineSetPreviousTemplate)
		}, "hash only"},
		"live template not yet recorded": {func(ms *model.MachineSet) {
			ms.Spec.Template.Spec.Resources.CPU = "8"
		}, "has not recorded the live template"},
		"tampered previous body": {func(ms *model.MachineSet) {
			ms.Metadata.Annotations[model.AnnotationMachineSetPreviousTemplate] = `{"spec":{"image":{"path":"/other.qcow2"}}}`
		}, "refusing to restore"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ms, _, _ := rolledOutMachineSet()
			tc.mutate(&ms)
			kc, store := newMachineSetStore(t, ms)
			_, _, err := rollbackMachineSet(context.Background(), kc, "prod", "web", false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if len(store.patches) != 0 {
				t.Fatalf("refused rollback still sent %d PATCH(es)", len(store.patches))
			}
		})
	}
}

// TestRollbackMachineSetConflictOnConcurrentEdit: the PATCH is guarded by
// resourceVersion, so an edit landing between GET and PATCH fails the
// rollback instead of being silently overwritten.
func TestRollbackMachineSetConflictOnConcurrentEdit(t *testing.T) {
	ms, _, _ := rolledOutMachineSet()
	kc, store := newMachineSetStore(t, ms)
	store.mu.Lock()
	orig := store.obj
	store.mu.Unlock()
	// Simulate a concurrent write: bump resourceVersion after GET by
	// wrapping the transport.
	kc.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPatch {
			store.mu.Lock()
			orig["metadata"].(map[string]any)["resourceVersion"] = "8"
			store.mu.Unlock()
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	if _, _, err := rollbackMachineSet(context.Background(), kc, "prod", "web", false); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("err = %v, want a 409 conflict", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReplaceMergePatchNullsRemovedKeys(t *testing.T) {
	from := map[string]any{"labels": map[string]any{"a": "1", "b": "2"}, "spec": map[string]any{"image": "x"}}
	to := map[string]any{"labels": map[string]any{"a": "1"}}
	got, err := replaceMergePatch(from, to)
	if err != nil {
		t.Fatal(err)
	}
	labels := got["labels"].(map[string]any)
	if v, ok := labels["b"]; !ok || v != nil {
		t.Errorf("labels.b should be null, got %v (present=%v)", v, ok)
	}
	if v, ok := got["spec"]; !ok || v != nil {
		t.Errorf("spec should be null, got %v (present=%v)", v, ok)
	}
}

func TestMachineSetHistoryShowsStoredTemplate(t *testing.T) {
	ms, v1, v2 := rolledOutMachineSet()
	kc, _ := newMachineSetStore(t, ms)
	var out strings.Builder
	if err := machineSetHistory(context.Background(), kc, "prod", "web", &out); err != nil {
		t.Fatal(err)
	}
	want := "web\tprod\t" + model.MachineSetTemplateHash(v2) + "\t" + model.MachineSetTemplateHash(v1) + "\tyes\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("history output = %q, want a row %q", out.String(), want)
	}
}

func TestPreviousTemplateState(t *testing.T) {
	_, v1, _ := rolledOutMachineSet()
	b, _ := json.Marshal(v1)
	h := model.MachineSetTemplateHash(v1)
	cases := []struct{ hash, body, want string }{
		{"", "", "-"},
		{h, "", "hash-only"},
		{h, `{"spec":{"image":{"path":"/x"}}}`, "invalid"},
		{h, "not json", "invalid"},
		{h, string(b), "yes"},
	}
	for _, c := range cases {
		if got := previousTemplateState(c.hash, c.body); got != c.want {
			t.Errorf("previousTemplateState(%q, %.20q) = %q, want %q", c.hash, c.body, got, c.want)
		}
	}
}

// TestMachineSetHistoryUnrecordedCurrent: right after an edit, before the
// controller catches up, CURRENT shows the live hash marked unrecorded.
func TestMachineSetHistoryUnrecordedCurrent(t *testing.T) {
	ms, _, _ := rolledOutMachineSet()
	ms.Spec.Template.Spec.Resources.CPU = "8"
	kc, _ := newMachineSetStore(t, ms)
	var out strings.Builder
	if err := machineSetHistory(context.Background(), kc, "prod", "web", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), model.MachineSetTemplateHash(ms.Spec.Template)+" (unrecorded)") {
		t.Fatalf("history output = %q, want the live hash marked (unrecorded)", out.String())
	}
}

// TestMachineSetRollbackCommandUsesRootNamespace runs the real cobra tree:
// -n prod reaches the request path and --dry-run sends no PATCH.
func TestMachineSetRollbackCommandUsesRootNamespace(t *testing.T) {
	ms, _, _ := rolledOutMachineSet()
	kc, store := newMachineSetStore(t, ms)
	t.Setenv("KAIRON_KUBE_URL", kc.BaseURL)
	t.Setenv("KUBECONFIG", "/nonexistent")
	root := NewRootCmd(nil)
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"machineset", "rollback", "web", "-n", "prod", "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (%s)", err, out.String())
	}
	if len(store.patches) != 0 {
		t.Fatalf("--dry-run sent %d PATCH(es)", len(store.patches))
	}
}

func statusFixture(conds []model.Condition) model.MachineSet {
	ms, _, _ := rolledOutMachineSet()
	ms.Metadata.Generation = 5
	ms.Spec.Replicas = 3
	ms.Status = model.MachineSetStatus{
		Replicas:           3,
		ReadyReplicas:      2,
		UpdatedReplicas:    2,
		ObservedGeneration: 5,
		Paused:             false,
		Conditions:         conds,
	}
	return ms
}

// TestWriteMachineSetStatusListsKnownConditionsEvenWhenAbsent: the table
// always has a row for each controller-written type, Unknown/- when missing,
// and AGE comes from lastTransitionTime (or "-" when it was never set).
func TestWriteMachineSetStatusListsKnownConditionsEvenWhenAbsent(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	ms := statusFixture(nil)
	ms.Status.ObservedGeneration = 4
	ms.Status.Paused = true
	ms.Status.Message = "add machineset finalizer: conflict"
	var out strings.Builder
	writeMachineSetStatus(&out, ms, now)
	got := out.String()
	header := "web\tprod\t3\t2\t2\t5\t4\ttrue\n"
	if !strings.Contains(got, "NAME\tNAMESPACE\tDESIRED\tREADY\tUPDATED\tGENERATION\tOBSERVED\tPAUSED\n") || !strings.Contains(got, header) {
		t.Fatalf("header missing:\n%s", got)
	}
	if !strings.Contains(got, "MESSAGE\tadd machineset finalizer: conflict\n") {
		t.Fatalf("status message missing:\n%s", got)
	}
	for _, typ := range []string{"Progressing", "Available", "ReplicaFailure", "Paused", "ScaleDownBlocked"} {
		if !machineSetStatusRowHas(got, typ, "Unknown", "-", "-", "-") {
			t.Fatalf("missing Unknown row for %s in:\n%s", typ, got)
		}
	}
}

func TestWriteMachineSetStatusPrintsConditionsAndAge(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	ms := statusFixture([]model.Condition{
		{Type: "Progressing", Status: "True", Reason: "Updating", Message: "2 of 3 replicas updated", LastTransitionTime: now.Add(-3 * time.Minute)},
		{Type: "Available", Status: "False", Reason: "MinimumReplicasUnavailable", Message: "2 of 3 ready", LastTransitionTime: now.Add(-3 * time.Minute)},
		{Type: "ReplicaFailure", Status: "False", Reason: "FailedReplicas", Message: "0 failed"},
		{Type: "Paused", Status: "False", Reason: "NotPaused", LastTransitionTime: now.Add(-2 * time.Hour)},
		{Type: "ScaleDownBlocked", Status: "True", Reason: "DisruptionBudget", Message: "budget web-mdb would drop available below minAvailable", LastTransitionTime: now.Add(-45 * time.Second)},
		{Type: "CustomOp", Status: "True", Reason: "Example", Message: "extra"},
	})
	var out strings.Builder
	writeMachineSetStatus(&out, ms, now)
	got := out.String()
	if !strings.Contains(got, "web\tprod\t3\t2\t2\t5\t5\tfalse\n") {
		t.Fatalf("header missing:\n%s", got)
	}
	cases := []struct{ typ, status, reason, msg, age string }{
		{"Progressing", "True", "Updating", "2 of 3 replicas updated", "3m"},
		{"Available", "False", "MinimumReplicasUnavailable", "2 of 3 ready", "3m"},
		{"ReplicaFailure", "False", "FailedReplicas", "0 failed", "-"},
		{"Paused", "False", "NotPaused", "-", "2h"},
		{"ScaleDownBlocked", "True", "DisruptionBudget", "budget web-mdb would drop available below minAvailable", "45s"},
		{"CustomOp", "True", "Example", "extra", "-"},
	}
	for _, c := range cases {
		if !machineSetStatusRowHas(got, c.typ, c.status, c.reason, c.msg, c.age) {
			t.Fatalf("missing row %s %s %s %s %s in:\n%s", c.typ, c.status, c.reason, c.msg, c.age, got)
		}
	}
}

func TestMachineSetStatusFetchesFromAPI(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	ms := statusFixture([]model.Condition{
		{Type: "Available", Status: "True", Reason: "MinimumReplicasAvailable", Message: "3 ready", LastTransitionTime: now.Add(-time.Hour)},
	})
	ms.Status.ReadyReplicas = 3
	ms.Status.UpdatedReplicas = 3
	kc, _ := newMachineSetStore(t, ms)
	var out strings.Builder
	if err := machineSetStatus(context.Background(), kc, "prod", "web", &out, now); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "web\tprod\t3\t3\t3\t5\t5\tfalse\n") {
		t.Fatalf("header missing:\n%s", got)
	}
	if !machineSetStatusRowHas(got, "Available", "True", "MinimumReplicasAvailable", "3 ready", "1h") {
		t.Fatalf("Available row missing:\n%s", got)
	}
	if !machineSetStatusRowHas(got, "Progressing", "Unknown", "-", "-", "-") {
		t.Fatalf("absent Progressing should be Unknown:\n%s", got)
	}
}

// TestMachineSetStatusCommandUsesRootNamespace runs the real cobra tree:
// -n prod reaches the request path used by the fake apiserver.
func TestMachineSetStatusCommandUsesRootNamespace(t *testing.T) {
	ms := statusFixture(nil)
	kc, _ := newMachineSetStore(t, ms)
	t.Setenv("KAIRON_KUBE_URL", kc.BaseURL)
	t.Setenv("KUBECONFIG", "/nonexistent")
	root := NewRootCmd(nil)
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"machineset", "status", "web", "-n", "prod"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (%s)", err, out.String())
	}
	if !strings.Contains(out.String(), "web\tprod\t3\t2\t2\t5\t5\tfalse\n") {
		t.Fatalf("command output = %q", out.String())
	}
}

func TestMachineSetStatusNotFound(t *testing.T) {
	ms := statusFixture(nil)
	kc, _ := newMachineSetStore(t, ms)
	var out strings.Builder
	err := machineSetStatus(context.Background(), kc, "prod", "missing", &out, time.Time{})
	if err == nil {
		t.Fatalf("expected a not-found error, got output %q", out.String())
	}
	if out.Len() != 0 {
		t.Fatalf("wrote output before the error: %q", out.String())
	}
	// Wrong namespace is the same 404: the client path includes the namespace.
	out.Reset()
	if err := machineSetStatus(context.Background(), kc, "other", "web", &out, time.Time{}); err == nil {
		t.Fatalf("expected a not-found error for the wrong namespace, got output %q", out.String())
	}
}

func machineSetStatusRowHas(out, typ, status, reason, msg, age string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != typ {
			continue
		}
		// MESSAGE may contain spaces, so the last field is AGE and the
		// middle is STATUS, REASON, then the remainder joined as MESSAGE.
		if len(fields) < 5 {
			return false
		}
		gotStatus, gotReason, gotAge := fields[1], fields[2], fields[len(fields)-1]
		gotMsg := strings.Join(fields[3:len(fields)-1], " ")
		return gotStatus == status && gotReason == reason && gotMsg == msg && gotAge == age
	}
	return false
}
