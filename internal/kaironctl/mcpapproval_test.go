// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/mcp"
)

type fakeApprovalStore struct {
	ann      map[string]string
	consumed int
}

func (f *fakeApprovalStore) GetAnnotations(context.Context, string, string, string) (map[string]string, error) {
	return f.ann, nil
}

func (f *fakeApprovalStore) ConsumeAnnotation(_ context.Context, _, _, _, key, value string) error {
	if f.ann[key] != value {
		return kube.ErrAnnotationChanged
	}
	delete(f.ann, key)
	f.consumed++
	return nil
}

func TestMCPApprovalIDStableAndArgumentBound(t *testing.T) {
	a, _ := mcpApprovalID("delete_machine", json.RawMessage(`{"name":"web","namespace":"prod"}`), "agent")
	b, _ := mcpApprovalID("delete_machine", json.RawMessage(`{"namespace":"prod","name":"web"}`), "agent")
	if a != b || len(a) != 12 {
		t.Fatalf("key order must not change the id: %q vs %q", a, b)
	}
	for _, other := range []struct{ tool, args, principal string }{
		{"delete_machine", `{"name":"db","namespace":"prod"}`, "agent"},
		{"fork_machine", `{"name":"web","namespace":"prod"}`, "agent"},
		{"delete_machine", `{"name":"web","namespace":"prod"}`, "other"},
	} {
		id, _ := mcpApprovalID(other.tool, json.RawMessage(other.args), other.principal)
		if id == a {
			t.Fatalf("%+v must get a different id", other)
		}
	}
}

func TestMCPApprovalTarget(t *testing.T) {
	cases := []struct {
		tool, args string
		want       string
		needed     bool
	}{
		{"delete_machine", `{"name":"web"}`, "machine/dflt/web", true},
		{"fork_machine", `{"name":"web","namespace":"prod"}`, "machine/prod/web", true},
		{"machine_backup", `{"action":"restore","name":"web","backup":"b1"}`, "machinebackup/dflt/b1", true},
		{"machine_backup", `{"action":"delete","backup":"b1"}`, "machinebackup/dflt/b1", true},
		{"machine_backup", `{"action":"create","name":"web"}`, "", false},
	}
	for _, tc := range cases {
		got, needed, err := mcpApprovalTarget(tc.tool, json.RawMessage(tc.args), "dflt")
		if err != nil || needed != tc.needed || (needed && got.String() != tc.want) {
			t.Errorf("%s %s: got %v needed=%v err=%v, want %s", tc.tool, tc.args, got, needed, err, tc.want)
		}
	}
}

func TestMCPApproveHook(t *testing.T) {
	t.Setenv("KAIRON_MCP_PRINCIPAL", "agent")
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	args := json.RawMessage(`{"name":"web","namespace":"prod"}`)
	id, _ := mcpApprovalID("delete_machine", args, "agent")
	store := &fakeApprovalStore{ann: map[string]string{}}
	hook := mcpApproveHook("default", func() (approvalStore, error) { return store, nil }, func() time.Time { return now })

	_, err := hook(context.Background(), "delete_machine", args)
	var ae *mcp.ApprovalError
	if !errors.As(err, &ae) || ae.Outcome != "approval-required" || !strings.Contains(err.Error(), "kaironctl approve machine/prod/web "+id) {
		t.Fatalf("expected approval-required naming the command, got %v", err)
	}

	store.ann[AnnotationMCPApproval] = formatApproval("000000000000", now.Add(time.Minute), "alice")
	if _, err := hook(context.Background(), "delete_machine", args); !errors.As(err, &ae) || ae.Outcome != "approval-required" {
		t.Fatalf("a different id must not approve this call, got %v", err)
	}

	store.ann[AnnotationMCPApproval] = formatApproval(id, now.Add(-time.Second), "alice")
	if _, err := hook(context.Background(), "delete_machine", args); !errors.As(err, &ae) || ae.Outcome != "approval-expired" {
		t.Fatalf("expected approval-expired, got %v", err)
	}

	store.ann[AnnotationMCPApproval] = formatApproval(id, now.Add(time.Minute), "alice")
	approver, err := hook(context.Background(), "delete_machine", args)
	if err != nil || approver != "alice" || store.consumed != 1 {
		t.Fatalf("expected approval by alice, consumed once; got %q %v consumed=%d", approver, err, store.consumed)
	}
	if _, err := hook(context.Background(), "delete_machine", args); !errors.As(err, &ae) || ae.Outcome != "approval-required" {
		t.Fatalf("an approval must be single-use, got %v", err)
	}

	if approver, err := hook(context.Background(), "machine_backup", json.RawMessage(`{"action":"create","name":"web"}`)); err != nil || approver != "" {
		t.Fatalf("backup create needs no approval, got %q %v", approver, err)
	}
}
