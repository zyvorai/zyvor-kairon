// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func eventsWithReason(events []model.Event, reason string) []model.Event {
	var out []model.Event
	for _, e := range events {
		if e.Reason == reason {
			out = append(out, e)
		}
	}
	return out
}

func checkMachineSetEvent(t *testing.T, ev model.Event, reason, typ, nameSubstring string) {
	t.Helper()
	if ev.Reason != reason || ev.Type != typ {
		t.Fatalf("event reason/type = %s/%s, want %s/%s (message %q)", ev.Reason, ev.Type, reason, typ, ev.Message)
	}
	if ev.InvolvedObject.Kind != model.KindMachineSet || ev.InvolvedObject.Name != "ms1" || ev.InvolvedObject.Namespace != "prod" || ev.InvolvedObject.UID != "ms1-uid" {
		t.Fatalf("involvedObject = %+v, want MachineSet ms1/prod uid=ms1-uid", ev.InvolvedObject)
	}
	if ev.Source.Component != "kairon-controller" {
		t.Fatalf("source.component = %q, want kairon-controller", ev.Source.Component)
	}
	if nameSubstring != "" && !strings.Contains(ev.Message, nameSubstring) {
		t.Fatalf("message %q does not contain %q", ev.Message, nameSubstring)
	}
}

func eventTestMachineSet(replicas int) model.MachineSet {
	ms := testMachineSet(replicas, "", "")
	ms.Metadata.UID = "ms1-uid"
	ms.Metadata.Finalizers = []string{model.FinalizerMachineSet}
	return ms
}

// TestReconcileMachineSetEmitsScaledUpEvents: creating missing replicas
// posts one Normal ScaledUp Event per Machine, with the running replica
// count in the message. A later steady-state tick (already at desired)
// posts nothing.
func TestReconcileMachineSetEmitsScaledUpEvents(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := eventTestMachineSet(3)
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, nil); err != nil {
		t.Fatalf("reconcileMachineSet: %v", err)
	}
	got := eventsWithReason(fake.events, machineSetReasonScaledUp)
	if len(got) != 3 {
		t.Fatalf("want 3 ScaledUp events, got %+v", fake.events)
	}
	for i, ev := range got {
		want := "replicas " + []string{"0->1", "1->2", "2->3"}[i]
		checkMachineSetEvent(t, ev, machineSetReasonScaledUp, "Normal", "created machine")
		if !strings.Contains(ev.Message, want) {
			t.Fatalf("event %d message %q, want %s", i, ev.Message, want)
		}
		if !strings.Contains(ev.Message, fake.created[i].Metadata.Name) {
			t.Fatalf("event %d message %q, want machine %s", i, ev.Message, fake.created[i].Metadata.Name)
		}
	}

	// Steady state: three current-template Running replicas, desired=3.
	ctl, fake = newMachineSetTestController(t)
	hash := machineSetTemplateHash(ms.Spec.Template)
	owned := []model.Machine{
		ownedMachine("ms1-a", hash, "Running"),
		ownedMachine("ms1-b", hash, "Running"),
		ownedMachine("ms1-c", hash, "Running"),
	}
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, owned); err != nil {
		t.Fatalf("steady reconcileMachineSet: %v", err)
	}
	if len(fake.events) != 0 || len(fake.created) != 0 || len(fake.deleted) != 0 {
		t.Fatalf("steady state must emit no events and mutate nothing, got events=%+v created=%d deleted=%v", fake.events, len(fake.created), fake.deleted)
	}
}

// TestReconcileMachineSetEmitsScaledDownEvents: a pure scale-down posts
// one Normal ScaledDown Event per deleted Machine. Reconciling again at
// the new desired count is silent.
func TestReconcileMachineSetEmitsScaledDownEvents(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := eventTestMachineSet(1)
	hash := machineSetTemplateHash(ms.Spec.Template)
	owned := []model.Machine{
		ownedMachine("ms1-a", hash, "Running"),
		ownedMachine("ms1-b", hash, "Running"),
		ownedMachine("ms1-c", hash, "Running"),
	}
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, owned); err != nil {
		t.Fatalf("reconcileMachineSet: %v", err)
	}
	got := eventsWithReason(fake.events, machineSetReasonScaledDown)
	if len(got) != 2 {
		t.Fatalf("want 2 ScaledDown events, got %+v", fake.events)
	}
	checkMachineSetEvent(t, got[0], machineSetReasonScaledDown, "Normal", "deleted machine")
	if !strings.Contains(got[0].Message, "replicas 3->2") || !strings.Contains(got[1].Message, "replicas 2->1") {
		t.Fatalf("want replica counts 3->2 then 2->1, got %q, %q", got[0].Message, got[1].Message)
	}
	for i, ev := range got {
		if !strings.Contains(ev.Message, fake.deleted[i]) {
			t.Fatalf("event %d message %q, want machine %s", i, ev.Message, fake.deleted[i])
		}
	}

	ctl, fake = newMachineSetTestController(t)
	// The two excess replicas (ms1-a, ms1-b) were deleted; only ms1-c remains.
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, owned[2:]); err != nil {
		t.Fatalf("steady reconcileMachineSet: %v", err)
	}
	if len(fake.events) != 0 || len(fake.deleted) != 0 {
		t.Fatalf("steady state must emit no events, got events=%+v deleted=%v", fake.events, fake.deleted)
	}
}

// TestReconcileMachineSetEmitsPausedAndResumedEvents: Paused and Resumed
// fire only when spec.paused flips against the previous status.paused.
func TestReconcileMachineSetEmitsPausedAndResumedEvents(t *testing.T) {
	hash := machineSetTemplateHash(testMachineSet(1, "", "").Spec.Template)
	owned := []model.Machine{ownedMachine("ms1-a", hash, "Running")}

	// First sighting, already unpaused and at desired: no Event.
	ctl, fake := newMachineSetTestController(t)
	ms := eventTestMachineSet(1)
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, owned); err != nil {
		t.Fatalf("initial: %v", err)
	}
	if len(fake.events) != 0 {
		t.Fatalf("unpaused first tick must emit no events, got %+v", fake.events)
	}

	// Flip to paused.
	ctl, fake = newMachineSetTestController(t)
	ms = eventTestMachineSet(1)
	ms.Spec.Paused = true
	ms.Status.Paused = false
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, owned); err != nil {
		t.Fatalf("pause: %v", err)
	}
	got := eventsWithReason(fake.events, machineSetReasonPaused)
	if len(fake.events) != 1 || len(got) != 1 {
		t.Fatalf("want exactly one Paused event, got %+v", fake.events)
	}
	checkMachineSetEvent(t, got[0], machineSetReasonPaused, "Normal", "spec.paused is true")
	if len(fake.created) != 0 || len(fake.deleted) != 0 {
		t.Fatalf("pause must not create or delete, created=%d deleted=%v", len(fake.created), fake.deleted)
	}

	// Stay paused: silent.
	ctl, fake = newMachineSetTestController(t)
	ms = eventTestMachineSet(1)
	ms.Spec.Paused = true
	ms.Status.Paused = true
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, owned); err != nil {
		t.Fatalf("stay paused: %v", err)
	}
	if len(fake.events) != 0 {
		t.Fatalf("already paused must emit no events, got %+v", fake.events)
	}

	// Resume.
	ctl, fake = newMachineSetTestController(t)
	ms = eventTestMachineSet(1)
	ms.Spec.Paused = false
	ms.Status.Paused = true
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, owned); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got = eventsWithReason(fake.events, machineSetReasonResumed)
	if len(fake.events) != 1 || len(got) != 1 {
		t.Fatalf("want exactly one Resumed event, got %+v", fake.events)
	}
	checkMachineSetEvent(t, got[0], machineSetReasonResumed, "Normal", "spec.paused is false")

	// Stay running: silent.
	ctl, fake = newMachineSetTestController(t)
	ms = eventTestMachineSet(1)
	ms.Spec.Paused = false
	ms.Status.Paused = false
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, owned); err != nil {
		t.Fatalf("stay running: %v", err)
	}
	if len(fake.events) != 0 {
		t.Fatalf("already running must emit no events, got %+v", fake.events)
	}
}
