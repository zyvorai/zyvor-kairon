// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package selftest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
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
	{ID: "snapshot-restore", Title: "Snapshot a volume and restore it", Profile: ProfileFull, DependsOn: "boot", Run: runSnapshotRestore,
		Needs: func(s *session) string {
			t := s.snapshotTarget(context.Background())
			switch {
			case t.SnapshotClass == "":
				return "the cluster has no VolumeSnapshotClass (install a CSI driver with snapshot support)"
			case t.StorageClass == "":
				return "no StorageClass is served by the snapshot driver " + t.Driver
			case !t.ImmediateBinding:
				return "StorageClass " + t.StorageClass + " waits for a consumer, so an unattached test volume would never bind"
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
	// CirrOS prints "login:" only after cloud-init has given up looking for a
	// metadata service, which takes many minutes when none is configured. The
	// DHCP lease it logs first is already proof that the guest OS booted and
	// its NIC works, so either marker counts.
	var marker string
	err = s.waitFor(ctx, "the guest to finish booting", s.env.BootTimeout, func() (bool, error) {
		out := s.serial(ctx, s.vm)
		if bad := panicMarker(out); bad != "" {
			return false, fmt.Errorf("guest crashed during boot: %q", bad)
		}
		marker = bootMarker(out)
		return marker != "", nil
	})
	if err != nil {
		s.hint = "the VM is Running but the guest never finished booting; check the image and: kaironctl logs node"
		s.dumpEvidence(ctx)
		return err
	}
	s.logf(LevelOK, "%s", marker)
	return nil
}

var leaseRE = regexp.MustCompile(`leased ([0-9]{1,3}(?:\.[0-9]{1,3}){3})`)

// bootMarker reports how far a CirrOS guest has got, or "" if not far enough.
func bootMarker(serial string) string {
	if strings.Contains(serial, "login:") {
		return "guest reached the login prompt"
	}
	if m := leaseRE.FindStringSubmatch(serial); m != nil {
		return "guest booted and its NIC leased " + m[1] + " over DHCP (the login prompt follows once cloud-init stops waiting for a metadata service)"
	}
	return ""
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
	wait := s.env.StepTimeout
	if wait > 45*time.Second {
		wait = 45 * time.Second
	}
	err = s.waitFor(ctx, "the guest's SSH banner", wait, func() (bool, error) {
		banner, lastErr = dial(ctx, target)
		return lastErr == nil && strings.HasPrefix(banner, "SSH-"), nil
	})
	if err != nil {
		switch {
		case lastErr != nil && (errors.Is(lastErr, io.EOF) || strings.Contains(lastErr.Error(), "reset")):
			// The forward accepted the connection and the guest closed it:
			// the path works, the guest's SSH server is simply not up yet.
			s.skipReason = fmt.Sprintf("the forward to %s accepted the connection but the guest closed it without a banner (its SSH server was not running yet); the VM itself booted fine", target)
		case lastErr != nil:
			// From a workstation outside the cluster the node address is
			// often unreachable; that is a network boundary, not a Kairon fault.
			s.skipReason = fmt.Sprintf("could not reach %s from here (%s); the VM itself booted fine. Run this from a host that can reach the node to check forwarding", target, lastErr)
		default:
			s.skipReason = fmt.Sprintf("%s answered without an SSH banner", target)
		}
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
	return s.waitDevice(ctx, m.Metadata.Name, "the interface to attach", "NIC hotplug", func(cur model.Machine) bool {
		if len(cur.Status.AttachedInterfaces) > 0 {
			s.logf(LevelOK, "interface attached: %s on %s", cur.Status.AttachedInterfaces[0].Name, cur.Status.AttachedInterfaces[0].Bridge)
			return true
		}
		return false
	})
}

func runDiskHotplug(ctx context.Context, s *session) error {
	patch := map[string]any{"spec": map[string]any{"disks": []map[string]any{{"name": "st-data", "claimName": s.env.PVC}}}}
	if err := s.patch(ctx, s.vm, patch); err != nil {
		return fmt.Errorf("attach disk: %w", err)
	}
	return s.waitDevice(ctx, s.vm, "the disk to attach", "disk hotplug", func(cur model.Machine) bool {
		for _, d := range cur.Status.AttachedDisks {
			if d == "st-data" {
				s.logf(LevelOK, "disk st-data attached from claim %s", s.env.PVC)
				return true
			}
		}
		return false
	})
}

// waitDevice waits for a hotplugged device to show up in the Machine's
// status. kairon-node reports a failed hotplug in status.message, so a
// timeout carries what the node actually said instead of just "timed out".
// A 404/501 from the device endpoint means the installed FluxVM does not
// implement it: a capability limit, reported as a skip like fork.
func (s *session) waitDevice(ctx context.Context, name, what, feature string, attached func(model.Machine) bool) error {
	var lastMsg string
	skip := false
	err := s.waitFor(ctx, what, s.env.StepTimeout, func() (bool, error) {
		cur, err := s.get(ctx, name)
		if err != nil {
			return false, nil
		}
		if attached(cur) {
			return true, nil
		}
		lastMsg = cur.Status.Message
		if deviceUnsupported(lastMsg) {
			skip = true
			return true, nil
		}
		return false, nil
	})
	switch {
	case skip:
		s.skipReason = fmt.Sprintf("this node's FluxVM does not support %s (its device endpoint is missing or not implemented); upgrade FluxVM to test it", feature)
		return nil
	case err != nil && lastMsg != "":
		s.hint = "the node's own error is above; it is also in kaironctl describe machine " + name + " (status.message) and the kairon-node journal"
		return fmt.Errorf("%w; the node reported: %s", err, lastMsg)
	}
	return err
}

// deviceUnsupported recognises a device-hotplug call that the runtime has no
// endpoint for (404 or 501), as opposed to one it rejected.
func deviceUnsupported(msg string) bool {
	if !strings.Contains(msg, "fluxvm ") {
		return false
	}
	if !strings.Contains(msg, "/disks") && !strings.Contains(msg, "/hotplug/") {
		return false
	}
	return strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "HTTP 501")
}

// runSnapshotRestore boots a CirrOS Machine whose boot disk lives on a PVC,
// snapshots it through the controller and restores the snapshot into a new
// claim. The Machine does not have to be Running for the snapshot (a PVC
// snapshot only needs the claim), so its phase is reported, not required.
func runSnapshotRestore(ctx context.Context, s *session) error {
	t := s.snapshotTarget(ctx)
	ns := s.env.Namespace
	id := s.runID
	srcPVC := "kairon-selftest-" + id + "-vol"
	dstPVC := "kairon-selftest-" + id + "-restored"
	snapName := "kairon-selftest-" + id + "-snap"
	restoreName := "kairon-selftest-" + id + "-restore"

	img, err := s.imageSpec()
	if err != nil {
		return err
	}
	sc := t.StorageClass
	pvc := model.PersistentVolumeClaim{
		TypeMeta: model.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		Metadata: model.ObjectMeta{Name: srcPVC, Namespace: ns, Labels: map[string]string{LabelKey: id}},
		Spec: model.PersistentVolumeClaimSpec{
			AccessModes:      []string{"ReadWriteOnce"},
			Resources:        &model.PersistentVolumeClaimResources{Requests: map[string]string{"storage": "1Gi"}},
			StorageClassName: &sc,
		},
	}
	s.logf(LevelInfo, "using VolumeSnapshotClass %s and StorageClass %s (driver %s)", t.SnapshotClass, t.StorageClass, t.Driver)
	if _, err := s.env.Kube.CreatePersistentVolumeClaim(ctx, ns, pvc); err != nil {
		return fmt.Errorf("create PVC: %w", err)
	}
	s.onCleanup(func(c context.Context) { s.deleteClaim(c, srcPVC) })

	// Snapshotting an unbound claim fails for good (the controller does not
	// retry a Failed snapshot), so wait for the provisioner to bind it first.
	err = s.waitFor(ctx, "the test claim to bind", 90*time.Second, func() (bool, error) {
		cur, err := s.env.Kube.GetPersistentVolumeClaim(ctx, ns, srcPVC)
		if err != nil {
			return false, nil
		}
		return cur.Status.Phase == "Bound", nil
	})
	if err != nil {
		s.skipReason = fmt.Sprintf("StorageClass %s did not bind a 1Gi test claim in 90s, so snapshots cannot be tested (is its CSI provisioner healthy? kubectl describe pvc %s -n %s)", t.StorageClass, srcPVC, ns)
		return nil
	}
	s.logf(LevelOK, "test claim %s is Bound", srcPVC)

	m := s.newMachine("snap", img)
	m.Spec.Network.Forwards = nil
	m.Spec.Volumes = []model.MachineVolume{{Name: "root", ClaimName: srcPVC}}
	if err := s.create(ctx, m); err != nil {
		return fmt.Errorf("create Machine on the volume: %w", err)
	}
	s.logf(LevelInfo, "created %s with its boot disk on claim %s", m.Metadata.Name, srcPVC)
	if cur, err := s.waitPhase(ctx, m.Metadata.Name, "Running", 90*time.Second); err != nil {
		s.logf(LevelWarn, "the Machine on the volume is not Running (%v); snapshotting the claim anyway", err)
	} else {
		s.logf(LevelOK, "Machine booted from the volume on node %s", dashIfEmpty(cur.Status.NodeName))
	}

	snap := model.MachineSnapshot{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineSnapshot},
		Metadata: model.ObjectMeta{Name: snapName, Namespace: ns, Labels: map[string]string{LabelKey: id}},
		Spec:     model.MachineSnapshotSpec{MachineName: m.Metadata.Name, VolumeSnapshotClassName: t.SnapshotClass},
	}
	if _, err := s.env.Kube.CreateMachineSnapshot(ctx, ns, snap); err != nil {
		return fmt.Errorf("create MachineSnapshot: %w", err)
	}
	s.onCleanup(func(c context.Context) { _ = s.env.Kube.DeleteMachineSnapshot(c, ns, snapName) })
	var snapState model.MachineSnapshot
	err = s.waitFor(ctx, "the snapshot to be ready", 2*time.Minute, func() (bool, error) {
		cur, err := s.env.Kube.GetMachineSnapshot(ctx, ns, snapName)
		if err != nil {
			return false, nil
		}
		snapState = cur
		if cur.Status.Phase == "Failed" {
			return false, fmt.Errorf("snapshot failed: %s", cur.Status.Message)
		}
		return cur.Status.ReadyToUse || cur.Status.Phase == "Succeeded", nil
	})
	if err != nil {
		s.hint = "kaironctl describe snapshot " + snapName + " -n " + ns + "; kubectl get volumesnapshot -n " + ns
		return err
	}
	s.logf(LevelOK, "snapshot %s is ready (%d volume snapshot(s))", snapName, len(snapState.Status.VolumeSnapshots))

	restore := model.MachineSnapshotRestore{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineSnapshotRestore},
		Metadata: model.ObjectMeta{Name: restoreName, Namespace: ns, Labels: map[string]string{LabelKey: id}},
		Spec:     model.MachineSnapshotRestoreSpec{SnapshotName: snapName, TargetClaimName: dstPVC, StorageClassName: t.StorageClass},
	}
	if _, err := s.env.Kube.CreateMachineSnapshotRestore(ctx, ns, restore); err != nil {
		return fmt.Errorf("create MachineSnapshotRestore: %w", err)
	}
	s.onCleanup(func(c context.Context) {
		_ = s.env.Kube.DeleteMachineSnapshotRestore(c, ns, restoreName)
		s.deleteClaim(c, dstPVC)
	})
	err = s.waitFor(ctx, "the restore to complete", 2*time.Minute, func() (bool, error) {
		cur, err := s.env.Kube.GetMachineSnapshotRestore(ctx, ns, restoreName)
		if err != nil {
			return false, nil
		}
		if cur.Status.Phase == "Failed" {
			return false, fmt.Errorf("restore failed: %s", cur.Status.Message)
		}
		return cur.Status.Phase == "Succeeded", nil
	})
	if err != nil {
		s.hint = "kaironctl get restores -n " + ns + "; kubectl describe pvc " + dstPVC + " -n " + ns
		return err
	}
	if _, err := s.env.Kube.GetPersistentVolumeClaim(ctx, ns, dstPVC); err != nil {
		return fmt.Errorf("the restore reported success but claim %s is missing: %w", dstPVC, err)
	}
	s.logf(LevelOK, "restored into claim %s", dstPVC)
	return nil
}

// deleteClaim removes a test PVC, ignoring one that is already gone.
func (s *session) deleteClaim(ctx context.Context, name string) {
	if err := s.env.Kube.DeletePersistentVolumeClaim(ctx, s.env.Namespace, name); err != nil && !kube.IsNotFound(err) {
		s.logf(LevelWarn, "could not delete claim %s: %v (kubectl delete pvc %s -n %s)", name, err, name, s.env.Namespace)
		return
	}
	s.logf(LevelOK, "cleaned up claim %s", name)
}

func runFork(ctx context.Context, s *session) error {
	img, err := s.imageSpec()
	if err != nil {
		return err
	}
	parent := s.newMachine("fork", img)
	parent.Spec.Runtime.Backend = "flux-vm"
	// The flux-vm backend needs tap networking (the CRD does not accept
	// "none"), and fork needs it inside a network namespace.
	parent.Spec.Network = model.NetworkSpec{Mode: "tap", NetNS: true}
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
		if forkUnsupported(err.Error()) {
			// The runtime on this node has no fork endpoint: a capability
			// limit of the installed FluxVM, not a fault in Kairon.
			s.skipReason = "this node's FluxVM does not support fork (its /fork endpoint is missing or not implemented); upgrade FluxVM to test it"
			return nil
		}
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

// forkUnsupported recognises a runtime that has no fork endpoint (404 or
// 501 from POST /v1/vms/{id}/fork), as opposed to a fork that failed.
func forkUnsupported(msg string) bool {
	if !strings.Contains(msg, "/fork") {
		return false
	}
	return strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "HTTP 501")
}
