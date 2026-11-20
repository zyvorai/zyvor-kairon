// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zyvorai/kairon/internal/fencing"
)

type fakeNodePatcher struct {
	node  string
	patch map[string]any
}

func (f *fakeNodePatcher) PatchNode(_ context.Context, name string, patch map[string]any) error {
	f.node, f.patch = name, patch
	return nil
}

func TestSetNodeFenced(t *testing.T) {
	for _, tc := range []struct {
		reason, want string
	}{
		{"ipmi off", `{"metadata":{"annotations":{"` + fencing.AnnotationNodeFenced + `":"ipmi off"}}}`},
		{"", `{"metadata":{"annotations":{"` + fencing.AnnotationNodeFenced + `":null}}}`},
	} {
		f := &fakeNodePatcher{}
		if err := setNodeFenced(context.Background(), f, "worker-3", tc.reason); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(f.patch)
		if f.node != "worker-3" || string(got) != tc.want {
			t.Fatalf("reason %q: patched %s with %s, want %s", tc.reason, f.node, got, tc.want)
		}
	}
}
