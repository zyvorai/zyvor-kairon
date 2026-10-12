// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const (
	condNetworkReady  = "NetworkReady"
	condDNSPublished  = "DNSPublished"
	condTestSucceeded = "LastTestSucceeded"
)

// keepPinnedIPs carries a VirtualNetwork address recorded earlier into a
// freshly mirrored spec whose source Machine no longer reports one (it
// was halted, or its runtime is gone).
func keepPinnedIPs(spec *model.MachineSpec, prev *model.MachineSpec) {
	vn := spec.Network.VirtualNetwork
	if vn == nil || vn.IP != "" || prev == nil || prev.Network.VirtualNetwork == nil {
		return
	}
	if old := prev.Network.VirtualNetwork; old.Name == vn.Name && old.IP != "" {
		pinned := *vn
		pinned.IP = old.IP
		spec.Network.VirtualNetwork = &pinned
	}
}

// checkNetwork reports, for preserveIP, which mirrored Machines could not
// get their address here, and sets NetworkReady.
func (r *protection) checkNetwork(ctx context.Context) {
	r.netProblems = nil
	if !r.plan.Spec.Network.PreserveIP {
		r.st.Conditions = removeCondition(r.st.Conditions, condNetworkReady)
		return
	}
	nets, err := r.c.Kube.ListVirtualNetworksNamespace(ctx, r.plan.Namespace())
	if err != nil && !kube.IsNotFound(err) {
		r.st.Conditions = model.SetCondition(r.st.Conditions, model.Condition{Type: condNetworkReady, Status: "Unknown", Reason: "ListFailed", Message: err.Error()})
		return
	}
	byName := map[string]model.VirtualNetwork{}
	for _, n := range nets {
		byName[n.Metadata.Name] = n
	}
	r.netProblems = map[string]string{}
	for _, ms := range r.st.Machines {
		if ms.Spec == nil || ms.Spec.Network.VirtualNetwork == nil {
			continue
		}
		if p := vnProblem(ms.Name, *ms.Spec.Network.VirtualNetwork, byName); p != "" {
			r.netProblems[ms.Name] = p
		}
	}
	cond := model.Condition{Type: condNetworkReady, Status: "True", Reason: "AddressesAvailable", Message: "every protected Machine's VirtualNetwork address fits here"}
	if len(r.netProblems) > 0 {
		var msgs []string
		for _, pm := range r.plan.Spec.Machines {
			if p := r.netProblems[pm.Name]; p != "" {
				msgs = append(msgs, pm.Name+": "+p)
			}
		}
		cond.Status, cond.Reason, cond.Message = "False", "AddressUnavailable", strings.Join(msgs, "; ")
	}
	r.st.Conditions = model.SetCondition(r.st.Conditions, cond)
}

// vnProblem says why machine cannot have vn's address on this site, or "".
func vnProblem(machine string, vn model.MachineVirtualNetwork, nets map[string]model.VirtualNetwork) string {
	if vn.IP == "" {
		return "no address recorded from the active site yet"
	}
	n, ok := nets[vn.Name]
	if !ok {
		return fmt.Sprintf("VirtualNetwork %s not found here", vn.Name)
	}
	if n.Status.Phase != "Ready" {
		return fmt.Sprintf("VirtualNetwork %s is not Ready", vn.Name)
	}
	p, err := model.VirtualNetworkPrefix(n.Spec.CIDR)
	ip, ipErr := netip.ParseAddr(vn.IP)
	if err != nil || ipErr != nil || !p.Contains(ip) {
		return fmt.Sprintf("%s is outside VirtualNetwork %s (%s)", vn.IP, vn.Name, n.Spec.CIDR)
	}
	for _, a := range n.Status.Allocations {
		if a.IP == vn.IP && a.Machine != machine {
			return fmt.Sprintf("%s is taken by %s here", vn.IP, a.Machine)
		}
	}
	return ""
}

// publishDNS keeps the plan's DNSEndpoint pointing at the Machines here.
func (r *protection) publishDNS(ctx context.Context) {
	recs := r.plan.Spec.Network.DNS
	if len(recs) == 0 {
		if _, had := model.FindCondition(r.st.Conditions, condDNSPublished); had {
			r.withdrawDNS(ctx)
		}
		return
	}
	eps := make([]model.DNSEndpointRecord, 0, len(recs))
	var waiting []string
	for _, rec := range recs {
		m, ok := r.machines[rec.Machine]
		addr := ""
		if ok && m.Status.Phase == "Running" {
			addr = rec.DNSAddress(m)
		}
		if addr == "" {
			waiting = append(waiting, rec.Hostname)
			continue
		}
		eps = append(eps, model.DNSEndpointRecord{DNSName: strings.TrimSuffix(rec.Hostname, "."), RecordType: "A", Targets: []string{addr}, RecordTTL: rec.TTL})
	}
	cond := model.Condition{Type: condDNSPublished, Status: "True", Reason: "Published", Message: fmt.Sprintf("%d of %d records", len(eps), len(recs))}
	if len(waiting) > 0 {
		cond.Message += "; waiting for an address: " + strings.Join(waiting, ", ")
	}
	if err := r.applyDNSEndpoint(ctx, eps); err != nil {
		cond.Status, cond.Reason, cond.Message = "False", "Failed", err.Error()
	} else {
		r.st.DNSRecords = len(eps)
	}
	r.st.Conditions = model.SetCondition(r.st.Conditions, cond)
}

func (r *protection) applyDNSEndpoint(ctx context.Context, eps []model.DNSEndpointRecord) error {
	name, ns := r.plan.PolicyName(), r.plan.Namespace()
	cur, err := r.c.Kube.GetDNSEndpoint(ctx, ns, name)
	switch {
	case kube.IsNotFound(err):
		err = r.c.Kube.CreateDNSEndpoint(ctx, ns, model.DNSEndpoint{
			TypeMeta: model.TypeMeta{APIVersion: model.DNSEndpointAPIVersion, Kind: model.KindDNSEndpoint},
			Metadata: model.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{model.ProtectionPlanLabel: r.plan.Metadata.Name}},
			Spec:     model.DNSEndpointSpec{Endpoints: eps},
		})
		if kube.IsNotFound(err) {
			return errors.New("the DNSEndpoint CRD (externaldns.k8s.io) is not installed")
		}
		return err
	case err != nil:
		return err
	case cur.Metadata.Labels[model.ProtectionPlanLabel] != r.plan.Metadata.Name:
		return fmt.Errorf("DNSEndpoint %s exists and is not this plan's", name)
	case reflect.DeepEqual(cur.Spec.Endpoints, eps):
		return nil
	}
	return r.c.Kube.PatchDNSEndpoint(ctx, ns, name, map[string]any{"spec": map[string]any{"endpoints": eps}})
}

// withdrawDNS removes this site's DNSEndpoint, so only the active site
// publishes the names.
func (r *protection) withdrawDNS(ctx context.Context) {
	r.st.DNSRecords = 0
	if len(r.plan.Spec.Network.DNS) == 0 {
		if _, had := model.FindCondition(r.st.Conditions, condDNSPublished); !had {
			return
		}
	}
	name, ns := r.plan.PolicyName(), r.plan.Namespace()
	cur, err := r.c.Kube.GetDNSEndpoint(ctx, ns, name)
	if err == nil && cur.Metadata.Labels[model.ProtectionPlanLabel] == r.plan.Metadata.Name {
		err = r.c.Kube.DeleteDNSEndpoint(ctx, ns, name)
	}
	if err != nil && !kube.IsNotFound(err) {
		r.st.Conditions = model.SetCondition(r.st.Conditions, model.Condition{Type: condDNSPublished, Status: "False", Reason: "WithdrawFailed", Message: err.Error()})
		return
	}
	r.st.Conditions = removeCondition(r.st.Conditions, condDNSPublished)
}

// scheduleTest starts a DR test when test.every has passed since the last
// one started.
func (r *protection) scheduleTest() {
	r.st.NextTest = nil
	every, _ := r.plan.Spec.TestEvery()
	if every == 0 {
		return
	}
	base := r.plan.Metadata.CreationTimestamp
	if t := r.st.Test; t != nil && t.StartedAt != nil {
		base = *t.StartedAt
	}
	next := base.Add(every)
	r.st.NextTest = &next
	if r.now.Before(next) || r.running(r.st.Failover) || r.st.Test != nil && r.st.Test.Phase != model.ProtectionRunEnded {
		return
	}
	if b := r.plan.Spec.Test.Bridge; b == "" || !slices.Contains(r.c.DR.TestBridges, b) {
		r.st.Message = "scheduled DR test not started: test.bridge must be in kairon-controller's --dr-test-bridges"
		return
	}
	now := r.now
	ttl, _ := r.plan.Spec.TestTTL()
	expires := now.Add(ttl)
	id := "scheduled-" + strconv.FormatInt(now.Unix(), 36)
	r.st.Test = &model.ProtectionRun{ID: id, Phase: model.ProtectionRunRunning, StartedAt: &now, ExpiresAt: &expires, Scheduled: true}
	next = now.Add(every)
	r.st.NextTest = &next
	r.c.Log.Info("protection plan scheduled DR test started", "namespace", r.plan.Namespace(), "plan", r.plan.Metadata.Name, "id", id)
}

// recordTest keeps a finished test's result, which outlives the run once
// its copies are removed.
func (r *protection) recordTest(run *model.ProtectionRun) {
	if run == nil || run.Phase != model.ProtectionRunSucceeded && run.Phase != model.ProtectionRunFailed {
		return
	}
	if lt := r.st.LastTest; lt != nil && lt.ID == run.ID {
		return
	}
	res := &model.ProtectionTestResult{ID: run.ID, Succeeded: run.Phase == model.ProtectionRunSucceeded, Scheduled: run.Scheduled,
		CompletedAt: run.CompletedAt, RTOSeconds: run.RTOSeconds, Message: run.Message}
	r.st.LastTest = res
	cond := model.Condition{Type: condTestSucceeded, Status: "True", Reason: "Succeeded", Message: "test " + run.ID + ": " + run.Message}
	if !res.Succeeded {
		cond.Status, cond.Reason = "False", "Failed"
	}
	r.st.Conditions = model.SetCondition(r.st.Conditions, cond)
}
