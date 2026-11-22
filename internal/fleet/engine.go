// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

type Engine struct {
	Kube             *kube.Client
	Scheduler        scheduler.Scheduler
	Log              *slog.Logger
	ControlNamespace string
	RedfishOrigins   []string
	IsolatedBridges  []string
	Metrics          *Prometheus
	Now              func() time.Time
	// AdmitDisruption spends this reconcile pass's shared disruption budget.
	AdmitDisruption func(model.Machine) error
	AdmitScaleDown  func([]model.Machine, int) error
	acted           map[string]bool
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}
func (e *Engine) Reconcile(ctx context.Context, machines []model.Machine, nodes []model.Node, migrations []model.MachineMigration) error {
	e.acted = map[string]bool{}
	resources := []string{"nodefencerequests", "machinehaprofiles", "machinebalancepolicies", "machineautoscalers", "machinebackupgroups", "machinerecoveryplans", "machineimportplans", "machinevirtualnetworks", "machinenetworkclaims", "machinetemplateclaims", "machineusageledgers"}
	var errs []error
	// Deterministic object order makes conflicts and overlapping intents reproducible.
	for _, r := range resources {
		objects, err := e.Kube.ListFleet(ctx, "", r)
		if kube.IsNotFound(err) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("list %s: %w", r, err))
			continue
		}
		sort.Slice(objects, func(i, j int) bool {
			return objects[i].Metadata.Namespace+"/"+objects[i].Metadata.Name < objects[j].Metadata.Namespace+"/"+objects[j].Metadata.Name
		})
		for _, o := range objects {
			if o.Metadata.DeletionTimestamp != nil {
				continue
			}
			before := o.Status
			if err := Validate(o); err != nil {
				o.Status.Phase = "Invalid"
				o.Status.Message = err.Error()
			} else {
				err = e.step(ctx, &o, machines, nodes, migrations)
				if err != nil {
					o.Status.Phase = "Blocked"
					o.Status.Message = err.Error()
					errs = append(errs, fmt.Errorf("%s %s/%s: %w", o.Kind, o.Metadata.Namespace, o.Metadata.Name, err))
				}
			}
			o.Status.ObservedGeneration = o.Metadata.Generation
			if !reflect.DeepEqual(before, o.Status) {
				if err := e.Kube.PatchFleetStatus(ctx, r, o, o.Status); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}
func (e *Engine) step(ctx context.Context, o *model.FleetResource, machines []model.Machine, nodes []model.Node, migrations []model.MachineMigration) error {
	switch o.Kind {
	case "NodeFenceRequest":
		return e.fenceNode(ctx, o, nodes)
	case "MachineHAProfile":
		return e.highAvailability(ctx, o, machines, nodes, migrations)
	case "MachineBalancePolicy":
		return e.balance(ctx, o, machines, nodes, migrations)
	case "MachineAutoscaler":
		return e.autoscale(ctx, o, machines)
	case "MachineBackupGroup":
		return e.backupGroup(ctx, o)
	case "MachineRecoveryPlan":
		return e.recovery(ctx, o)
	case "MachineImportPlan":
		return e.importPlan(ctx, o)
	case "MachineVirtualNetwork":
		o.Status.Phase = "Ready"
		o.Status.Message = "IPAM ready; bridge and inter-host L2 provisioning are administrator prerequisites"
		return nil
	case "MachineNetworkClaim":
		return e.networkClaim(ctx, o)
	case "MachineTemplateClaim":
		return e.templateClaim(ctx, o)
	case "MachineUsageLedger":
		return e.meter(ctx, o, machines)
	}
	return fmt.Errorf("unhandled fleet kind %s", o.Kind)
}
func childName(o model.FleetResource, role string) string {
	h := sha256.Sum256([]byte(o.Metadata.UID + "/" + o.Metadata.Namespace + "/" + o.Metadata.Name + "/" + role))
	return "fleet-" + hex.EncodeToString(h[:])[:24]
}
func childMeta(o model.FleetResource, name string) model.ObjectMeta {
	return model.ObjectMeta{Name: name, Namespace: o.Metadata.Namespace, Labels: map[string]string{ManagedLabel: o.Metadata.Name, ManagedUIDLabel: o.Metadata.UID}}
}
func owns(o model.FleetResource, meta model.ObjectMeta) bool {
	return o.Metadata.UID != "" && meta.Labels[ManagedUIDLabel] == o.Metadata.UID && meta.Labels[ManagedLabel] == o.Metadata.Name
}
func selected(o model.FleetResource, selector map[string]string, m model.Machine) bool {
	return m.Namespace() == o.Metadata.Namespace && m.Metadata.DeletionTimestamp == nil && model.LabelsMatch(m.Metadata.Labels, selector)
}
func contains(items []string, s string) bool {
	for _, v := range items {
		if v == s {
			return true
		}
	}
	return false
}
func hasMigration(m model.Machine, migrations []model.MachineMigration) bool {
	for _, mig := range migrations {
		if mig.Namespace() == m.Namespace() && mig.Spec.MachineName == m.Metadata.Name && !contains([]string{"Succeeded", "Failed", "Blocked", "Cancelled"}, mig.Status.Phase) {
			return true
		}
	}
	return false
}
func (e *Engine) disrupt(m model.Machine) error {
	if e.AdmitDisruption == nil {
		return fmt.Errorf("disruption guard is not configured")
	}
	return e.AdmitDisruption(m)
}
func (e *Engine) status(o *model.FleetResource, phase, message string) {
	o.Status.Phase = phase
	o.Status.Message = message
}
func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
