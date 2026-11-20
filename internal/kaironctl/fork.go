// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const maxForkCount = 32

// cmdFork creates fork children of a running Machine and, unless
// --no-wait, waits for them to reach Running.
func cmdFork(ctx context.Context, kc *kube.Client, args []string) {
	ns, args := nsFlag(args)
	if len(args) < 1 || args[0] == "" || args[0][0] == '-' {
		fatal(fmt.Errorf("usage: kaironctl fork MACHINE [--count N] [--prefix P] [--wait 60s | --no-wait]"))
	}
	parent := args[0]
	fs := flag.NewFlagSet("fork", flag.ExitOnError)
	count := fs.Int("count", 1, "number of children (1-32)")
	prefix := fs.String("prefix", "", "child name prefix; children are PREFIX-1..N (default MACHINE-fork-XXXXXX)")
	wait := fs.Duration("wait", 60*time.Second, "how long to wait for the children to run")
	noWait := fs.Bool("no-wait", false, "create the children and return immediately")
	_ = fs.Parse(args[1:])
	names, err := forkMachine(ctx, kc, ns, parent, *count, *prefix)
	if err != nil {
		fatal(err)
	}
	if *noWait {
		okf("created %s", strings.Join(names, ", "))
		return
	}
	if err := waitForRunning(ctx, kc, ns, names, *wait); err != nil {
		fatal(err)
	}
	okf("forked machine/%s into %s", parent, strings.Join(names, ", "))
}

// forkMachine creates count child Machines of running Machine parent. Each
// child copies the parent's spec, is pinned to the parent's node, and
// carries AnnotationForkFrom; kairon-node then forks the parent's FluxVM
// runtime instead of booting the child.
func forkMachine(ctx context.Context, kc *kube.Client, ns, parent string, count int, prefix string) ([]string, error) {
	if count < 1 || count > maxForkCount {
		return nil, fmt.Errorf("count must be 1-%d", maxForkCount)
	}
	src, err := kc.GetMachine(ctx, ns, parent)
	if err != nil {
		return nil, err
	}
	if src.Status.Phase != "Running" || src.Spec.NodeName == "" {
		return nil, fmt.Errorf("machine %s/%s is %q; only a Running Machine can be forked", ns, parent, src.Status.Phase)
	}
	if len(src.Spec.Volumes) > 0 || len(src.Spec.Disks) > 0 || len(src.Spec.DeviceClaims) > 0 {
		return nil, fmt.Errorf("machine %s/%s has volumes, disks or device claims; fork copies only the boot disk", ns, parent)
	}
	if prefix == "" {
		buf := make([]byte, 3)
		_, _ = rand.Read(buf)
		prefix = parent + "-fork-" + hex.EncodeToString(buf)
	}
	names := make([]string, count)
	for i := range names {
		names[i] = fmt.Sprintf("%s-%d", prefix, i+1)
		// FluxVM names the child runtime RuntimeName()+"-1", max 63 chars.
		if n := len("kairon-"+ns+"-"+names[i]) + 2; n > 63 {
			return nil, fmt.Errorf("child name %q is too long for a FluxVM runtime name (%d > 63 chars); use a shorter --prefix", names[i], n)
		}
	}
	spec := src.Spec
	spec.PowerState = "Running"
	for _, name := range names {
		child := model.Machine{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
			Metadata: model.ObjectMeta{
				Name:        name,
				Namespace:   ns,
				Labels:      map[string]string{model.LabelForkedFrom: parent, model.AssignedNodeLabel: src.Spec.NodeName},
				Annotations: map[string]string{model.AnnotationForkFrom: parent},
			},
			Spec: spec,
		}
		if _, err := kc.CreateMachine(ctx, ns, child); err != nil {
			return nil, fmt.Errorf("create %s/%s: %w", ns, name, err)
		}
	}
	return names, nil
}

func waitForRunning(ctx context.Context, kc *kube.Client, ns string, names []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pending := append([]string(nil), names...)
	for {
		var still []string
		for _, name := range pending {
			m, err := kc.GetMachine(ctx, ns, name)
			if err != nil || m.Status.Phase != "Running" {
				still = append(still, name)
			}
		}
		if len(still) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s to run", strings.Join(still, ", "))
		}
		pending = still
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
