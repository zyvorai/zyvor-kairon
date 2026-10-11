// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/ratelimit"
)

// fakeKube is a minimal in-memory Kubernetes API double, just enough to
// exercise every internal/uiapi handler against a real *kube.Client over
// real HTTP (not a hand-rolled uiapi.Server double) -- the same intent as
// internal/integration's fakeCluster, but scoped to what this package's
// handlers actually call.
type fakeKube struct {
	mu                sync.Mutex
	machines          map[string]model.Machine
	migrations        map[string]model.MachineMigration
	snapshots         map[string]model.MachineSnapshot
	restores          map[string]model.MachineSnapshotRestore
	quotas            map[string]model.MachineQuota
	budgets           map[string]model.MachineDisruptionBudget
	machineSets       map[string]model.MachineSet
	instanceTypes     map[string]model.MachineInstanceType
	migrationPolicies map[string]model.MigrationPolicy
	snapshotSchedules map[string]model.MachineSnapshotSchedule
	networkPolicies   map[string]model.MachineNetworkPolicy
	securityGroups    map[string]model.NetworkSecurityGroup
	nodes             []model.Node
	events            []model.Event
	backups           map[string]model.MachineBackup
	backupRestores    []model.MachineBackupRestore
	// patches records every raw merge-patch body, keyed by object path.
	patches  map[string][]string
	podsJSON []map[string]any
	// secrets holds only stringData, keyed by "namespace/name" -- enough
	// to exercise Client.PatchSecretStringData (see
	// uiapi.Server.persistUsers) without modeling a full core/v1 Secret.
	secrets map[string]map[string]string
	// sar scripts every SubjectAccessReview response this fake returns --
	// nil (the default) makes the endpoint respond with an HTTP error, so
	// a test that doesn't expect a SAR call at all still fails loudly
	// rather than silently allowing.
	sar func(model.SubjectAccessReview) model.SubjectAccessReviewStatus
}

func newFakeKube() *fakeKube {
	return &fakeKube{
		machines:          map[string]model.Machine{},
		migrations:        map[string]model.MachineMigration{},
		snapshots:         map[string]model.MachineSnapshot{},
		restores:          map[string]model.MachineSnapshotRestore{},
		quotas:            map[string]model.MachineQuota{},
		budgets:           map[string]model.MachineDisruptionBudget{},
		machineSets:       map[string]model.MachineSet{},
		instanceTypes:     map[string]model.MachineInstanceType{},
		migrationPolicies: map[string]model.MigrationPolicy{},
		snapshotSchedules: map[string]model.MachineSnapshotSchedule{},
		networkPolicies:   map[string]model.MachineNetworkPolicy{},
		securityGroups:    map[string]model.NetworkSecurityGroup{},
		secrets:           map[string]map[string]string{},
	}
}

func (f *fakeKube) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("watch") == "true":
			if r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines" {
				for _, m := range f.machines {
					raw, _ := json.Marshal(m)
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "ADDED", "object": json.RawMessage(raw)})
				}
			}
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinebackups":
			var b model.MachineBackup
			_ = json.NewDecoder(r.Body).Decode(&b)
			if f.backups == nil {
				f.backups = map[string]model.MachineBackup{}
			}
			f.backups[b.Metadata.Name] = b
			_ = json.NewEncoder(w).Encode(b)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinebackups/"):
			delete(f.backups, strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinebackups/"))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinebackuprestores":
			var br model.MachineBackupRestore
			_ = json.NewDecoder(r.Body).Decode(&br)
			f.backupRestores = append(f.backupRestores, br)
			_ = json.NewEncoder(w).Encode(br)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/pods":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": f.podsJSON})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/default/events":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": f.events})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			items := make([]model.Machine, 0, len(f.machines))
			for _, m := range f.machines {
				items = append(items, m)
			}
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: items})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machines":
			items := make([]model.Machine, 0, len(f.machines))
			for _, m := range f.machines {
				items = append(items, m)
			}
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: items})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/"):
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/")
			m, ok := f.machines[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(m)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machines":
			var m model.Machine
			_ = json.NewDecoder(r.Body).Decode(&m)
			f.machines[m.Metadata.Name] = m
			_ = json.NewEncoder(w).Encode(m)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/"):
			if raw, err := io.ReadAll(r.Body); err == nil {
				if f.patches == nil {
					f.patches = map[string][]string{}
				}
				f.patches[r.URL.Path] = append(f.patches[r.URL.Path], string(raw))
				r.Body = io.NopCloser(bytes.NewReader(raw))
			}
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/")
			m, ok := f.machines[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			var patch struct {
				Spec struct {
					PowerState string `json:"powerState"`
					Priority   *int32 `json:"priority"`
				} `json:"spec"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			if patch.Spec.PowerState != "" {
				m.Spec.PowerState = patch.Spec.PowerState
			}
			if patch.Spec.Priority != nil {
				m.Spec.Priority = *patch.Spec.Priority
			}
			f.machines[name] = m
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/"):
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/")
			if _, ok := f.machines[name]; !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			delete(f.machines, name)
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			items := make([]model.MachineMigration, 0, len(f.migrations))
			for _, m := range f.migrations {
				items = append(items, m)
			}
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: items})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinemigrations":
			items := make([]model.MachineMigration, 0, len(f.migrations))
			for _, m := range f.migrations {
				items = append(items, m)
			}
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: items})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinemigrations/"):
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinemigrations/")
			m, ok := f.migrations[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(m)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinemigrations":
			var m model.MachineMigration
			_ = json.NewDecoder(r.Body).Decode(&m)
			f.migrations[m.Metadata.Name] = m
			_ = json.NewEncoder(w).Encode(m)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinemigrations/"):
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinemigrations/")
			m, ok := f.migrations[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			var patch struct {
				Spec struct {
					Recovery *model.MachineMigrationRecoverySpec `json:"recovery"`
					Cancel   *bool                               `json:"cancel"`
				} `json:"spec"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			m.Spec.Recovery = patch.Spec.Recovery
			if patch.Spec.Cancel != nil {
				m.Spec.Cancel = *patch.Spec.Cancel
			}
			f.migrations[name] = m
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && (r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshots" || r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots"):
			items := make([]model.MachineSnapshot, 0, len(f.snapshots))
			for _, s := range f.snapshots {
				items = append(items, s)
			}
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotList{Items: items})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshots":
			var s model.MachineSnapshot
			_ = json.NewDecoder(r.Body).Decode(&s)
			f.snapshots[s.Metadata.Name] = s
			_ = json.NewEncoder(w).Encode(s)

		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshotrestores":
			items := make([]model.MachineSnapshotRestore, 0, len(f.restores))
			for _, res := range f.restores {
				items = append(items, res)
			}
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotRestoreList{Items: items})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshotrestores":
			var res model.MachineSnapshotRestore
			_ = json.NewDecoder(r.Body).Decode(&res)
			f.restores[res.Metadata.Name] = res
			_ = json.NewEncoder(w).Encode(res)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshotrestores/"):
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshotrestores/")
			if _, ok := f.restores[name]; !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			delete(f.restores, name)
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinequotas":
			items := make([]model.MachineQuota, 0, len(f.quotas))
			for _, q := range f.quotas {
				items = append(items, q)
			}
			_ = json.NewEncoder(w).Encode(model.MachineQuotaList{Items: items})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinedisruptionbudgets":
			items := make([]model.MachineDisruptionBudget, 0, len(f.budgets))
			for _, b := range f.budgets {
				items = append(items, b)
			}
			_ = json.NewEncoder(w).Encode(model.MachineDisruptionBudgetList{Items: items})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesets":
			items := make([]model.MachineSet, 0, len(f.machineSets))
			for _, ms := range f.machineSets {
				items = append(items, ms)
			}
			_ = json.NewEncoder(w).Encode(model.MachineSetList{Items: items})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesets/"):
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesets/")
			if _, ok := f.machineSets[name]; !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			delete(f.machineSets, name)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesets/"):
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesets/")
			ms, ok := f.machineSets[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			var patch struct {
				Spec struct {
					Replicas int `json:"replicas"`
				} `json:"spec"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			ms.Spec.Replicas = patch.Spec.Replicas
			f.machineSets[name] = ms
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesets/"):
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesets/")
			ms, ok := f.machineSets[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(ms)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machineinstancetypes":
			items := make([]model.MachineInstanceType, 0, len(f.instanceTypes))
			for _, it := range f.instanceTypes {
				items = append(items, it)
			}
			_ = json.NewEncoder(w).Encode(model.MachineInstanceTypeList{Items: items})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/migrationpolicies":
			items := make([]model.MigrationPolicy, 0, len(f.migrationPolicies))
			for _, p := range f.migrationPolicies {
				items = append(items, p)
			}
			_ = json.NewEncoder(w).Encode(model.MigrationPolicyList{Items: items})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshotschedules":
			items := make([]model.MachineSnapshotSchedule, 0, len(f.snapshotSchedules))
			for _, s := range f.snapshotSchedules {
				items = append(items, s)
			}
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: items})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshotschedules/"):
			if raw, err := io.ReadAll(r.Body); err == nil {
				if f.patches == nil {
					f.patches = map[string][]string{}
				}
				f.patches[r.URL.Path] = append(f.patches[r.URL.Path], string(raw))
				r.Body = io.NopCloser(bytes.NewReader(raw))
			}
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshotschedules/")
			s, ok := f.snapshotSchedules[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			var patch struct {
				Spec struct {
					Suspend bool `json:"suspend"`
				} `json:"spec"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			s.Spec.Suspend = patch.Spec.Suspend
			f.snapshotSchedules[name] = s
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinenetworkpolicies":
			items := make([]model.MachineNetworkPolicy, 0, len(f.networkPolicies))
			for _, p := range f.networkPolicies {
				items = append(items, p)
			}
			_ = json.NewEncoder(w).Encode(model.MachineNetworkPolicyList{Items: items})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/networksecuritygroups":
			items := make([]model.NetworkSecurityGroup, 0, len(f.securityGroups))
			for _, g := range f.securityGroups {
				items = append(items, g)
			}
			_ = json.NewEncoder(w).Encode(model.NetworkSecurityGroupList{Items: items})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshotschedules/"):
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshotschedules/")
			s, ok := f.snapshotSchedules[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(s)

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: f.nodes})

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/") && strings.Contains(r.URL.Path, "/secrets/"):
			key := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/")
			key = strings.Replace(key, "/secrets/", "/", 1)
			var patch struct {
				StringData map[string]string `json:"stringData"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			existing := f.secrets[key]
			if existing == nil {
				existing = map[string]string{}
			}
			for k, v := range patch.StringData {
				existing[k] = v
			}
			f.secrets[key] = existing
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && r.URL.Path == "/apis/authorization.k8s.io/v1/subjectaccessreviews":
			if f.sar == nil {
				http.Error(w, "unexpected SubjectAccessReview call", http.StatusNotFound)
				return
			}
			var sar model.SubjectAccessReview
			_ = json.NewDecoder(r.Body).Decode(&sar)
			sar.Status = f.sar(sar)
			_ = json.NewEncoder(w).Encode(sar)

		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	})
}

func newTestServer(t *testing.T, fk *fakeKube, token string) *Server {
	t.Helper()
	srv := httptest.NewServer(fk.handler())
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	return &Server{Kube: kc, Token: token}
}

func doJSON(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestDecodeJSONRejectsBodyOverTheDefaultLimit proves decodeJSON now
// actually bounds request body size -- before defaultMaxRequestBodyBytes
// existed, nothing in kairon-ui's HTTP stack capped a JSON body at all.
func TestDecodeJSONRejectsBodyOverTheDefaultLimit(t *testing.T) {
	oversized := `{"path":"` + strings.Repeat("a", defaultMaxRequestBodyBytes+1) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(oversized))
	rr := httptest.NewRecorder()
	var out execRequest
	if err := decodeJSON(rr, req, &out); err == nil {
		t.Fatalf("expected an error decoding a body over defaultMaxRequestBodyBytes, got nil")
	}
}

func TestDecodeJSONAcceptsAnOrdinaryBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"path":"/etc/hostname"}`))
	rr := httptest.NewRecorder()
	var out execRequest
	if err := decodeJSON(rr, req, &out); err != nil {
		t.Fatalf("expected no error decoding an ordinary small body, got %v", err)
	}
	if out.Path != "/etc/hostname" {
		t.Fatalf("expected the body to actually decode, got %+v", out)
	}
}

// TestDecodeJSONWithLimitHonorsAnExplicitOverride proves
// handleAgentPutFile's maxAgentFileBodyBytes override actually takes
// effect -- a body larger than defaultMaxRequestBodyBytes but within the
// larger explicit limit must still decode successfully, not silently fall
// back to the smaller default.
func TestDecodeJSONWithLimitHonorsAnExplicitOverride(t *testing.T) {
	big := strings.Repeat("a", defaultMaxRequestBodyBytes+1024)
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"contentBase64":"`+big+`"}`))
	rr := httptest.NewRecorder()
	var out agentPutFileRequest
	if err := decodeJSONWithLimit(rr, req, &out, maxAgentFileBodyBytes); err != nil {
		t.Fatalf("expected the larger explicit limit to accept this body, got %v", err)
	}
	if len(out.ContentBase64) != len(big) {
		t.Fatalf("expected the full content to decode, got length %d, want %d", len(out.ContentBase64), len(big))
	}
}

func TestAuthRejectsMissingOrWrongToken(t *testing.T) {
	s := newTestServer(t, newFakeKube(), "secret")
	h := s.Handler()

	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines", "", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: expected 401, got %d", rr.Code)
	}
	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines", "wrong", nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: expected 401, got %d", rr.Code)
	}
	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines", "secret", nil); rr.Code != http.StatusOK {
		t.Fatalf("correct token: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	// Health probes must never require the token -- Kubernetes never sends one.
	if rr := doJSON(t, h, http.MethodGet, "/healthz", "", nil); rr.Code != http.StatusOK {
		t.Fatalf("healthz: expected 200 without token, got %d", rr.Code)
	}
}

func TestAuditLogsMutatingRequestsNotReads(t *testing.T) {
	var logBuf bytes.Buffer
	fk := newFakeKube()
	fk.machines["db"] = model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}}
	srv := httptest.NewServer(fk.handler())
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	s := &Server{Kube: kc, Log: slog.New(slog.NewTextHandler(&logBuf, nil))}
	h := s.Handler()

	// A read (GET) must not be audited.
	doJSON(t, h, http.MethodGet, "/api/v1/machines", "", nil)
	if logBuf.Len() != 0 {
		t.Fatalf("expected no audit log for a GET request, got: %s", logBuf.String())
	}

	// A mutating request must be audited, including its resulting status.
	doJSON(t, h, http.MethodPost, "/api/v1/machines/default/db/stop", "", nil)
	logged := logBuf.String()
	if !strings.Contains(logged, "uiapi request") ||
		!strings.Contains(logged, "method=POST") ||
		!strings.Contains(logged, "path=/api/v1/machines/default/db/stop") ||
		!strings.Contains(logged, "status=204") {
		t.Fatalf("expected an audit log line with method/path/status, got: %s", logged)
	}
}

func TestMetricsRouteAndRequestObservationAreOptIn(t *testing.T) {
	fk := newFakeKube()
	fk.machines["a"] = model.Machine{Metadata: model.ObjectMeta{Name: "a", Namespace: "default"}}
	fk.machines["b"] = model.Machine{Metadata: model.ObjectMeta{Name: "b", Namespace: "default"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	// Without Metrics set, /metrics isn't a route at all -- falls through
	// to serveWeb's "no WebDir configured" JSON response, not a 200 with
	// Prometheus exposition text.
	rr := doJSON(t, h, http.MethodGet, "/metrics", "", nil)
	if strings.Contains(rr.Body.String(), "# HELP") {
		t.Fatalf("expected no /metrics route without Server.Metrics set, got: %s", rr.Body.String())
	}

	rec := metrics.NewUIRecorder()
	s.Metrics = rec
	h = s.Handler()

	doJSON(t, h, http.MethodGet, "/api/v1/overview", "", nil)
	// Two distinct real Machine paths under the same dynamic
	// {namespace}/{name} pattern -- must collapse into one route label,
	// not blow up cardinality one bucket per Machine.
	doJSON(t, h, http.MethodGet, "/api/v1/machines/default/a", "", nil)
	doJSON(t, h, http.MethodGet, "/api/v1/machines/default/b", "", nil)
	// Matches top's own "/api/v1/" prefix pattern (dispatching into the
	// api sub-mux) but nothing registered inside api itself -- unlike
	// "/does/not/exist", which the top-level "/" SPA catch-all would
	// itself report as a real, if generic, route.
	doJSON(t, h, http.MethodGet, "/api/v1/this-route-does-not-exist", "", nil)

	rr = doJSON(t, h, http.MethodGet, "/metrics", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /metrics: got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "kairon_ui_request_duration_seconds") {
		t.Errorf("missing kairon_ui_request_duration_seconds in:\n%s", body)
	}
	if !strings.Contains(body, `route="/api/v1/overview"`) {
		t.Errorf("missing overview route label in:\n%s", body)
	}
	if !strings.Contains(body, `kairon_ui_request_duration_seconds_count{method="GET",route="/api/v1/machines/{namespace}/{name}",status_class="2xx"} 2`) {
		t.Errorf("expected both distinct Machine paths to collapse into one route label with count 2, got:\n%s", body)
	}
	if !strings.Contains(body, `route="unmatched"`) {
		t.Errorf("expected a 404 to report route=\"unmatched\", got:\n%s", body)
	}
}

func TestRateLimitIsOptInAndThrottlesPerRemoteAddr(t *testing.T) {
	fk := newFakeKube()
	s := newTestServer(t, fk, "")
	h := s.Handler()

	// Without RateLimit set, nothing throttles -- a burst of requests all
	// succeed.
	for range 5 {
		rr := doJSON(t, h, http.MethodGet, "/api/v1/auth/config", "", nil)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatal("expected no rate limiting without Server.RateLimit set")
		}
	}

	s.RateLimit = ratelimit.New(1, 2) // 1 req/s, burst 2
	h = s.Handler()

	var got429 bool
	for range 5 {
		rr := doJSON(t, h, http.MethodGet, "/api/v1/auth/config", "", nil)
		if rr.Code == http.StatusTooManyRequests {
			got429 = true
			if rr.Header().Get("Retry-After") == "" {
				t.Error("expected a Retry-After header on a 429")
			}
			break
		}
	}
	if !got429 {
		t.Fatal("expected a burst of 5 requests against burst=2 to eventually hit 429")
	}
}

func TestClientIPIsRemoteAddrWithoutTrustedProxyConfig(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := s.clientIP(req); got != "203.0.113.9" {
		t.Fatalf("expected the header to be ignored with no TrustedProxyHeader/CIDRs configured, got %q", got)
	}
}

func TestClientIPUsesHeaderOnlyFromATrustedPeer(t *testing.T) {
	_, trustedNet, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{TrustedProxyHeader: "X-Forwarded-For", TrustedProxyCIDRs: []*net.IPNet{trustedNet}}

	// Direct peer is inside the trusted CIDR (the load balancer itself) --
	// the header's leftmost entry (the real client) is trusted.
	trusted := httptest.NewRequest(http.MethodGet, "/", nil)
	trusted.RemoteAddr = "10.0.0.5:5555"
	trusted.Header.Set("X-Forwarded-For", "198.51.100.1, 10.0.0.5")
	if got := s.clientIP(trusted); got != "198.51.100.1" {
		t.Fatalf("expected the header's leftmost entry from a trusted peer, got %q", got)
	}

	// Direct peer is NOT inside the trusted CIDR -- a client talking
	// directly to kairon-ui (bypassing the load balancer, or a load
	// balancer with an untrusted address) cannot spoof its way into an
	// arbitrary bucket by setting the header itself.
	untrusted := httptest.NewRequest(http.MethodGet, "/", nil)
	untrusted.RemoteAddr = "203.0.113.9:5555"
	untrusted.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := s.clientIP(untrusted); got != "203.0.113.9" {
		t.Fatalf("expected the header to be ignored from an untrusted peer, got %q", got)
	}

	// Trusted peer, but no header present -- falls back to the peer
	// address rather than an empty key.
	noHeader := httptest.NewRequest(http.MethodGet, "/", nil)
	noHeader.RemoteAddr = "10.0.0.5:5555"
	if got := s.clientIP(noHeader); got != "10.0.0.5" {
		t.Fatalf("expected fallback to RemoteAddr when the header is absent, got %q", got)
	}

	// Trusted peer, malformed header value -- falls back rather than
	// keying by garbage.
	malformed := httptest.NewRequest(http.MethodGet, "/", nil)
	malformed.RemoteAddr = "10.0.0.5:5555"
	malformed.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := s.clientIP(malformed); got != "10.0.0.5" {
		t.Fatalf("expected fallback to RemoteAddr on a malformed header value, got %q", got)
	}
}

func TestRateLimitKeysByTrustedForwardedForNotSharedProxyAddress(t *testing.T) {
	_, trustedNet, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	fk := newFakeKube()
	s := newTestServer(t, fk, "")
	s.RateLimit = ratelimit.New(1, 2) // 1 req/s, burst 2
	s.TrustedProxyHeader = "X-Forwarded-For"
	s.TrustedProxyCIDRs = []*net.IPNet{trustedNet}
	h := s.Handler()

	// Two distinct real clients, both arriving via the same trusted
	// load-balancer peer address -- without trusting the forwarded
	// header, both would collapse into one rate-limit bucket keyed by
	// the LB's own address. With it, client A being throttled must not
	// throttle client B.
	requestFrom := func(clientIP string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil)
		req.RemoteAddr = "10.0.0.5:5555"
		req.Header.Set("X-Forwarded-For", clientIP)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	var aThrottled bool
	for range 5 {
		if requestFrom("198.51.100.1") == http.StatusTooManyRequests {
			aThrottled = true
			break
		}
	}
	if !aThrottled {
		t.Fatal("expected client A to eventually hit 429 against burst=2")
	}
	if got := requestFrom("198.51.100.2"); got == http.StatusTooManyRequests {
		t.Fatalf("client B should have its own bucket, keyed by its own forwarded address, got %d", got)
	}
}

func TestCreateAndListMachines(t *testing.T) {
	s := newTestServer(t, newFakeKube(), "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines", "", createMachineRequest{Name: "db", Image: "/images/db.qcow2"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var created model.Machine
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Spec.Resources.CPU != "2" || created.Spec.Resources.Memory != "2Gi" || created.Spec.Runtime.Backend != "qemu" {
		t.Fatalf("expected defaults applied, got %+v", created.Spec)
	}

	rr = doJSON(t, h, http.MethodGet, "/api/v1/machines", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rr.Code)
	}
	var items []model.Machine
	if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(items) != 1 || items[0].Metadata.Name != "db" {
		t.Fatalf("expected one machine named db, got %+v", items)
	}
}

func TestCreateMachineRequiresNameAndImage(t *testing.T) {
	s := newTestServer(t, newFakeKube(), "")
	h := s.Handler()
	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines", "", createMachineRequest{Name: "db"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 missing image, got %d", rr.Code)
	}
}

func TestGetMachineNotFoundMapsTo404(t *testing.T) {
	s := newTestServer(t, newFakeKube(), "")
	h := s.Handler()
	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/nope", "", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestPowerMachine(t *testing.T) {
	fk := newFakeKube()
	fk.machines["db"] = model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/db/stop", "", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("stop: expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	fk.mu.Lock()
	got := fk.machines["db"].Spec.PowerState
	fk.mu.Unlock()
	if got != "Stopped" {
		t.Fatalf("expected powerState Stopped, got %q", got)
	}
}

func TestSetMachinePriority(t *testing.T) {
	fk := newFakeKube()
	fk.machines["db"] = model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/db/priority", "", setPriorityRequest{Priority: 20})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	fk.mu.Lock()
	got := fk.machines["db"].Spec.Priority
	fk.mu.Unlock()
	if got != 20 {
		t.Fatalf("expected priority 20, got %d", got)
	}
}

func TestSetMachinePriorityAllowsNegative(t *testing.T) {
	fk := newFakeKube()
	fk.machines["db"] = model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}, Spec: model.MachineSpec{Priority: 5}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/db/priority", "", setPriorityRequest{Priority: -3})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	fk.mu.Lock()
	got := fk.machines["db"].Spec.Priority
	fk.mu.Unlock()
	if got != -3 {
		t.Fatalf("expected priority -3 (no fixed range, same as kaironctl edit machine), got %d", got)
	}
}

func TestDeleteMachine(t *testing.T) {
	fk := newFakeKube()
	fk.machines["db"] = model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()
	rr := doJSON(t, h, http.MethodDelete, "/api/v1/machines/default/db", "", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
	fk.mu.Lock()
	_, ok := fk.machines["db"]
	fk.mu.Unlock()
	if ok {
		t.Fatalf("expected db to be deleted")
	}
}

func TestCreateMigrationDefaultsAndAutoName(t *testing.T) {
	s := newTestServer(t, newFakeKube(), "")
	h := s.Handler()
	rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations", "", createMigrationRequest{Machine: "db", TargetNode: "worker-2"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var out model.MachineMigration
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Metadata.Name == "" || !strings.HasPrefix(out.Metadata.Name, "migration-db-") {
		t.Fatalf("expected auto-generated name prefixed migration-db-, got %q", out.Metadata.Name)
	}
	if out.Spec.Strategy != "auto" || out.Spec.Mode != "pre-copy" {
		t.Fatalf("expected default strategy/mode applied, got %+v", out.Spec)
	}
}

func TestCreateMigrationAdaptiveStrategy(t *testing.T) {
	s := newTestServer(t, newFakeKube(), "")
	h := s.Handler()
	req := createMigrationRequest{Machine: "db", Strategy: "adaptive", Adaptive: &model.MachineMigrationAdaptiveSpec{Compression: &model.MigrationCompression{Algorithm: "zstd", Level: 3}}}
	rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations", "", req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var out model.MachineMigration
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Spec.Strategy != "live" || out.Spec.Adaptive == nil || out.Spec.MultifdChannels != 2 {
		t.Fatalf("spec = %+v", out.Spec)
	}
	for _, bad := range []createMigrationRequest{
		{Machine: "db", Strategy: "cold", Adaptive: &model.MachineMigrationAdaptiveSpec{}},
		{Machine: "db", Strategy: "adaptive", Adaptive: &model.MachineMigrationAdaptiveSpec{ProgressTimeout: "soon"}},
	} {
		if rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations", "", bad); rr.Code != http.StatusBadRequest {
			t.Errorf("%+v: expected 400, got %d", bad, rr.Code)
		}
	}
}

func TestCreateMigrationRequiresMachine(t *testing.T) {
	s := newTestServer(t, newFakeKube(), "")
	h := s.Handler()
	rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations", "", createMigrationRequest{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestEvacuateCreatesOneMigrationPerAssignedMachine(t *testing.T) {
	fk := newFakeKube()
	fk.machines["a"] = model.Machine{Metadata: model.ObjectMeta{Name: "a", Namespace: "default"}, Spec: model.MachineSpec{NodeName: "worker-1"}}
	fk.machines["b"] = model.Machine{Metadata: model.ObjectMeta{Name: "b", Namespace: "default"}, Spec: model.MachineSpec{NodeName: "worker-1"}}
	fk.machines["c"] = model.Machine{Metadata: model.ObjectMeta{Name: "c", Namespace: "default"}, Spec: model.MachineSpec{NodeName: "worker-2"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations/evacuate", "", evacuateRequest{Node: "worker-1"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out evacuateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Created) != 2 {
		t.Fatalf("expected 2 migrations created for worker-1, got %d: %+v", len(out.Created), out.Created)
	}
}

func TestRecoverMigrationRequiresNeedsRecoveryPhase(t *testing.T) {
	fk := newFakeKube()
	fk.migrations["m1"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m1", Namespace: "default"}, Status: model.MachineMigrationStatus{Phase: "Running"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations/default/m1/recover", "", recoverRequest{
		Action: "ForceAbort", AcknowledgedDiagnosis: "Unknown", Reason: "test",
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for non-NeedsRecovery phase, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRecoverMigrationRequiresAllFields(t *testing.T) {
	s := newTestServer(t, newFakeKube(), "")
	h := s.Handler()
	rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations/default/m1/recover", "", recoverRequest{Action: "ForceAbort"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 missing diagnosis/reason, got %d", rr.Code)
	}
}

func TestRecoverMigrationAppliesPatchWhenValid(t *testing.T) {
	fk := newFakeKube()
	fk.migrations["m1"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m1", Namespace: "default"}, Status: model.MachineMigrationStatus{Phase: "NeedsRecovery"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations/default/m1/recover", "", recoverRequest{
		Action: "ForceAbort", AcknowledgedDiagnosis: "Unknown", Reason: "operator drill",
	})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	fk.mu.Lock()
	spec := fk.migrations["m1"].Spec.Recovery
	fk.mu.Unlock()
	if spec == nil || spec.Action != "ForceAbort" || spec.Reason != "operator drill" {
		t.Fatalf("expected recovery spec patched, got %+v", spec)
	}
}

func TestCancelMigrationRequiresStartingOrRunningPhase(t *testing.T) {
	fk := newFakeKube()
	fk.migrations["m1"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m1", Namespace: "default"}, Status: model.MachineMigrationStatus{Phase: "Cutover", EffectiveStrategy: "live"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations/default/m1/cancel", "", struct{}{})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 once phase has moved past Starting/Running, got %d: %s", rr.Code, rr.Body.String())
	}
	fk.mu.Lock()
	cancel := fk.migrations["m1"].Spec.Cancel
	fk.mu.Unlock()
	if cancel {
		t.Fatal("expected spec.cancel to stay false when the request is rejected")
	}
}

func TestCancelMigrationRequiresLiveStrategy(t *testing.T) {
	fk := newFakeKube()
	fk.migrations["m1"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m1", Namespace: "default"}, Status: model.MachineMigrationStatus{Phase: "Running", EffectiveStrategy: "cold"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations/default/m1/cancel", "", struct{}{})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a cold-strategy migration, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCancelMigrationRefusedAfterPostCopyStarted(t *testing.T) {
	fk := newFakeKube()
	fk.migrations["m1"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m1", Namespace: "default"}, Status: model.MachineMigrationStatus{
		Phase: "Running", EffectiveStrategy: "live",
		Adaptive: &model.MachineMigrationAdaptiveStatus{Stage: model.AdaptiveStagePostCopy, PostCopyStarted: true},
	}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations/default/m1/cancel", "", struct{}{})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 once post-copy has started, got %d: %s", rr.Code, rr.Body.String())
	}
	fk.mu.Lock()
	cancel := fk.migrations["m1"].Spec.Cancel
	fk.mu.Unlock()
	if cancel {
		t.Fatal("expected spec.cancel to stay false after post-copy started")
	}
}

func TestCancelMigrationAppliesPatchWhenValid(t *testing.T) {
	fk := newFakeKube()
	fk.migrations["m1"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m1", Namespace: "default"}, Status: model.MachineMigrationStatus{Phase: "Running", EffectiveStrategy: "live"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/migrations/default/m1/cancel", "", struct{}{})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	fk.mu.Lock()
	cancel := fk.migrations["m1"].Spec.Cancel
	fk.mu.Unlock()
	if !cancel {
		t.Fatal("expected spec.cancel to be patched true")
	}
}

func TestCreateSnapshot(t *testing.T) {
	s := newTestServer(t, newFakeKube(), "")
	h := s.Handler()
	rr := doJSON(t, h, http.MethodPost, "/api/v1/snapshots", "", createSnapshotRequest{Machine: "db"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListSnapshots(t *testing.T) {
	fk := newFakeKube()
	fk.snapshots["s1"] = model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "s1", Namespace: "default"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()
	rr := doJSON(t, h, http.MethodGet, "/api/v1/snapshots", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var items []model.MachineSnapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 1 || items[0].Metadata.Name != "s1" {
		t.Fatalf("expected one snapshot named s1, got %+v", items)
	}
}

func TestCreateRestoreRequiresSnapshotAndTargetClaim(t *testing.T) {
	s := newTestServer(t, newFakeKube(), "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/restores", "", createRestoreRequest{TargetClaimName: "restored-pvc"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no snapshotName, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doJSON(t, h, http.MethodPost, "/api/v1/restores", "", createRestoreRequest{SnapshotName: "s1"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no targetClaimName, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateRestore(t *testing.T) {
	fk := newFakeKube()
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/restores", "", createRestoreRequest{SnapshotName: "s1", TargetClaimName: "restored-pvc"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var out model.MachineSnapshotRestore
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Metadata.Name == "" {
		t.Fatal("expected a generated name when Name is left empty")
	}
	if out.Spec.SnapshotName != "s1" || out.Spec.TargetClaimName != "restored-pvc" {
		t.Fatalf("unexpected spec: %+v", out.Spec)
	}
	fk.mu.Lock()
	_, ok := fk.restores[out.Metadata.Name]
	fk.mu.Unlock()
	if !ok {
		t.Fatal("expected the restore to be persisted")
	}
}

func TestListRestores(t *testing.T) {
	fk := newFakeKube()
	fk.restores["r1"] = model.MachineSnapshotRestore{Metadata: model.ObjectMeta{Name: "r1", Namespace: "default"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()
	rr := doJSON(t, h, http.MethodGet, "/api/v1/restores", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var items []model.MachineSnapshotRestore
	if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 1 || items[0].Metadata.Name != "r1" {
		t.Fatalf("expected one restore named r1, got %+v", items)
	}
}

func TestDeleteRestore(t *testing.T) {
	fk := newFakeKube()
	fk.restores["r1"] = model.MachineSnapshotRestore{Metadata: model.ObjectMeta{Name: "r1", Namespace: "default"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodDelete, "/api/v1/restores/default/r1", "", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	fk.mu.Lock()
	_, ok := fk.restores["r1"]
	fk.mu.Unlock()
	if ok {
		t.Fatal("expected r1 to be deleted")
	}

	rr = doJSON(t, h, http.MethodDelete, "/api/v1/restores/default/nope", "", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a nonexistent restore, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListQuotasBudgetsMachineSetsInstanceTypesMigrationPolicies(t *testing.T) {
	fk := newFakeKube()
	fk.quotas["q1"] = model.MachineQuota{Metadata: model.ObjectMeta{Name: "q1", Namespace: "default"}}
	fk.budgets["b1"] = model.MachineDisruptionBudget{Metadata: model.ObjectMeta{Name: "b1", Namespace: "default"}}
	fk.machineSets["ms1"] = model.MachineSet{Metadata: model.ObjectMeta{Name: "ms1", Namespace: "default"}}
	fk.instanceTypes["it1"] = model.MachineInstanceType{Metadata: model.ObjectMeta{Name: "it1", Namespace: "default"}}
	fk.migrationPolicies["mp1"] = model.MigrationPolicy{Metadata: model.ObjectMeta{Name: "mp1", Namespace: "default"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	cases := []struct {
		path string
		want string
	}{
		{"/api/v1/quotas", "q1"},
		{"/api/v1/disruption-budgets", "b1"},
		{"/api/v1/machinesets", "ms1"},
		{"/api/v1/instancetypes", "it1"},
		{"/api/v1/migration-policies", "mp1"},
	}
	for _, c := range cases {
		rr := doJSON(t, h, http.MethodGet, c.path, "", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", c.path, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), c.want) {
			t.Fatalf("%s: expected body to contain %q, got %s", c.path, c.want, rr.Body.String())
		}
	}
}

// TestListNetworkPoliciesAndSecurityGroups covers the two routes that
// close kairon-ui's last CRD visibility gap: until now, MachineNetworkPolicy
// and NetworkSecurityGroup -- the objects that actually drive FluxVM's
// eBPF/TC enforcement -- had no dashboard/REST route at all, unlike every
// other kairon.zyvor.dev kind.
func TestListNetworkPoliciesAndSecurityGroups(t *testing.T) {
	fk := newFakeKube()
	fk.networkPolicies["np1"] = model.MachineNetworkPolicy{Metadata: model.ObjectMeta{Name: "np1", Namespace: "default"}}
	fk.securityGroups["sg1"] = model.NetworkSecurityGroup{Metadata: model.ObjectMeta{Name: "sg1", Namespace: "default"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	cases := []struct {
		path string
		want string
	}{
		{"/api/v1/network-policies", "np1"},
		{"/api/v1/security-groups", "sg1"},
	}
	for _, c := range cases {
		rr := doJSON(t, h, http.MethodGet, c.path, "", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", c.path, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), c.want) {
			t.Fatalf("%s: expected body to contain %q, got %s", c.path, c.want, rr.Body.String())
		}
	}
}

func TestDeleteMachineSet(t *testing.T) {
	fk := newFakeKube()
	fk.machineSets["ms1"] = model.MachineSet{Metadata: model.ObjectMeta{Name: "ms1", Namespace: "default"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodDelete, "/api/v1/machinesets/default/ms1", "", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, ok := fk.machineSets["ms1"]; ok {
		t.Fatal("expected ms1 to be deleted from the fake store")
	}

	rr = doJSON(t, h, http.MethodDelete, "/api/v1/machinesets/default/nope", "", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("delete missing: expected 404, got %d", rr.Code)
	}
}

func TestScaleMachineSet(t *testing.T) {
	fk := newFakeKube()
	fk.machineSets["ms1"] = model.MachineSet{
		Metadata: model.ObjectMeta{Name: "ms1", Namespace: "default"},
		Spec:     model.MachineSetSpec{Replicas: 3},
	}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPatch, "/api/v1/machinesets/default/ms1/scale", "", map[string]any{"replicas": 5})
	if rr.Code != http.StatusOK {
		t.Fatalf("scale: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"replicas":5`) {
		t.Fatalf("scale: expected response body to reflect replicas=5, got %s", rr.Body.String())
	}
	if fk.machineSets["ms1"].Spec.Replicas != 5 {
		t.Fatalf("expected ms1.spec.replicas to be 5 in the fake store, got %d", fk.machineSets["ms1"].Spec.Replicas)
	}

	// Scaling down to zero is a legitimate way to pause a MachineSet
	// without deleting it, so 0 must round-trip too.
	rr = doJSON(t, h, http.MethodPatch, "/api/v1/machinesets/default/ms1/scale", "", map[string]any{"replicas": 0})
	if rr.Code != http.StatusOK {
		t.Fatalf("scale to zero: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if fk.machineSets["ms1"].Spec.Replicas != 0 {
		t.Fatalf("expected ms1.spec.replicas to be 0 in the fake store, got %d", fk.machineSets["ms1"].Spec.Replicas)
	}

	rr = doJSON(t, h, http.MethodPatch, "/api/v1/machinesets/default/ms1/scale", "", map[string]any{"replicas": -1})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("negative replicas: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, h, http.MethodPatch, "/api/v1/machinesets/default/nope/scale", "", map[string]any{"replicas": 1})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("scale missing: expected 404, got %d", rr.Code)
	}
}

func TestListAndSuspendMachineSnapshotSchedule(t *testing.T) {
	fk := newFakeKube()
	fk.snapshotSchedules["daily"] = model.MachineSnapshotSchedule{
		Metadata: model.ObjectMeta{Name: "daily", Namespace: "default"},
		Spec:     model.MachineSnapshotScheduleSpec{Selector: map[string]string{"backup": "true"}, IntervalSeconds: 86400},
	}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/snapshot-schedules", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "daily") {
		t.Fatalf("list: expected body to contain %q, got %s", "daily", rr.Body.String())
	}

	rr = doJSON(t, h, http.MethodPatch, "/api/v1/snapshot-schedules/default/daily/suspend", "", map[string]any{"suspend": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("suspend: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !fk.snapshotSchedules["daily"].Spec.Suspend {
		t.Fatal("expected daily.spec.suspend to be true after suspending")
	}

	rr = doJSON(t, h, http.MethodPatch, "/api/v1/snapshot-schedules/default/daily/suspend", "", map[string]any{"suspend": false})
	if rr.Code != http.StatusOK {
		t.Fatalf("resume: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if fk.snapshotSchedules["daily"].Spec.Suspend {
		t.Fatal("expected daily.spec.suspend to be false after resuming")
	}

	rr = doJSON(t, h, http.MethodPatch, "/api/v1/snapshot-schedules/default/nope/suspend", "", map[string]any{"suspend": true})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("suspend missing: expected 404, got %d", rr.Code)
	}
}

func TestListAndGetMigrations(t *testing.T) {
	fk := newFakeKube()
	fk.migrations["m1"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m1", Namespace: "default"}, Status: model.MachineMigrationStatus{Phase: "Running"}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/migrations", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var items []model.MachineMigration
	if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(items) != 1 || items[0].Metadata.Name != "m1" {
		t.Fatalf("expected one migration named m1, got %+v", items)
	}

	rr = doJSON(t, h, http.MethodGet, "/api/v1/migrations/default/m1", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got model.MachineMigration
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.Status.Phase != "Running" {
		t.Fatalf("expected phase Running, got %q", got.Status.Phase)
	}

	rr = doJSON(t, h, http.MethodGet, "/api/v1/migrations/default/nope", "", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get missing: expected 404, got %d", rr.Code)
	}
}

func TestListNodes(t *testing.T) {
	fk := newFakeKube()
	fk.nodes = []model.Node{{Metadata: model.ObjectMeta{Name: "worker-1"}}, {Metadata: model.ObjectMeta{Name: "worker-2"}}}
	s := newTestServer(t, fk, "")
	h := s.Handler()
	rr := doJSON(t, h, http.MethodGet, "/api/v1/nodes", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var items []model.Node
	if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(items))
	}
}

func TestOverviewAggregatesCounts(t *testing.T) {
	fk := newFakeKube()
	fk.machines["a"] = model.Machine{Metadata: model.ObjectMeta{Name: "a"}, Status: model.MachineStatus{Phase: "Running"}}
	fk.machines["b"] = model.Machine{Metadata: model.ObjectMeta{Name: "b"}, Status: model.MachineStatus{Phase: "Running"}}
	fk.migrations["m1"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m1"}, Status: model.MachineMigrationStatus{Phase: "NeedsRecovery"}}
	fk.migrations["m2"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m2"}, Status: model.MachineMigrationStatus{Phase: "Succeeded"}}
	// Cancelled is a real terminal phase (isNonTerminalMigrationPhase),
	// like Succeeded/Failed/Blocked -- it must not inflate Active.
	fk.migrations["m3"] = model.MachineMigration{Metadata: model.ObjectMeta{Name: "m3"}, Status: model.MachineMigrationStatus{Phase: "Cancelled"}}
	fk.nodes = []model.Node{{Metadata: model.ObjectMeta{Name: "worker-1"}}}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/overview", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out overviewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Machines.Total != 2 || out.Machines.ByPhase["Running"] != 2 {
		t.Fatalf("expected 2 running machines, got %+v", out.Machines)
	}
	if out.Migrations.Total != 3 || out.Migrations.NeedsRecovery != 1 || out.Migrations.Active != 1 {
		t.Fatalf("expected 1 NeedsRecovery / 1 active of 3 total migrations (Cancelled excluded from Active), got %+v", out.Migrations)
	}
	if out.Nodes != 1 {
		t.Fatalf("expected 1 node, got %d", out.Nodes)
	}
}

func TestServeWebFallsBackToIndexAndBlocksTraversal(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "webdir")
	secret := filepath.Join(parent, "secret")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir webdir: %v", err)
	}
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatalf("mkdir secret: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>kairon-ui</html>"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatalf("write app.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secret, "outside.txt"), []byte("do not serve me"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	s := &Server{Kube: mustKubeClient(t), WebDir: dir}
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/does-not-exist", "", nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "kairon-ui") {
		t.Fatalf("expected SPA fallback to index.html, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doJSON(t, h, http.MethodGet, "/app.js", "", nil)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") == "" {
		t.Fatalf("expected app.js served with a Content-Type, got %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}

	// http.ServeMux itself already cleans ".." out of the request path and
	// redirects before dispatch, so a request through the full Handler()
	// never actually reaches serveWeb's own guard with a raw ".." path --
	// call it directly to exercise that guard specifically, the same way
	// an attacker who found a way to smuggle an uncleaned path to this
	// handler (e.g. a future refactor that stops using http.ServeMux)
	// would be stopped by serveWeb itself, not just by the mux in front of it.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = "/../secret/outside.txt"
	rr = httptest.NewRecorder()
	s.serveWeb(rr, req)
	if strings.Contains(rr.Body.String(), "do not serve me") {
		t.Fatalf("path traversal was not blocked: %s", rr.Body.String())
	}
}

func mustKubeClient(t *testing.T) *kube.Client {
	t.Helper()
	kc, err := kube.New("http://127.0.0.1:0", "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	return kc
}
