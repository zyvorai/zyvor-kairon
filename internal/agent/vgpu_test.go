// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

func TestMediatedDevicesAreCreatedBeforeBootAndRemovedAfter(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(body))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	a := &Agent{Flux: fluxvm.New(srv.URL, ""), VFIOAllowlist: map[string]struct{}{"0000:86:00.0": {}}}
	m := model.Machine{}
	m.Spec.TopologyAllocation = &model.TopologyAllocation{Devices: []model.AllocatedDevice{
		{Name: "v", Parent: "mbochs", Profile: "mbochs-small", UUID: "5A3C1D2E-0000-5000-8000-000000000001", FramebufferMiB: 4},
		{Name: "g", BDF: "0000:86:00.0"},
	}}

	devs, err := a.topologyVFIODevices(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"5a3c1d2e-0000-5000-8000-000000000001", "0000:86:00.0"}; !reflect.DeepEqual(devs, want) {
		t.Fatalf("vfio devices %v, want %v", devs, want)
	}
	if err := a.releaseMediatedDevices(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`POST /v1/host/mdev {"parent":"mbochs","type":"mbochs-small","uuid":"5A3C1D2E-0000-5000-8000-000000000001"}`,
		`POST /v1/host/pci/bind {"bdf":"0000:86:00.0"}`,
		"DELETE /v1/host/mdev/5a3c1d2e-0000-5000-8000-000000000001 ",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("FluxVM calls\n got %q\nwant %q", calls, want)
	}
}

func TestParseMdevParents(t *testing.T) {
	if ParseMdevParents(" , ") != nil {
		t.Fatal("empty list must allow every parent (nil)")
	}
	got := ParseMdevParents("mbochs, 0000:3B:00.0")
	if want := map[string]struct{}{"mbochs": {}, "0000:3b:00.0": {}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}
