// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agentplane

import "github.com/zyvorai/kairon/internal/model"

// EligibleWarm reports whether a Running warm Machine may be bound to
// this claim. Empty tenant and hypervisor constraints match anything.
func EligibleWarm(claim model.MachineClaim, m model.Machine) bool {
	if m.Status.Phase != "" && m.Status.Phase != "Running" && (m.Status.Phase != "Paused" || m.Spec.PowerState != "Paused") {
		return false
	}
	if m.Metadata.Labels != nil && m.Metadata.Labels[model.LabelPoolState] != "" && m.Metadata.Labels[model.LabelPoolState] != model.PoolStateWarm {
		return false
	}
	wantTenant := ""
	if claim.Spec.Labels != nil {
		wantTenant = claim.Spec.Labels["kairon.zyvor.dev/tenant"]
	}
	if wantTenant != "" && m.Spec.Tenant != "" && m.Spec.Tenant != wantTenant {
		return false
	}
	wantHV := ""
	if claim.Metadata.Annotations != nil {
		wantHV = claim.Metadata.Annotations[AnnHypervisor]
	}
	gotHV := ""
	if m.Metadata.Annotations != nil {
		gotHV = m.Metadata.Annotations[AnnHypervisor]
	}
	if wantHV != "" && gotHV != "" && wantHV != gotHV {
		return false
	}
	return true
}

// WantsSnapshot is the snapshot-on-release opt-in.
func WantsSnapshot(claim model.MachineClaim) bool {
	if claim.Metadata.Annotations == nil {
		return false
	}
	return truthy(claim.Metadata.Annotations[AnnSnapshotOnRelease])
}

// ReleaseSnapshotName is stable so a retry does not create a second snapshot.
func ReleaseSnapshotName(claim string) string {
	name := claim + "-release"
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

// StatusAnnotations is the status the controller can project without a
// CRD change. Empty values are omitted.
func StatusAnnotations(confidential ConfidentialStatus, gateway string, gpuCount int, liveMigrate bool) map[string]string {
	out := map[string]string{}
	if confidential.Kind != "" {
		if confidential.Sealed {
			out["kairon.zyvor.dev/confidential-sealed"] = "true"
		} else {
			out["kairon.zyvor.dev/confidential-sealed"] = "false"
		}
		if confidential.Reason != "" {
			out["kairon.zyvor.dev/confidential-reason"] = confidential.Reason
		}
	}
	if gateway != "" {
		out[AnnGateway] = gateway
	}
	if gpuCount > 0 {
		out[AnnGPUCount] = itoa(gpuCount)
		if liveMigrate {
			out[AnnLiveMigrate] = "true"
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
