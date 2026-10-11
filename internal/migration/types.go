// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	safeID   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,252}$`)
)

var ErrUnsupported = errors.New("live migration backend unsupported")

type Session struct {
	ID                string `json:"id"`
	Namespace         string `json:"namespace"`
	Machine           string `json:"machine"`
	SourceNode        string `json:"sourceNode"`
	TargetNode        string `json:"targetNode"`
	RuntimeID         string `json:"runtimeID,omitempty"`
	Phase             string `json:"phase"`
	Backend           string `json:"backend,omitempty"`
	Endpoint          string `json:"endpoint,omitempty"`
	TransferSupported bool   `json:"transferSupported"`
	Reason            string `json:"reason,omitempty"`
	TransferID        string `json:"transferID,omitempty"`
	// DataPlaneEncrypted mirrors PrepareResult.DataPlaneEncrypted -- set once
	// from the destination driver's Prepare response.
	DataPlaneEncrypted bool            `json:"dataPlaneEncrypted,omitempty"`
	NetworkSnapshot    json.RawMessage `json:"networkSnapshot,omitempty"`
	// ConntrackSnapshot is the per-Machine eBPF conntrack map exported on
	// the source before live migration. Empty keeps the previous protocol.
	ConntrackSnapshot json.RawMessage `json:"conntrackSnapshot,omitempty"`
	// DiskPath, MAC, VCPUs and MemoryMiB describe the source runtime's
	// current, live configuration -- not just the Machine's original spec
	// -- so a real hypervisor-level adapter has enough information to
	// provision a topology-matching receiver on the target (storage
	// contract v1: shared storage, no disk copy, so DiskPath must name the
	// exact file the target will also open; MAC must be identical on both
	// sides since the destination device config must match the source's
	// migrated device state exactly). Populated by the source node's
	// agent from its own FluxVM record, not from the Machine spec, since
	// FluxVM may have resolved defaults (e.g. an auto-generated MAC) the
	// original spec never pinned down. Additive/optional: a stub or
	// protocol-only adapter that doesn't need them simply ignores them.
	DiskPath  string `json:"diskPath,omitempty"`
	MAC       string `json:"mac,omitempty"`
	VCPUs     uint32 `json:"vcpus,omitempty"`
	MemoryMiB uint64 `json:"memoryMiB,omitempty"`
	// MigrationNetwork names a migration network configured on the
	// destination node's adapter, used to pick which address the transfer
	// binds/advertises instead of the adapter's default. Empty is fully
	// backward compatible -- the adapter falls back to today's behavior.
	MigrationNetwork string `json:"migrationNetwork,omitempty"`
	// SourceRecord is the source runtime's FluxVM record (GET /v1/vms/{id}
	// on the source node), so the destination can launch a receiver with
	// the identical device model and adopt it as a real VM afterwards.
	SourceRecord json.RawMessage `json:"sourceRecord,omitempty"`
	// Stream lists the stream capabilities the source will use. QEMU
	// refuses a stream whose capabilities differ between the two ends, so
	// the receiver must enable the same ones before it starts listening.
	Stream *StreamCapabilities `json:"stream,omitempty"`
	// DestinationRuntimeID is the destination runtime the adapter created
	// for this session (set by the destination from PrepareResult). Network
	// state is restored there after commit.
	DestinationRuntimeID string `json:"destinationRuntimeID,omitempty"`
	// AcceptedStream is PrepareResult.Stream, persisted for idempotent
	// prepare replies. Set by the destination.
	AcceptedStream *StreamCapabilities `json:"acceptedStream,omitempty"`
	CreatedAt      time.Time           `json:"createdAt"`
	UpdatedAt      time.Time           `json:"updatedAt"`
}

// StreamCapabilities must match on both ends of a QEMU migration stream.
type StreamCapabilities struct {
	MultifdChannels uint8        `json:"multifdChannels,omitempty"`
	Compression     *Compression `json:"compression,omitempty"`
	PostCopy        bool         `json:"postCopy,omitempty"`
}

// Compression is multifd stream compression: algorithm none|zlib|zstd.
type Compression struct {
	Algorithm string `json:"algorithm"`
	Level     *uint8 `json:"level,omitempty"`
}

func (s Session) Validate() error {
	if !safeID.MatchString(s.ID) {
		return fmt.Errorf("invalid id %q", s.ID)
	}
	for name, value := range map[string]string{
		"namespace": s.Namespace, "machine": s.Machine,
		"sourceNode": s.SourceNode, "targetNode": s.TargetNode,
	} {
		if !safeName.MatchString(value) {
			return fmt.Errorf("invalid %s %q", name, value)
		}
	}
	if s.SourceNode == s.TargetNode {
		return errors.New("sourceNode and targetNode must differ")
	}
	if s.MigrationNetwork != "" && !safeName.MatchString(s.MigrationNetwork) {
		return fmt.Errorf("invalid migrationNetwork %q", s.MigrationNetwork)
	}
	return nil
}

func (s Session) SameIdentity(other Session) bool {
	return s.ID == other.ID && s.Namespace == other.Namespace && s.Machine == other.Machine &&
		s.SourceNode == other.SourceNode && s.TargetNode == other.TargetNode && s.RuntimeID == other.RuntimeID
}

type PrepareRequest struct {
	Session Session `json:"session"`
}

type PrepareResult struct {
	TransferSupported bool   `json:"transferSupported"`
	Endpoint          string `json:"endpoint,omitempty"`
	Backend           string `json:"backend,omitempty"`
	Reason            string `json:"reason,omitempty"`
	// DataPlaneEncrypted reports whether the destination adapter has TLS
	// configured for the QEMU migration data stream (distinct from this
	// RPC's own always-on mTLS). See model.MachineMigrationStatus's field
	// of the same name for the full contract.
	DataPlaneEncrypted bool `json:"dataPlaneEncrypted,omitempty"`
	// DestinationRuntimeID names the runtime the receiver becomes once
	// committed. Empty for adapters whose destination runtime keeps the
	// source's ID; network restore then targets session.RuntimeID.
	DestinationRuntimeID string `json:"destinationRuntimeID,omitempty"`
	// Stream is what the receiver actually enabled; the source must start
	// with exactly these. Nil from adapters that predate stream
	// negotiation, which only ever honored multifd and the mode.
	Stream *StreamCapabilities `json:"stream,omitempty"`
}

type PrepareResponse struct {
	SessionID          string `json:"sessionID"`
	Phase              string `json:"phase"`
	TransferSupported  bool   `json:"transferSupported"`
	Endpoint           string `json:"endpoint,omitempty"`
	Backend            string `json:"backend,omitempty"`
	Reason             string `json:"reason,omitempty"`
	DataPlaneEncrypted bool   `json:"dataPlaneEncrypted,omitempty"`
	// Stream echoes PrepareResult.Stream.
	Stream *StreamCapabilities `json:"stream,omitempty"`
}

type DestinationDriver interface {
	Prepare(context.Context, Session) (PrepareResult, error)
	Commit(context.Context, Session) error
	Abort(context.Context, Session) error
}

// DiagnosisResult is a read-only snapshot of what's actually known about a
// migration stuck in NeedsRecovery -- both the destination's own durable
// session-store phase and, when a Flux client is available, live ground
// truth from the destination's FluxVM. SessionPhase alone is NOT proof of
// whether the destination actually committed: Commit() only advances the
// store to "Committed" after both the inner adapter commit AND network
// resume succeed, so a destination VM can be genuinely running while the
// store still says "Prepared" (see NetworkAwareDestination.Commit).
type DiagnosisResult struct {
	SessionPhase             string    `json:"sessionPhase"`
	DestinationRuntimeFound  bool      `json:"destinationRuntimeFound"`
	DestinationRuntimeStatus string    `json:"destinationRuntimeStatus,omitempty"`
	DestinationRuntimeID     string    `json:"destinationRuntimeID,omitempty"`
	DestinationGuestIP       string    `json:"destinationGuestIP,omitempty"`
	ObservedAt               time.Time `json:"observedAt"`
}

// Diagnosable is implemented by a DestinationDriver that can report live
// ground truth about a session, beyond its own durable phase -- kept
// separate from DestinationDriver (rather than added to it) so existing
// implementations and test fakes don't need changes. Callers should
// type-assert for it and degrade gracefully (report only the session-store
// phase) when a driver doesn't implement it.
type Diagnosable interface {
	Diagnose(ctx context.Context, session Session) (DiagnosisResult, error)
}

type UnsupportedDestinationDriver struct{ Reason string }

func (d UnsupportedDestinationDriver) Prepare(context.Context, Session) (PrepareResult, error) {
	reason := strings.TrimSpace(d.Reason)
	if reason == "" {
		reason = "no destination migration adapter is configured"
	}
	return PrepareResult{TransferSupported: false, Reason: reason}, nil
}
func (UnsupportedDestinationDriver) Commit(context.Context, Session) error { return ErrUnsupported }
func (UnsupportedDestinationDriver) Abort(context.Context, Session) error  { return nil }

type SourceOptions struct {
	Mode            string `json:"mode,omitempty"`
	BandwidthMbps   uint64 `json:"bandwidthMbps,omitempty"`
	MaxDowntimeMs   uint64 `json:"maxDowntimeMs,omitempty"`
	MultifdChannels uint8  `json:"multifdChannels,omitempty"`
	// AutoConverge enables QEMU auto-converge with these throttle
	// percentages; Tune raises them later.
	AutoConverge *AutoConverge `json:"autoConverge,omitempty"`
	Compression  *Compression  `json:"compression,omitempty"`
	// PostCopy is disabled|immediate|armed. Empty keeps Mode's meaning
	// (post-copy = immediate).
	PostCopy string `json:"postCopy,omitempty"`
}

const (
	PostCopyDisabled  = "disabled"
	PostCopyImmediate = "immediate"
	PostCopyArmed     = "armed"
)

// AutoConverge throttle, in percent of guest vCPU time (1-99).
type AutoConverge struct {
	InitialPct   uint8 `json:"initialPct,omitempty"`
	IncrementPct uint8 `json:"incrementPct,omitempty"`
	MaxPct       uint8 `json:"maxPct,omitempty"`
}

// TuneRequest changes an in-flight transfer; zero fields are left alone.
type TuneRequest struct {
	BandwidthMbps           uint64 `json:"bandwidthMbps,omitempty"`
	MaxDowntimeMs           uint64 `json:"maxDowntimeMs,omitempty"`
	CPUThrottleInitialPct   uint8  `json:"cpuThrottleInitialPct,omitempty"`
	CPUThrottleIncrementPct uint8  `json:"cpuThrottleIncrementPct,omitempty"`
	MaxCPUThrottlePct       uint8  `json:"maxCPUThrottlePct,omitempty"`
}

func (t TuneRequest) IsZero() bool { return t == TuneRequest{} }

// Capabilities is what the local adapter can do beyond start/status/abort.
// The zero value (an adapter without GET /v1/capabilities) means none.
type Capabilities struct {
	Tune                bool     `json:"tune,omitempty"`
	PostCopySwitch      bool     `json:"postCopySwitch,omitempty"`
	AutoConverge        bool     `json:"autoConverge,omitempty"`
	Compression         []string `json:"compression,omitempty"`
	PostCopyWithMultifd bool     `json:"postCopyWithMultifd,omitempty"`
	Finish              bool     `json:"finish,omitempty"`
}

func (c Capabilities) SupportsCompression(algorithm string) bool {
	if algorithm == "" || algorithm == "none" {
		return true
	}
	for _, a := range c.Compression {
		if a == algorithm {
			return true
		}
	}
	return false
}

// Telemetry is optional transfer detail; adapters report what they have.
type Telemetry struct {
	DirtyPagesRate     uint64  `json:"dirtyPagesRate,omitempty"`
	DirtySyncCount     uint64  `json:"dirtySyncCount,omitempty"`
	PageSize           uint64  `json:"pageSize,omitempty"`
	DuplicatePages     uint64  `json:"duplicatePages,omitempty"`
	NormalBytes        uint64  `json:"normalBytes,omitempty"`
	Mbps               float64 `json:"mbps,omitempty"`
	ExpectedDowntimeMs uint64  `json:"expectedDowntimeMs,omitempty"`
	CPUThrottlePct     uint64  `json:"cpuThrottlePct,omitempty"`
	SetupTimeMs        uint64  `json:"setupTimeMs,omitempty"`
	PostcopyRequests   uint64  `json:"postcopyRequests,omitempty"`
}

// DirtyRateMbps converts the dirty page rate to megabits per second, the
// unit the transfer rate is reported in. 0 when either input is unknown.
func (t Telemetry) DirtyRateMbps() float64 {
	if t.DirtyPagesRate == 0 || t.PageSize == 0 {
		return 0
	}
	return float64(t.DirtyPagesRate*t.PageSize) * 8 / 1e6
}

type SourceRequest struct {
	Session   Session       `json:"session"`
	RuntimeID string        `json:"runtimeID"`
	Endpoint  string        `json:"endpoint"`
	Options   SourceOptions `json:"options,omitempty"`
}

type TransferStatus struct {
	TransferID     string `json:"transferID,omitempty"`
	Phase          string `json:"phase"`
	Message        string `json:"message,omitempty"`
	RAMTransferred uint64 `json:"ramTransferred,omitempty"`
	RAMRemaining   uint64 `json:"ramRemaining,omitempty"`
	RAMTotal       uint64 `json:"ramTotal,omitempty"`
	TotalTimeMs    uint64 `json:"totalTimeMs,omitempty"`
	DowntimeMs     uint64 `json:"downtimeMs,omitempty"`
	// PostCopyActive is true once the transfer has switched to post-copy:
	// from then on the destination owns guest memory.
	PostCopyActive bool       `json:"postCopyActive,omitempty"`
	Telemetry      *Telemetry `json:"telemetry,omitempty"`
}

type SourceDriver interface {
	Start(context.Context, SourceRequest) (TransferStatus, error)
	Status(context.Context, Session, string) (TransferStatus, error)
	Abort(context.Context, Session, string) error
}

// Tuner is implemented by source drivers that can steer an in-flight
// transfer. Callers type-assert for it and check Capabilities first, the
// same way Diagnosable is optional.
type Tuner interface {
	Capabilities(context.Context) (Capabilities, error)
	Tune(context.Context, Session, string, TuneRequest) (TransferStatus, error)
	StartPostCopy(context.Context, Session, string) (TransferStatus, error)
}

// Finisher is implemented by source drivers that must release the paused
// source runtime once the destination has committed.
type Finisher interface {
	Finish(context.Context, Session, string) error
}

type UnsupportedSourceDriver struct{ Reason string }

func (d UnsupportedSourceDriver) Start(context.Context, SourceRequest) (TransferStatus, error) {
	reason := strings.TrimSpace(d.Reason)
	if reason == "" {
		reason = "no source migration adapter is configured"
	}
	return TransferStatus{}, fmt.Errorf("%w: %s", ErrUnsupported, reason)
}
func (UnsupportedSourceDriver) Status(context.Context, Session, string) (TransferStatus, error) {
	return TransferStatus{}, ErrUnsupported
}
func (UnsupportedSourceDriver) Abort(context.Context, Session, string) error { return nil }
