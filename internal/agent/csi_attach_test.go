// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// fakeStorageAPI serves CSIDriver, VolumeAttachment and Secret objects.
// When autoAttach is set it plays the external-attacher: a created
// VolumeAttachment comes back attached with attachmentMetadata.
type fakeStorageAPI struct {
	mu             sync.Mutex
	attachRequired *bool
	autoAttach     bool
	attachError    string
	vas            map[string]model.VolumeAttachment
	deleted        []string
	secrets        map[string]model.Secret
}

func (f *fakeStorageAPI) client(t *testing.T) *kube.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = srv.Client()
	return kc
}

func (f *fakeStorageAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const vaPrefix = "/apis/storage.k8s.io/v1/volumeattachments"
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apis/storage.k8s.io/v1/csidrivers/"):
		if f.attachRequired == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(model.CSIDriver{Spec: model.CSIDriverSpec{AttachRequired: f.attachRequired}})
	case r.Method == http.MethodPost && r.URL.Path == vaPrefix:
		var va model.VolumeAttachment
		_ = json.NewDecoder(r.Body).Decode(&va)
		if f.autoAttach {
			va.Status = model.VolumeAttachmentStatus{Attached: true, AttachmentMetadata: map[string]string{"devicePath": "/dev/sdx"}}
		}
		if f.attachError != "" {
			va.Status.AttachError = &model.VolumeError{Message: f.attachError}
		}
		f.vas[va.Metadata.Name] = va
		_ = json.NewEncoder(w).Encode(va)
	case strings.HasPrefix(r.URL.Path, vaPrefix+"/"):
		name := strings.TrimPrefix(r.URL.Path, vaPrefix+"/")
		va, ok := f.vas[name]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			delete(f.vas, name)
			f.deleted = append(f.deleted, name)
		}
		_ = json.NewEncoder(w).Encode(va)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/"):
		s, ok := f.secrets[r.URL.Path]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(s)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func boolPtr(b bool) *bool { return &b }

func newAttachAgent(t *testing.T, api *fakeStorageAPI) (*Agent, *fakeCSINodeServer) {
	t.Helper()
	if api.vas == nil {
		api.vas = map[string]model.VolumeAttachment{}
	}
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	return &Agent{
		Kube:                 api.client(t),
		NodeName:             "node-a",
		CSIStagingDir:        t.TempDir(),
		CSIPublishDir:        t.TempDir(),
		ThirdPartyCSIDrivers: map[string]string{"block.example.com": socketPath},
	}, fake
}

func TestThirdPartyCSIAttachRequiredCreatesVolumeAttachmentAndPassesPublishContext(t *testing.T) {
	api := &fakeStorageAPI{attachRequired: boolPtr(true), autoAttach: true}
	a, fake := newAttachAgent(t, api)
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}
	pv := testThirdPartyCSIPV("pv-1", "block.example.com", "vol-123")

	_, st, err := a.resolveCSIVolume(context.Background(), m, pv)
	if err != nil {
		t.Fatalf("resolveCSIVolume: %v", err)
	}
	name := a.csiAttachmentName("block.example.com", "vol-123")
	va, ok := api.vas[name]
	if !ok {
		t.Fatalf("expected VolumeAttachment %s, have %v", name, api.vas)
	}
	if va.Spec.Attacher != "block.example.com" || va.Spec.NodeName != "node-a" || va.Spec.Source.PersistentVolumeName == nil || *va.Spec.Source.PersistentVolumeName != "pv-1" {
		t.Fatalf("unexpected VolumeAttachment spec: %+v", va.Spec)
	}
	if got := fake.stageCalls[0].GetPublishContext()["devicePath"]; got != "/dev/sdx" {
		t.Fatalf("stage publish_context devicePath = %q", got)
	}
	if got := fake.publishCalls[0].GetPublishContext()["devicePath"]; got != "/dev/sdx" {
		t.Fatalf("publish publish_context devicePath = %q", got)
	}

	m.Status.VolumeStagingPath, m.Status.VolumePublishPath = st.StagingPath, st.PublishPath
	m.Status.VolumeHandle, m.Status.VolumeDriver = st.VolumeID, st.Driver
	if err := a.teardownCSIVolume(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if len(fake.unstage) != 1 || len(api.deleted) != 1 || api.deleted[0] != name {
		t.Fatalf("expected unstage then VolumeAttachment delete, got unstage=%d deleted=%v", len(fake.unstage), api.deleted)
	}
}

func TestThirdPartyCSIWaitsUntilAttached(t *testing.T) {
	api := &fakeStorageAPI{attachRequired: boolPtr(true)}
	a, fake := newAttachAgent(t, api)
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}
	pv := testThirdPartyCSIPV("pv-1", "block.example.com", "vol-123")

	_, _, err := a.resolveCSIVolume(context.Background(), m, pv)
	if err == nil || !strings.Contains(err.Error(), "waiting for") {
		t.Fatalf("expected a waiting error, got %v", err)
	}
	if len(fake.stageCalls) != 0 {
		t.Fatal("NodeStage must not run before the volume is attached")
	}

	name := a.csiAttachmentName("block.example.com", "vol-123")
	va := api.vas[name]
	va.Status = model.VolumeAttachmentStatus{Attached: true, AttachmentMetadata: map[string]string{"lun": "3"}}
	api.vas[name] = va
	if _, _, err := a.resolveCSIVolume(context.Background(), m, pv); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if fake.stageCalls[0].GetPublishContext()["lun"] != "3" {
		t.Fatalf("publish context not passed: %+v", fake.stageCalls[0].GetPublishContext())
	}
}

func TestThirdPartyCSIAttachErrorSurfaces(t *testing.T) {
	api := &fakeStorageAPI{attachRequired: boolPtr(true), attachError: "LUN busy"}
	a, _ := newAttachAgent(t, api)
	pv := testThirdPartyCSIPV("pv-1", "block.example.com", "vol-123")
	_, _, err := a.resolveCSIVolume(context.Background(), model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}, pv)
	if err == nil || !strings.Contains(err.Error(), "LUN busy") {
		t.Fatalf("expected attach error, got %v", err)
	}
}

func TestThirdPartyCSINoAttachWhenNotRequiredOrNoCSIDriver(t *testing.T) {
	for _, required := range []*bool{nil, boolPtr(false)} {
		api := &fakeStorageAPI{attachRequired: required}
		a, fake := newAttachAgent(t, api)
		pv := testThirdPartyCSIPV("pv-1", "block.example.com", "vol-123")
		if _, _, err := a.resolveCSIVolume(context.Background(), model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}, pv); err != nil {
			t.Fatalf("attachRequired=%v: %v", required, err)
		}
		if len(api.vas) != 0 || len(fake.stageCalls[0].GetPublishContext()) != 0 {
			t.Fatalf("attachRequired=%v: expected no VolumeAttachment, got %v", required, api.vas)
		}
	}
}

func TestThirdPartyCSIRejectsForeignVolumeAttachment(t *testing.T) {
	api := &fakeStorageAPI{attachRequired: boolPtr(true), autoAttach: true}
	a, _ := newAttachAgent(t, api)
	name := a.csiAttachmentName("block.example.com", "vol-123")
	api.vas[name] = model.VolumeAttachment{Spec: model.VolumeAttachmentSpec{Attacher: "block.example.com", NodeName: "node-b"}}
	pv := testThirdPartyCSIPV("pv-1", "block.example.com", "vol-123")
	if _, _, err := a.resolveCSIVolume(context.Background(), model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}, pv); err == nil {
		t.Fatal("expected an error for a VolumeAttachment bound to another node")
	}
}

func TestThirdPartyCSISecretsFromAllowlistedNamespace(t *testing.T) {
	api := &fakeStorageAPI{secrets: map[string]model.Secret{
		"/api/v1/namespaces/kairon-system/secrets/stage": {Data: map[string][]byte{"userID": []byte("admin"), "userKey": []byte("k")}},
		"/api/v1/namespaces/kairon-system/secrets/pub":   {Data: map[string][]byte{"token": []byte("t")}},
	}}
	a, fake := newAttachAgent(t, api)
	a.ThirdPartyCSISecretNamespace = "kairon-system"
	pv := testThirdPartyCSIPV("pv-1", "block.example.com", "vol-123")
	pv.Spec.CSI.NodeStageSecretRef = &model.SecretReference{Name: "stage"}
	pv.Spec.CSI.NodePublishSecretRef = &model.SecretReference{Name: "pub", Namespace: "kairon-system"}

	if _, _, err := a.resolveCSIVolume(context.Background(), model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}, pv); err != nil {
		t.Fatal(err)
	}
	if s := fake.stageCalls[0].GetSecrets(); s["userID"] != "admin" || s["userKey"] != "k" {
		t.Fatalf("stage secrets = %v", s)
	}
	if s := fake.publishCalls[0].GetSecrets(); s["token"] != "t" || len(s) != 1 {
		t.Fatalf("publish secrets = %v", s)
	}
}

func TestThirdPartyCSISecretsFailClosed(t *testing.T) {
	cases := map[string]struct {
		allowNS string
		ref     model.SecretReference
	}{
		"no namespace configured": {"", model.SecretReference{Name: "s"}},
		"other namespace":         {"kairon-system", model.SecretReference{Name: "s", Namespace: "prod"}},
		"missing name":            {"kairon-system", model.SecretReference{}},
	}
	for name, tc := range cases {
		api := &fakeStorageAPI{}
		a, fake := newAttachAgent(t, api)
		a.ThirdPartyCSISecretNamespace = tc.allowNS
		pv := testThirdPartyCSIPV("pv-1", "block.example.com", "vol-123")
		ref := tc.ref
		pv.Spec.CSI.NodeStageSecretRef = &ref
		if _, _, err := a.resolveCSIVolume(context.Background(), model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}, pv); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if len(fake.stageCalls) != 0 {
			t.Errorf("%s: NodeStage must not run", name)
		}
	}
}
