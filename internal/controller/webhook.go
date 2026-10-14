// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/zyvorai/kairon/internal/admission"
	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/conversion"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/preempt"
	"github.com/zyvorai/kairon/internal/tenantfence"
	"github.com/zyvorai/kairon/internal/tlsreload"
)

// WebhookHandler returns the mux for kairon-controller's validating
// admission webhook: closes the gaps documented on
// MachineQuota/MachineDisruptionBudget (see internal/model/quota.go,
// internal/model/disruption.go) by rejecting the writes that used to just
// slip through -- a Machine create that would immediately push its
// namespace over quota, and a MachineMigration create that would violate a
// MachineDisruptionBudget. Both handlers reuse exactly the same decision
// functions the reconcile loop (admitQuota) and `kaironctl evacuate`
// (AdmitDisruption) already use -- this file is only the HTTP/admission
// wiring around them, no new enforcement logic.
//
// /validate-machinenetworkpolicy and /validate-networksecuritygroup are
// different in kind, not just target: unlike the two above, there is no
// pre-existing in-process decision function to reuse here, because
// nothing in kairon-controller or kairon-node ever validated
// AllowCidrs/DenyCidrs/AllowPorts syntax before this -- see
// model.ValidateVmNetworkPolicy's doc comment for the failure mode this
// closes (a malformed policy stuck retrying forever instead of being
// rejected once, at write time).
//
// /validate-machinequota closes the same *kind* of gap for a third,
// unrelated field family: Machine.spec.resources.{cpu,memory,maxCpu,
// maxMemory} and MachineQuota.spec.{maxTotalCpu,maxTotalMemory} are all
// free-form quantity strings parsed by model.ParseVCPUs/ParseMemoryMiB,
// and until now nothing checked their syntax at write time either -- see
// validateResourceQuantities and validateMachineQuotaObject below for the
// two failure modes this prevents *going forward* (a Machine stuck
// retrying VM creation forever with a malformed spec.resources, silently
// counted as zero footprint against quota the whole time via
// MachineFootprint; and a brand-new malformed MachineQuota that would
// otherwise abort BuildQuotaTrackers -- see quota.go -- for every
// namespace on every reconcile tick, not just its own). This is
// admission-time prevention only, same opt-in webhook.enabled gate as
// every other check in this file: an already-existing malformed
// MachineQuota (created before this existed, or with the webhook
// disabled) still aborts cluster-wide quota enforcement exactly as
// before -- containing that blast radius would mean changing
// BuildQuotaTrackers' own error contract, a separate, more invasive
// change left out of scope here.
//
// validateNetworkMAC (called from both validateMachineCreate and
// validateMachineResize, same as validateResourceQuantities above)
// closes a fourth instance of the same family of gap, but for a
// field with no FluxVM-specific grammar to mirror at all:
// Machine.spec.network.mac is passed straight through to FluxVM's
// CreateVmRequest.network.mac field exactly as authored (see
// docs/network-fabric.md's field-mapping table), with nothing on
// either side of kairon ever parsing it. A malformed value sails
// through kubectl apply and only fails once kairon-node actually asks
// FluxVM to attach the NIC, at VM create or edit time.
func (c *Controller) WebhookHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /validate-fleet", admission.Handler(c.Log, c.validateFleet, c.observeWebhookDecision))
	mux.HandleFunc("POST /validate-machine", admission.Handler(c.Log, c.validateMachine, c.observeWebhookDecision))
	mux.HandleFunc("POST /validate-machinemigration", admission.Handler(c.Log, c.validateMachineMigration, c.observeWebhookDecision))
	mux.HandleFunc("POST /validate-machinenetworkpolicy", admission.Handler(c.Log, c.validateMachineNetworkPolicy, c.observeWebhookDecision))
	mux.HandleFunc("POST /validate-networksecuritygroup", admission.Handler(c.Log, c.validateNetworkSecurityGroup, c.observeWebhookDecision))
	mux.HandleFunc("POST /validate-machinequota", admission.Handler(c.Log, c.validateMachineQuotaObject, c.observeWebhookDecision))
	// /convert/machinequotas is a scaffold, not live enforcement: every
	// kairon.zyvor.dev version (v1, v1beta1, v1alpha1) shares one schema,
	// so the API server uses the None strategy and never calls this route
	// today. It exists, and is tested, so a future schema-changing version
	// is "wire the CRD's spec.conversion at that version," not "build a
	// conversion webhook from scratch" -- see docs/guides/crd-versioning.md and
	// internal/conversion's package doc comment.
	mux.HandleFunc("POST /convert/machinequotas", conversion.Handler(c.Log, model.KindMachineQuota, conversion.ConvertMachineQuota))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// observeWebhookDecision is the admission.Handler observe callback for
// both routes above -- one choke point for kairon_webhook_decisions_total
// regardless of which resource/validator produced the decision.
func (c *Controller) observeWebhookDecision(resource, operation string, allowed bool) {
	if c.Metrics != nil {
		c.Metrics.ObserveWebhookDecision(resource, operation, allowed)
	}
}

// RunWebhook serves the admission webhook over TLS on addr until ctx is
// canceled. The API server requires HTTPS for webhook calls, so unlike
// internal/health.Server this always needs a certificate -- tlsConfig is
// built once at startup by WebhookTLSConfig (see
// cmd/kairon-controller/main.go), the same "fail fast on a bad cert before
// starting anything" posture as kairon-node's migration TLS setup, rather
// than only discovering a bad cert/key pair on the first admission
// request. certWatcher (WebhookTLSConfig's other return value) is run
// alongside the server so a renewed certificate is picked up without a
// restart -- see internal/tlsreload.
func (c *Controller) RunWebhook(ctx context.Context, addr string, tlsConfig *tls.Config, certWatcher *tlsreload.Watcher, tlsReloadInterval time.Duration) error {
	go certWatcher.Run(ctx, tlsReloadInterval)
	srv := &http.Server{Addr: addr, Handler: c.WebhookHandler(), TLSConfig: tlsConfig}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err := srv.ListenAndServeTLS("", "")
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// validateMachine enforces MachineQuota on a Machine CREATE (admitQuota,
// the exact same check the reconcile loop's own scheduling-time admission
// uses) and, separately, on an UPDATE that grows an already-scheduled
// Machine's spec.resources (admitQuotaResize) -- closing the gap
// documented until now as a real, accepted limitation: "an
// already-scheduled Machine growing past quota via hotplug isn't caught
// by either the webhook or the reconcile loop."
//
// The two cases aren't symmetric the way CREATE-vs-nothing might suggest.
// For CREATE, this webhook is defense-in-depth: the reconcile loop's own
// scheduling-time admitQuota call already backstops the exact same
// decision, so the webhook can never become *stricter* than what it
// backstops. For a resize UPDATE, there is no reconcile-loop equivalent to
// backstop at all -- hotplug is entirely kairon-node's agent's own
// concern (internal/agent/hotplug.go), reconciled per-node with no
// cluster-wide MachineQuota visibility, and kairon-controller's reconcile
// loop only ever calls admitQuota once, at initial scheduling. So unlike
// the CREATE case, this IS new enforcement, not just closing a bypass
// around something already enforced elsewhere -- with webhook.enabled
// false (the default), a resize past quota still isn't caught anywhere,
// same as before this existed.
func (c *Controller) validateMachine(r *http.Request, req *admission.Request) admission.Decision {
	if req.Resource.Resource != "machines" {
		return admission.Allow()
	}
	switch req.Operation {
	case admission.OperationCreate:
		return c.validateMachineCreate(r, req)
	case admission.OperationUpdate:
		return c.validateMachineResize(r, req)
	default:
		return admission.Allow()
	}
}

// validateResourceQuantities checks the free-form CPU/memory quantity
// strings on a Machine's spec.resources against the exact same grammar
// model.ParseVCPUs/model.ParseMemoryMiB already enforce everywhere these
// values are actually consumed (scheduling, kairon-node's hotplug agent,
// and -- the real failure point -- internal/fluxvm's buildCreateRequest,
// called only once kairon-controller actually tries to create the VM).
// Until now nothing checked them at write time: a malformed value (an
// empty string aside, since that's the pre-existing "awaiting
// spec.instanceTypeName resolution" state -- see
// machineNeedsInstanceType in instancetype.go, deliberately left alone
// here) sailed through kubectl apply, then failed only once VM creation
// was attempted, leaving the Machine retrying forever with no path to
// self-heal -- the same failure mode 1c2ae6c already closed for
// MachineNetworkPolicy/NetworkSecurityGroup CIDRs, just for a different
// grammar. Worse for CPU/memory specifically: MachineFootprint's own
// parse calls silently treat the same malformed value as a *zero*
// footprint (internal/controller/quota.go), so a Machine like this
// doesn't just fail late -- it evades MachineQuota accounting the entire
// time it sits stuck.
//
// CPU/Memory are only checked when non-empty, deliberately: the CRD's own
// OpenAPI schema already requires a non-empty string *if* spec.resources
// is set at all (minLength: 1), but a Machine naming
// spec.instanceTypeName instead is expected to leave spec.resources
// entirely unset until kairon-controller's own resolveInstanceTypes
// (instancetype.go) fills it in from the referenced MachineInstanceType --
// rejecting that here would break a working, documented pattern, not
// close a gap. MaxCPU/MaxMemory are always optional.
func validateResourceQuantities(res model.ResourceSpec) error {
	if res.CPU != "" {
		if _, err := model.ParseVCPUs(res.CPU); err != nil {
			return fmt.Errorf("spec.resources.cpu: %v", err)
		}
	}
	if res.Memory != "" {
		if _, err := model.ParseMemoryMiB(res.Memory); err != nil {
			return fmt.Errorf("spec.resources.memory: %v", err)
		}
	}
	if res.MaxCPU != "" {
		if _, err := model.ParseVCPUs(res.MaxCPU); err != nil {
			return fmt.Errorf("spec.resources.maxCpu: %v", err)
		}
	}
	if res.MaxMemory != "" {
		if _, err := model.ParseMemoryMiB(res.MaxMemory); err != nil {
			return fmt.Errorf("spec.resources.maxMemory: %v", err)
		}
	}
	return nil
}

// validateNetworkMAC checks Machine.spec.network.mac, when set, is a
// syntactically valid Ethernet hardware address. Unlike
// validateResourceQuantities above, this isn't
// mirroring a grammar FluxVM re-parses server-side for its own business
// rules -- MAC syntax is a fixed, universal standard (six colon- or
// hyphen-separated hex octets, e.g. "52:54:00:12:34:56" per
// docs/guides/machine-network.md's own tap/macvtap examples), not a
// FluxVM-specific one. Nothing in kairon checks this today:
// NetworkSpec.MAC is passed straight through, unparsed, from the
// Machine spec to FluxVM's CreateVmRequest.network.mac field
// (internal/fluxvm/network.go) -- see docs/network-fabric.md's
// field-mapping table ("VM create / edit"). A malformed value (odd
// hex-digit count, non-hex characters, a wrong separator) sails
// through kubectl apply and only ever fails once kairon-node actually
// asks FluxVM to attach the NIC, at VM create or edit time -- the
// identical "stuck retrying forever, no self-heal" failure mode
// 1c2ae6c and c6bb5b6 already closed for CIDR/port and CPU/memory
// syntax.
//
// net.ParseMAC also accepts an EUI-64 (8-byte) and a 20-byte
// InfiniBand link-layer form; only the 6-byte EUI-48 form is a valid
// Ethernet NIC address, which is what every one of FluxVM's network
// modes (user/tap/macvtap) actually attaches, so those other two
// widths are rejected here too rather than silently accepted as some
// other kind of "valid" hardware address.
func validateNetworkMAC(ns model.NetworkSpec) error {
	if ns.MAC == "" {
		return nil
	}
	hw, err := net.ParseMAC(ns.MAC)
	if err != nil {
		return fmt.Errorf("spec.network.mac %q: %v", ns.MAC, err)
	}
	if len(hw) != 6 {
		return fmt.Errorf("spec.network.mac %q: must be a 6-octet Ethernet address (e.g. 52:54:00:12:34:56), got %d octets", ns.MAC, len(hw))
	}
	return nil
}

func (c *Controller) validateMachineCreate(r *http.Request, req *admission.Request) admission.Decision {
	var m model.Machine
	if err := json.Unmarshal(req.Object, &m); err != nil {
		return admission.Deny(fmt.Sprintf("decode Machine: %v", err))
	}
	// Defense in depth: kairon-node's own reconcile also rejects a
	// malformed spec.image.source, but that only surfaces as a stuck
	// Machine status, not an immediate, actionable API error.
	if err := model.ValidateImageSource(m.Spec.Image); err != nil {
		return admission.Deny(err.Error())
	}
	if err := validateResourceQuantities(m.Spec.Resources); err != nil {
		return admission.Deny(err.Error())
	}
	if m.Metadata.Namespace == "" {
		m.Metadata.Namespace = req.Namespace
	}
	if msg := c.checkInstanceTypeRefs(r.Context(), m); msg != "" {
		return admission.Deny(msg)
	}
	if err := validateNetworkMAC(m.Spec.Network); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateDisks(m.Spec.Disks); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateRootImage(m.Spec.Image); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateCdroms(m.Spec.Cdroms); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateTopology(m.Spec); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateAccelerators(m.Spec, m.Metadata.Annotations); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateElasticMemory(m.Spec); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateExtraInterfaces(m.Spec.Network); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateDataplaneMode(m.Spec.Network); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateCiliumAttach(m.Spec.Network); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateMachineVirtualNetwork(m.Spec.Network); err != nil {
		return admission.Deny(err.Error())
	}
	if err := tenantfence.CheckCreate(m.Spec.Tenant, m.Metadata.Annotations); err != nil {
		return admission.Deny(err.Error())
	}
	if err := preempt.ValidatePolicy(m.Metadata.Annotations[preempt.AnnPolicy]); err != nil {
		return admission.Deny(err.Error())
	}
	if err := agentplane.AdmitMachine(agentplane.MachineAdmission{
		Namespace: req.Namespace, Name: m.Metadata.Name, Tenant: m.Spec.Tenant,
		Annotations: m.Metadata.Annotations, ImageDigest: m.Spec.Image.Digest,
		DataplaneMode: m.Spec.Network.DataplaneMode, DeviceClaims: len(m.Spec.DeviceClaims),
	}); err != nil {
		return admission.Deny(err.Error())
	}
	if err := c.verifyAgentImage(r.Context(), m); err != nil {
		return admission.Deny(err.Error())
	}
	if err := c.checkAttestationWrite(req.UserInfo.Username, nil, m.Metadata.Annotations); err != nil {
		return admission.Deny(err.Error())
	}
	trackers, ok, err := QuotaTrackersForNamespace(r.Context(), c.Kube, req.Namespace)
	if err != nil {
		return admission.Deny(err.Error())
	}
	if !ok {
		return admission.Allow()
	}
	if blocker := admitQuota(trackers, m); blocker != "" {
		return admission.Deny(blocker)
	}
	return admission.Allow()
}

// validateMachineResize only ever denies a *growing* resize of an
// already-scheduled Machine -- see validateMachine's own doc comment for
// why this is the one quota check with no reconcile-loop backstop. A
// Machine that isn't yet scheduled falls through to Allow(): the
// reconcile loop's own scheduling-time admitQuota call covers that case
// exactly like a CREATE would (machineCountsTowardQuota mirrors
// buildQuotaTrackers' own seed-pass predicate exactly, so "not yet
// scheduled" here means the same thing it means there). A shrink or
// no-change UPDATE is also always allowed, without even listing quotas:
// it can only ever lower usage below what was already accepted.
func (c *Controller) validateMachineResize(r *http.Request, req *admission.Request) admission.Decision {
	var oldM, newM model.Machine
	if err := json.Unmarshal(req.OldObject, &oldM); err != nil {
		return admission.Deny(fmt.Sprintf("decode old Machine: %v", err))
	}
	if err := json.Unmarshal(req.Object, &newM); err != nil {
		return admission.Deny(fmt.Sprintf("decode Machine: %v", err))
	}
	// Checked unconditionally, before the MachineCountsTowardQuota
	// short-circuit below: a malformed resize should be rejected
	// regardless of whether this Machine happens to count toward a quota
	// right now, the same way validateMachineCreate's own
	// validateResourceQuantities/validateNetworkMAC calls run before
	// quotas even enter the picture. validateNetworkMAC matters here too,
	// not just on CREATE: an UPDATE is also how spec.network.mac gets
	// edited on an already-existing Machine (docs/network-fabric.md's
	// field-mapping table lists "VM create / edit" together), and this is
	// the same validateMachineResize call that handles every Machine
	// UPDATE, not just a resource resize.
	if err := validateResourceQuantities(newM.Spec.Resources); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateTopologyUpdate(oldM.Spec, newM.Spec); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateElasticMemoryUpdate(oldM.Spec, newM.Spec); err != nil {
		return admission.Deny(err.Error())
	}
	if err := tenantfence.CheckUpdate(oldM.Spec.Tenant, newM.Spec.Tenant, newM.Metadata.Annotations); err != nil {
		return admission.Deny(err.Error())
	}
	if err := preempt.ValidatePolicy(newM.Metadata.Annotations[preempt.AnnPolicy]); err != nil {
		return admission.Deny(err.Error())
	}
	if err := c.checkAttestationWrite(req.UserInfo.Username, oldM.Metadata.Annotations, newM.Metadata.Annotations); err != nil {
		return admission.Deny(err.Error())
	}
	if err := validateNetworkMAC(newM.Spec.Network); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateDataplaneMode(newM.Spec.Network); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateCiliumAttach(newM.Spec.Network); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateCdroms(newM.Spec.Cdroms); err != nil {
		return admission.Deny(err.Error())
	}
	if err := model.ValidateCdromsUpdate(oldM.Spec.Cdroms, newM.Spec.Cdroms); err != nil {
		return admission.Deny(err.Error())
	}
	if !MachineCountsTowardQuota(oldM) {
		return admission.Allow()
	}
	oldCPU, oldMem := MachineFootprint(oldM)
	newCPU, newMem := MachineFootprint(newM)
	if newCPU <= oldCPU && newMem <= oldMem {
		return admission.Allow()
	}
	trackers, ok, err := QuotaTrackersForNamespace(r.Context(), c.Kube, req.Namespace)
	if err != nil {
		return admission.Deny(err.Error())
	}
	if !ok {
		return admission.Allow()
	}
	if blocker := AdmitQuotaResize(trackers, req.Namespace, oldCPU, newCPU, oldMem, newMem); blocker != "" {
		return admission.Deny(blocker)
	}
	return admission.Allow()
}

// validateMachineMigration only applies to CREATE: a MachineMigration is
// never mutated in a way that changes which Machine it disrupts after
// creation (see model.MachineMigrationSpec), so there's nothing for
// UPDATE to re-check.
func (c *Controller) validateMachineMigration(r *http.Request, req *admission.Request) admission.Decision {
	if req.Resource.Resource != "machinemigrations" || req.Operation != admission.OperationCreate {
		return admission.Allow()
	}
	var mig model.MachineMigration
	if err := json.Unmarshal(req.Object, &mig); err != nil {
		return admission.Deny(fmt.Sprintf("decode MachineMigration: %v", err))
	}
	ctx := r.Context()
	budgets, err := c.Kube.ListMachineDisruptionBudgetsNamespace(ctx, req.Namespace)
	if err != nil {
		return admission.Deny(fmt.Sprintf("list MachineDisruptionBudgets: %v", err))
	}
	if len(budgets) == 0 {
		return admission.Allow()
	}
	machines, err := c.Kube.ListMachinesNamespace(ctx, req.Namespace)
	if err != nil {
		return admission.Deny(fmt.Sprintf("list Machines: %v", err))
	}
	var target model.Machine
	found := false
	for _, m := range machines {
		if m.Metadata.Name == mig.Spec.MachineName {
			target, found = m, true
			break
		}
	}
	if !found {
		// No matching Machine to check a budget's Selector against --
		// consistent with AdmitDisruption's own "no matching budget,
		// always allowed" behavior for an unmanaged Machine, not a new
		// leniency invented here.
		return admission.Allow()
	}
	migrations, err := c.Kube.ListMachineMigrationsNamespace(ctx, req.Namespace)
	if err != nil {
		return admission.Deny(fmt.Sprintf("list MachineMigrations: %v", err))
	}
	states, err := LoadBudgetStates(budgets, machines, migrations)
	if err != nil {
		return admission.Deny(err.Error())
	}
	if blocker := AdmitDisruption(states, target); blocker != "" {
		return admission.Deny(blocker)
	}
	return admission.Allow()
}

// validateMachineNetworkPolicy rejects a MachineNetworkPolicy whose
// spec.policy has a syntax FluxVM's own eBPF dataplane will never
// accept -- see model.ValidateVmNetworkPolicy's doc comment for exactly
// which fields and why this exists at all (until now, nothing checked
// these at write time; a malformed entry only ever surfaced as a status
// stuck permanently in Error, retried forever by every node the policy
// selected a Machine on). Both CREATE and UPDATE are checked: unlike
// MachineMigration, a MachineNetworkPolicy is routinely edited in place
// (`kubectl edit`) and internal/agent/network.go re-applies whatever is
// currently in spec on every reconcile tick regardless of which
// operation last changed it.
func (c *Controller) validateMachineNetworkPolicy(r *http.Request, req *admission.Request) admission.Decision {
	if req.Resource.Resource != "machinenetworkpolicies" {
		return admission.Allow()
	}
	switch req.Operation {
	case admission.OperationCreate, admission.OperationUpdate:
	default:
		return admission.Allow()
	}
	var p model.MachineNetworkPolicy
	if err := json.Unmarshal(req.Object, &p); err != nil {
		return admission.Deny(fmt.Sprintf("decode MachineNetworkPolicy: %v", err))
	}
	if err := model.ValidateVmNetworkPolicy(p.Spec.Policy); err != nil {
		return admission.Deny(fmt.Sprintf("spec.policy.%v", err))
	}
	if err := tenantfence.CheckPolicy(p.Spec.Selector, p.Metadata.Labels); err != nil {
		return admission.Deny(err.Error())
	}
	return admission.Allow()
}

// validateNetworkSecurityGroup is validateMachineNetworkPolicy's exact
// counterpart for NetworkSecurityGroup -- same underlying
// model.VmNetworkPolicy embedded field, same FluxVM-side consumer
// (UpsertNetworkGroup instead of SetVMNetworkPolicy), same
// reconcile-loop-retries-forever failure mode without this
// (reconcileSecurityGroup in internal/agent/network.go).
func (c *Controller) validateNetworkSecurityGroup(r *http.Request, req *admission.Request) admission.Decision {
	if req.Resource.Resource != "networksecuritygroups" {
		return admission.Allow()
	}
	switch req.Operation {
	case admission.OperationCreate, admission.OperationUpdate:
	default:
		return admission.Allow()
	}
	var g model.NetworkSecurityGroup
	if err := json.Unmarshal(req.Object, &g); err != nil {
		return admission.Deny(fmt.Sprintf("decode NetworkSecurityGroup: %v", err))
	}
	if err := model.ValidateVmNetworkPolicy(g.Spec.Policy); err != nil {
		return admission.Deny(fmt.Sprintf("spec.policy.%v", err))
	}
	return admission.Allow()
}

// validateMachineQuotaObject rejects a MachineQuota whose
// maxTotalCpu/maxTotalMemory can't be parsed by
// model.ParseVCPUs/model.ParseMemoryMiB -- named ...Object, not
// ...MachineQuota, to keep it unambiguously distinct from
// validateMachine's own MachineQuota-enforcement-against-a-Machine logic
// above (which validates Machines, not MachineQuota objects themselves,
// and is registered under the existing "machinequota.kairon.zyvor.dev"
// ValidatingWebhookConfiguration entry). Until now nothing validated
// these two fields at write time: BuildQuotaTrackers (quota.go) is the
// only thing that ever parses them, and it does so for *every*
// MachineQuota in the cluster on *every* reconcile tick -- so a single
// malformed value anywhere aborts quota enforcement and Machine
// scheduling cluster-wide, not just for its own namespace. See
// validateResourceQuantities's doc comment above for this change's
// overall scope and honest limits (admission-time prevention only, opt-in,
// doesn't retroactively fix an already-existing malformed object).
func (c *Controller) validateMachineQuotaObject(r *http.Request, req *admission.Request) admission.Decision {
	if req.Resource.Resource != "machinequotas" {
		return admission.Allow()
	}
	switch req.Operation {
	case admission.OperationCreate, admission.OperationUpdate:
	default:
		return admission.Allow()
	}
	var q model.MachineQuota
	if err := json.Unmarshal(req.Object, &q); err != nil {
		return admission.Deny(fmt.Sprintf("decode MachineQuota: %v", err))
	}
	if q.Spec.MaxTotalCPU != "" {
		if _, err := model.ParseVCPUs(q.Spec.MaxTotalCPU); err != nil {
			return admission.Deny(fmt.Sprintf("spec.maxTotalCpu: %v", err))
		}
	}
	if q.Spec.MaxTotalMemory != "" {
		if _, err := model.ParseMemoryMiB(q.Spec.MaxTotalMemory); err != nil {
			return admission.Deny(fmt.Sprintf("spec.maxTotalMemory: %v", err))
		}
	}
	return admission.Allow()
}

// WebhookTLSConfig loads the webhook's serving certificate into a
// tlsreload.Watcher and builds a *tls.Config around it. Called once by
// cmd/kairon-controller/main.go at startup, before RunWebhook, so a bad
// cert/key pair is a startup failure, not a surprise on the first
// admission request. The returned watcher must be passed to RunWebhook so
// a renewed certificate is picked up without a restart -- see
// internal/tlsreload.
func WebhookTLSConfig(log *slog.Logger, certFile, keyFile string) (*tls.Config, *tlsreload.Watcher, error) {
	watcher, err := tlsreload.New(log, certFile, keyFile)
	if err != nil {
		return nil, nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: watcher.GetCertificate}, watcher, nil
}
