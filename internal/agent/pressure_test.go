// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func pressureAgent(t *testing.T, hostBody string) (*Agent, *[]map[string]any) {
	t.Helper()
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/host/pressure":
			_, _ = io.WriteString(w, hostBody)
		case "/v1/vms/vm-1/pressure":
			_, _ = io.WriteString(w, `{"cpu_some":{"avg10":9,"avg60":12.345,"avg300":1,"total":1},"io_some":{"avg10":0,"avg60":0.04,"avg300":0,"total":1}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fs.Close)
	var patches []map[string]any
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/nodes/n1" && r.Method == http.MethodPatch {
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			patches = append(patches, p)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(ks.Close)
	kc, _ := kube.New(ks.URL, "", "", false)
	a := &Agent{NodeName: "n1", Kube: kc, Flux: fluxvm.New(fs.URL, "")}
	running := model.Machine{Metadata: model.ObjectMeta{Name: "m1", Namespace: "prod"}}
	running.Spec.NodeName = "n1"
	running.Status.Phase = "Running"
	running.Status.RuntimeID = "vm-1"
	elsewhere := running
	elsewhere.Metadata.Name = "m2"
	elsewhere.Spec.NodeName = "n2"
	a.memory.setMachines([]model.Machine{running, elsewhere})
	return a, &patches
}

func TestPublishPressureWritesHostAndMachinePSI(t *testing.T) {
	a, patches := pressureAgent(t, `{"cpu_some":{"avg10":20,"avg60":19.86,"avg300":18,"total":1},"memory_some":{"avg10":0,"avg60":0,"avg300":0,"total":1},"io_some":{"avg10":40,"avg60":30.67,"avg300":25,"total":1}}`)
	a.publishPressure(t.Context())
	if len(*patches) != 1 {
		t.Fatalf("node patches=%d, want 1", len(*patches))
	}
	raw := (*patches)[0]["metadata"].(map[string]any)["annotations"].(map[string]any)[model.AnnotationNodePressure].(string)
	n := model.Node{Metadata: model.ObjectMeta{Annotations: map[string]string{model.AnnotationNodePressure: raw}}}
	p, ok := model.NodePressureOf(n)
	if !ok {
		t.Fatalf("annotation %q does not decode", raw)
	}
	if p.Host != (model.PSI{CPU: 19.9, IO: 30.7}) {
		t.Fatalf("host=%+v, want avg60 rounded to 0.1", p.Host)
	}
	if len(p.Machines) != 1 || p.Machines["prod/m1"] != (model.PSI{CPU: 12.3}) {
		t.Fatalf("machines=%+v, want only this node's m1", p.Machines)
	}
	if age, ok := p.Age(time.Now()); !ok || age > time.Minute {
		t.Fatalf("sampledAt=%q", p.SampledAt)
	}
}

func TestPublishPressureSkipsAHostWithoutPSI(t *testing.T) {
	a, patches := pressureAgent(t, `{"cpu_some":null,"memory_some":null,"memory_full":null,"io_some":null,"io_full":null}`)
	a.publishPressure(t.Context())
	if len(*patches) != 0 {
		t.Fatalf("patched %v: a host without PSI must not be published as calm", *patches)
	}
}
