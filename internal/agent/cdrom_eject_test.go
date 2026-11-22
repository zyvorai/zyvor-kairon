// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agent

import (
	"reflect"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

func TestCdromsToEject(t *testing.T) {
	var rec fluxvm.Record
	rec.Request.Cdroms = []fluxvm.Cdrom{
		{Name: "install", Path: "/cache/ws.iso"},
		{Name: "virtio", Path: "/cache/virtio.iso"},
		{Name: "old", Path: ""},
	}
	spec := []model.MachineCdrom{{Name: "virtio", ImageRef: "virtio-win"}}
	if got := cdromsToEject(spec, &rec); !reflect.DeepEqual(got, []string{"install"}) {
		t.Fatalf("got %v, want [install] (virtio still listed, old already empty)", got)
	}
	if got := cdromsToEject(nil, &rec); !reflect.DeepEqual(got, []string{"install", "virtio"}) {
		t.Fatalf("got %v", got)
	}
	var none fluxvm.Record
	if got := cdromsToEject(nil, &none); got != nil {
		t.Fatalf("got %v for a VM without cdroms", got)
	}
}
