// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/mcp"
	"github.com/zyvorai/kairon/internal/model"
)

type fakeKairon struct {
	mu      sync.Mutex
	patches []string
}

func (f *fakeKairon) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"web","namespace":"default"},"spec":{"powerState":"Running","resources":{"cpu":"1","memory":"1Gi"}},"status":{"phase":"Running","nodeName":"n1","guestIP":"10.0.0.5"}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/web":
			_, _ = io.WriteString(w, `{"metadata":{"name":"web","namespace":"default"},"spec":{"resources":{"cpu":"1","memory":"1Gi"}},"status":{"phase":"Running"}}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/web":
			b, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.patches = append(f.patches, string(b))
			f.mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/machines/default/web/network-drops":
			if r.Header.Get("Authorization") != "Bearer ui-secret" || r.URL.Query().Get("limit") != "5" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"items":[{"reason":"dns_deny","packets":3}]}`)
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func callMCP(t *testing.T, allowWrite bool, srvURL string, lines ...string) map[string]string {
	t.Helper()
	s := mcp.NewServer("kairon", "test", allowWrite)
	s.Add(kaironTools(&Options{Namespace: "default"}, func() (*kube.Client, error) { return kube.New(srvURL, "", "", false) })...)
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var resp struct {
			ID     int `json:"id"`
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool              `json:"isError"`
				Tools   []json.RawMessage `json:"tools"`
			} `json:"result"`
			Error *struct{ Message string } `json:"error"`
		}
		if err := json.Unmarshal([]byte(l), &resp); err != nil {
			t.Fatal(err)
		}
		key := string(rune('0' + resp.ID))
		switch {
		case resp.Error != nil:
			got[key] = "rpc-error: " + resp.Error.Message
		case resp.Result.Tools != nil:
			got[key] = l
		case len(resp.Result.Content) > 0:
			prefix := ""
			if resp.Result.IsError {
				prefix = "tool-error: "
			}
			got[key] = prefix + resp.Result.Content[0].Text
		}
	}
	return got
}

func TestMCPReadTools(t *testing.T) {
	f := &fakeKairon{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "ui-secret")

	got := callMCP(t, false, srv.URL,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_machines","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_machine","arguments":{"name":"web"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"machine_network","arguments":{"name":"web","kind":"network-drops","limit":5}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"machine_network","arguments":{"name":"web","kind":"bogus"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"machine_edge_identity","arguments":{"name":"web"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"get_machine","arguments":{"name":"web","extra":1}}}`,
	)
	if !strings.Contains(got["1"], `"guestIP": "10.0.0.5"`) || !strings.Contains(got["1"], `"node": "n1"`) {
		t.Fatalf("list_machines: %s", got["1"])
	}
	if !strings.Contains(got["2"], `"phase": "Running"`) {
		t.Fatalf("get_machine: %s", got["2"])
	}
	if !strings.Contains(got["3"], "dns_deny") {
		t.Fatalf("machine_network: %s", got["3"])
	}
	if !strings.HasPrefix(got["4"], "tool-error: kind must be") {
		t.Fatalf("bad kind: %s", got["4"])
	}
	if !strings.Contains(got["5"], `"identity"`) {
		t.Fatalf("identity: %s", got["5"])
	}
	if !strings.HasPrefix(got["6"], "tool-error: invalid arguments") {
		t.Fatalf("unknown field: %s", got["6"])
	}
}

func TestMCPWriteToolsAreGated(t *testing.T) {
	f := &fakeKairon{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	list := callMCP(t, false, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)["1"]
	for _, name := range []string{"set_power_state", "create_snapshot", "network_capture"} {
		if strings.Contains(list, name) {
			t.Fatalf("%s listed without --allow-write", name)
		}
	}
	denied := callMCP(t, false, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"set_power_state","arguments":{"name":"web","state":"Stopped"}}}`)["1"]
	if !strings.Contains(denied, "--allow-write") || len(f.patches) != 0 {
		t.Fatalf("write not gated: %s patches=%v", denied, f.patches)
	}

	got := callMCP(t, true, srv.URL,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"set_power_state","arguments":{"name":"web","state":"Stopped"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"set_power_state","arguments":{"name":"web","state":"Off"}}}`,
	)
	if !strings.Contains(got["1"], "powerState set to Stopped") || len(f.patches) != 1 || !strings.Contains(f.patches[0], `"powerState":"Stopped"`) {
		t.Fatalf("power: %s patches=%v", got["1"], f.patches)
	}
	if !strings.HasPrefix(got["2"], "tool-error: state must be") {
		t.Fatalf("bad state: %s", got["2"])
	}
}

func TestMCPVolumeTools(t *testing.T) {
	var created []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/db":
			_, _ = io.WriteString(w, `{"metadata":{"name":"db","namespace":"default","annotations":{"kairon.zyvor.dev/atlas-volumes":"{\"root\":{\"volumeID\":\"vol-1\",\"nativeID\":\"rbd:vms/db-root\",\"mode\":\"rbd\",\"phase\":\"Ready\"}}"}},
				"spec":{"volumes":[{"name":"root","atlas":{"size":"20Gi","mode":"rbd","pool":"vms"}},{"name":"data","claimName":"db-data"}]}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshots":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			created = append(created, string(b))
			mu.Unlock()
			_, _ = w.Write(b)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinesnapshots/s1":
			_, _ = io.WriteString(w, `{"metadata":{"name":"s1","namespace":"default"},"spec":{"machineName":"db"},"status":{"phase":"Succeeded","readyToUse":true,"volumeSnapshots":[{"volumeName":"root","volumeSnapshotName":"x","atlasSnapshotID":"snap-9"}]}}`)
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got := callMCP(t, true, srv.URL,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"machine_volumes","arguments":{"name":"db"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"snapshot_volume","arguments":{"name":"db","volume":"root","snapshotName":"s1"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"snapshot_volume","arguments":{"name":"db","volume":"nope"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_machine_snapshot","arguments":{"name":"s1"}}}`,
	)
	for _, want := range []string{`"source": "atlas-rbd"`, `"nativeID": "rbd:vms/db-root"`, `"phase": "Ready"`, `"claimName": "db-data"`} {
		if !strings.Contains(got["1"], want) {
			t.Fatalf("machine_volumes missing %s: %s", want, got["1"])
		}
	}
	if !strings.Contains(got["2"], "created for volume root") || len(created) != 1 || !strings.Contains(created[0], `"volumeNames":["root"]`) {
		t.Fatalf("snapshot_volume: %s created=%v", got["2"], created)
	}
	if !strings.HasPrefix(got["3"], "tool-error: machine default/db has no volume") || len(created) != 1 {
		t.Fatalf("unknown volume: %s", got["3"])
	}
	if !strings.Contains(got["4"], `"atlasSnapshotID": "snap-9"`) {
		t.Fatalf("get_machine_snapshot: %s", got["4"])
	}

	list := callMCP(t, false, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)["1"]
	if strings.Contains(list, "snapshot_volume") || !strings.Contains(list, "machine_volumes") {
		t.Fatalf("snapshot_volume must be write-gated: %s", list)
	}
}

func TestMCPPoolTools(t *testing.T) {
	var claimed, deleted []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machinepools":
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"agents"},"spec":{"replicas":4},"status":{"readyReplicas":3,"claimed":2}}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machineclaims":
			b, _ := io.ReadAll(r.Body)
			claimed = append(claimed, string(b))
			_, _ = w.Write(b)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machineclaims/job-1":
			_, _ = io.WriteString(w, `{"metadata":{"name":"job-1"},"spec":{"poolName":"agents"},"status":{"phase":"Bound","machineName":"agents-ab12","bindMillis":180}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machineclaims/job-1":
			deleted = append(deleted, "claim/job-1")
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/web-x":
			_, _ = io.WriteString(w, `{"metadata":{"name":"web-x","labels":{"kairon.zyvor.dev/machineset":"web"}}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/solo":
			_, _ = io.WriteString(w, `{"metadata":{"name":"solo"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/solo":
			deleted = append(deleted, "machine/solo")
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got := callMCP(t, true, srv.URL,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_machine_pools","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"claim_machine","arguments":{"pool":"agents","name":"job-1","labels":{"team":"ml"},"retain":true,"waitSeconds":5,"egress":{"allowFqdns":["pypi.org"],"allowPorts":["443"]}}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"release_claim","arguments":{"name":"job-1"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"delete_machine","arguments":{"name":"web-x"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"delete_machine","arguments":{"name":"solo"}}}`,
	)
	if !strings.Contains(got["1"], `"ready": 3`) || !strings.Contains(got["1"], `"claimed": 2`) {
		t.Fatalf("list_machine_pools: %s", got["1"])
	}
	if !strings.Contains(got["2"], `"machine": "agents-ab12"`) || len(claimed) != 1 ||
		!strings.Contains(claimed[0], `"poolName":"agents"`) || !strings.Contains(claimed[0], `"reclaimPolicy":"Retain"`) || !strings.Contains(claimed[0], `"team":"ml"`) ||
		!strings.Contains(claimed[0], `"egress":{"allowPorts":["443"],"allowFqdns":["pypi.org"]}`) {
		t.Fatalf("claim_machine: %s body=%v", got["2"], claimed)
	}
	if !strings.HasPrefix(got["4"], "tool-error:") || !strings.Contains(got["4"], "MachineSet web") {
		t.Fatalf("delete_machine on a MachineSet replica must refuse: %s", got["4"])
	}
	sort.Strings(deleted)
	if strings.Join(deleted, ",") != "claim/job-1,machine/solo" {
		t.Fatalf("deleted = %v", deleted)
	}

	list := callMCP(t, false, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)["1"]
	for _, w := range []string{"claim_machine", "release_claim", "delete_machine", "fork_machine"} {
		if strings.Contains(list, w) {
			t.Fatalf("%s must be write-gated", w)
		}
	}
	if !strings.Contains(list, "list_machine_pools") {
		t.Fatalf("list_machine_pools missing: %s", list)
	}
}

func TestWriteVolumesTable(t *testing.T) {
	var b bytes.Buffer
	if err := writeVolumes(&b, []volumeInfo{{Name: "root", Source: "atlas-pvc", ClaimName: "c", Size: "10Gi", Phase: "Ready", NativeID: "pvc:c"}}, "table"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "NAME") || !strings.Contains(b.String(), "atlas-pvc") {
		t.Fatalf("table: %s", b.String())
	}
	if err := writeVolumes(&b, nil, "yaml"); err == nil {
		t.Fatal("expected error for unknown output")
	}
}

func TestMCPHotplugTools(t *testing.T) {
	var patches []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const path = "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/fw"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == path:
			_, _ = io.WriteString(w, `{"metadata":{"name":"fw","namespace":"default","uid":"u1","resourceVersion":"7"},
				"spec":{"network":{"mode":"tap","bridge":"br0","extraInterfaces":[{"name":"lan","bridge":"br-lan"}]},"disks":[{"name":"data","claimName":"c1"}]}}`)
		case r.Method == http.MethodPatch && r.URL.Path == path:
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			patches = append(patches, string(b))
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got := callMCP(t, true, srv.URL,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"machine_disk","arguments":{"name":"fw","action":"attach","disk":"logs","claim":"c2"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"machine_disk","arguments":{"name":"fw","action":"attach","disk":"data","claim":"c3"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"machine_nic","arguments":{"name":"fw","action":"remove","nic":"lan"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"machine_nic","arguments":{"name":"fw","action":"add","nic":"dmz"}}}`,
	)
	if !strings.Contains(got["1"], "disk logs attach requested") || !strings.HasPrefix(got["2"], "tool-error: disk \"data\" is already") {
		t.Fatalf("machine_disk: %q / %q", got["1"], got["2"])
	}
	if !strings.Contains(got["3"], "nic lan remove requested") || !strings.HasPrefix(got["4"], "tool-error: bridge is required") {
		t.Fatalf("machine_nic: %q / %q", got["3"], got["4"])
	}
	sort.Strings(patches)
	if len(patches) != 2 ||
		!strings.Contains(patches[0], `"disks":[{"name":"data","claimName":"c1"},{"name":"logs","claimName":"c2"}]`) ||
		!strings.Contains(patches[0], `"resourceVersion":"7"`) ||
		!strings.Contains(patches[1], `"extraInterfaces":[]`) {
		t.Fatalf("patches = %v", patches)
	}
}

func TestMCPBackupTools(t *testing.T) {
	var created []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/apis/kairon.zyvor.dev/v1/namespaces/default/"
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && (r.URL.Path == base+"machinebackups" || r.URL.Path == base+"machinebackuprestores"):
			b, _ := io.ReadAll(r.Body)
			created = append(created, string(b))
			_, _ = w.Write(b)
		case r.Method == http.MethodGet && r.URL.Path == base+"machinebackups":
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"nightly"},"spec":{"machineName":"web"},"status":{"phase":"Succeeded","message":"","disk":{"phase":"Succeeded","message":"","quiesced":true}}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == base+"machinebackuprestores":
			_, _ = io.WriteString(w, `{"items":[]}`)
		case r.Method == http.MethodDelete && r.URL.Path == base+"machinebackups/nightly":
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got := callMCP(t, true, srv.URL,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"machine_backup","arguments":{"name":"web","action":"create","backup":"nightly","quiesce":"required","atlas":true}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_backups","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"machine_backup","arguments":{"name":"web","action":"restore"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"machine_backup","arguments":{"name":"web","action":"restore","backup":"nightly"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"machine_backup","arguments":{"name":"web","action":"delete","backup":"nightly"}}}`,
	)
	if !strings.Contains(got["1"], "machinebackup default/nightly created") || !strings.Contains(got["2"], `"quiesced": true`) {
		t.Fatalf("create/list: %q / %q", got["1"], got["2"])
	}
	if !strings.HasPrefix(got["3"], "tool-error: backup is required") || !strings.Contains(got["4"], "once machine web is halted") || !strings.Contains(got["5"], "deleted") {
		t.Fatalf("restore/delete: %q / %q / %q", got["3"], got["4"], got["5"])
	}
	sort.Strings(created) // MachineBackup sorts before MachineBackupRestore by kind
	if len(created) != 2 ||
		!strings.Contains(created[0], `"quiesce":"required"`) || !strings.Contains(created[0], `"atlas":{}`) ||
		!strings.Contains(created[1], `"backupName":"nightly"`) || !strings.Contains(created[1], `"machineName":"web"`) {
		t.Fatalf("created = %v", created)
	}
}

func TestMCPForkMachine(t *testing.T) {
	var created []model.Machine
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		const base = "/apis/kairon.zyvor.dev/v1/namespaces/default/machines"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base+"/base":
			_, _ = io.WriteString(w, `{"metadata":{"name":"base","labels":{"kairon.zyvor.dev/machineset":"web"}},"spec":{"nodeName":"n1","image":{"path":"/i.qcow2"},"resources":{"cpu":"1","memory":"512Mi"},"runtime":{"backend":"flux-vm"}},"status":{"phase":"Running"}}`)
		case r.Method == http.MethodGet && r.URL.Path == base+"/busy":
			_, _ = io.WriteString(w, `{"metadata":{"name":"busy"},"spec":{"nodeName":"n1","volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"c"}}]},"status":{"phase":"Running"}}`)
		case r.Method == http.MethodPost && r.URL.Path == base:
			var m model.Machine
			_ = json.NewDecoder(r.Body).Decode(&m)
			created = append(created, m)
			_ = json.NewEncoder(w).Encode(m)
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got := callMCP(t, true, srv.URL,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fork_machine","arguments":{"name":"base","count":2,"prefix":"try","waitSeconds":0}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fork_machine","arguments":{"name":"busy","waitSeconds":0}}}`,
	)
	if !strings.Contains(got["1"], `"try-1"`) || !strings.Contains(got["1"], `"try-2"`) || len(created) != 2 {
		t.Fatalf("fork_machine: %s created=%d", got["1"], len(created))
	}
	kid := created[0]
	if kid.Spec.NodeName != "n1" || kid.Spec.Runtime.Backend != "flux-vm" ||
		kid.Metadata.Annotations[model.AnnotationForkFrom] != "base" ||
		kid.Metadata.Labels[model.LabelForkedFrom] != "base" ||
		kid.Metadata.Labels[model.AssignedNodeLabel] != "n1" ||
		kid.Metadata.Labels["kairon.zyvor.dev/machineset"] != "" {
		t.Fatalf("child = %+v", kid)
	}
	if !strings.HasPrefix(got["2"], "tool-error:") || !strings.Contains(got["2"], "volumes") {
		t.Fatalf("forking a Machine with volumes must refuse: %s", got["2"])
	}
}
