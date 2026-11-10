// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestValidateTenantForwards(t *testing.T) {
	ok := []model.PortForward{{HostPort: 2222, GuestPort: 22}, {HostPort: 8080, GuestPort: 80}}
	if err := validateTenantForwards(ok, false); err != nil {
		t.Fatalf("unprivileged forwards must be allowed: %v", err)
	}
	priv := []model.PortForward{{HostPort: 22, GuestPort: 22}}
	if err := validateTenantForwards(priv, false); err == nil {
		t.Fatal("a non-admin must not forward a privileged host port")
	}
	if err := validateTenantForwards(priv, true); err != nil {
		t.Fatalf("an admin may forward a privileged host port: %v", err)
	}
	if err := validateTenantForwards([]model.PortForward{{HostPort: 0, GuestPort: 22}}, true); err == nil {
		t.Fatal("a zero host port is never valid")
	}
	if err := validateTenantForwards(nil, false); err != nil {
		t.Fatalf("no forwards is fine: %v", err)
	}
}
