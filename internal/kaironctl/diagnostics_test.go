// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	chartpkg "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
)

// fakeCluster is a minimal Kubernetes API for the diagnostics commands.
type fakeCluster struct {
	nodes        int    // capable nodes
	controllerOK bool   // deployment ready and /readyz 200
	nodeReadyz   int    // HTTP status of kairon-node pod /readyz
	crds         bool   // CRDs installed (storage v1)
	crdStorage   string // storage version when crds
	webhookNoCA  bool
	noWorkloads  bool // controller/node Deployment and DaemonSet absent (systemd install)
	logTail      string
	requests     []string
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{nodes: 2, controllerOK: true, nodeReadyz: 200, crds: true, crdStorage: "v1", logTail: "starting\nreconciled\n"}
}

func podsJSON(names ...string) string {
	var items []string
	for _, n := range names {
		port := 32302
		if strings.Contains(n, "controller") {
			port = 32301
		}
		items = append(items, fmt.Sprintf(`{"metadata":{"name":%q},"spec":{"nodeName":"n1","containers":[{"name":"main","ports":[{"name":"health","containerPort":%d}]}]},
"status":{"phase":"Running","podIP":"10.0.0.1","containerStatuses":[{"ready":true,"restartCount":0}]}}`, n, port))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

func (c *fakeCluster) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.requests = append(c.requests, r.URL.Path)
		p := r.URL.Path
		write := func(body string) { _, _ = io.WriteString(w, body) }
		switch {
		case p == "/version":
			write(`{"gitVersion":"v1.31.2"}`)
		case p == "/api/v1/nodes":
			items := make([]string, c.nodes)
			for i := range items {
				items[i] = fmt.Sprintf(`{"metadata":{"name":"n%d","labels":{"kairon.zyvor.dev/capable":"true"}}}`, i)
			}
			write(`{"items":[` + strings.Join(items, ",") + `]}`)
		case strings.HasPrefix(p, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"):
			if !c.crds {
				http.NotFound(w, r)
				return
			}
			write(`{"spec":{"versions":[{"name":"v1","served":true,"storage":` + fmt.Sprint(c.crdStorage == "v1") + `},{"name":"v1beta1","served":true,"storage":` + fmt.Sprint(c.crdStorage == "v1beta1") + `}]}}`)
		case c.noWorkloads && strings.HasPrefix(p, "/apis/apps/v1/namespaces/kairon-system/"):
			http.NotFound(w, r)
		case p == "/apis/apps/v1/namespaces/kairon-system/deployments/kairon-controller":
			ready := 1
			if !c.controllerOK {
				ready = 0
			}
			write(fmt.Sprintf(`{"status":{"replicas":1,"readyReplicas":%d}}`, ready))
		case p == "/apis/apps/v1/namespaces/kairon-system/daemonsets/kairon-node":
			write(`{"status":{"desiredNumberScheduled":2,"numberReady":2}}`)
		case p == "/apis/apps/v1/namespaces/kairon-system/deployments/kairon-ui":
			http.NotFound(w, r)
		case p == "/api/v1/namespaces/kairon-system/pods":
			switch r.URL.Query().Get("labelSelector") {
			case "app.kubernetes.io/name=kairon-controller":
				write(podsJSON("kairon-controller-abc"))
			case "app.kubernetes.io/name=kairon-node":
				write(podsJSON("kairon-node-1", "kairon-node-2"))
			case "app.kubernetes.io/name=kairon-ui":
				write(`{"items":[]}`)
			default:
				write(podsJSON("kairon-controller-abc", "kairon-node-1", "kairon-node-2"))
			}
		case strings.HasSuffix(p, "/proxy/healthz"):
			write(`{"ok":true}`)
		case strings.HasSuffix(p, "/proxy/readyz"):
			if strings.Contains(p, "kairon-node-2") && c.nodeReadyz != 200 {
				http.Error(w, `{"ready":false}`, c.nodeReadyz)
				return
			}
			if strings.Contains(p, "kairon-controller") && !c.controllerOK {
				http.Error(w, `{"ready":false}`, http.StatusServiceUnavailable)
				return
			}
			write(`{"ready":true}`)
		case strings.HasPrefix(p, "/api/v1/namespaces/kairon-system/services/") && strings.HasSuffix(p, "/proxy/readyz"):
			http.NotFound(w, r)
		case p == "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/kairon-controller-webhook":
			if c.webhookNoCA {
				write(`{"webhooks":[{"name":"machinequota.kairon.zyvor.dev","failurePolicy":"Fail","clientConfig":{}}]}`)
				return
			}
			http.NotFound(w, r)
		case p == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			write(`{"items":[]}`)
		case p == "/apis/kairon.zyvor.dev/v1/machines":
			write(`{"kind":"MachineList","items":[{"metadata":{"name":"vm1"},"spec":{"cloudInit":{"runCmd":["echo password=hunter2"]},"secretToken":"tok-123"}}]}`)
		case strings.HasPrefix(p, "/apis/kairon.zyvor.dev/v1/"):
			write(`{"items":[]}`)
		case p == "/api/v1/namespaces/kairon-system/events":
			write(`{"items":[
{"metadata":{"namespace":"kairon-system"},"type":"Warning","reason":"BackOff","message":"restarting","involvedObject":{"kind":"Pod","name":"kairon-node-1"},"lastTimestamp":"2026-10-11T10:05:00Z"},
{"metadata":{"namespace":"kairon-system"},"type":"Normal","reason":"Pulled","message":"pulled image","involvedObject":{"kind":"Pod","name":"kairon-node-1"},"lastTimestamp":"2026-10-11T10:00:00Z"}]}`)
		case strings.HasSuffix(p, "/log"):
			pod := p[strings.LastIndex(p[:strings.LastIndex(p, "/log")], "/")+1 : strings.LastIndex(p, "/log")]
			for _, line := range strings.Split(strings.TrimSpace(c.logTail), "\n") {
				_, _ = fmt.Fprintf(w, "%s %s\n", line, pod)
			}
		case strings.HasPrefix(p, "/apis/apps/v1/namespaces/kairon-system/"), strings.HasPrefix(p, "/api/v1/namespaces/kairon-system/services"):
			write(`{"items":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	clearKubeEnv(t)
	t.Setenv("KAIRON_KUBE_URL", srv.URL)
	// No Helm release by default; tests that need one stub getReleaseFn.
	old := getReleaseFn
	getReleaseFn = func(*releaseRef) (*release.Release, error) { return nil, errors.New("no release") }
	t.Cleanup(func() { getReleaseFn = old })
}

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	root := NewRootCmd(&Options{Name: "kaironctl", Version: "v-test"})
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

func TestDoctorHealthy(t *testing.T) {
	c := newFakeCluster()
	c.start(t)
	out, err := runRoot(t, "doctor", "-o", "json")
	if err != nil {
		t.Fatalf("healthy cluster must exit zero: %v\n%s", err, out)
	}
	var rep doctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !rep.Healthy || rep.Fail != 0 {
		t.Fatalf("unexpected report: %+v", rep)
	}
	byName := map[string]doctorCheck{}
	for _, ck := range rep.Checks {
		byName[ck.Name] = ck
	}
	for _, n := range []string{"API server", "Capable nodes", "CRDs", "Controller", "Node agent"} {
		if byName[n].Status != checkOK {
			t.Errorf("%s: %+v", n, byName[n])
		}
	}
	if byName["Dashboard (kairon-ui)"].Status != checkSkip || byName["Admission webhook"].Status != checkSkip {
		t.Errorf("optional components must be skipped: %+v %+v", byName["Dashboard (kairon-ui)"], byName["Admission webhook"])
	}
}

func TestDoctorReportsFailuresWithHints(t *testing.T) {
	c := newFakeCluster()
	c.nodes = 0
	c.controllerOK = false
	c.nodeReadyz = 503
	c.crdStorage = "v1beta1"
	c.webhookNoCA = true
	c.start(t)
	out, err := runRoot(t, "doctor")
	if err == nil || !strings.Contains(err.Error(), "failure") {
		t.Fatalf("failing cluster must exit non-zero, got %v", err)
	}
	for _, want := range []string{"Capable nodes", "kubectl label node", "storage version is not v1", "no caBundle", "ready 0/1", "/readyz"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorPreInstallOnEmptyCluster(t *testing.T) {
	c := newFakeCluster()
	c.crds = false
	c.start(t)
	out, err := runRoot(t, "doctor", "--pre-install")
	if err != nil {
		t.Fatalf("pre-install on a clean cluster must pass: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not installed yet") {
		t.Fatalf("expected the not-installed note:\n%s", out)
	}
	// Without --pre-install the same cluster fails on the missing CRDs.
	if _, err := runRoot(t, "doctor"); err == nil {
		t.Fatal("missing CRDs must fail a normal doctor run")
	}
}

func TestDoctorUnreachableAPI(t *testing.T) {
	clearKubeEnv(t)
	t.Setenv("KAIRON_KUBE_URL", "http://127.0.0.1:1")
	out, err := runRoot(t, "doctor")
	if err == nil || !strings.Contains(out, "API server") {
		t.Fatalf("want an API server failure, got %v\n%s", err, out)
	}
}

func TestLogsPrefixesAndFilters(t *testing.T) {
	c := newFakeCluster()
	c.start(t)
	out, err := runRoot(t, "logs", "node", "--tail", "50")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[kairon-node-1] starting kairon-node-1") || !strings.Contains(out, "[kairon-node-2] reconciled kairon-node-2") {
		t.Fatalf("multi-pod logs must be prefixed:\n%s", out)
	}
	out, err = runRoot(t, "logs", "controller")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[") || !strings.Contains(out, "starting kairon-controller-abc") {
		t.Fatalf("single pod logs must be unprefixed:\n%s", out)
	}
	out, err = runRoot(t, "logs", "node", "--pod", "kairon-node-2")
	if err != nil || strings.Contains(out, "kairon-node-1") {
		t.Fatalf("--pod filter: %v\n%s", err, out)
	}
	if _, err := runRoot(t, "logs", "nonsense"); err == nil {
		t.Fatal("unknown component accepted")
	}
	if _, err := runRoot(t, "logs", "ui"); err == nil || !strings.Contains(err.Error(), "no kairon-ui pods") {
		t.Fatalf("missing pods must be reported, got %v", err)
	}
}

func TestEventsFilterAndOrder(t *testing.T) {
	c := newFakeCluster()
	c.start(t)
	out, err := runRoot(t, "events")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(out, "Pulled") > strings.Index(out, "BackOff") {
		t.Fatalf("events must be oldest first:\n%s", out)
	}
	out, err = runRoot(t, "events", "--warnings", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var evs []eventSummary
	if err := json.Unmarshal([]byte(out), &evs); err != nil || len(evs) != 1 || evs[0].Reason != "BackOff" {
		t.Fatalf("--warnings: %v %+v", err, evs)
	}
}

func readBundle(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = string(b)
	}
	return files
}

func TestSysdumpCollectsAndRedacts(t *testing.T) {
	c := newFakeCluster()
	c.start(t)
	oldVals, oldRel := releaseValuesFn, getReleaseFn
	defer func() { releaseValuesFn, getReleaseFn = oldVals, oldRel }()
	releaseValuesFn = func(*releaseRef, bool) (map[string]any, error) {
		return map[string]any{"ui": map[string]any{"enabled": true, "token": "supersecret-ui-token"}}, nil
	}
	getReleaseFn = func(*releaseRef) (*release.Release, error) {
		return &release.Release{
			Name: "kairon", Namespace: "kairon-system", Version: 2,
			Info:     &release.Info{Status: release.StatusDeployed},
			Chart:    &chartpkg.Chart{Metadata: &chartpkg.Metadata{Name: "kairon", Version: "0.8.0", AppVersion: "0.8.0"}},
			Manifest: "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ok\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: kairon-ui-session\nstringData:\n  password: manifest-secret-pw\n",
		}, nil
	}
	out := filepath.Join(t.TempDir(), "dump.tar.gz")
	if _, err := runRoot(t, "sysdump", "-o", out); err != nil {
		t.Fatal(err)
	}
	files := readBundle(t, out)
	for _, want := range []string{"version.json", "doctor.json", "helm/values.yaml", "helm/manifest.yaml", "k8s/pods.json", "k8s/events.json",
		"crs/machines.json", "logs/kairon-controller-abc/main.log", "logs/kairon-node-1/main.log", "README.txt"} {
		if _, ok := files[want]; !ok {
			t.Errorf("bundle missing %s (have %d files)", want, len(files))
		}
	}
	for name, body := range files {
		for _, secret := range []string{"supersecret-ui-token", "manifest-secret-pw", "hunter2", "tok-123"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaks %q", name, secret)
			}
		}
	}
	if !strings.Contains(files["helm/values.yaml"], "enabled: true") || !strings.Contains(files["helm/values.yaml"], redacted) {
		t.Errorf("values should keep non-secrets and mark secrets:\n%s", files["helm/values.yaml"])
	}
	if !strings.Contains(files["helm/manifest.yaml"], "name: ok") || !strings.Contains(files["helm/manifest.yaml"], "Secret document omitted") {
		t.Errorf("manifest redaction:\n%s", files["helm/manifest.yaml"])
	}
	// The destination must never be clobbered.
	if _, err := runRoot(t, "sysdump", "-o", out); err == nil {
		t.Fatal("sysdump overwrote an existing file")
	}
}

func TestSysdumpFlagsSkipSections(t *testing.T) {
	c := newFakeCluster()
	c.start(t)
	out := filepath.Join(t.TempDir(), "d.tar.gz")
	if _, err := runRoot(t, "sysdump", "-o", out, "--no-logs", "--no-crs"); err != nil {
		t.Fatal(err)
	}
	for name := range readBundle(t, out) {
		if strings.HasPrefix(name, "logs/") || strings.HasPrefix(name, "crs/") {
			t.Errorf("unexpected %s", name)
		}
	}
}

func TestRedactValue(t *testing.T) {
	in := map[string]any{
		"name":        "keepme",
		"password":    "p",
		"apiToken":    "t",
		"tlsSecret":   map[string]any{"a": "b", "n": 1.0},
		"nested":      map[string]any{"caBundle": "LS0t", "port": 443.0},
		"list":        []any{map[string]any{"clientSecret": "s", "ok": true}},
		"kind":        "Secret",
		"data":        map[string]any{"k": "v"},
		"cloudInit":   map[string]any{"runCmd": []any{"xcmd"}},
		"secretName":  "kairon-ui-session",
		"env":         []any{map[string]any{"name": "KAIRON_UI_TOKEN", "value": "envleak"}, map[string]any{"name": "LOG_LEVEL", "value": "debug"}},
		"replicaKeys": 3.0,
	}
	b, _ := json.Marshal(redactValue(in))
	s := string(b)
	for _, leak := range []string{`"p"`, `"t"`, `"b"`, `"LS0t"`, `"s"`, `"v"`, `"xcmd"`, `"envleak"`} {
		if strings.Contains(s, leak) {
			t.Errorf("leaked %s in %s", leak, s)
		}
	}
	for _, keep := range []string{`"name":"keepme"`, `"port":443`, `"ok":true`, `"replicaKeys":3`, `"value":"debug"`} {
		if !strings.Contains(s, keep) {
			t.Errorf("lost %s in %s", keep, s)
		}
	}
	if in["password"] != "p" {
		t.Error("redactValue mutated its input")
	}
}

func TestConnectivityProbes(t *testing.T) {
	c := newFakeCluster()
	c.start(t)
	out, err := runRoot(t, "connectivity", "test")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"controller kairon-controller-abc", "node agent kairon-node-1", "node agent kairon-node-2", "dashboard"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	c.nodeReadyz = 503
	out, err = runRoot(t, "connectivity", "test")
	if err == nil || !strings.Contains(out, "kairon-node-2") || !strings.Contains(out, "/readyz") {
		t.Fatalf("a failing probe must fail the test: %v\n%s", err, out)
	}
}

// lifecycleCluster is a fake API where a created Machine walks through
// phases and honours deletion.
type lifecycleCluster struct {
	phases    []string // phase returned on successive GETs (last one sticks)
	createErr bool
	created   *model_machine
	gets      int
	deleted   bool
	delGone   int // GETs that still succeed after DELETE before 404
}

type model_machine struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Image struct {
			Path string `json:"path"`
		} `json:"image"`
		TTLSeconds int64 `json:"ttlSeconds"`
	} `json:"spec"`
}

func (c *lifecycleCluster) start(t *testing.T) {
	t.Helper()
	machinePollInterval = time.Millisecond
	t.Cleanup(func() { machinePollInterval = 2 * time.Second })
	base := "/apis/kairon.zyvor.dev/v1/namespaces/default/machines"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPost && p == base:
			if c.createErr {
				http.Error(w, `{"message":"quota exceeded"}`, http.StatusForbidden)
				return
			}
			var m model_machine
			_ = json.NewDecoder(r.Body).Decode(&m)
			c.created = &m
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"metadata":{"name":"`+m.Metadata.Name+`"}}`)
		case r.Method == http.MethodGet && c.created != nil && p == base+"/"+c.created.Metadata.Name:
			if c.deleted {
				if c.delGone <= 0 {
					http.NotFound(w, r)
					return
				}
				c.delGone--
			}
			i := c.gets
			if i >= len(c.phases) {
				i = len(c.phases) - 1
			}
			c.gets++
			_, _ = io.WriteString(w, `{"metadata":{"name":"`+c.created.Metadata.Name+`"},"status":{"phase":"`+c.phases[i]+`","nodeName":"n1","guestIP":"10.0.0.9"}}`)
		case r.Method == http.MethodDelete && c.created != nil && p == base+"/"+c.created.Metadata.Name:
			c.deleted = true
			_, _ = io.WriteString(w, `{}`)
		case p == "/apis/kairon.zyvor.dev/v1/machines":
			_, _ = io.WriteString(w, `{"items":[]}`)
		default:
			// Control-plane probes: report no pods so only the lifecycle check runs.
			if p == "/api/v1/namespaces/kairon-system/pods" {
				_, _ = io.WriteString(w, `{"items":[]}`)
				return
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	clearKubeEnv(t)
	t.Setenv("KAIRON_KUBE_URL", srv.URL)
}

func TestConnectivityMachineLifecycle(t *testing.T) {
	c := &lifecycleCluster{phases: []string{"Pending", "Pending", "Running"}, delGone: 2}
	c.start(t)
	out, err := runRoot(t, "connectivity", "test", "--machine", "--image", "/images/test.qcow2", "--machine-timeout", "10s")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Machine create", "Machine running", "Running after", "node n1", "guest IP 10.0.0.9", "Machine cleanup", "deleted"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if c.created == nil || !strings.HasPrefix(c.created.Metadata.Name, "kairon-connectivity-") ||
		c.created.Metadata.Labels[connectivityLabel] != "true" || c.created.Spec.Image.Path != "/images/test.qcow2" || c.created.Spec.TTLSeconds <= 0 {
		t.Fatalf("created machine: %+v", c.created)
	}
	if !c.deleted {
		t.Fatal("test Machine was not deleted")
	}
}

func TestConnectivityMachineFailureStillCleansUp(t *testing.T) {
	c := &lifecycleCluster{phases: []string{"Pending", "Failed"}}
	c.start(t)
	out, err := runRoot(t, "connectivity", "test", "--machine", "--image", "/i", "--machine-timeout", "10s")
	if err == nil || !strings.Contains(out, "phase Failed") {
		t.Fatalf("failed Machine must fail the test: %v\n%s", err, out)
	}
	if !c.deleted || !strings.Contains(out, "deleted") {
		t.Fatalf("failed Machine must still be deleted:\n%s", out)
	}
}

func TestConnectivityMachineTimeout(t *testing.T) {
	c := &lifecycleCluster{phases: []string{"Pending"}}
	c.start(t)
	out, err := runRoot(t, "connectivity", "test", "--machine", "--image", "/i", "--machine-timeout", "50ms")
	if err == nil || !strings.Contains(out, "not Running after") || !c.deleted {
		t.Fatalf("timeout: %v deleted=%v\n%s", err, c.deleted, out)
	}
}

func TestConnectivityMachineCreateRejectedAndKeep(t *testing.T) {
	c := &lifecycleCluster{phases: []string{"Running"}, createErr: true}
	c.start(t)
	out, err := runRoot(t, "connectivity", "test", "--machine", "--image", "/i")
	if err == nil || !strings.Contains(out, "Machine create") || !strings.Contains(out, "quota") {
		t.Fatalf("rejected create: %v\n%s", err, out)
	}

	k := &lifecycleCluster{phases: []string{"Running"}}
	k.start(t)
	out, err = runRoot(t, "connectivity", "test", "--machine", "--image", "/i", "--keep")
	if err != nil || k.deleted || !strings.Contains(out, "--keep") {
		t.Fatalf("--keep: %v deleted=%v\n%s", err, k.deleted, out)
	}
	if _, err := runRoot(t, "connectivity", "test", "--machine"); err == nil || !strings.Contains(err.Error(), "--image") {
		t.Fatalf("--machine without --image must be rejected, got %v", err)
	}
}

func TestDoctorSystemdInstallWarnsInsteadOfFailing(t *testing.T) {
	c := newFakeCluster()
	c.noWorkloads = true
	c.start(t)
	out, err := runRoot(t, "doctor")
	if err != nil {
		t.Fatalf("absent workloads must warn, not fail: %v\n%s", err, out)
	}
	for _, want := range []string{"cannot verify from the API", "systemctl status kairon-controller", "systemctl status kairon-node"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if _, err := runRoot(t, "doctor", "--strict"); err == nil {
		t.Fatal("--strict must still fail on the warnings")
	}
}
