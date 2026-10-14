// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestResolveBootDiskPathFallsBackToImagePathWithoutVolumes(t *testing.T) {
	a := &Agent{Log: slog.Default()}
	m := model.Machine{Spec: model.MachineSpec{Image: model.ImageSpec{Path: "/images/db.qcow2"}}}
	got, _, err := a.resolveBootDiskPath(context.Background(), m)
	if err != nil {
		t.Fatalf("resolveBootDiskPath: %v", err)
	}
	if got != "/images/db.qcow2" {
		t.Fatalf("got %q, want /images/db.qcow2", got)
	}
}

func TestResolveBootDiskPathRequiresClaimName(t *testing.T) {
	a := &Agent{Log: slog.Default()}
	m := model.Machine{Spec: model.MachineSpec{Volumes: []model.MachineVolume{{Name: "root"}}}}
	if _, _, err := a.resolveBootDiskPath(context.Background(), m); err == nil || !strings.Contains(err.Error(), "claimName") {
		t.Fatalf("expected a claimName error, got %v", err)
	}
}

func TestResolveBootDiskPathResolvesBoundHostPathPV(t *testing.T) {
	pvc := model.PersistentVolumeClaim{
		Metadata: model.ObjectMeta{Name: "root-pvc", Namespace: "prod"},
		Spec:     model.PersistentVolumeClaimSpec{VolumeName: "pv-root"},
		Status:   model.PersistentVolumeClaimStatus{Phase: "Bound"},
	}
	pv := model.PersistentVolume{
		Metadata: model.ObjectMeta{Name: "pv-root"},
		Spec:     model.PersistentVolumeSpec{HostPath: &model.HostPathVolumeSource{Path: "/var/lib/kairon/volumes/root-pvc"}},
	}
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/prod/persistentvolumeclaims/root-pvc":
			_ = json.NewEncoder(w).Encode(pvc)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/persistentvolumes/pv-root":
			_ = json.NewEncoder(w).Encode(pv)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer ks.Close()
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()

	a := &Agent{Kube: kc, Log: slog.Default()}
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod"},
		Spec:     model.MachineSpec{Volumes: []model.MachineVolume{{Name: "root", ClaimName: "root-pvc"}}},
	}
	got, _, err := a.resolveBootDiskPath(context.Background(), m)
	if err != nil {
		t.Fatalf("resolveBootDiskPath: %v", err)
	}
	want := filepath.Join("/var/lib/kairon/volumes/root-pvc", bootDiskFileName)
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveBootDiskPathSharedHostPathBootsInPlace(t *testing.T) {
	for _, c := range []struct {
		name        string
		annotations map[string]string
		local       bool
		want        bool
	}{
		{"annotated hostPath", map[string]string{model.AnnotationSharedStorage: "true"}, false, true},
		{"plain hostPath", nil, false, false},
		{"annotation not true", map[string]string{model.AnnotationSharedStorage: "yes"}, false, false},
		{"annotated local PV", map[string]string{model.AnnotationSharedStorage: "true"}, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			pvc := model.PersistentVolumeClaim{
				Metadata: model.ObjectMeta{Name: "root-pvc", Namespace: "prod"},
				Spec:     model.PersistentVolumeClaimSpec{VolumeName: "pv-root"},
				Status:   model.PersistentVolumeClaimStatus{Phase: "Bound"},
			}
			pv := model.PersistentVolume{Metadata: model.ObjectMeta{Name: "pv-root", Annotations: c.annotations}}
			if c.local {
				pv.Spec.Local = &model.LocalVolumeSource{Path: "/srv/shared/root"}
			} else {
				pv.Spec.HostPath = &model.HostPathVolumeSource{Path: "/srv/shared/root"}
			}
			ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/namespaces/prod/persistentvolumeclaims/root-pvc":
					_ = json.NewEncoder(w).Encode(pvc)
				case "/api/v1/persistentvolumes/pv-root":
					_ = json.NewEncoder(w).Encode(pv)
				default:
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			}))
			defer ks.Close()
			kc, _ := kube.New(ks.URL, "", "", false)
			kc.HTTP = ks.Client()
			a := &Agent{Kube: kc, Log: slog.Default()}
			m := model.Machine{
				Metadata: model.ObjectMeta{Namespace: "prod"},
				Spec:     model.MachineSpec{Volumes: []model.MachineVolume{{Name: "root", ClaimName: "root-pvc"}}},
			}
			got, vs, err := a.resolveBootDiskPath(context.Background(), m)
			if err != nil {
				t.Fatalf("resolveBootDiskPath: %v", err)
			}
			if got != filepath.Join("/srv/shared/root", bootDiskFileName) || vs.Shared != c.want {
				t.Fatalf("got %q shared=%v, want shared=%v", got, vs.Shared, c.want)
			}
		})
	}
}

func TestResolveBootDiskPathRejectsUnboundPVC(t *testing.T) {
	pvc := model.PersistentVolumeClaim{
		Metadata: model.ObjectMeta{Name: "root-pvc", Namespace: "prod"},
		Status:   model.PersistentVolumeClaimStatus{Phase: "Pending"},
	}
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(pvc)
	}))
	defer ks.Close()
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()

	a := &Agent{Kube: kc, Log: slog.Default()}
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod"},
		Spec:     model.MachineSpec{Volumes: []model.MachineVolume{{Name: "root", ClaimName: "root-pvc"}}},
	}
	if _, _, err := a.resolveBootDiskPath(context.Background(), m); err == nil || !strings.Contains(err.Error(), "not Bound") {
		t.Fatalf("expected a not-Bound error, got %v", err)
	}
}

func TestResolveBootDiskPathBlockHostPathBootsDevice(t *testing.T) {
	pvc := model.PersistentVolumeClaim{
		Spec:   model.PersistentVolumeClaimSpec{VolumeName: "pv-root"},
		Status: model.PersistentVolumeClaimStatus{Phase: "Bound"},
	}
	pv := model.PersistentVolume{Spec: model.PersistentVolumeSpec{
		VolumeMode: "Block",
		HostPath:   &model.HostPathVolumeSource{Path: "/dev/sdb"},
	}}
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/prod/persistentvolumeclaims/root-pvc":
			_ = json.NewEncoder(w).Encode(pvc)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/persistentvolumes/pv-root":
			_ = json.NewEncoder(w).Encode(pv)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer ks.Close()
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()

	a := &Agent{Kube: kc, Log: slog.Default()}
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod"},
		Spec:     model.MachineSpec{Volumes: []model.MachineVolume{{Name: "root", ClaimName: "root-pvc"}}},
	}
	path, _, err := a.resolveBootDiskPath(context.Background(), m)
	if err != nil || path != "/dev/sdb" {
		t.Fatalf("got %q, %v; want the device path /dev/sdb", path, err)
	}

	pv.Spec.VolumeMode = "Bogus"
	if _, _, err := a.resolveBootDiskPath(context.Background(), m); err == nil || !strings.Contains(err.Error(), "volumeMode") {
		t.Fatalf("expected a volumeMode error, got %v", err)
	}
}

func TestHostDirForPVRejectsUnsupportedSource(t *testing.T) {
	pv := model.PersistentVolume{Metadata: model.ObjectMeta{Name: "pv-nfs"}}
	if _, err := hostDirForPV(pv); err == nil || !strings.Contains(err.Error(), "hostPath- or local-backed") {
		t.Fatalf("expected an unsupported-source error, got %v", err)
	}
}

func TestReconcileCreatesFluxVMFromPVCBackedVolume(t *testing.T) {
	machine := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec: model.MachineSpec{
			NodeName:   "worker-1",
			Resources:  model.ResourceSpec{CPU: "2", Memory: "2Gi"},
			Runtime:    model.RuntimeSpec{Backend: "qemu"},
			PowerState: "Running",
			Volumes:    []model.MachineVolume{{Name: "root", ClaimName: "root-pvc"}},
		},
	}
	pvc := model.PersistentVolumeClaim{
		Spec:   model.PersistentVolumeClaimSpec{VolumeName: "pv-root"},
		Status: model.PersistentVolumeClaimStatus{Phase: "Bound"},
	}
	pv := model.PersistentVolume{Spec: model.PersistentVolumeSpec{HostPath: &model.HostPathVolumeSource{Path: "/mnt/outside-image-root/root-pvc"}}}

	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/prod/persistentvolumeclaims/root-pvc":
			_ = json.NewEncoder(w).Encode(pvc)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/persistentvolumes/pv-root":
			_ = json.NewEncoder(w).Encode(pv)
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer ks.Close()

	var createdImage string
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms":
			_ = json.NewEncoder(w).Encode([]fluxvm.Record{})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			var req fluxvm.CreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			createdImage = req.Image
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-456", Status: "Running"})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer fs.Close()

	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	// ImageRoot is set to a directory the PV's hostPath is deliberately
	// outside of -- proving the PVC-resolved path is exempt from the
	// spec.image.path allowlist check (it went through a stronger gate:
	// the PVC had to exist and be Bound).
	a := &Agent{NodeName: "worker-1", Kube: kc, Flux: fc, ImageRoot: "/var/lib/fluxvm/images", DefaultBackend: "qemu", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/mnt/outside-image-root/root-pvc", bootDiskFileName)
	if createdImage != want {
		t.Fatalf("got image %q, want %q", createdImage, want)
	}
}

func TestHostDirForPVPrefersHostPathThenLocal(t *testing.T) {
	pv := model.PersistentVolume{Spec: model.PersistentVolumeSpec{Local: &model.LocalVolumeSource{Path: "/mnt/local/root-pvc"}}}
	got, err := hostDirForPV(pv)
	if err != nil {
		t.Fatalf("hostDirForPV: %v", err)
	}
	if got != "/mnt/local/root-pvc" {
		t.Fatalf("got %q", got)
	}
}
