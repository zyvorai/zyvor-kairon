// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestResolveDataVolumesMapsVirtiofsShares(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/default/persistentvolumeclaims/data-pvc":
			_ = json.NewEncoder(w).Encode(model.PersistentVolumeClaim{
				Spec:   model.PersistentVolumeClaimSpec{VolumeName: "pv-data"},
				Status: model.PersistentVolumeClaimStatus{Phase: "Bound"},
			})
		case "/api/v1/persistentvolumes/pv-data":
			_ = json.NewEncoder(w).Encode(model.PersistentVolume{
				Spec: model.PersistentVolumeSpec{
					VolumeMode: "Filesystem",
					HostPath:   &model.HostPathVolumeSource{Path: "/var/lib/kairon/data"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{Kube: kc}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "default"},
		Spec: model.MachineSpec{
			Volumes: []model.MachineVolume{
				{Name: "root", ClaimName: "root-pvc"},
				{Name: "data", ClaimName: "data-pvc", GuestPath: "/var/lib/pg"},
			},
		},
	}
	shares, err := a.resolveDataVolumes(context.Background(), m)
	if err != nil {
		t.Fatalf("resolveDataVolumes: %v", err)
	}
	if len(shares) != 1 || shares[0].HostPath != "/var/lib/kairon/data" || shares[0].GuestPath != "/var/lib/pg" {
		t.Fatalf("unexpected shares: %+v", shares)
	}
}
