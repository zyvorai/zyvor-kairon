// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"regexp"
	"sort"
	"time"
)

var protectedNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const (
	KindMachineProtectionPlan = "MachineProtectionPlan"
	// ProtectionPlanLabel marks the Machines, backup policy and backups a
	// MachineProtectionPlan owns.
	ProtectionPlanLabel = "kairon.zyvor.dev/protection-plan"
	// ReplicatedFromAnnotation marks a MachineBackup imported from the
	// peer site: "<namespace>/<name>" of the backup there. kairon-controller
	// never runs it and deleting it leaves the S3 objects alone.
	ReplicatedFromAnnotation = "kairon.zyvor.dev/replicated-from"
	// ProtectionHandoffAnnotation on the active site's plan asks it to halt
	// its Machines and take a final backup for a planned failover; the
	// value is the requesting action's ID.
	ProtectionHandoffAnnotation = "kairon.zyvor.dev/protection-handoff"
	// DRPeerCredentialsLabel must be set on a peer credentials Secret.
	DRPeerCredentialsLabel = "kairon.zyvor.dev/dr-peer-credentials"

	ProtectionRolePrimary   = "Primary"
	ProtectionRoleSecondary = "Secondary"

	ProtectionActionFailover = "Failover"
	ProtectionActionTest     = "Test"
	ProtectionActionEndTest  = "EndTest"

	ProtectionPhaseActive      = "Active"
	ProtectionPhaseStandby     = "Standby"
	ProtectionPhaseFailingOver = "FailingOver"
	ProtectionPhaseInvalid     = "Invalid"

	ProtectionRunRunning   = "Running"
	ProtectionRunSucceeded = "Succeeded"
	ProtectionRunFailed    = "Failed"
	ProtectionRunEnded     = "Ended"

	ProtectionHandoffHalting = "Halting"
	ProtectionHandoffBackup  = "FinalBackup"
	ProtectionHandoffReady   = "Ready"
)

// MachineProtectionPlan protects a group of Machines across two clusters.
// The same plan exists on both sites. The active site replicates the
// Machines to S3 as incremental backups; the standby site mirrors their
// specs and restore points from the peer's API and can fail over, in
// dependency order, or run a DR test into an isolated network.
type MachineProtectionPlan struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta                  `json:"metadata"`
	Spec     MachineProtectionPlanSpec   `json:"spec"`
	Status   MachineProtectionPlanStatus `json:"status,omitempty"`
}

func (p MachineProtectionPlan) Namespace() string { return p.Metadata.Namespace }

type MachineProtectionPlanList struct {
	TypeMeta `json:",inline"`
	Items    []MachineProtectionPlan `json:"items"`
}

type MachineProtectionPlanSpec struct {
	// Role is the site that runs the Machines first: Primary or Secondary.
	Role        string                `json:"role"`
	Machines    []ProtectedMachine    `json:"machines"`
	Replication ProtectionReplication `json:"replication"`
	Objectives  ProtectionObjectives  `json:"objectives,omitempty"`
	Peer        ProtectionPeer        `json:"peer"`
	Test        ProtectionTest        `json:"test,omitempty"`
	// Action runs once per new ID.
	Action *ProtectionAction `json:"action,omitempty"`
}

// ProtectedMachine is one Machine in the plan's namespace. A failover
// starts it only once every Machine in DependsOn is Running.
type ProtectedMachine struct {
	Name      string   `json:"name"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

type ProtectionReplication struct {
	// Interval is how often each Machine is backed up (at least 5m).
	Interval string `json:"interval"`
	// FullEvery starts a new chain this often (default 24h).
	FullEvery string `json:"fullEvery,omitempty"`
	// Target is the bucket both sites can read. On the standby site it is
	// where restores read from, so it may name a replica of the active
	// site's bucket with the same keys.
	Target     BackupTarget      `json:"target"`
	Encryption *BackupEncryption `json:"encryption,omitempty"`
	Quiesce    string            `json:"quiesce,omitempty"`
	// Retention of the replicated backups (default: the newest of the
	// last 24 hours and 7 days).
	Retention *BackupRetention `json:"retention,omitempty"`
}

type ProtectionObjectives struct {
	// RPO is the most data the plan may lose, e.g. "15m". The standby
	// site reports RPOMet=False when its newest restore point is older.
	RPO string `json:"rpo,omitempty"`
	// RTO is how long a failover may take; the last failover and test
	// report whether they met it.
	RTO string `json:"rto,omitempty"`
}

// ProtectionPeer is the other site's Kubernetes API and plan.
type ProtectionPeer struct {
	Server string `json:"server"`
	// CredentialsSecretRef names a Secret in kairon-controller's namespace
	// labelled kairon.zyvor.dev/dr-peer-credentials=true with a bearer
	// "token" and optionally "ca.crt".
	CredentialsSecretRef string `json:"credentialsSecretRef"`
	// Namespace and Plan default to this plan's namespace and name.
	Namespace string `json:"namespace,omitempty"`
	Plan      string `json:"plan,omitempty"`
}

type ProtectionTest struct {
	// Bridge is an isolated host bridge test copies attach to; it must be
	// in kairon-controller's --dr-test-bridges allowlist.
	Bridge string `json:"bridge,omitempty"`
	// TTL removes the test copies after this long (default 1h).
	TTL string `json:"ttl,omitempty"`
}

type ProtectionAction struct {
	// Type is Failover, Test or EndTest.
	Type string `json:"type"`
	ID   string `json:"id"`
	// Planned (Failover only) asks the reachable active site to halt its
	// Machines and take a final backup first, so nothing is lost.
	Planned bool `json:"planned,omitempty"`
}

type MachineProtectionPlanStatus struct {
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	// Active is true on the site that runs the Machines.
	Active bool `json:"active"`
	// Epoch goes up by one with every failover; the site with the higher
	// epoch is the active one.
	Epoch int64 `json:"epoch,omitempty"`
	// RPOSeconds is the age of the oldest of the Machines' newest restore
	// points.
	RPOSeconds *float64                 `json:"rpoSeconds,omitempty"`
	Peer       *ProtectionPeerStatus    `json:"peer,omitempty"`
	Machines   []ProtectedMachineStatus `json:"machines,omitempty"`
	Conditions []Condition              `json:"conditions,omitempty"`
	// LastAction is the ID of the last action this site took up.
	LastAction string             `json:"lastAction,omitempty"`
	Failover   *ProtectionRun     `json:"failover,omitempty"`
	Test       *ProtectionRun     `json:"test,omitempty"`
	Handoff    *ProtectionHandoff `json:"handoff,omitempty"`
}

type ProtectionPeerStatus struct {
	Reachable   bool       `json:"reachable"`
	LastContact *time.Time `json:"lastContact,omitempty"`
	Active      bool       `json:"active"`
	Epoch       int64      `json:"epoch,omitempty"`
	Message     string     `json:"message,omitempty"`
}

type ProtectedMachineStatus struct {
	Name string `json:"name"`
	// RestorePoint is the newest backup a failover would restore, and
	// PointInTime the instant it returns to.
	RestorePoint string     `json:"restorePoint,omitempty"`
	PointInTime  *time.Time `json:"pointInTime,omitempty"`
	// Spec is the Machine's spec as last seen on the active site, used to
	// create it on failover. Placement fields are cleared.
	Spec *MachineSpec `json:"spec,omitempty"`
	// Labels are the Machine's labels on the active site.
	Labels  map[string]string `json:"labels,omitempty"`
	Message string            `json:"message,omitempty"`
}

// ProtectionRun is a failover or DR test.
type ProtectionRun struct {
	ID          string     `json:"id"`
	Planned     bool       `json:"planned,omitempty"`
	Phase       string     `json:"phase"`
	Message     string     `json:"message,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	// RTOSeconds is start to every Machine Running.
	RTOSeconds *float64 `json:"rtoSeconds,omitempty"`
	// DataLossSeconds is the start minus the oldest restore point used.
	DataLossSeconds *float64 `json:"dataLossSeconds,omitempty"`
	// Restored lists the Machines Running so far, in order.
	Restored []string `json:"restored,omitempty"`
	// Machines are the Machines the run created or restores into.
	Machines []string `json:"machines,omitempty"`
	// Points is the backup each protected Machine is restored from, fixed
	// when the run starts restoring.
	Points map[string]string `json:"points,omitempty"`
	// ExpiresAt removes a test's copies.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// ProtectionHandoff is the active site's side of a planned failover.
type ProtectionHandoff struct {
	ID    string `json:"id"`
	Phase string `json:"phase"`
	// Backups are the final backups the standby site must import.
	Backups []string `json:"backups,omitempty"`
	Message string   `json:"message,omitempty"`
}

const (
	defaultProtectionFullEvery = 24 * time.Hour
	defaultProtectionTestTTL   = time.Hour
)

// PolicyName is the MachineBackupPolicy the active site replicates with.
func (p MachineProtectionPlan) PolicyName() string { return "dr-" + p.Metadata.Name }

// PeerNamespace and PeerPlan name the plan on the other site.
func (p MachineProtectionPlan) PeerNamespace() string {
	if p.Spec.Peer.Namespace != "" {
		return p.Spec.Peer.Namespace
	}
	return p.Metadata.Namespace
}

func (p MachineProtectionPlan) PeerPlan() string {
	if p.Spec.Peer.Plan != "" {
		return p.Spec.Peer.Plan
	}
	return p.Metadata.Name
}

// BackupPolicySpec is the replication policy for the plan's Machines.
func (s MachineProtectionPlanSpec) BackupPolicySpec(planName string) MachineBackupPolicySpec {
	full := s.Replication.FullEvery
	if full == "" {
		full = defaultProtectionFullEvery.String()
	}
	retention := BackupRetention{Hourly: 24, Daily: 7}
	if s.Replication.Retention != nil {
		retention = *s.Replication.Retention
	}
	return MachineBackupPolicySpec{
		Selector:         map[string]string{ProtectionPlanLabel: planName},
		IncrementalEvery: s.Replication.Interval,
		FullEvery:        full,
		Target:           s.Replication.Target,
		Encryption:       s.Replication.Encryption,
		Quiesce:          s.Replication.Quiesce,
		Retention:        retention,
	}
}

// TestTTL is how long DR test copies live.
func (s MachineProtectionPlanSpec) TestTTL() (time.Duration, error) {
	if s.Test.TTL == "" {
		return defaultProtectionTestTTL, nil
	}
	return ParseBackupDuration(s.Test.TTL)
}

// Objective parses an RPO or RTO; "" is none.
func Objective(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	return ParseBackupDuration(v)
}

func (s MachineProtectionPlanSpec) Validate(planName string) error {
	if s.Role != ProtectionRolePrimary && s.Role != ProtectionRoleSecondary {
		return fmt.Errorf("role must be Primary or Secondary")
	}
	if _, err := s.Order(); err != nil {
		return err
	}
	if err := s.BackupPolicySpec(planName).Validate(); err != nil {
		return fmt.Errorf("replication: %w", err)
	}
	if s.Peer.Server == "" || s.Peer.CredentialsSecretRef == "" {
		return fmt.Errorf("peer.server and peer.credentialsSecretRef are required")
	}
	for name, v := range map[string]string{"objectives.rpo": s.Objectives.RPO, "objectives.rto": s.Objectives.RTO} {
		if _, err := Objective(v); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if _, err := s.TestTTL(); err != nil {
		return fmt.Errorf("test.ttl: %w", err)
	}
	if a := s.Action; a != nil {
		switch a.Type {
		case ProtectionActionFailover, ProtectionActionTest, ProtectionActionEndTest:
		default:
			return fmt.Errorf("action.type must be Failover, Test or EndTest")
		}
		if a.ID == "" {
			return fmt.Errorf("action.id is required")
		}
		if a.Planned && a.Type != ProtectionActionFailover {
			return fmt.Errorf("action.planned applies to Failover only")
		}
	}
	return nil
}

// Order returns the Machines in waves: every Machine in a wave depends
// only on Machines in earlier waves. Names within a wave are sorted.
func (s MachineProtectionPlanSpec) Order() ([][]string, error) {
	if len(s.Machines) == 0 {
		return nil, fmt.Errorf("machines must not be empty")
	}
	deps := map[string][]string{}
	for _, m := range s.Machines {
		if !protectedNameRE.MatchString(m.Name) {
			return nil, fmt.Errorf("machine name %q is not a DNS label", m.Name)
		}
		if _, dup := deps[m.Name]; dup {
			return nil, fmt.Errorf("machine %s is listed twice", m.Name)
		}
		deps[m.Name] = m.DependsOn
	}
	for name, ds := range deps {
		for _, d := range ds {
			if _, ok := deps[d]; !ok {
				return nil, fmt.Errorf("machine %s depends on %s, which is not in the plan", name, d)
			}
		}
	}
	done := map[string]bool{}
	var waves [][]string
	for len(done) < len(deps) {
		var wave []string
		for name, ds := range deps {
			if done[name] {
				continue
			}
			ready := true
			for _, d := range ds {
				ready = ready && done[d]
			}
			if ready {
				wave = append(wave, name)
			}
		}
		if len(wave) == 0 {
			return nil, fmt.Errorf("machines depend on each other in a cycle")
		}
		sort.Strings(wave)
		for _, n := range wave {
			done[n] = true
		}
		waves = append(waves, wave)
	}
	return waves, nil
}

// MirrorSpec is spec with the fields that only mean something on the
// site that ran it cleared: placement, allocations and device claims.
func MirrorSpec(spec MachineSpec) MachineSpec {
	spec.NodeName = ""
	spec.TopologyAllocation = nil
	spec.Resources.AllocatedCPUSet = nil
	spec.DeviceClaims = nil
	return spec
}

// Replicated reports whether b was imported from the peer site.
func (b MachineBackup) Replicated() bool {
	return b.Metadata.Annotations[ReplicatedFromAnnotation] != ""
}
