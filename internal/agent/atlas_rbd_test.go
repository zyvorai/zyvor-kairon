// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func atlasRBDMachine(nativeID, phase string) model.Machine {
	states, _ := json.Marshal(map[string]model.AtlasVolumeState{
		"root": {VolumeID: "vol_x", NativeID: nativeID, Mode: model.AtlasModeRBD, Phase: phase},
	})
	return model.Machine{
		Metadata: model.ObjectMeta{
			Name: "db", Namespace: "prod", Finalizers: []string{model.Finalizer},
			Annotations: map[string]string{model.AnnotationAtlasVolumes: string(states)},
		},
		Spec: model.MachineSpec{
			NodeName:   "worker-1",
			Resources:  model.ResourceSpec{CPU: "2", Memory: "2Gi"},
			PowerState: "Running",
			Volumes: []model.MachineVolume{{
				Name: "root", Atlas: &model.AtlasVolumeSource{Size: "20Gi", Mode: model.AtlasModeRBD},
			}},
		},
	}
}

func TestResolveAtlasRBDBoot(t *testing.T) {
	own := model.AtlasVolumeName("prod", "db", "root")
	a := &Agent{NodeName: "worker-1", AtlasRBDPools: []string{"rbd"}}
	got, ok, err := a.resolveAtlasRBDBoot(atlasRBDMachine("rbd:rbd/"+own, model.AtlasPhaseReady))
	if err != nil || !ok || got != "rbd/"+own {
		t.Fatalf("got %q ok=%v err=%v", got, ok, err)
	}

	for name, tc := range map[string]struct {
		agent *Agent
		m     model.Machine
		want  string
	}{
		"disabled":      {&Agent{NodeName: "worker-1"}, atlasRBDMachine("rbd:rbd/"+own, model.AtlasPhaseReady), "--atlas-rbd-pools"},
		"not ready":     {a, atlasRBDMachine("", model.AtlasPhaseProvisioning), "not Ready"},
		"foreign image": {a, atlasRBDMachine("rbd:rbd/someone-elses-disk", model.AtlasPhaseReady), "does not belong"},
		"foreign pool":  {a, atlasRBDMachine("rbd:secret-pool/"+own, model.AtlasPhaseReady), "not in this node"},
		"not rbd":       {a, atlasRBDMachine("pvc:x", model.AtlasPhaseReady), "no rbd native id"},
	} {
		if _, _, err := tc.agent.resolveAtlasRBDBoot(tc.m); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v want %q", name, err, tc.want)
		}
	}

	if _, ok, err := a.resolveAtlasRBDBoot(model.Machine{}); ok || err != nil {
		t.Fatalf("non-atlas machine: ok=%v err=%v", ok, err)
	}
}

func TestReconcileBootsAtlasRBDInPlace(t *testing.T) {
	own := model.AtlasVolumeName("prod", "db", "root")
	machine := atlasRBDMachine("rbd:rbd/"+own, model.AtlasPhaseReady)
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer ks.Close()
	var gotBody map[string]any
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms":
			_ = json.NewEncoder(w).Encode([]fluxvm.Record{})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Running"})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer fs.Close()
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{
		NodeName: "worker-1", Kube: kc, Flux: fc, DefaultBackend: "qemu",
		AtlasRBDPools: []string{"rbd"},
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotBody["storage"] != fluxvm.StorageCephRBDInPlace || gotBody["image"] != "rbd/"+own {
		t.Fatalf("create body=%v", gotBody)
	}
}

func TestImageStorageNotSettableFromAPI(t *testing.T) {
	var m model.Machine
	if err := json.Unmarshal([]byte(`{"spec":{"image":{"path":"/x","storage":"ceph-rbd-in-place"}}}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Spec.Image.Storage != "" {
		t.Fatal("spec.image.storage must not be decodable from the API")
	}
}
