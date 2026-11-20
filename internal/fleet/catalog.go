// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fleet

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"sort"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func AllocateAddress(s VirtualNetworkSpec, allocations map[string]string) (string, error) {
	prefix, err := netip.ParsePrefix(s.CIDR)
	if err != nil {
		return "", err
	}
	gateway, err := netip.ParseAddr(s.Gateway)
	if err != nil {
		return "", err
	}
	used := map[string]bool{gateway.String(): true, prefix.Addr().String(): true}
	for _, ip := range allocations {
		used[ip] = true
	}
	// A bounded deterministic first-free search avoids unbounded /64 scans.
	ip := prefix.Addr().Next()
	for i := 0; i < 65536 && ip.IsValid() && prefix.Contains(ip); i++ {
		next := ip.Next()
		broadcast := prefix.Addr().Is4() && (!next.IsValid() || !prefix.Contains(next))
		if !broadcast && !used[ip.String()] {
			return ip.String(), nil
		}
		ip = next
	}
	return "", fmt.Errorf("network address pool is exhausted (65536-address allocation window)")
}
func (e *Engine) networkClaim(ctx context.Context, o *model.FleetResource) error {
	s, _ := decode[NetworkClaimSpec](o.Spec)
	network, err := e.Kube.GetFleet(ctx, o.Metadata.Namespace, "machinevirtualnetworks", s.NetworkName)
	if err != nil {
		return err
	}
	if err := Validate(network); err != nil {
		return err
	}
	ns, _ := decode[VirtualNetworkSpec](network.Spec)
	networks, err := e.Kube.ListFleet(ctx, "", "machinevirtualnetworks")
	if err != nil {
		return err
	}
	for _, other := range networks {
		if other.Metadata.UID == network.Metadata.UID {
			continue
		}
		otherSpec, err := decode[VirtualNetworkSpec](other.Spec)
		if err == nil && otherSpec.Bridge == ns.Bridge {
			return fmt.Errorf("bridge is shared by another virtual network; isolation cannot be established")
		}
	}
	if network.Status.Allocations == nil {
		network.Status.Allocations = map[string]string{}
	}
	// Claim UID is the allocation identity. Reusing a deleted claim's name
	// cannot steal its predecessor's address; release is explicit and guarded.
	ip, found := network.Status.Allocations[o.Metadata.UID]
	if !found {
		ip, err = AllocateAddress(ns, network.Status.Allocations)
		if err != nil {
			return err
		}
		network.Status.Allocations[o.Metadata.UID] = ip
		if err := e.Kube.PatchFleetStatus(ctx, "machinevirtualnetworks", network, network.Status); err != nil {
			return err
		}
	}
	o.Status.Allocations = map[string]string{"address": ip, "network": s.NetworkName, "machine": s.MachineName}
	e.status(o, "Bound", "address reserved; MachineTemplateClaim applies bridge and guest network config")
	return nil
}
func (e *Engine) templateClaim(ctx context.Context, o *model.FleetResource) error {
	s, _ := decode[TemplateClaimSpec](o.Spec)
	ns := o.Metadata.Namespace
	template, err := e.Kube.GetFleet(ctx, ns, "machinetemplateversions", s.TemplateName)
	if err != nil {
		return err
	}
	if err := Validate(template); err != nil {
		return err
	}
	t, _ := decode[TemplateSpec](template.Spec)
	if s.TTLSeconds > t.MaxTTLSeconds {
		return fmt.Errorf("claim TTL exceeds template maximum")
	}
	m, err := e.Kube.GetMachine(ctx, ns, s.MachineName)
	if err == nil {
		if !owns(*o, m.Metadata) {
			return fmt.Errorf("the Machine name is owned by another request")
		}
		e.status(o, m.Status.Phase, "Machine provisioned from template "+t.Version)
		return nil
	}
	if !kube.IsNotFound(err) {
		return err
	}
	// Deep-copy the immutable template before adding per-claim values.
	spec, err := decode[model.MachineSpec](raw(t.Template.Spec))
	if err != nil {
		return err
	}
	spec.TTLSeconds = s.TTLSeconds
	labels := map[string]string{}
	for k, v := range t.Template.Labels {
		labels[k] = v
	}
	meta := childMeta(*o, s.MachineName)
	for k, v := range meta.Labels {
		labels[k] = v
	}
	meta.Labels = labels
	if s.NetworkClaim != "" {
		claim, err := e.Kube.GetFleet(ctx, ns, "machinenetworkclaims", s.NetworkClaim)
		if err != nil {
			return err
		}
		c, err := decode[NetworkClaimSpec](claim.Spec)
		if err != nil {
			return err
		}
		if c.MachineName != s.MachineName || claim.Status.Phase != "Bound" {
			return fmt.Errorf("network claim must be Bound to this Machine")
		}
		network, err := e.Kube.GetFleet(ctx, ns, "machinevirtualnetworks", c.NetworkName)
		if err != nil {
			return err
		}
		n, err := decode[VirtualNetworkSpec](network.Spec)
		if err != nil {
			return err
		}
		ip := claim.Status.Allocations["address"]
		if network.Status.Allocations[claim.Metadata.UID] != ip || ip == "" {
			return fmt.Errorf("network allocation is not durable")
		}
		hash := sha256.Sum256([]byte(claim.Metadata.UID))
		mac := fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", hash[0], hash[1], hash[2], hash[3], hash[4])
		spec.Network = model.NetworkSpec{Mode: "tap", Bridge: n.Bridge, NetNS: true, MAC: mac, DataplaneMode: "ebpf", DataplaneRequired: true}
		if spec.Placement.NodeSelector == nil {
			spec.Placement.NodeSelector = map[string]string{}
		}
		for k, v := range n.NodeSelector {
			if existing := spec.Placement.NodeSelector[k]; existing != "" && existing != v {
				return fmt.Errorf("network and template node selectors conflict")
			}
			spec.Placement.NodeSelector[k] = v
		}
		prefix, _ := netip.ParsePrefix(n.CIDR)
		config := "[Match]\nMACAddress=" + mac + "\n[Network]\nAddress=" + ip + fmt.Sprintf("/%d\n", prefix.Bits()) + "Gateway=" + n.Gateway + "\n"
		for _, server := range n.DNSServers {
			config += "DNS=" + server + "\n"
		}
		// Templates used with IPAM must have systemd-networkd enabled in the image.
		spec.CloudInit.WriteFiles = append(spec.CloudInit.WriteFiles, model.CloudInitFile{Path: "/etc/systemd/network/10-kairon.network", Content: config, Permissions: "0644"})
		spec.CloudInit.RunCmd = append(spec.CloudInit.RunCmd, "systemctl restart systemd-networkd")
		meta.Annotations = map[string]string{NetworkAllocationAnnotation: claim.Metadata.UID, "fleet.kairon.zyvor.dev/guest-address": ip}
	}
	_, err = e.Kube.CreateMachine(ctx, ns, model.Machine{TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine}, Metadata: meta, Spec: spec})
	if err != nil {
		return err
	}
	e.status(o, "Pending", "Machine created from immutable template")
	return nil
}

func (e *Engine) releaseAddress(ctx context.Context, ns, networkName, claimUID string) error {
	network, err := e.Kube.GetFleet(ctx, ns, "machinevirtualnetworks", networkName)
	if err != nil {
		return err
	}
	machines, err := e.Kube.ListMachinesNamespace(ctx, ns)
	if err != nil {
		return err
	}
	for _, m := range machines {
		if m.Metadata.Annotations[NetworkAllocationAnnotation] == claimUID {
			return fmt.Errorf("allocation is still referenced by a Machine")
		}
	}
	claims, err := e.Kube.ListFleet(ctx, ns, "machinenetworkclaims")
	if err != nil {
		return err
	}
	for _, c := range claims {
		if c.Metadata.UID == claimUID {
			return fmt.Errorf("delete the network claim before releasing its address")
		}
	}
	if _, exists := network.Status.Allocations[claimUID]; !exists {
		return nil
	}
	// Merge-patch needs explicit null to remove a map key, not an omitted key.
	return e.Kube.PatchFleetStatusFields(ctx, "machinevirtualnetworks", network, map[string]any{"allocations": map[string]any{claimUID: nil}})
}

// Address release is exposed through a status subresource CAS helper, not a
// tenant-authored patch to another claim's allocation map.
func (e *Engine) ReleaseAddress(ctx context.Context, ns, networkName, claimUID string) error {
	return e.releaseAddress(ctx, ns, networkName, claimUID)
}

func SortedResources() []string {
	resources := make([]string, 0, len(model.FleetKinds))
	for r := range model.FleetKinds {
		resources = append(resources, r)
	}
	sort.Strings(resources)
	return resources
}
