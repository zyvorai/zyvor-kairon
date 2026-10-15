// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// TestContainerSandboxAgainstLiveFluxVM creates a real container through
// the client and reads its exit code back. It needs a FluxVM that can run
// the container track (macOS with the OCI boot artifacts installed) and is
// skipped unless KAIRON_FLUXVM_OCI_URL points at it:
//
//	KAIRON_FLUXVM_OCI_URL=http://127.0.0.1:7788 go test ./internal/fluxvm -run LiveFluxVM -v
func TestContainerSandboxAgainstLiveFluxVM(t *testing.T) {
	url := os.Getenv("KAIRON_FLUXVM_OCI_URL")
	if url == "" {
		t.Skip("set KAIRON_FLUXVM_OCI_URL to a FluxVM with the container track to run this")
	}
	c := New(url, os.Getenv("KAIRON_FLUXVM_OCI_TOKEN"))
	image := os.Getenv("KAIRON_FLUXVM_OCI_IMAGE")
	if image == "" {
		image = "alpine:3.22"
	}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "kairon-oci-it", Namespace: "default"},
		Spec: model.MachineSpec{
			Sandbox: &model.SandboxSpec{OCI: &model.OCISpec{
				Image:      image,
				Command:    []string{"sh", "-c", "echo kairon-container-ok; exit 3"},
				ExitPolicy: "poweroff",
			}},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	rec, err := c.CreateSandboxForMachine(ctx, m)
	if err != nil {
		t.Fatalf("create container sandbox: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), rec.ID()) })

	deadline := time.Now().Add(2 * time.Minute)
	for {
		logs, err := c.SandboxLogs(ctx, rec.ID(), 50)
		if err != nil {
			t.Fatalf("sandbox logs: %v", err)
		}
		if cs := logs.ContainerStatus(); cs != nil && cs.ExitCode != nil {
			if *cs.ExitCode != 3 {
				t.Fatalf("exit code = %d, want 3 (log: %q)", *cs.ExitCode, logs.Log)
			}
			if !strings.Contains(logs.Log, "kairon-container-ok") {
				t.Fatalf("console missing the container's output: %q", logs.Log)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container did not exit in time: %+v", logs)
		}
		time.Sleep(time.Second)
	}
}
