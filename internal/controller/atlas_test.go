// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	atlas "github.com/zyvorai/atlas/clients/go"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type fakeAtlas struct {
	mu        sync.Mutex
	creates   []map[string]any
	deletes   []string
	jobPolls  int
	jobDoneAt int
	createErr int
	// delStates, when non-nil, makes deletes async; each poll of the delete
	// job pops the next state.
	delStates []string
}

func (f *fakeAtlas) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, atlas.APIPrefix)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && (p == "/volumes" || p == "/rbd-images"):
			if f.createErr != 0 {
				w.WriteHeader(f.createErr)
				_, _ = io.WriteString(w, `{"error":{"code":"invalid_request","message":"bad name"}}`)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.creates = append(f.creates, body)
			name := body["name"].(string)
			res := map[string]any{"volume_id": "vol-" + name, "pvc": name}
			if p == "/rbd-images" {
				res = map[string]any{"volume_id": "vol_rbd_" + name, "rbd": "rbd/" + name}
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"job_id": "job-1", "state": "queued", "resource": res})
		case r.Method == http.MethodGet && p == "/jobs/job-1":
			f.jobPolls++
			state := "running"
			if f.jobPolls >= f.jobDoneAt {
				state = "succeeded"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-1", "state": state})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/volumes/vol_rbd_"):
			image := strings.TrimPrefix(p, "/volumes/vol_rbd_")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "vol_rbd_" + image, "backend_native_id": "rbd:rbd/" + image})
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/volumes/"):
			id := strings.TrimPrefix(p, "/volumes/")
			pvc := strings.TrimPrefix(id, "vol-")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "pvc_name": pvc, "backend_native_id": "pvc:" + pvc})
		case r.Method == http.MethodGet && p == "/jobs/job-del":
			state := "running"
			if len(f.delStates) > 0 {
				state, f.delStates = f.delStates[0], f.delStates[1:]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-del", "state": state, "error": "ceph busy"})
		case r.Method == http.MethodDelete:
			f.deletes = append(f.deletes, p)
			if f.delStates != nil {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{"job_id": "job-del", "state": "queued", "resource": map[string]any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true})
		default:
			t.Errorf("unexpected atlas call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}
}

type fakeKubeMachines struct {
	mu       sync.Mutex
	patches  []map[string]any
	statuses []model.MachineStatus
}

func (f *fakeKubeMachines) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/status"):
			var body struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.statuses = append(f.statuses, body.Status)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/machines/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.patches = append(f.patches, body)
		default:
			t.Errorf("unexpected kube call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
		w.WriteHeader(http.StatusOK)
	}
}

var rootName = model.AtlasVolumeName("default", "web", "root")

func newAtlasTestController(t *testing.T, fa *fakeAtlas, fk *fakeKubeMachines) *Controller {
	t.Helper()
	ks := httptest.NewServer(fk.handler(t))
	t.Cleanup(ks.Close)
	as := httptest.NewServer(fa.handler(t))
	t.Cleanup(as.Close)
	kc, err := kube.New(ks.URL, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = ks.Client()
	ac, err := atlas.New(as.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Controller{
		Kube:  kc,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Atlas: AtlasConfig{Client: ac, TenantID: "kairon", DefaultPolicy: "gold"},
	}
}

func atlasMachine(vols ...model.MachineVolume) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-1"},
		Spec:     model.MachineSpec{Volumes: vols},
	}
}

func TestAtlasPVCVolumeLifecycle(t *testing.T) {
	fa := &fakeAtlas{jobDoneAt: 2}
	fk := &fakeKubeMachines{}
	ctl := newAtlasTestController(t, fa, fk)
	ctx := context.Background()
	machines := []model.Machine{atlasMachine(model.MachineVolume{
		Name: "root", Atlas: &model.AtlasVolumeSource{Size: "10Gi"},
	})}

	machines, notReady := ctl.reconcileAtlasVolumes(ctx, machines)
	if notReady["default/web"] == "" {
		t.Fatal("machine schedulable before its atlas volume exists")
	}
	if len(fa.creates) != 1 {
		t.Fatalf("creates=%v", fa.creates)
	}
	req := fa.creates[0]
	if req["name"] != rootName || req["tenant_id"] != "kairon" || req["policy"] != "gold" || req["size_bytes"].(float64) != 10<<30 {
		t.Fatalf("create request=%v", req)
	}
	if owner := req["owner"].(map[string]any); owner["role"] != "root_disk" || owner["product"] != "kairon" {
		t.Fatalf("owner=%v", owner)
	}
	if !model.HasFinalizerList(machines[0].Metadata.Finalizers, model.FinalizerAtlasVolumes) {
		t.Fatal("atlas finalizer not added")
	}
	if st := model.AtlasVolumeStates(machines[0])["root"]; st.Phase != model.AtlasPhaseProvisioning || st.JobID != "job-1" {
		t.Fatalf("state after create=%+v", st)
	}
	if len(fk.statuses) != 1 || fk.statuses[0].Phase != "Pending" {
		t.Fatalf("statuses=%+v", fk.statuses)
	}

	machines, notReady = ctl.reconcileAtlasVolumes(ctx, machines)
	if notReady["default/web"] == "" || len(fa.creates) != 1 {
		t.Fatalf("running job should keep waiting without re-creating: notReady=%v creates=%d", notReady, len(fa.creates))
	}

	machines, notReady = ctl.reconcileAtlasVolumes(ctx, machines)
	if reason := notReady["default/web"]; reason != "" {
		t.Fatalf("still not ready: %s", reason)
	}
	if got := machines[0].Spec.Volumes[0].ClaimName; got != rootName {
		t.Fatalf("claimName=%q", got)
	}
	if st := model.AtlasVolumeStates(machines[0])["root"]; st.Phase != model.AtlasPhaseReady || st.NativeID != "pvc:"+rootName {
		t.Fatalf("ready state=%+v", st)
	}

	now := time.Now()
	machines[0].Metadata.DeletionTimestamp = &now
	machines[0].Metadata.Finalizers = append(machines[0].Metadata.Finalizers, model.Finalizer)
	machines, _ = ctl.reconcileAtlasVolumes(ctx, machines)
	if len(fa.deletes) != 0 {
		t.Fatalf("deleted while node still holds runtime-cleanup: %v", fa.deletes)
	}

	machines[0].Metadata.Finalizers = model.RemoveFinalizer(machines[0].Metadata.Finalizers, model.Finalizer)
	machines, _ = ctl.reconcileAtlasVolumes(ctx, machines)
	if len(fa.deletes) != 1 || fa.deletes[0] != "/volumes/vol-"+rootName {
		t.Fatalf("deletes=%v", fa.deletes)
	}
	if model.HasFinalizerList(machines[0].Metadata.Finalizers, model.FinalizerAtlasVolumes) {
		t.Fatal("atlas finalizer not removed")
	}
}

func TestAtlasRBDVolumeAndRetain(t *testing.T) {
	fa := &fakeAtlas{jobDoneAt: 1}
	fk := &fakeKubeMachines{}
	ctl := newAtlasTestController(t, fa, fk)
	ctx := context.Background()
	machines := []model.Machine{atlasMachine(
		model.MachineVolume{Name: "root", Atlas: &model.AtlasVolumeSource{Size: "20G", Mode: model.AtlasModeRBD, Pool: "rbd"}},
		model.MachineVolume{Name: "data", Atlas: &model.AtlasVolumeSource{Size: "1Gi", Retain: true}},
	)}
	machines, _ = ctl.reconcileAtlasVolumes(ctx, machines)
	machines, notReady := ctl.reconcileAtlasVolumes(ctx, machines)
	if reason := notReady["default/web"]; reason != "" {
		t.Fatalf("not ready: %s", reason)
	}
	states := model.AtlasVolumeStates(machines[0])
	if st := states["root"]; st.NativeID != "rbd:rbd/"+rootName || st.Mode != model.AtlasModeRBD {
		t.Fatalf("rbd state=%+v", st)
	}
	if machines[0].Spec.Volumes[0].ClaimName != "" {
		t.Fatal("rbd volume must not get a claimName")
	}
	now := time.Now()
	machines[0].Metadata.DeletionTimestamp = &now
	_, _ = ctl.reconcileAtlasVolumes(ctx, machines)
	if len(fa.deletes) != 1 || fa.deletes[0] != "/rbd-images/rbd/"+rootName {
		t.Fatalf("deletes=%v (data volume is retained)", fa.deletes)
	}
}

func TestAtlasAsyncDeleteHoldsFinalizerUntilJobSucceeds(t *testing.T) {
	fa := &fakeAtlas{jobDoneAt: 1}
	ctl := newAtlasTestController(t, fa, &fakeKubeMachines{})
	ctx := context.Background()
	machines := []model.Machine{atlasMachine(model.MachineVolume{Name: "root", Atlas: &model.AtlasVolumeSource{Size: "1Gi"}})}
	machines, _ = ctl.reconcileAtlasVolumes(ctx, machines)
	machines, _ = ctl.reconcileAtlasVolumes(ctx, machines)
	if st := model.AtlasVolumeStates(machines[0])["root"]; st.Phase != model.AtlasPhaseReady {
		t.Fatalf("state=%+v", st)
	}

	fa.delStates = []string{"failed", "running", "succeeded"}
	now := time.Now()
	machines[0].Metadata.DeletionTimestamp = &now
	held := func() bool {
		return model.HasFinalizerList(machines[0].Metadata.Finalizers, model.FinalizerAtlasVolumes)
	}

	machines, _ = ctl.reconcileAtlasVolumes(ctx, machines) // issue delete
	if !held() || model.AtlasVolumeStates(machines[0])["root"].Phase != model.AtlasPhaseDeleting {
		t.Fatal("finalizer released before delete job finished")
	}
	machines, _ = ctl.reconcileAtlasVolumes(ctx, machines) // job failed
	if !held() || !strings.Contains(model.AtlasVolumeStates(machines[0])["root"].Message, "ceph busy") {
		t.Fatalf("failed delete must keep finalizer and record error: %+v", model.AtlasVolumeStates(machines[0])["root"])
	}
	machines, _ = ctl.reconcileAtlasVolumes(ctx, machines) // re-issue
	if len(fa.deletes) != 2 {
		t.Fatalf("failed delete not retried: %v", fa.deletes)
	}
	machines, _ = ctl.reconcileAtlasVolumes(ctx, machines) // running
	if !held() {
		t.Fatal("released while delete job running")
	}
	machines, _ = ctl.reconcileAtlasVolumes(ctx, machines) // succeeded
	if held() {
		t.Fatal("finalizer not released after delete job succeeded")
	}
}

func TestAtlasClientErrorMarksVolumeFailed(t *testing.T) {
	fa := &fakeAtlas{createErr: http.StatusBadRequest}
	ctl := newAtlasTestController(t, fa, &fakeKubeMachines{})
	machines := []model.Machine{atlasMachine(model.MachineVolume{Name: "root", Atlas: &model.AtlasVolumeSource{Size: "1Gi"}})}
	machines, notReady := ctl.reconcileAtlasVolumes(context.Background(), machines)
	if !strings.Contains(notReady["default/web"], "provisioning failed") {
		t.Fatalf("reason=%q", notReady["default/web"])
	}
	if st := model.AtlasVolumeStates(machines[0])["root"]; st.Phase != model.AtlasPhaseFailed {
		t.Fatalf("state=%+v", st)
	}
}

func TestAtlasDisabledKeepsMachinePending(t *testing.T) {
	ctl := &Controller{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	machines := []model.Machine{atlasMachine(model.MachineVolume{Name: "root", Atlas: &model.AtlasVolumeSource{Size: "1Gi"}})}
	_, notReady := ctl.reconcileAtlasVolumes(context.Background(), machines)
	if !strings.Contains(notReady["default/web"], "--atlas-url") {
		t.Fatalf("reason=%q", notReady["default/web"])
	}
}

func TestAtlasValidation(t *testing.T) {
	for _, tc := range []struct {
		vol  model.MachineVolume
		want string
	}{
		{model.MachineVolume{Name: "a", Atlas: &model.AtlasVolumeSource{Size: "lots"}}, "atlas.size"},
		{model.MachineVolume{Name: "a", Atlas: &model.AtlasVolumeSource{Size: "1Gi", Mode: "nfs"}}, "must be pvc or rbd"},
		{model.MachineVolume{Name: "a", ClaimName: "mine", Atlas: &model.AtlasVolumeSource{Size: "1Gi"}}, "claimName must be empty"},
		{model.MachineVolume{Name: "a", ClaimName: "mine", Atlas: &model.AtlasVolumeSource{Size: "1Gi", Mode: "rbd"}}, "not used with atlas.mode=rbd"},
	} {
		err := validateAtlasVolume(0, tc.vol, model.AtlasVolumeState{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err=%v want %q", tc.vol, err, tc.want)
		}
	}
	for _, tc := range []struct {
		vol  model.MachineVolume
		want string
	}{
		{model.MachineVolume{Name: "d", Atlas: &model.AtlasVolumeSource{Size: "1Gi", Mode: "rbd"}}, "only for the boot disk"},
	} {
		err := validateAtlasVolume(1, tc.vol, model.AtlasVolumeState{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err=%v want %q", tc.vol, err, tc.want)
		}
	}
}

// TestAtlasContract drives the reconciler against a live atlas-gateway
// (fake Ceph drivers are enough for rbd mode):
//
//	KAIRON_ATLAS_CONTRACT_URL=http://127.0.0.1:5110 go test ./internal/controller -run AtlasContract
func TestAtlasContract(t *testing.T) {
	base := os.Getenv("KAIRON_ATLAS_CONTRACT_URL")
	if base == "" {
		t.Skip("KAIRON_ATLAS_CONTRACT_URL not set")
	}
	fk := &fakeKubeMachines{}
	ks := httptest.NewServer(fk.handler(t))
	defer ks.Close()
	kc, err := kube.New(ks.URL, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = ks.Client()
	ac, err := atlas.New(base, atlas.WithToken(os.Getenv("KAIRON_ATLAS_CONTRACT_TOKEN")))
	if err != nil {
		t.Fatal(err)
	}
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Atlas: AtlasConfig{Client: ac, TenantID: "kairon"}}
	m := atlasMachine(model.MachineVolume{Name: "root", Atlas: &model.AtlasVolumeSource{Size: "64Mi", Mode: model.AtlasModeRBD}})
	m.Metadata.Name = fmt.Sprintf("contract-%d", time.Now().UnixNano())
	machines := []model.Machine{m}
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var notReady map[string]string
		machines, notReady = ctl.reconcileAtlasVolumes(ctx, machines)
		if notReady["default/"+m.Metadata.Name] == "" {
			break
		}
		if st := model.AtlasVolumeStates(machines[0])["root"]; st.Phase == model.AtlasPhaseFailed || time.Now().After(deadline) {
			t.Fatalf("not ready: %s (state %+v)", notReady["default/"+m.Metadata.Name], st)
		}
		time.Sleep(500 * time.Millisecond)
	}
	st := model.AtlasVolumeStates(machines[0])["root"]
	pool, image, ok := atlas.ParseRBD(st.NativeID)
	if !ok || image != model.AtlasVolumeName("default", m.Metadata.Name, "root") {
		t.Fatalf("state=%+v pool=%q image=%q", st, pool, image)
	}
	now := time.Now()
	machines[0].Metadata.DeletionTimestamp = &now
	for model.HasFinalizerList(machines[0].Metadata.Finalizers, model.FinalizerAtlasVolumes) {
		if time.Now().After(deadline) {
			t.Fatalf("finalizer not released; state %+v", model.AtlasVolumeStates(machines[0])["root"])
		}
		machines, _ = ctl.reconcileAtlasVolumes(ctx, machines)
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := ac.GetVolume(ctx, st.VolumeID); !atlas.IsNotFound(err) {
		t.Fatalf("volume %s still present after release: %v", st.VolumeID, err)
	}
}

func TestAtlasVolumeName(t *testing.T) {
	n := model.AtlasVolumeName("default", "web", "root")
	if !strings.HasPrefix(n, "web-root-") || len(n) != len("web-root-")+10 {
		t.Fatalf("got %q", n)
	}
	if model.AtlasVolumeName("a-b", "c", "root") == model.AtlasVolumeName("a", "b-c", "root") {
		t.Fatal("namespace/machine split collided")
	}
	long := model.AtlasVolumeName("team-a", strings.Repeat("M", 80), "Data_1")
	if len(long) > 63 || strings.ToLower(long) != long || strings.ContainsAny(long, "_") {
		t.Fatalf("bad name %q", long)
	}
}
