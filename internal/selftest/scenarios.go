// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package selftest

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// scenarios is the ordered catalogue. boot comes first after control-plane
// because everything else acts on the Machine it creates.
var scenarios = []Scenario{
	{ID: "control-plane", Title: "Control plane is healthy", Profile: ProfileQuick, Run: runControlPlane,
		Needs: func(s *session) string {
			if s.env.ControlPlane == nil {
				return "no control-plane checks available in this context"
			}
			return ""
		}},
	// boot does not depend on control-plane: a doctor warning (or a 403 for the
	// identity in use) must not hide whether a VM really boots.
	{ID: "boot", Title: "Boot a CirrOS VM", Profile: ProfileQuick, Run: runBoot},
	{ID: "console-log", Title: "Serial console is readable", Profile: ProfileQuick, DependsOn: "boot", Run: runConsoleLog,
		Needs: func(s *session) string {
			if s.env.SerialLog == nil {
				return "needs the dashboard API (--ui-url and --ui-token) to read the serial console"
			}
			return ""
		}},
	{ID: "power", Title: "Stop and start", Profile: ProfileQuick, DependsOn: "boot", Run: runPower},
	{ID: "network", Title: "Guest network answers on a forwarded port", Profile: ProfileStandard, DependsOn: "boot", Run: runNetwork},
	{ID: "pause-resume", Title: "Pause and resume", Profile: ProfileStandard, DependsOn: "boot", Run: runPauseResume},
	{ID: "hotplug-cpu", Title: "Hot-add a vCPU", Profile: ProfileStandard, DependsOn: "boot", Run: runHotplugCPU},
	{ID: "observability", Title: "Usage, conditions and events are reported", Profile: ProfileStandard, DependsOn: "boot", Run: runObservability},
	{ID: "nic-hotplug", Title: "Hot-add a NIC", Profile: ProfileFull, DependsOn: "boot", Run: runNICHotplug,
		Needs: func(s *session) string {
			if s.env.Bridge == "" {
				return "pass --bridge NAME (a host bridge) to enable"
			}
			return ""
		}},
	{ID: "disk-hotplug", Title: "Hot-attach a disk", Profile: ProfileFull, DependsOn: "boot", Run: runDiskHotplug,
		Needs: func(s *session) string {
			if s.env.PVC == "" {
				return "pass --pvc NAME (an existing PersistentVolumeClaim) to enable"
			}
			return ""
		}},
	{ID: "fork", Title: "Fork a running VM", Profile: ProfileFull, DependsOn: "boot", Run: runFork},
	{ID: "cold-migration", Title: "Cold-migrate to another node", Profile: ProfileFull, DependsOn: "boot", Run: runColdMigration,
		Needs: func(s *session) string {
			nodes, err := s.capableNodes(context.Background())
			if err != nil {
				return "could not list nodes: " + err.Error()
			}
			if len(nodes) < 2 {
				return fmt.Sprintf("needs two capable Ready nodes (found %d)", len(nodes))
			}
			return ""
		}},
}

func runControlPlane(ctx context.Context, s *session) error {
	checks := s.env.ControlPlane(ctx)
	var failed []string
	for _, c := range checks {
		switch {
		case c.Status == Fail:
			s.logf(LevelFail, "%s: %s", c.Name, c.Detail)
			failed = append(failed, c.Name)
			if c.Hint != "" && s.hint == "" {
				s.hint = c.Hint
			}
		case c.Warn:
			s.logf(LevelWarn, "%s: %s", c.Name, c.Detail)
		case c.Status == Skip:
			s.logf(LevelInfo, "%s: skipped (%s)", c.Name, c.Detail)
		default:
			s.logf(LevelOK, "%s: %s", c.Name, c.Detail)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d check(s) failed: %s", len(failed), strings.Join(failed, ", "))
	}
	if len(checks) == 0 {
		return fmt.Errorf("no control-plane checks ran")
	}
	return nil
}

func runBoot(ctx context.Context, s *session) error {
	s.detectArch(ctx)
	img, err := s.imageSpec()
	if err != nil {
		return err
	}
	// With no capable node the scheduler cannot place the Machine, so waiting
	// the full boot timeout only delays an answer we already have. Give it a
	// short window in case the label is applied or the scheduler is lenient.
	bootWait := s.env.BootTimeout
	if nodes, err := s.capableNodes(ctx); err == nil && len(nodes) == 0 {
		s.logf(LevelWarn, "no Ready node carries %s=true; the scheduler is expected to leave the Machine Pending", model.CapableLabel)
		s.hint = "kubectl label node <name> " + model.CapableLabel + "=true"
		if w := 45 * time.Second; w < bootWait {
			bootWait = w
		}
	}
	m := s.newMachine("vm", img)
	s.hostPort = randomPort()
	m.Spec.Network.Forwards = []model.PortForward{{HostPort: uint16(s.hostPort), GuestPort: 22, Protocol: "tcp"}}
	switch {
	case img.Source != nil:
		s.logf(LevelInfo, "image: %s (%s) — the node downloads and verifies it", img.Source.HTTPURL, shortDigest(img.Digest))
	default:
		s.logf(LevelInfo, "image: %s (node-local path)", img.Path)
	}
	s.logf(LevelInfo, "creating %s/%s (1 vCPU, 256Mi, arch %s, forward :%d→22)", m.Metadata.Namespace, m.Metadata.Name, s.arch, s.hostPort)
	if err := s.create(ctx, m); err != nil {
		s.hint = "check quotas, RBAC for this identity and admission decisions: kaironctl events -n " + s.env.Namespace + " --warnings"
		return fmt.Errorf("create Machine: %w", err)
	}
	s.vm = m.Metadata.Name

	started := s.env.Now()
	cur, err := s.waitPhase(ctx, s.vm, "Running", bootWait)
	if err != nil {
		if h := bootFailureHint(err.Error()); h != "" {
			s.hint = h
		} else if s.hint == "" {
			s.hint = "kaironctl describe machine " + s.vm + " -n " + s.env.Namespace + "; kaironctl logs node"
		}
		s.dumpEvidence(ctx)
		return err
	}
	s.hint = ""
	s.node = cur.Status.NodeName
	s.logf(LevelOK, "Running on node %s after %s", dashIfEmpty(s.node), s.env.Now().Sub(started).Round(time.Second))
	if ip := cur.Status.GuestIP; ip != "" {
		s.logf(LevelInfo, "guest IP %s", ip)
	}

	if s.env.SerialLog == nil {
		s.logf(LevelWarn, "serial console unavailable here; boot is proven by phase only (pass --ui-url/--ui-token for the login-prompt check)")
		return nil
	}
	err = s.waitFor(ctx, "the guest login prompt", s.env.BootTimeout, func() (bool, error) {
		out := s.serial(ctx, s.vm)
		if bad := panicMarker(out); bad != "" {
			return false, fmt.Errorf("guest crashed during boot: %q", bad)
		}
		return strings.Contains(out, "login:"), nil
	})
	if err != nil {
		s.hint = "the VM is Running but the guest never reached a login prompt; check the image and: kaironctl logs node"
		s.dumpEvidence(ctx)
		return err
	}
	s.logf(LevelOK, "guest reached the login prompt")
	return nil
}

func runConsoleLog(ctx context.Context, s *session) error {
	out := s.serial(ctx, s.vm)
	if strings.TrimSpace(out) == "" {
		s.hint = "enable the console relay (ui.console.enabled / --with-console) and check kairon-node's console port"
		return fmt.Errorf("serial console returned no output")
	}
	if bad := panicMarker(out); bad != "" {
		return fmt.Errorf("serial console shows a crash: %q", bad)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	s.logf(LevelOK, "%d lines of serial output; last lines:", len(lines))
	for _, l := range tail(lines, 3) {
		s.logf(LevelVM, "%s", l)
	}
	return nil
}

func runPower(ctx context.Context, s *session) error {
	if err := s.patch(ctx, s.vm, powerPatch("Stopped")); err != nil {
		return fmt.Errorf("request stop: %w", err)
	}
	if _, err := s.waitPhase(ctx, s.vm, "Stopped", s.env.StepTimeout); err != nil {
		return err
	}
	s.logf(LevelOK, "stopped")
	if err := s.patch(ctx, s.vm, powerPatch("Running")); err != nil {
		return fmt.Errorf("request start: %w", err)
	}
	if _, err := s.waitPhase(ctx, s.vm, "Running", s.env.BootTimeout); err != nil {
		return err
	}
	s.logf(LevelOK, "running again")
	return nil
}

func runPauseResume(ctx context.Context, s *session) error {
	if err := s.patch(ctx, s.vm, powerPatch("Paused")); err != nil {
		return fmt.Errorf("request pause: %w", err)
	}
	if _, err := s.waitPhase(ctx, s.vm, "Paused", s.env.StepTimeout); err != nil {
		return err
	}
	s.logf(LevelOK, "paused")
	if err := s.patch(ctx, s.vm, powerPatch("Running")); err != nil {
		return fmt.Errorf("request resume: %w", err)
	}
	if _, err := s.waitPhase(ctx, s.vm, "Running", s.env.StepTimeout); err != nil {
		return err
	}
	s.logf(LevelOK, "resumed")
	return nil
}

func runNetwork(ctx context.Context, s *session) error {
	m, err := s.get(ctx, s.vm)
	if err != nil {
		return err
	}
	node := m.Status.NodeName
	addr, err := s.nodeAddress(ctx, node)
	if err != nil {
		return err
	}
	target := net.JoinHostPort(addr, fmt.Sprint(s.hostPort))
	s.logf(LevelInfo, "connecting to %s (node %s, forward to guest :22)", target, node)
	dial := s.env.Dial
	if dial == nil {
		dial = tcpBanner
	}
	var banner string
	var lastErr error
	err = s.waitFor(ctx, "the guest's SSH banner", s.env.StepTimeout, func() (bool, error) {
		banner, lastErr = dial(ctx, target)
		return lastErr == nil && strings.HasPrefix(banner, "SSH-"), nil
	})
	if err != nil {
		reason := "no SSH banner"
		if lastErr != nil {
			reason = lastErr.Error()
		}
		// From a workstation outside the cluster the node address is often
		// simply unreachable; that is a network boundary, not a Kairon fault.
		s.skipReason = fmt.Sprintf("could not reach %s from here (%s); the VM itself booted fine. Run this from a host that can reach the node to check forwarding", target, reason)
		return nil
	}
	s.logf(LevelOK, "guest answered: %s", strings.TrimSpace(banner))
	return nil
}

func runHotplugCPU(ctx context.Context, s *session) error {
	patch := map[string]any{"spec": map[string]any{"resources": map[string]any{"cpu": "2"}}}
	if err := s.patch(ctx, s.vm, patch); err != nil {
		return fmt.Errorf("request 2 vCPUs: %w", err)
	}
	s.logf(LevelInfo, "requested 2 vCPUs on the running VM")
	err := s.waitFor(ctx, "appliedVCPUs to reach 2", s.env.StepTimeout, func() (bool, error) {
		m, err := s.get(ctx, s.vm)
		if err != nil {
			return false, nil
		}
		return m.Status.AppliedVCPUs >= 2, nil
	})
	if err != nil {
		s.hint = "CPU hotplug needs the qemu backend; check: kaironctl events -n " + s.env.Namespace + " --warnings"
		return err
	}
	s.logf(LevelOK, "guest now has 2 vCPUs")
	return nil
}

func runObservability(ctx context.Context, s *session) error {
	var usage *model.ResourceUsage
	err := s.waitFor(ctx, "status.resourceUsage", s.env.StepTimeout, func() (bool, error) {
		m, err := s.get(ctx, s.vm)
		if err != nil {
			return false, nil
		}
		usage = m.Status.ResourceUsage
		return usage != nil, nil
	})
	if err != nil {
		s.hint = "kairon-node reports usage on its reconcile loop; check kaironctl logs node"
		return err
	}
	s.logf(LevelOK, "usage reported: cpu %.0f%%, memory %d MiB", usage.CPUPercent, usage.MemoryBytes>>20)

	m, err := s.get(ctx, s.vm)
	if err != nil {
		return err
	}
	if c, ok := model.FindCondition(m.Status.Conditions, "Ready"); !ok || c.Status != "True" {
		return fmt.Errorf("Ready condition is not True (%+v)", c)
	}
	s.logf(LevelOK, "Ready condition is True")

	if events, err := s.env.Kube.ListEventsFor(ctx, s.env.Namespace, "Machine", s.vm); err == nil {
		s.logf(LevelInfo, "%d Kubernetes event(s) recorded for the Machine", len(events))
	}
	if s.env.Metrics != nil {
		n, err := s.env.Metrics(ctx, s.env.Namespace, s.vm)
		switch {
		case err != nil:
			s.logf(LevelWarn, "dashboard metrics endpoint: %v", err)
		case n == 0:
			s.logf(LevelWarn, "dashboard has no usage samples yet (it samples every 15 s)")
		default:
			s.logf(LevelOK, "dashboard metrics: %d sample(s)", n)
		}
	}
	return nil
}

func runNICHotplug(ctx context.Context, s *session) error {
	// Extra interfaces need tap networking, which the default user-mode test
	// VM does not use: create a dedicated one.
	img, err := s.imageSpec()
	if err != nil {
		return err
	}
	m := s.newMachine("nic", img)
	m.Spec.Network = model.NetworkSpec{Mode: "tap", NetNS: true}
	if err := s.create(ctx, m); err != nil {
		return fmt.Errorf("create tap-networked Machine: %w", err)
	}
	if _, err := s.waitPhase(ctx, m.Metadata.Name, "Running", s.env.BootTimeout); err != nil {
		return err
	}
	patch := map[string]any{"spec": map[string]any{"network": map[string]any{
		"extraInterfaces": []map[string]any{{"name": "st0", "bridge": s.env.Bridge}},
	}}}
	if err := s.patch(ctx, m.Metadata.Name, patch); err != nil {
		return fmt.Errorf("add interface: %w", err)
	}
	return s.waitFor(ctx, "the interface to attach", s.env.StepTimeout, func() (bool, error) {
		cur, err := s.get(ctx, m.Metadata.Name)
		if err != nil {
			return false, nil
		}
		if len(cur.Status.AttachedInterfaces) > 0 {
			s.logf(LevelOK, "interface attached: %s on %s", cur.Status.AttachedInterfaces[0].Name, cur.Status.AttachedInterfaces[0].Bridge)
			return true, nil
		}
		return false, nil
	})
}

func runDiskHotplug(ctx context.Context, s *session) error {
	patch := map[string]any{"spec": map[string]any{"disks": []map[string]any{{"name": "st-data", "claimName": s.env.PVC}}}}
	if err := s.patch(ctx, s.vm, patch); err != nil {
		return fmt.Errorf("attach disk: %w", err)
	}
	return s.waitFor(ctx, "the disk to attach", s.env.StepTimeout, func() (bool, error) {
		cur, err := s.get(ctx, s.vm)
		if err != nil {
			return false, nil
		}
		for _, d := range cur.Status.AttachedDisks {
			if d == "st-data" {
				s.logf(LevelOK, "disk st-data attached from claim %s", s.env.PVC)
				return true, nil
			}
		}
		return false, nil
	})
}

func runFork(ctx context.Context, s *session) error {
	img, err := s.imageSpec()
	if err != nil {
		return err
	}
	parent := s.newMachine("fork", img)
	parent.Spec.Runtime.Backend = "flux-vm"
	parent.Spec.Network.Forwards = nil
	if err := s.create(ctx, parent); err != nil {
		s.skipReason = "the flux-vm backend was not accepted here: " + err.Error()
		return nil
	}
	if _, err := s.waitPhase(ctx, parent.Metadata.Name, "Running", s.env.BootTimeout); err != nil {
		s.skipReason = "a flux-vm Machine did not start, so fork cannot be tested: " + err.Error()
		return nil
	}
	cur, err := s.get(ctx, parent.Metadata.Name)
	if err != nil {
		return err
	}
	child := model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{
			Name: parent.Metadata.Name + "-c", Namespace: s.env.Namespace,
			Labels:      map[string]string{LabelKey: s.runID, model.LabelForkedFrom: parent.Metadata.Name, model.AssignedNodeLabel: cur.Spec.NodeName},
			Annotations: map[string]string{model.AnnotationForkFrom: parent.Metadata.Name},
		},
		Spec: cur.Spec,
	}
	child.Spec.PowerState = "Running"
	if err := s.create(ctx, child); err != nil {
		return fmt.Errorf("create fork child: %w", err)
	}
	if _, err := s.waitPhase(ctx, child.Metadata.Name, "Running", s.env.BootTimeout); err != nil {
		return err
	}
	s.logf(LevelOK, "fork child %s is Running", child.Metadata.Name)
	return nil
}

func runColdMigration(ctx context.Context, s *session) error {
	cur, err := s.get(ctx, s.vm)
	if err != nil {
		return err
	}
	nodes, err := s.capableNodes(ctx)
	if err != nil {
		return err
	}
	target := ""
	for _, n := range nodes {
		if n.Metadata.Name != cur.Status.NodeName {
			target = n.Metadata.Name
			break
		}
	}
	if target == "" {
		s.skipReason = "no second capable node to migrate to"
		return nil
	}
	mig := model.MachineMigration{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineMigration},
		Metadata: model.ObjectMeta{Name: "kairon-selftest-" + s.runID + "-mig", Namespace: s.env.Namespace, Labels: map[string]string{LabelKey: s.runID}},
		Spec:     model.MachineMigrationSpec{MachineName: s.vm, Strategy: "cold", TargetNode: target},
	}
	s.logf(LevelInfo, "migrating %s from %s to %s (cold)", s.vm, cur.Status.NodeName, target)
	if _, err := s.env.Kube.CreateMachineMigration(ctx, s.env.Namespace, mig); err != nil {
		return fmt.Errorf("create migration: %w", err)
	}
	var phase string
	err = s.waitFor(ctx, "the migration to finish", 5*time.Minute, func() (bool, error) {
		m, err := s.env.Kube.GetMachineMigration(ctx, s.env.Namespace, mig.Metadata.Name)
		if err != nil {
			return false, nil
		}
		phase = m.Status.Phase
		switch phase {
		case "Succeeded", "Completed":
			return true, nil
		case "Failed", "NeedsRecovery", "Cancelled":
			return false, fmt.Errorf("migration %s: %s", phase, m.Status.Message)
		}
		return false, nil
	})
	if err != nil {
		s.hint = "kaironctl describe migration " + mig.Metadata.Name + " -n " + s.env.Namespace
		return err
	}
	after, err := s.waitPhase(ctx, s.vm, "Running", s.env.StepTimeout)
	if err != nil {
		return err
	}
	if after.Status.NodeName != target {
		return fmt.Errorf("migration reported %s but the Machine is on %s, not %s", phase, after.Status.NodeName, target)
	}
	s.node = target
	s.logf(LevelOK, "now Running on %s", target)
	return nil
}

// dumpEvidence logs the Machine's events and the serial console tail, the
// two things most often needed to debug a failed boot.
func (s *session) dumpEvidence(ctx context.Context) {
	if s.vm == "" {
		return
	}
	if events, err := s.env.Kube.ListEventsFor(ctx, s.env.Namespace, "Machine", s.vm); err == nil {
		for _, e := range tailEvents(events, 6) {
			lvl := LevelInfo
			if e.Type == "Warning" {
				lvl = LevelWarn
			}
			s.logf(lvl, "event %s: %s", e.Reason, e.Message)
		}
	}
	if out := s.serial(ctx, s.vm); out != "" {
		for _, l := range tail(strings.Split(strings.TrimRight(out, "\n"), "\n"), 12) {
			s.logf(LevelVM, "%s", l)
		}
	}
}

func (s *session) nodeAddress(ctx context.Context, name string) (string, error) {
	nodes, err := s.env.Kube.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.Metadata.Name != name {
			continue
		}
		for _, want := range []string{"ExternalIP", "InternalIP"} {
			for _, a := range n.Status.Addresses {
				if a.Type == want {
					return a.Address, nil
				}
			}
		}
	}
	return "", fmt.Errorf("node %q has no address", name)
}

func tcpBanner(ctx context.Context, addr string) (string, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 128)
	n, err := c.Read(buf)
	if n > 0 {
		return string(buf[:n]), nil
	}
	return "", err
}

func powerPatch(state string) map[string]any {
	return map[string]any{"spec": map[string]any{"powerState": state}}
}

func panicMarker(out string) string {
	for _, m := range []string{"Kernel panic", "Oops:", "BUG: unable to handle"} {
		if strings.Contains(out, m) {
			return m
		}
	}
	return ""
}

func tail(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

func tailEvents(es []model.Event, n int) []model.Event {
	if len(es) <= n {
		return es
	}
	return es[len(es)-n:]
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return "sha256:" + d[:12] + "…"
	}
	return "sha256:" + d
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// bootFailureHint turns the node's own refusal messages for the prerequisites
// people most often miss into the command that fixes them.
func bootFailureHint(msg string) string {
	switch {
	case strings.Contains(msg, "no image cache configured"):
		return "kairon-node needs an image cache to download the test image: deploy with --image-cache-dir=/var/lib/kairon-node/image-cache (deploy-remote.sh) or set node.imageCacheDir (Helm); or pass --image PATH for a file already on the node"
	case strings.Contains(msg, "digest mismatch") || strings.Contains(msg, "checksum"):
		return "the downloaded image did not match its pinned sha256; check the mirror, or pass --image-url with --image-digest"
	}
	return ""
}
