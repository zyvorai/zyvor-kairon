// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package rehearsal runs isolated, disposable application upgrade rehearsals.
// It never promotes a copy or applies an upgrade to a source Machine.
package rehearsal

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

const RunLabel = "kairon.zyvor.dev/rehearsal"
const BridgeAnnotation = "kairon.zyvor.dev/rehearsal-bridge"
const ReservationAnnotation = "kairon.zyvor.dev/rehearsal-reservation"
const GuardAnnotation = "kairon.zyvor.dev/rehearsal-guard"

var dnsName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var bridgeName = regexp.MustCompile(`^krh-[a-z0-9-]{1,11}$`)

// Plan is a JSON-only, versioned input. Members and steps run in order.
// Snapshot names optionally select existing CSI snapshots for every volume.
// Otherwise new, independently timed, crash-consistent snapshots are taken.
type Plan struct {
	Purpose        string              `json:"purpose,omitempty"`
	Objectives     *RecoveryObjectives `json:"objectives,omitempty"`
	Incident       *IncidentReference  `json:"incident,omitempty"`
	APIVersion     string              `json:"apiVersion"`
	Name           string              `json:"name"`
	Namespace      string              `json:"namespace"`
	Node           string              `json:"node"`
	Bridge         string              `json:"bridge"`
	TTLSeconds     int64               `json:"ttlSeconds"`
	TimeoutSeconds int64               `json:"timeoutSeconds"`
	SnapshotClass  string              `json:"snapshotClass,omitempty"`
	Consistency    string              `json:"consistency"`
	Members        []Member            `json:"members"`
	Setup          []Step              `json:"setup,omitempty"`
	Baseline       []Step              `json:"baseline"`
	Changes        []Step              `json:"changes"`
	Verify         []Step              `json:"verify"`
}

type RecoveryObjectives struct {
	MaxRTOSeconds float64 `json:"maxRTOSeconds,omitempty"`
	MaxRPOSeconds float64 `json:"maxRPOSeconds,omitempty"`
}

type IncidentReference struct {
	SHA256      string `json:"sha256"`
	Machine     string `json:"machine"`
	SourceUID   string `json:"sourceUID"`
	CollectedAt string `json:"collectedAt"`
}

type Member struct {
	SourceSnapshot      string            `json:"sourceSnapshot,omitempty"`
	RestoreStorageClass map[string]string `json:"restoreStorageClass,omitempty"`
	Name                string            `json:"name"`
	Snapshots           map[string]string `json:"snapshots,omitempty"`
}

type Step struct {
	ExpectedExitCode int64    `json:"expectedExitCode,omitempty"`
	Name             string   `json:"name"`
	Machine          string   `json:"machine"`
	Path             string   `json:"path"`
	Args             []string `json:"args,omitempty"`
	TimeoutSeconds   uint64   `json:"timeoutSeconds"`
}

func Decode(r io.Reader) (Plan, error) {
	var p Plan
	d := json.NewDecoder(io.LimitReader(r, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return p, fmt.Errorf("expected exactly one JSON plan")
	}
	return p, p.Validate()
}

func (p Plan) Validate() error {
	if p.Purpose != "" && p.Purpose != "rehearsal" && p.Purpose != "recovery" && p.Purpose != "incident" {
		return fmt.Errorf("purpose must be rehearsal, recovery or incident")
	}
	if p.Objectives != nil && (p.Purpose != "recovery" || p.Objectives.MaxRTOSeconds < 0 || p.Objectives.MaxRPOSeconds < 0) {
		return fmt.Errorf("nonnegative objectives apply only to recovery")
	}
	if p.Purpose == "incident" && (p.Incident == nil || len(p.Incident.SHA256) != 64 || p.Incident.SourceUID == "" || p.Incident.Machine == "") {
		return fmt.Errorf("incident purpose needs a bound incident manifest")
	}

	if p.APIVersion != "rehearsal.kairon.zyvor.dev/v1alpha1" {
		return fmt.Errorf("unsupported rehearsal apiVersion")
	}
	for key, s := range map[string]string{"name": p.Name, "namespace": p.Namespace, "node": p.Node} {
		if len(s) > 63 || !dnsName.MatchString(s) {
			return fmt.Errorf("%s must be a DNS label of at most 63 characters", key)
		}
	}
	if !bridgeName.MatchString(p.Bridge) {
		return fmt.Errorf("bridge must start krh- and be at most 15 characters")
	}
	if p.TimeoutSeconds < 1 || p.TimeoutSeconds > 3600 || p.TTLSeconds < p.TimeoutSeconds+120 || p.TTLSeconds > 86400 {
		return fmt.Errorf("timeout must be 1..3600 seconds; TTL must be timeout+120..86400")
	}
	if p.Consistency != "crash" && p.Consistency != "existing" {
		return fmt.Errorf("consistency must be crash or existing (externally coordinated snapshots)")
	}
	if len(p.Members) < 1 || len(p.Members) > 16 {
		return fmt.Errorf("members must contain 1..16 Machines")
	}
	names := map[string]bool{}
	for _, m := range p.Members {
		if !dnsName.MatchString(m.Name) || len(m.Name) > 63 || names[m.Name] {
			return fmt.Errorf("invalid or duplicate member %q", m.Name)
		}
		names[m.Name] = true
		if (p.Purpose == "recovery" || p.Purpose == "incident") && m.SourceSnapshot == "" {
			return fmt.Errorf("%s purpose requires sourceSnapshot for every member", p.Purpose)
		}
		if m.SourceSnapshot != "" && (p.Consistency != "existing" || len(m.Snapshots) > 0) {
			return fmt.Errorf("sourceSnapshot selects its own CSI recovery points; use existing mode without snapshots map")
		}
		if m.SourceSnapshot == "" && (p.Consistency == "existing") != (len(m.Snapshots) > 0) {
			return fmt.Errorf("member %s: snapshots are required only in existing mode", m.Name)
		}
	}
	if len(p.Verify) == 0 || (p.Purpose != "recovery" && (len(p.Baseline) == 0 || len(p.Changes) == 0)) || (p.Purpose == "recovery" && (len(p.Baseline) > 0 || len(p.Changes) > 0)) {
		return fmt.Errorf("rehearsal/incident require baseline, changes and verify; recovery requires verify and forbids baseline/changes")
	}
	if p.Purpose == "incident" {
		found := false
		for _, m := range p.Members {
			found = found || m.Name == p.Incident.Machine
		}
		if !found {
			return fmt.Errorf("incident Machine must be a member")
		}
		reproduced := false
		for _, step := range p.Baseline {
			reproduced = reproduced || step.ExpectedExitCode != 0
		}
		if !reproduced {
			return fmt.Errorf("incident baseline must declare an expected failure")
		}
	}
	steps := map[string]bool{}
	for _, group := range [][]Step{p.Setup, p.Baseline, p.Changes, p.Verify} {
		for _, s := range group {
			if s.Name == "" || steps[s.Name] || !names[s.Machine] || !strings.HasPrefix(s.Path, "/") || s.TimeoutSeconds < 1 || s.TimeoutSeconds > 240 {
				return fmt.Errorf("step %q: unique name, member, absolute executable path and timeout 1..240 are required", s.Name)
			}
			if s.ExpectedExitCode < 0 || s.ExpectedExitCode > 255 {
				return fmt.Errorf("expected exit code must be 0..255")
			}
			if s.ExpectedExitCode != 0 && (p.Purpose != "incident" || !incidentBaseline(p, s.Name)) {
				return fmt.Errorf("expected failure is allowed only in incident baseline")
			}
			steps[s.Name] = true
		}
	}
	return nil
}

// CopySpec uses an allowlist: no original identity, network attachment,
// cloud-init commands, service registration, devices or lifecycle is inherited.
// All storage references must have a restored independent claim.
func CopySpec(src model.Machine, claims map[string]string, p Plan) (model.MachineSpec, error) {
	if src.Spec.Runtime.Backend != "qemu" || !src.Spec.GuestAgent.Enabled || src.Spec.Sandbox != nil || src.Spec.Security.TPM || src.Spec.Security.SecureBoot || len(src.Spec.Disks) > 0 || len(src.Spec.Devices) > 0 || len(src.Spec.DeviceClaims) > 0 || len(src.Spec.Accelerators) > 0 || len(src.Spec.Cdroms) > 0 || len(src.Spec.Volumes) == 0 {
		return model.MachineSpec{}, fmt.Errorf("%s: requires QEMU, QGA, PVC boot; TPM, secure boot, hotplug disks, devices, accelerators, CD-ROMs and sandboxes are unsupported", src.Metadata.Name)
	}
	if src.Spec.Resources.CPUPinning || len(src.Spec.Resources.AllocatedCPUSet) > 0 || src.Spec.Resources.Hugepages || src.Spec.Resources.NUMANode != nil || src.Spec.Resources.CPUSet != "" || src.Spec.Topology != nil || src.Spec.Runtime.Kernel != "" {
		return model.MachineSpec{}, fmt.Errorf("%s: host-specific CPU/NUMA/hugepage/kernel configurations are unsupported", src.Metadata.Name)
	}
	if src.Spec.Network.Bridge == p.Bridge {
		return model.MachineSpec{}, fmt.Errorf("source already uses the rehearsal bridge")
	}
	if src.Status.Phase != "Running" && src.Status.Phase != "Halted" {
		return model.MachineSpec{}, fmt.Errorf("source %s must be Running or Halted", src.Metadata.Name)
	}
	volumes := make([]model.MachineVolume, 0, len(src.Spec.Volumes))
	seen := map[string]bool{}
	for _, v := range src.Spec.Volumes {
		if v.Name == "" || seen[v.Name] || v.ClaimName == "" || v.Atlas != nil || claims[v.Name] == "" || claims[v.Name] == v.ClaimName {
			return model.MachineSpec{}, fmt.Errorf("%s: volume %s needs a distinct restored CSI claim", src.Metadata.Name, v.Name)
		}
		seen[v.Name] = true
		v.ClaimName = claims[v.Name]
		volumes = append(volumes, v)
	}
	return model.MachineSpec{
		NodeName: p.Node, Resources: src.Spec.Resources, Runtime: src.Spec.Runtime,
		Network:    model.NetworkSpec{Mode: "tap", Bridge: p.Bridge, DataplaneMode: "legacy"},
		GuestAgent: model.GuestAgentSpec{Enabled: true}, Volumes: volumes, PowerState: "Running",
		Lifecycle: &model.LifecycleSpec{MaxLifetimeSeconds: p.TTLSeconds},
	}, nil
}

func incidentBaseline(p Plan, name string) bool {
	for _, s := range p.Baseline {
		if s.Name == name {
			return true
		}
	}
	return false
}
