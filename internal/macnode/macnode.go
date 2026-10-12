// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package macnode lets a Mac run kairon-node without a kubelet: it publishes the Mac as a (virtual) Kubernetes
// Node, with labels, capacity and a Ready heartbeat, so Kairon's controller can schedule Machines to it and
// kairon-node can drive them through FluxVM's Apple Virtualization.framework backend ("vz").
//
// Nothing here talks to hardware directly; Discover (per-OS) fills Info and the rest is pure, testable construction.
package macnode

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// Label and annotation keys published on the Mac Node; the Apple capability keys are in internal/model.
const (
	LabelBackendVZ     = model.LabelBackendVZ
	LabelMLX           = "kairon.zyvor.dev/mlx"
	AnnotationInferURL = "kairon.zyvor.dev/inference-url"
	// TaintVMOnly keeps ordinary pods off the node: it has no kubelet, only Kairon Machines run there.
	TaintVMOnly = model.TaintVMOnly
)

// optionalLabels and optionalAnnotations are removed from the Node when the Mac stops reporting them.
var (
	optionalLabels = []string{LabelMLX, model.LabelAppleChip, model.LabelAppleChipGeneration, model.LabelMacOSVersion,
		model.LabelVZRosetta, model.LabelVZNested, model.LabelThunderbolt, model.LabelThunderboltFabric}
	optionalAnnotations = []string{AnnotationInferURL, model.AnnotationAppleModel, model.AnnotationAppleCores,
		model.AnnotationThunderboltInterface, model.AnnotationThunderboltAddresses}
)

// Info describes the Mac. Discover fills it; tests construct it directly.
type Info struct {
	CPUs       int
	MemoryMiB  uint64
	Arch       string // kubernetes.io/arch value, normally "arm64"
	OS         string // kubernetes.io/os value, "darwin"
	InternalIP string
	// MLXURL, when set, is the loopback OpenAI-compatible endpoint of Velora's MLX runtime on this Mac.
	MLXURL string

	// Chip is machdep.cpu.brand_string ("Apple M4 Max"), Model is hw.model ("Mac16,9").
	Chip  string
	Model string
	// PerformanceCores and EfficiencyCores are hw.perflevel0/1.physicalcpu.
	PerformanceCores int
	EfficiencyCores  int
	// MacOSVersion is kern.osproductversion, or FluxVM's report of it.
	MacOSVersion string
	// Rosetta and NestedVirtualization come from FluxVM's /v1/host/apple.
	Rosetta              bool
	NestedVirtualization bool
	// ThunderboltInterface is the Thunderbolt Bridge's BSD name and ThunderboltAddresses its IPv4 addresses.
	ThunderboltInterface string
	ThunderboltAddresses []string
	// ThunderboltFabric names the Macs cabled together over Thunderbolt (kairon-node --thunderbolt-fabric).
	ThunderboltFabric string
}

// SystemReserveMiB is the memory left to macOS and the user's own apps: at least 3 GiB or a quarter of RAM.
func SystemReserveMiB(totalMiB uint64) uint64 {
	q := totalMiB / 4
	if q < 3072 {
		q = 3072
	}
	if q > totalMiB {
		return totalMiB
	}
	return q
}

// Allocatable returns the cpu and memory quantities the scheduler may hand out.
func Allocatable(i Info) map[string]string {
	mem := uint64(0)
	if i.MemoryMiB > 0 {
		mem = i.MemoryMiB - SystemReserveMiB(i.MemoryMiB)
	}
	return map[string]string{
		"cpu":    fmt.Sprintf("%d", i.CPUs),
		"memory": fmt.Sprintf("%dMi", mem),
		"pods":   "0",
	}
}

// Labels returns the labels the Mac Node carries.
func Labels(name string, i Info) map[string]string {
	l := map[string]string{
		"kubernetes.io/hostname": name,
		"kubernetes.io/arch":     i.Arch,
		"kubernetes.io/os":       i.OS,
		model.CapableLabel:       "true",
		LabelBackendVZ:           "true",
	}
	if i.MLXURL != "" {
		l[LabelMLX] = "true"
	}
	if chip, gen, ok := model.ParseAppleChip(i.Chip); ok {
		l[model.LabelAppleChip] = chip
		l[model.LabelAppleChipGeneration] = strconv.Itoa(gen)
	}
	if v := model.MacOSMajor(i.MacOSVersion); v != "" {
		l[model.LabelMacOSVersion] = v
	}
	if i.Rosetta {
		l[model.LabelVZRosetta] = "true"
	}
	if i.NestedVirtualization {
		l[model.LabelVZNested] = "true"
	}
	if len(i.ThunderboltAddresses) > 0 {
		l[model.LabelThunderbolt] = "true"
		if i.ThunderboltFabric != "" {
			l[model.LabelThunderboltFabric] = i.ThunderboltFabric
		}
	}
	return l
}

// Annotations returns the annotations the Mac Node carries.
func Annotations(i Info) map[string]string {
	a := map[string]string{}
	if i.MLXURL != "" {
		a[AnnotationInferURL] = i.MLXURL
	}
	if i.Model != "" {
		a[model.AnnotationAppleModel] = i.Model
	}
	if i.PerformanceCores > 0 {
		a[model.AnnotationAppleCores] = fmt.Sprintf("%dP+%dE", i.PerformanceCores, i.EfficiencyCores)
	}
	if len(i.ThunderboltAddresses) > 0 {
		a[model.AnnotationThunderboltInterface] = i.ThunderboltInterface
		a[model.AnnotationThunderboltAddresses] = strings.Join(i.ThunderboltAddresses, ",")
	}
	return a
}

// metadataPatch sets the labels and annotations, and removes the optional ones the Mac no longer reports.
func metadataPatch(name string, i Info) map[string]any {
	labels, annotations := map[string]any{}, map[string]any{}
	for _, k := range optionalLabels {
		labels[k] = nil
	}
	for _, k := range optionalAnnotations {
		annotations[k] = nil
	}
	for k, v := range Labels(name, i) {
		labels[k] = v
	}
	for k, v := range Annotations(i) {
		annotations[k] = v
	}
	return map[string]any{"metadata": map[string]any{"labels": labels, "annotations": annotations}}
}

// HardwarePort is one entry of `networksetup -listallhardwareports`.
type HardwarePort struct {
	Port   string
	Device string
}

// ParseHardwarePorts reads `networksetup -listallhardwareports`.
func ParseHardwarePorts(out string) []HardwarePort {
	var ports []HardwarePort
	var cur HardwarePort
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Hardware Port":
			cur = HardwarePort{Port: v}
		case "Device":
			if cur.Port != "" {
				cur.Device = v
				ports = append(ports, cur)
				cur = HardwarePort{}
			}
		}
	}
	return ports
}

// ThunderboltBridge is the Thunderbolt Bridge's BSD name ("bridge0"), or "".
func ThunderboltBridge(ports []HardwarePort) string {
	for _, p := range ports {
		if p.Port == "Thunderbolt Bridge" {
			return p.Device
		}
	}
	return ""
}

// InterfaceIPv4 returns the IPv4 addresses on the interface called name.
func InterfaceIPv4(name string) []string {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, _ := ifc.Addrs()
	var out []string
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip := ipn.IP.To4(); ip != nil {
				out = append(out, ip.String())
			}
		}
	}
	return out
}

// BuildNode is the object POSTed when the Node does not exist yet.
func BuildNode(name string, i Info) map[string]any {
	meta := map[string]any{"name": name, "labels": Labels(name, i)}
	if a := Annotations(i); len(a) > 0 {
		meta["annotations"] = a
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   meta,
		"spec": map[string]any{
			"taints": []map[string]string{{"key": TaintVMOnly, "value": "true", "effect": "NoSchedule"}},
		},
	}
}

// BuildStatus is the status merge-patch sent on registration and then as a heartbeat.
func BuildStatus(i Info, now time.Time) map[string]any {
	t := now.UTC().Format(time.RFC3339)
	alloc := Allocatable(i)
	addr := []map[string]string{}
	if i.InternalIP != "" {
		addr = append(addr, map[string]string{"type": "InternalIP", "address": i.InternalIP})
	}
	return map[string]any{"status": map[string]any{
		"conditions": []map[string]string{{
			"type": "Ready", "status": "True", "reason": "KaironNodeReady",
			"message":           "kairon-node is running on this Mac and can start Machines through FluxVM",
			"lastHeartbeatTime": t, "lastTransitionTime": t,
		}},
		"addresses":   addr,
		"capacity":    map[string]string{"cpu": alloc["cpu"], "memory": fmt.Sprintf("%dMi", i.MemoryMiB), "pods": "0"},
		"allocatable": alloc,
	}}
}

// API is the slice of kube.Client that registration needs.
type API interface {
	GetNode(ctx context.Context, name string) (model.Node, error)
	CreateNode(ctx context.Context, node map[string]any) error
	PatchNode(ctx context.Context, name string, patch map[string]any) error
	PatchNodeStatus(ctx context.Context, name string, patch map[string]any) error
}

// Register creates the Node when missing (or refreshes its labels), then publishes capacity and Ready.
func Register(ctx context.Context, api API, name string, i Info, now time.Time) error {
	if _, err := api.GetNode(ctx, name); err != nil {
		if !kube.IsNotFound(err) {
			return fmt.Errorf("looking up node %s: %w", name, err)
		}
		if err := api.CreateNode(ctx, BuildNode(name, i)); err != nil {
			return fmt.Errorf("creating node %s: %w", name, err)
		}
	} else {
		if err := api.PatchNode(ctx, name, metadataPatch(name, i)); err != nil {
			return fmt.Errorf("refreshing node %s: %w", name, err)
		}
	}
	if err := api.PatchNodeStatus(ctx, name, BuildStatus(i, now)); err != nil {
		return fmt.Errorf("publishing node status: %w", err)
	}
	return nil
}

// Run registers the node and heartbeats until ctx ends. Errors are reported through onError and retried next tick.
// discover runs every tick, so capabilities and Thunderbolt addresses that change are picked up; after each
// registration, sync (when set) runs too.
func Run(ctx context.Context, api API, name string, discover func(context.Context) Info, every time.Duration, sync func(context.Context) error, onError func(error)) {
	tick := func() {
		if err := Register(ctx, api, name, discover(ctx), time.Now()); err != nil && onError != nil {
			onError(err)
		}
		if sync != nil {
			if err := sync(ctx); err != nil && onError != nil {
				onError(err)
			}
		}
	}
	tick()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// PrimaryIPv4 returns the first non-loopback private IPv4 address of this host, or "".
func PrimaryIPv4() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip := ipn.IP.To4(); ip != nil && !ip.IsLoopback() && ip.IsPrivate() {
				return ip.String()
			}
		}
	}
	return ""
}
