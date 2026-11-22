// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package fleet implements opt-in fleet automation using durable Kubernetes
// intent and existing Kairon primitives. No model inference runs here.
package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

const ManagedLabel = "fleet.kairon.zyvor.dev/owner"
const ManagedUIDLabel = "fleet.kairon.zyvor.dev/owner-uid"
const NetworkAllocationAnnotation = "fleet.kairon.zyvor.dev/network-allocation"
const HAProfileAnnotation = "fleet.kairon.zyvor.dev/ha-profile"

var dnsName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func validName(s string) bool { return len(s) > 0 && len(s) <= 63 && dnsName.MatchString(s) }

type HAProfileSpec struct {
	Selector            map[string]string        `json:"selector"`
	Nodes               map[string]RedfishTarget `json:"nodes"`
	MaxRestarts         int                      `json:"maxRestarts"`
	FailureGraceSeconds int64                    `json:"failureGraceSeconds"`
}
type RedfishTarget struct {
	Endpoint   string `json:"endpoint"`
	SystemID   string `json:"systemID"`
	SecretName string `json:"secretName"`
}
type FenceSpec struct {
	Node        string `json:"node"`
	NodeUID     string `json:"nodeUID"`
	ProfileName string `json:"profileName"`
}
type ApprovalSpec struct {
	Action           string    `json:"action"`
	ArgumentsHash    string    `json:"argumentsHash"`
	Principal        string    `json:"principal"`
	TargetResource   string    `json:"targetResource"`
	TargetName       string    `json:"targetName"`
	TargetUID        string    `json:"targetUID"`
	TargetGeneration int64     `json:"targetGeneration"`
	Approver         string    `json:"approver"`
	ExpiresAt        time.Time `json:"expiresAt"`
}
type BalanceSpec struct {
	Selector              map[string]string `json:"selector"`
	MinImprovementPercent float64           `json:"minImprovementPercent"`
	CooldownSeconds       int64             `json:"cooldownSeconds"`
	Strategy              string            `json:"strategy"`
	DryRun                bool              `json:"dryRun"`
}
type AutoscalerSpec struct {
	TargetKind           string  `json:"targetKind"`
	TargetName           string  `json:"targetName"`
	MinReplicas          int     `json:"minReplicas"`
	MaxReplicas          int     `json:"maxReplicas"`
	TargetCPUPercent     float64 `json:"targetCPUPercent"`
	StabilizationSeconds int64   `json:"stabilizationSeconds"`
	MaxStep              int     `json:"maxStep"`
}
type RecoveryStep struct {
	MachineName string   `json:"machineName"`
	BackupName  string   `json:"backupName"`
	DependsOn   []string `json:"dependsOn,omitempty"`
}
type RecoverySpec struct {
	Steps             []RecoveryStep `json:"steps"`
	Mode              string         `json:"mode"` // RestoreInPlace or TestRestore: cross-cluster replication is not silently fabricated.
	MaxDataAgeSeconds int64          `json:"maxDataAgeSeconds"`
	TestBridge        string         `json:"testBridge,omitempty"`
	Start             bool           `json:"start"`
}
type BackupGroupSpec struct {
	Machines        []string `json:"machines"`
	Quiesce         string   `json:"quiesce"`
	AtlasBucketID   string   `json:"atlasBucketID,omitempty"`
	IntervalSeconds int64    `json:"intervalSeconds,omitempty"`
	KeepLast        int      `json:"keepLast,omitempty"`
}
type ImportEntry struct {
	Name      string             `json:"name"`
	Image     model.ImageSpec    `json:"image"`
	Resources model.ResourceSpec `json:"resources"`
	Backend   string             `json:"backend,omitempty"`
}
type ImportSpec struct {
	Entries       []ImportEntry `json:"entries"`
	MaxConcurrent int           `json:"maxConcurrent"`
	PowerState    string        `json:"powerState"`
}
type TemplateSpec struct {
	Version       string                `json:"version"`
	Template      model.MachineTemplate `json:"template"`
	MaxTTLSeconds int64                 `json:"maxTTLSeconds"`
}
type TemplateClaimSpec struct {
	TemplateName string `json:"templateName"`
	MachineName  string `json:"machineName"`
	TTLSeconds   int64  `json:"ttlSeconds"`
	NetworkClaim string `json:"networkClaim,omitempty"`
}
type VirtualNetworkSpec struct {
	CIDR         string            `json:"cidr"`
	Gateway      string            `json:"gateway"`
	Bridge       string            `json:"bridge"`
	NodeSelector map[string]string `json:"nodeSelector"`
	DNSServers   []string          `json:"dnsServers,omitempty"`
}
type NetworkClaimSpec struct {
	NetworkName string `json:"networkName"`
	MachineName string `json:"machineName"`
}
type LedgerSpec struct {
	Selector      map[string]string `json:"selector"`
	MaxGapSeconds int64             `json:"maxGapSeconds"`
}

func decode[T any](raw json.RawMessage) (T, error) {
	var out T
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	err := d.Decode(&out)
	return out, err
}

func Validate(o model.FleetResource) error {
	if !validName(o.Metadata.Name) || !validName(o.Metadata.Namespace) {
		return fmt.Errorf("name and namespace must be DNS labels, at most 63 characters")
	}
	switch o.Kind {
	case "MachineHAProfile":
		s, e := decode[HAProfileSpec](o.Spec)
		if e != nil {
			return e
		}
		if len(s.Selector) == 0 || len(s.Nodes) == 0 || s.MaxRestarts < 1 || s.FailureGraceSeconds < 30 {
			return fmt.Errorf("HA needs a nonempty selector, nodes, maxRestarts >=1 and grace >=30s")
		}
		for name, t := range s.Nodes {
			u, e := url.Parse(t.Endpoint)
			if !validName(name) || e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || !validName(t.SystemID) || !validName(t.SecretName) {
				return fmt.Errorf("invalid Redfish target for %s", name)
			}
		}
	case "NodeFenceRequest":
		s, e := decode[FenceSpec](o.Spec)
		if e != nil {
			return e
		}
		if !validName(s.Node) || !validName(s.ProfileName) || s.NodeUID == "" {
			return fmt.Errorf("node, nodeUID and profileName are required")
		}
	case "MachineActionApproval":
		s, e := decode[ApprovalSpec](o.Spec)
		if e != nil {
			return e
		}
		if s.Action == "" || len(s.ArgumentsHash) != 64 || s.Principal == "" || !validName(s.TargetName) || s.TargetUID == "" || s.Approver == "" || s.ExpiresAt.IsZero() {
			return fmt.Errorf("approval must bind action, hash, principal, target identity, approver and expiry")
		}
		if s.TargetResource != "machines" && s.TargetResource != "machinebackups" {
			return fmt.Errorf("unsupported approval target")
		}
	case "MachineBalancePolicy":
		s, e := decode[BalanceSpec](o.Spec)
		if e != nil {
			return e
		}
		if len(s.Selector) == 0 || s.MinImprovementPercent < 1 || s.MinImprovementPercent > 100 || s.CooldownSeconds < 30 || (s.Strategy != "cold" && s.Strategy != "auto") {
			return fmt.Errorf("balance needs selector, improvement 1..100, cooldown >=30s, strategy cold|auto")
		}
	case "MachineAutoscaler":
		s, e := decode[AutoscalerSpec](o.Spec)
		if e != nil {
			return e
		}
		if (s.TargetKind != "MachineSet" && s.TargetKind != "MachinePool") || !validName(s.TargetName) || s.MinReplicas < 1 || s.MaxReplicas < s.MinReplicas || s.MaxReplicas > 10000 || s.TargetCPUPercent <= 0 || s.TargetCPUPercent > 100 || s.StabilizationSeconds < 30 || s.MaxStep < 1 {
			return fmt.Errorf("invalid autoscaler bounds, target or stabilization")
		}
	case "MachineRecoveryPlan":
		s, e := decode[RecoverySpec](o.Spec)
		if e != nil {
			return e
		}
		if s.Mode != "RestoreInPlace" && s.Mode != "TestRestore" {
			return fmt.Errorf("mode must be RestoreInPlace or TestRestore")
		}
		if s.MaxDataAgeSeconds < 1 {
			return fmt.Errorf("maxDataAgeSeconds must be positive")
		}
		return validateRecovery(s)
	case "MachineBackupGroup":
		s, e := decode[BackupGroupSpec](o.Spec)
		if e != nil {
			return e
		}
		if len(s.Machines) == 0 || !model.ValidBackupQuiesce(s.Quiesce) || s.IntervalSeconds < 0 || s.KeepLast < 0 {
			return fmt.Errorf("invalid backup group")
		}
		return uniqueNames(s.Machines)
	case "MachineImportPlan":
		s, e := decode[ImportSpec](o.Spec)
		if e != nil {
			return e
		}
		if len(s.Entries) == 0 || s.MaxConcurrent < 1 || s.MaxConcurrent > 100 || (s.PowerState != "Stopped" && s.PowerState != "Running") {
			return fmt.Errorf("invalid import concurrency or powerState")
		}
		names := []string{}
		for _, entry := range s.Entries {
			names = append(names, entry.Name)
			if entry.Image.Source == nil || entry.Image.Source.HTTPURL == "" || entry.Image.Source.Format != "ova" {
				return fmt.Errorf("import entries require OVA URLs")
			}
			if e := model.ValidateImageSource(entry.Image); e != nil {
				return e
			}
			if entry.Resources.CPU == "" || entry.Resources.Memory == "" {
				return fmt.Errorf("resources required")
			}
		}
		return uniqueNames(names)
	case "MachineTemplateVersion":
		s, e := decode[TemplateSpec](o.Spec)
		if e != nil {
			return e
		}
		if s.Version == "" || s.MaxTTLSeconds < 1 || s.Template.Spec.Resources.CPU == "" || s.Template.Spec.Resources.Memory == "" {
			return fmt.Errorf("template needs version, resources and positive maxTTLSeconds")
		}
		if s.Template.Spec.Image.Source == nil || s.Template.Spec.Image.Digest == "" {
			return fmt.Errorf("approved templates require a digest-pinned image source")
		}
		return model.ValidateImageSource(s.Template.Spec.Image)
	case "MachineTemplateClaim":
		s, e := decode[TemplateClaimSpec](o.Spec)
		if e != nil {
			return e
		}
		if !validName(s.TemplateName) || !validName(s.MachineName) || s.TTLSeconds < 1 {
			return fmt.Errorf("invalid template claim")
		}
	case "MachineVirtualNetwork":
		s, e := decode[VirtualNetworkSpec](o.Spec)
		if e != nil {
			return e
		}
		p, e := netip.ParsePrefix(s.CIDR)
		if e != nil || p != p.Masked() {
			return fmt.Errorf("CIDR must be canonical")
		}
		g, e := netip.ParseAddr(s.Gateway)
		if e != nil || !p.Contains(g) || g == p.Addr() {
			return fmt.Errorf("gateway must be a usable address inside CIDR")
		}
		if !validName(s.Bridge) || len(s.Bridge) > 15 || len(s.NodeSelector) == 0 {
			return fmt.Errorf("bridge (<=15 chars) and nodeSelector required")
		}
		if p.Addr().Is4() && p.Bits() > 30 {
			return fmt.Errorf("IPv4 network must have host addresses")
		}
		if !p.Addr().Is4() && p.Bits() > 120 {
			return fmt.Errorf("IPv6 prefix must be /120 or larger")
		}
		for _, d := range s.DNSServers {
			if _, e := netip.ParseAddr(d); e != nil {
				return fmt.Errorf("invalid DNS IP")
			}
		}
	case "MachineNetworkClaim":
		s, e := decode[NetworkClaimSpec](o.Spec)
		if e != nil {
			return e
		}
		if !validName(s.NetworkName) || !validName(s.MachineName) {
			return fmt.Errorf("invalid network claim")
		}
	case "MachineUsageLedger":
		s, e := decode[LedgerSpec](o.Spec)
		if e != nil {
			return e
		}
		if len(s.Selector) == 0 || s.MaxGapSeconds < 30 || s.MaxGapSeconds > 3600 {
			return fmt.Errorf("ledger needs selector and maxGapSeconds 30..3600")
		}
	default:
		return fmt.Errorf("unknown fleet kind %q", o.Kind)
	}
	return nil
}
func uniqueNames(names []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		if !validName(n) || seen[n] {
			return fmt.Errorf("invalid or duplicate name %q", n)
		}
		seen[n] = true
	}
	return nil
}
func validateRecovery(s RecoverySpec) error {
	names := []string{}
	byName := map[string]RecoveryStep{}
	for _, step := range s.Steps {
		names = append(names, step.MachineName)
		byName[step.MachineName] = step
		if !validName(step.BackupName) {
			return fmt.Errorf("invalid backup name")
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("recovery steps are required")
	}
	if e := uniqueNames(names); e != nil {
		return e
	}
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(n string) error {
		if visiting[n] {
			return fmt.Errorf("recovery dependency cycle at %s", n)
		}
		if done[n] {
			return nil
		}
		step, ok := byName[n]
		if !ok {
			return fmt.Errorf("unknown dependency %s", n)
		}
		visiting[n] = true
		for _, d := range step.DependsOn {
			if e := visit(d); e != nil {
				return e
			}
		}
		visiting[n] = false
		done[n] = true
		return nil
	}
	for n := range byName {
		if e := visit(n); e != nil {
			return e
		}
	}
	return nil
}
func ResourceForKind(kind string) (string, error) {
	for r, k := range model.FleetKinds {
		if strings.EqualFold(kind, k) || kind == r {
			return r, nil
		}
	}
	return "", fmt.Errorf("unknown fleet kind %q", kind)
}
