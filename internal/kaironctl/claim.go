// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// cmdClaim creates a MachineClaim against POOL and, unless --no-wait,
// waits for it to bind and prints the Machine name.
func cmdClaim(ctx context.Context, kc *kube.Client, args []string) {
	ns, args := nsFlag(args)
	if len(args) < 1 || args[0] == "" || args[0][0] == '-' {
		fatal(fmt.Errorf("usage: kaironctl claim POOL [NAME] [--label k=v] [--retain] [--allow-fqdn H ...] [--wait 60s | --no-wait]"))
	}
	pool := args[0]
	args = args[1:]
	name := ""
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("claim", flag.ExitOnError)
	var labels stringSliceFlag
	fs.Var(&labels, "label", "label key=value added to the claimed Machine (repeatable)")
	retain := fs.Bool("retain", false, "keep the Machine when the claim is deleted (default: delete it)")
	ttl := fs.Duration("ttl", 0, "delete the claim (and release its Machine) this long after it binds")
	wait := fs.Duration("wait", 60*time.Second, "how long to wait for the claim to bind")
	noWait := fs.Bool("no-wait", false, "create the claim and return immediately")
	var fqdns, sni, cidrs, ports, dns stringSliceFlag
	fs.Var(&fqdns, "allow-fqdn", "confine the Machine's egress; allow this hostname (repeatable)")
	fs.Var(&sni, "allow-sni", "allow TLS to this SNI name, exact or *.suffix (repeatable)")
	fs.Var(&cidrs, "allow-cidr", "allow this destination CIDR (repeatable)")
	fs.Var(&ports, "allow-port", "allow this destination port or range, e.g. 443 (repeatable)")
	fs.Var(&dns, "allow-dns", "allow DNS queries for this name (repeatable)")
	icmp := fs.Bool("allow-icmp", false, "allow ICMP when egress is confined")
	_ = fs.Parse(args)
	labelMap, err := parseKeyValues(labels)
	if err != nil {
		fatal(err)
	}
	if name == "" {
		buf := make([]byte, 3)
		_, _ = rand.Read(buf)
		name = pool + "-claim-" + hex.EncodeToString(buf)
	}
	claim := newMachineClaim(ns, name, pool, labelMap, *retain)
	claim.Spec.TTLSeconds = int64(ttl.Seconds())
	if len(fqdns)+len(sni)+len(cidrs)+len(ports)+len(dns) > 0 || *icmp {
		claim.Spec.Egress = &model.ClaimEgress{AllowFqdns: fqdns, AllowSNI: sni, AllowCidrs: cidrs, AllowPorts: ports, AllowDNS: dns, AllowIcmp: *icmp}
	}
	if _, err := kc.CreateMachineClaim(ctx, ns, claim); err != nil {
		fatal(err)
	}
	if *noWait {
		okf("machineclaim/%s created", name)
		return
	}
	got, err := waitForClaim(ctx, kc, ns, name, *wait)
	if err != nil {
		fatal(err)
	}
	okf("machineclaim/%s bound to machine/%s in %dms", name, got.Status.MachineName, got.Status.BindMillis)
}

func newMachineClaim(ns, name, pool string, labels map[string]string, retain bool) model.MachineClaim {
	policy := ""
	if retain {
		policy = model.ReclaimRetain
	}
	return model.MachineClaim{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineClaim},
		Metadata: model.ObjectMeta{Name: name, Namespace: ns},
		Spec:     model.MachineClaimSpec{PoolName: pool, Labels: labels, ReclaimPolicy: policy},
	}
}

func waitForClaim(ctx context.Context, kc *kube.Client, ns, name string, timeout time.Duration) (model.MachineClaim, error) {
	deadline := time.Now().Add(timeout)
	for {
		got, err := kc.GetMachineClaim(ctx, ns, name)
		if err == nil && got.Status.Phase == model.ClaimBound && got.Status.MachineName != "" {
			return got, nil
		}
		if time.Now().After(deadline) {
			msg := got.Status.Message
			if err != nil {
				msg = err.Error()
			}
			return got, fmt.Errorf("machineclaim/%s not bound after %s: %s", name, timeout, dash(msg))
		}
		select {
		case <-ctx.Done():
			return got, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
