# AI tests (kairon)

_Generated 2026-10-20 by claude-opus-5-5 from a snapshot of tracked sources; verify before acting._

# Unit tests for the 8 most valuable untested units in kairon

## Selection

I picked these by risk: wire formats, concurrency safety, admission and security gates, and metric correctness. For every unit, the full implementation is visible in the dump and no existing test covers it.

| # | Unit | Why it matters |
|---|---|---|
| 1 | `ebpfedge.DropCounters` | Turns raw dataplane totals into Prometheus increments. A counter reset, a stale baseline or aliasing the caller's map would silently corrupt drop metrics. |
| 2 | `model.SetCondition` / `FindCondition` / `MachineStatusEqualIgnoringVolatile` | Used on every reconcile. `LastTransitionTime` semantics and "don't write etcd for usage-only changes" are easy to regress. |
| 3 | `model.MicroTime` | Its doc says Lease writes 400 against a real API server unless the fractional seconds are exactly 6 digits. Leader election depends on it. |
| 4 | `kube.ConsumeAnnotation` / `SetAnnotation` / `GetAnnotations` | The one-shot MCP approval relies on an atomic JSON-Patch test+remove and on mapping 422 to `ErrAnnotationChanged`. |
| 5 | `agentplane.AdmitConfidential` / `ProjectConfidential` | Decides whether a Machine is reported "sealed" (SEV-SNP/TDX). It must fail closed. |
| 6 | `agentplane.Record` / `Replay` / `Log` | Audit IDs must be deterministic and outcome-sensitive. The replay must not alias stored events, and `Log` must be race-free. |
| 7 | `agentplane.AdmitImage` / `AdmitGPU` / `BindGateway` | Agent-pool admission shape checks: cosign digest/signature, a refused GPU live-migration, and port/protocol bounds. |
| 8 | `web/src/lib/route.ts` `parseHash` / `formatHash` | All dashboard deep links (insights, inspector targets) go through it. Encoding round-trips must hold. |

---

## `internal/ebpfedge/dropcounters_test.go`

```go
// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package ebpfedge

import (
	"reflect"
	"testing"
)

const (
	webPrefix = "demo/web/"
	dbPrefix  = "demo/db/"
)

func TestDropCountersFirstObservationReportsFullValue(t *testing.T) {
	var c DropCounters // zero value must be usable
	got := c.Observe(webPrefix, map[string]uint64{webPrefix + "sni_deny": 5, webPrefix + "spoof_mac": 2})
	want := map[string]uint64{webPrefix + "sni_deny": 5, webPrefix + "spoof_mac": 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("deltas = %v, want %v", got, want)
	}
}

func TestDropCountersReportsIncrementsAndNothingWhenUnchanged(t *testing.T) {
	var c DropCounters
	c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 5})
	if got := c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 8}); !reflect.DeepEqual(got, map[string]uint64{webPrefix + "a": 3}) {
		t.Fatalf("increment deltas = %v, want a:3", got)
	}
	got := c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 8})
	if got == nil || len(got) != 0 {
		t.Fatalf("unchanged counter: deltas = %#v, want empty non-nil map", got)
	}
}

func TestDropCountersTreatsDecreaseAsReset(t *testing.T) {
	var c DropCounters
	c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 100})
	// The dataplane reset or recreated the flow: the new value is all new drops.
	if got := c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 2}); !reflect.DeepEqual(got, map[string]uint64{webPrefix + "a": 2}) {
		t.Fatalf("reset deltas = %v, want a:2", got)
	}
	// The reset value becomes the new baseline.
	if got := c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 5}); !reflect.DeepEqual(got, map[string]uint64{webPrefix + "a": 3}) {
		t.Fatalf("post-reset deltas = %v, want a:3", got)
	}
}

func TestDropCountersIgnoresKeysOutsideThePrefix(t *testing.T) {
	var c DropCounters
	got := c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 1, dbPrefix + "x": 9})
	if !reflect.DeepEqual(got, map[string]uint64{webPrefix + "a": 1}) {
		t.Fatalf("deltas = %v, want only the web key", got)
	}
	if _, stored := c.byMachine[webPrefix][dbPrefix+"x"]; stored {
		t.Fatal("a foreign Machine's key was stored under this Machine's baseline")
	}
	// Observing only foreign keys must not leave an empty entry behind.
	c.Observe(dbPrefix+"other/", map[string]uint64{webPrefix + "a": 1})
	if _, ok := c.byMachine[dbPrefix+"other/"]; ok {
		t.Fatal("empty baseline retained for a Machine with no matching keys")
	}
}

func TestDropCountersForgetsVanishedKeys(t *testing.T) {
	var c DropCounters
	c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 5, webPrefix + "b": 3})
	if got := c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 5}); len(got) != 0 {
		t.Fatalf("deltas = %v, want none", got)
	}
	// b was evicted from the snapshot; when it reappears it is a fresh flow.
	if got := c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 5, webPrefix + "b": 3}); !reflect.DeepEqual(got, map[string]uint64{webPrefix + "b": 3}) {
		t.Fatalf("reappeared deltas = %v, want b:3", got)
	}
}

func TestDropCountersCopiesTheInputMap(t *testing.T) {
	var c DropCounters
	totals := map[string]uint64{webPrefix + "a": 5}
	c.Observe(webPrefix, totals)
	totals[webPrefix+"a"] = 100 // caller mutation after Observe
	if got := c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 5}); len(got) != 0 {
		t.Fatalf("baseline aliased the caller's map: deltas = %v", got)
	}
}

func TestDropCountersMachinesAreIndependentAndPruned(t *testing.T) {
	var c DropCounters
	c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 5})
	c.Observe(dbPrefix, map[string]uint64{dbPrefix + "x": 2})

	c.Prune(map[string]bool{webPrefix: true})

	if got := c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 5}); len(got) != 0 {
		t.Fatalf("active Machine lost its baseline: %v", got)
	}
	if got := c.Observe(dbPrefix, map[string]uint64{dbPrefix + "x": 2}); !reflect.DeepEqual(got, map[string]uint64{dbPrefix + "x": 2}) {
		t.Fatalf("pruned Machine kept its baseline: %v", got)
	}
}

func TestDropCountersEmptySnapshotDropsMachine(t *testing.T) {
	var c DropCounters
	c.Observe(webPrefix, map[string]uint64{webPrefix + "a": 5})
	c.Observe(webPrefix, map[string]uint64{})
	if _, ok := c.byMachine[webPrefix]; ok {
		t.Fatal("Machine with an empty snapshot still tracked")
	}
}
```

---

## `internal/model/conditions_test.go`

```go
// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"testing"
	"time"
)

func TestFindCondition(t *testing.T) {
	conds := []Condition{{Type: "Scheduled", Status: "True"}, {Type: "Ready", Status: "False"}, {Type: "Ready", Status: "True"}}
	got, ok := FindCondition(conds, "Ready")
	if !ok || got.Status != "False" {
		t.Fatalf("got %+v ok=%v, want the first Ready (False)", got, ok)
	}
	if _, ok := FindCondition(conds, "Migrating"); ok {
		t.Fatal("found a condition that is not there")
	}
	if _, ok := FindCondition(nil, "Ready"); ok {
		t.Fatal("found a condition in a nil list")
	}
}

func TestSetConditionAppendsNewTypeWithUTCTransitionTime(t *testing.T) {
	before := time.Now().UTC()
	out := SetCondition(nil, Condition{Type: "Ready", Status: "True", Reason: "Booted"})
	if len(out) != 1 || out[0].Type != "Ready" {
		t.Fatalf("out = %+v", out)
	}
	ltt := out[0].LastTransitionTime
	if ltt.IsZero() || ltt.Before(before) || ltt.Location() != time.UTC {
		t.Fatalf("LastTransitionTime = %v, want a UTC time at or after %v", ltt, before)
	}
}

func TestSetConditionKeepsTransitionTimeWhenUnchanged(t *testing.T) {
	prev := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	conds := []Condition{{Type: "Ready", Status: "True", Reason: "Booted", Message: "ok", LastTransitionTime: prev}}
	// Even a caller-supplied time must not move an unchanged condition.
	out := SetCondition(conds, Condition{Type: "Ready", Status: "True", Reason: "Booted", Message: "ok", LastTransitionTime: prev.Add(time.Hour)})
	if !out[0].LastTransitionTime.Equal(prev) {
		t.Fatalf("LastTransitionTime = %v, want unchanged %v", out[0].LastTransitionTime, prev)
	}
}

func TestSetConditionBumpsTransitionTimeOnAnyChange(t *testing.T) {
	prev := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := Condition{Type: "Ready", Status: "True", Reason: "Booted", Message: "ok", LastTransitionTime: prev}
	for name, next := range map[string]Condition{
		"status":  {Type: "Ready", Status: "False", Reason: "Booted", Message: "ok"},
		"reason":  {Type: "Ready", Status: "True", Reason: "Resumed", Message: "ok"},
		"message": {Type: "Ready", Status: "True", Reason: "Booted", Message: "changed"},
	} {
		out := SetCondition([]Condition{base}, next)
		if !out[0].LastTransitionTime.After(prev) {
			t.Errorf("%s change: LastTransitionTime = %v, want bumped past %v", name, out[0].LastTransitionTime, prev)
		}
	}
}

func TestSetConditionHonoursExplicitTimeOnChange(t *testing.T) {
	prev := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	out := SetCondition([]Condition{{Type: "Ready", Status: "True", LastTransitionTime: prev}},
		Condition{Type: "Ready", Status: "False", LastTransitionTime: at})
	if !out[0].LastTransitionTime.Equal(at) {
		t.Fatalf("LastTransitionTime = %v, want caller-supplied %v", out[0].LastTransitionTime, at)
	}
}

func TestSetConditionReplacesInPlaceWithoutMutatingInput(t *testing.T) {
	conds := []Condition{{Type: "Scheduled", Status: "True"}, {Type: "Ready", Status: "True"}, {Type: "Migrating", Status: "False"}}
	out := SetCondition(conds, Condition{Type: "Ready", Status: "False", Reason: "Crashed"})
	if len(out) != 3 || out[0].Type != "Scheduled" || out[1].Type != "Ready" || out[2].Type != "Migrating" {
		t.Fatalf("order changed: %+v", out)
	}
	if out[1].Status != "False" || out[1].Reason != "Crashed" {
		t.Fatalf("Ready not replaced: %+v", out[1])
	}
	if conds[1].Status != "True" {
		t.Fatal("SetCondition mutated its input slice")
	}
}

func TestMachineStatusEqualIgnoringVolatile(t *testing.T) {
	a := MachineStatus{Phase: "Running", ResourceUsage: &ResourceUsage{CPUPercent: 1}}
	b := MachineStatus{Phase: "Running", ResourceUsage: &ResourceUsage{CPUPercent: 99}}
	if !MachineStatusEqualIgnoringVolatile(a, b) {
		t.Fatal("usage-only difference must compare equal")
	}
	if !MachineStatusEqualIgnoringVolatile(a, MachineStatus{Phase: "Running"}) {
		t.Fatal("nil vs set usage must compare equal")
	}
	if MachineStatusEqualIgnoringVolatile(a, MachineStatus{Phase: "Stopped", ResourceUsage: a.ResourceUsage}) {
		t.Fatal("phase difference must compare unequal")
	}
	if a.ResourceUsage == nil || b.ResourceUsage == nil {
		t.Fatal("comparison cleared the callers' ResourceUsage")
	}
}
```

---

## `internal/model/microtime_test.go`

```go
// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMicroTimeMarshalUsesSixFractionalDigitsInUTC(t *testing.T) {
	for name, tc := range map[string]struct {
		in   time.Time
		want string
	}{
		"micros":          {time.Date(2026, 9, 14, 14, 50, 38, 85612000, time.UTC), `"2026-09-14T14:50:38.085612Z"`},
		"whole second":    {time.Date(2026, 9, 14, 14, 50, 38, 0, time.UTC), `"2026-09-14T14:50:38.000000Z"`},
		"nanos truncated": {time.Date(2026, 9, 14, 14, 50, 38, 85612999, time.UTC), `"2026-09-14T14:50:38.085612Z"`},
		"non-UTC zone":    {time.Date(2026, 9, 14, 16, 50, 38, 1000, time.FixedZone("CEST", 2*3600)), `"2026-09-14T14:50:38.000001Z"`},
	} {
		got, err := json.Marshal(NewMicroTime(tc.in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: got %s, want %s", name, got, tc.want)
		}
	}
}

func TestMicroTimeMarshalInsideStruct(t *testing.T) {
	type lease struct {
		RenewTime MicroTime `json:"renewTime"`
	}
	got, err := json.Marshal(lease{RenewTime: NewMicroTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"renewTime":"2026-01-02T03:04:05.000000Z"}` {
		t.Fatalf("got %s", got)
	}
}

func TestMicroTimeUnmarshalRoundTripsAndAcceptsVariants(t *testing.T) {
	at := time.Date(2026, 9, 14, 14, 50, 38, 85612000, time.UTC)
	raw, _ := json.Marshal(NewMicroTime(at))
	var mt MicroTime
	if err := json.Unmarshal(raw, &mt); err != nil || !mt.Equal(at) {
		t.Fatalf("round trip: %v, %v", mt, err)
	}
	for _, in := range []string{`"2026-09-14T14:50:38Z"`, `"2026-09-14T14:50:38.085612345Z"`, `"2026-09-14T16:50:38.1+02:00"`} {
		var v MicroTime
		if err := json.Unmarshal([]byte(in), &v); err != nil || v.IsZero() {
			t.Errorf("%s: %v, %v", in, v, err)
		}
	}
}

func TestMicroTimeUnmarshalEmptyAndInvalid(t *testing.T) {
	v := NewMicroTime(time.Now())
	if err := json.Unmarshal([]byte(`""`), &v); err != nil || !v.IsZero() {
		t.Fatalf("empty string: %v, %v; want zero time", v, err)
	}
	for _, bad := range []string{`"not-a-time"`, `123`, `"2026-13-01T00:00:00Z"`} {
		var x MicroTime
		if err := json.Unmarshal([]byte(bad), &x); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}
```

---

## `internal/kube/annotations_test.go`

```go
// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recordedRequest struct {
	method, path, contentType string
	body                      []byte
}

func annotationServer(t *testing.T, status int, resp string) (*Client, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method, rec.path, rec.contentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		rec.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.HTTP.CloseIdleConnections)
	return c, rec
}

func TestConsumeAnnotationSendsAtomicTestAndRemove(t *testing.T) {
	c, rec := annotationServer(t, http.StatusOK, `{}`)
	if err := c.ConsumeAnnotation(context.Background(), "machines", "prod", "web", "kairon.zyvor.dev/mcp-approval", "tok-1"); err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPatch || !strings.HasSuffix(rec.path, "/namespaces/prod/machines/web") {
		t.Fatalf("request %s %s", rec.method, rec.path)
	}
	if !strings.HasPrefix(rec.contentType, "application/json-patch+json") {
		t.Fatalf("content type %q", rec.contentType)
	}
	var ops []map[string]any
	if err := json.Unmarshal(rec.body, &ops); err != nil {
		t.Fatalf("body %s: %v", rec.body, err)
	}
	const path = "/metadata/annotations/kairon.zyvor.dev~1mcp-approval"
	if len(ops) != 2 ||
		ops[0]["op"] != "test" || ops[0]["path"] != path || ops[0]["value"] != "tok-1" ||
		ops[1]["op"] != "remove" || ops[1]["path"] != path {
		t.Fatalf("patch = %s", rec.body)
	}
}

func TestConsumeAnnotationEscapesJSONPointer(t *testing.T) {
	c, rec := annotationServer(t, http.StatusOK, `{}`)
	if err := c.ConsumeAnnotation(context.Background(), "machines", "prod", "web", "a~b/c", "v"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rec.body), `"/metadata/annotations/a~0b~1c"`) {
		t.Fatalf("JSON pointer not RFC 6901 escaped: %s", rec.body)
	}
}

func TestConsumeAnnotationMapsFailedTestTo422Sentinel(t *testing.T) {
	c, _ := annotationServer(t, http.StatusUnprocessableEntity, `{"kind":"Status","code":422}`)
	err := c.ConsumeAnnotation(context.Background(), "machines", "prod", "web", "k", "v")
	if !errors.Is(err, ErrAnnotationChanged) {
		t.Fatalf("err = %v, want ErrAnnotationChanged", err)
	}
}

func TestConsumeAnnotationPassesThroughOtherErrors(t *testing.T) {
	c, _ := annotationServer(t, http.StatusInternalServerError, `{"kind":"Status","code":500}`)
	err := c.ConsumeAnnotation(context.Background(), "machines", "prod", "web", "k", "v")
	if err == nil || errors.Is(err, ErrAnnotationChanged) {
		t.Fatalf("err = %v, want a non-sentinel error", err)
	}
	c, _ = annotationServer(t, http.StatusNotFound, `{"kind":"Status","code":404}`)
	if err := c.ConsumeAnnotation(context.Background(), "machines", "prod", "gone", "k", "v"); !IsNotFound(err) {
		t.Fatalf("404: err = %v, want IsNotFound", err)
	}
}

func TestSetAnnotationMergePatches(t *testing.T) {
	c, rec := annotationServer(t, http.StatusOK, `{}`)
	if err := c.SetAnnotation(context.Background(), "machines", "prod", "web", "kairon.zyvor.dev/evacuate", "true"); err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPatch || !strings.HasPrefix(rec
