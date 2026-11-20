// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/admission"
	"github.com/zyvorai/kairon/internal/fleet"
	"github.com/zyvorai/kairon/internal/model"
)

func TestFleetApprovalAdmissionIdentityAndReplay(t *testing.T) {
	now := time.Now().UTC()
	spec := fleet.ApprovalSpec{Action: "delete", ArgumentsHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Principal: "agent", Approver: "human", TargetResource: "machines", TargetName: "vm", TargetUID: "uid", ExpiresAt: now.Add(time.Minute)}
	raw, _ := json.Marshal(spec)
	obj := model.FleetResource{TypeMeta: model.TypeMeta{Kind: "MachineActionApproval", APIVersion: model.FleetAPIVersion}, Metadata: model.ObjectMeta{Name: "approval", Namespace: "default"}, Spec: raw}
	encode := func(o model.FleetResource) json.RawMessage { b, _ := json.Marshal(o); return b }
	req := &admission.Request{Operation: "CREATE", Namespace: "default", Resource: admission.GroupVersionResource{Group: "fleet.kairon.zyvor.dev", Resource: "machineactionapprovals"}, UserInfo: admission.UserInfo{Username: "agent"}, Object: encode(obj)}
	c := &Controller{}
	if c.validateFleet(nil, req).Allowed {
		t.Fatal("agent impersonated approver")
	}
	req.UserInfo.Username = "human"
	if !c.validateFleet(nil, req).Allowed {
		t.Fatal("human approval denied")
	}
	req.Operation = "UPDATE"
	req.OldObject = encode(obj)
	req.UserInfo.Username = "human"
	obj.Status = model.FleetStatus{Phase: "Consumed", LastActionTime: &now}
	req.Object = encode(obj)
	if c.validateFleet(nil, req).Allowed {
		t.Fatal("approver consumed agent's intent")
	}
	req.UserInfo.Username = "agent"
	if !c.validateFleet(nil, req).Allowed {
		t.Fatal("bound agent consume denied")
	}
	req.OldObject = encode(obj)
	obj.Status = model.FleetStatus{}
	req.Object = encode(obj)
	if c.validateFleet(nil, req).Allowed {
		t.Fatal("consumed approval reset allowed")
	}
}

func TestFleetRecoveryAdmissionAllowsStartOnly(t *testing.T) {
	spec := fleet.RecoverySpec{Mode: "RestoreInPlace", MaxDataAgeSeconds: 3600, Steps: []fleet.RecoveryStep{{MachineName: "vm", BackupName: "backup"}}}
	raw, _ := json.Marshal(spec)
	obj := model.FleetResource{TypeMeta: model.TypeMeta{Kind: "MachineRecoveryPlan"}, Metadata: model.ObjectMeta{Name: "plan", Namespace: "default"}, Spec: raw}
	old, _ := json.Marshal(obj)
	spec.Start = true
	obj.Spec, _ = json.Marshal(spec)
	next, _ := json.Marshal(obj)
	req := &admission.Request{Operation: "UPDATE", Resource: admission.GroupVersionResource{Group: "fleet.kairon.zyvor.dev", Resource: "machinerecoveryplans"}, OldObject: old, Object: next}
	c := &Controller{}
	if !c.validateFleet(nil, req).Allowed {
		t.Fatal("paused plan cannot start")
	}
	spec.Steps[0].BackupName = "other"
	obj.Spec, _ = json.Marshal(spec)
	req.Object, _ = json.Marshal(obj)
	if c.validateFleet(nil, req).Allowed {
		t.Fatal("recovery target changed")
	}
}
