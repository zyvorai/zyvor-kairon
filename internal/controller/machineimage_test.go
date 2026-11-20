// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const imgDigest = "sha256:" + "cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34"

type imageTestAPI struct {
	mu           sync.Mutex
	specPatches  []map[string]any
	statusPhases []string
}

func newImageTestController(t *testing.T, images []model.MachineImage) (*Controller, *imageTestAPI) {
	t.Helper()
	api := &imageTestAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machineimages":
			_ = json.NewEncoder(w).Encode(model.MachineImageList{Items: images})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/status"):
			var p struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			api.statusPhases = append(api.statusPhases, p.Status.Phase+": "+p.Status.Message)
		case r.Method == http.MethodPatch:
			var p struct {
				Spec map[string]any `json:"spec"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			api.specPatches = append(api.specPatches, p.Spec)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	return &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, api
}

func machineImage(name, kind string) model.MachineImage {
	return model.MachineImage{
		Metadata: model.ObjectMeta{Name: name},
		Spec: model.MachineImageSpec{
			Kind:     kind,
			Source:   model.ImageSource{HTTPURL: "https://images.example/" + name},
			Digest:   imgDigest,
			Defaults: model.MachineImageDefaults{CPU: "4", Memory: "8Gi", DiskSize: "80Gi"},
		},
	}
}

func TestResolveImageRefsPinsSourceAndDefaults(t *testing.T) {
	ctl, api := newImageTestController(t, []model.MachineImage{machineImage("ws2022", "disk"), machineImage("virtio-win", "iso")})
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "win01"},
		Spec: model.MachineSpec{
			Image:  model.ImageSpec{ImageRef: "ws2022"},
			Cdroms: []model.MachineCdrom{{Name: "virtio", ImageRef: "virtio-win"}},
		},
	}
	out, waiting := ctl.resolveImageRefs(context.Background(), []model.Machine{m})
	if len(waiting) != 0 {
		t.Fatalf("unexpected waiting: %v", waiting)
	}
	got := out[0].Spec
	if got.Image.Source == nil || got.Image.Source.HTTPURL != "https://images.example/ws2022" || got.Image.Digest != imgDigest {
		t.Fatalf("image not pinned: %+v", got.Image)
	}
	if got.Image.DiskSize != "80Gi" || got.Resources.CPU != "4" || got.Resources.Memory != "8Gi" {
		t.Fatalf("defaults not applied: %+v %+v", got.Image, got.Resources)
	}
	if got.Cdroms[0].Source == nil || got.Cdroms[0].Digest != imgDigest {
		t.Fatalf("cdrom not pinned: %+v", got.Cdroms[0])
	}
	if len(api.specPatches) != 1 {
		t.Fatalf("want one spec patch, got %d", len(api.specPatches))
	}
	for _, k := range []string{"image", "resources", "cdroms"} {
		if _, ok := api.specPatches[0][k]; !ok {
			t.Errorf("patch missing %q: %v", k, api.specPatches[0])
		}
	}
	if m.Spec.Cdroms[0].Source != nil {
		t.Error("input Machine's cdrom slice was mutated")
	}
}

func TestResolveImageRefsKeepsExplicitResources(t *testing.T) {
	ctl, _ := newImageTestController(t, []model.MachineImage{machineImage("ws2022", "disk")})
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "win01"},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{ImageRef: "ws2022", DiskSize: "200Gi"},
			Resources: model.ResourceSpec{CPU: "2", Memory: "4Gi"},
		},
	}
	out, _ := ctl.resolveImageRefs(context.Background(), []model.Machine{m})
	if out[0].Spec.Resources.CPU != "2" || out[0].Spec.Image.DiskSize != "200Gi" {
		t.Fatalf("explicit values overridden: %+v %+v", out[0].Spec.Resources, out[0].Spec.Image)
	}
}

func TestResolveImageRefsHoldsBackUnresolvable(t *testing.T) {
	ctl, api := newImageTestController(t, []model.MachineImage{machineImage("win-iso", "iso")})
	missing := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "a"}, Spec: model.MachineSpec{Image: model.ImageSpec{ImageRef: "nope"}}}
	wrongKind := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "b"}, Spec: model.MachineSpec{Image: model.ImageSpec{ImageRef: "win-iso"}}}
	resolved := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "c"}, Spec: model.MachineSpec{Image: model.ImageSpec{
		ImageRef: "gone", Source: &model.ImageSource{HTTPURL: "https://x"}, Digest: imgDigest,
	}}}
	_, waiting := ctl.resolveImageRefs(context.Background(), []model.Machine{missing, wrongKind, resolved})
	if !strings.Contains(waiting["prod/a"], `waiting for MachineImage "nope"`) {
		t.Errorf("missing image: %q", waiting["prod/a"])
	}
	if !strings.Contains(waiting["prod/b"], "kind iso, need disk") {
		t.Errorf("wrong kind: %q", waiting["prod/b"])
	}
	if _, ok := waiting["prod/c"]; ok {
		t.Error("an already-pinned Machine must not wait on its (deleted) image")
	}
	if len(api.specPatches) != 0 || len(api.statusPhases) != 2 {
		t.Fatalf("want 0 spec patches and 2 Pending statuses, got %v / %v", api.specPatches, api.statusPhases)
	}
}
