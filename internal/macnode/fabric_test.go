// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package macnode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

const hardwarePorts = `
Hardware Port: Ethernet
Device: en0
Ethernet Address: 00:11:22:33:44:55

Hardware Port: Thunderbolt Bridge
Device: bridge0
Ethernet Address: 36:aa:bb:cc:dd:00

Hardware Port: Thunderbolt 1
Device: en1
Ethernet Address: 36:aa:bb:cc:dd:01

VLAN Configurations
===================
`

func TestThunderboltBridgeFromNetworksetup(t *testing.T) {
	ports := ParseHardwarePorts(hardwarePorts)
	if len(ports) != 3 || ports[2] != (HardwarePort{"Thunderbolt 1", "en1"}) {
		t.Fatalf("ports = %+v", ports)
	}
	if ThunderboltBridge(ports) != "bridge0" || ThunderboltBridge(ports[:1]) != "" {
		t.Fatal("bridge")
	}
}

func studio() Info {
	i := mac
	i.Chip, i.Model, i.MacOSVersion = "Apple M4 Max", "Mac16,9", "27.2"
	i.PerformanceCores, i.EfficiencyCores = 12, 4
	i.Rosetta, i.NestedVirtualization = true, true
	i.ThunderboltInterface, i.ThunderboltAddresses, i.ThunderboltFabric = "bridge0", []string{"169.254.20.1"}, "tb-a"
	return i
}

func TestCapabilityLabelsAndAnnotations(t *testing.T) {
	l, a := Labels("studio-1", studio()), Annotations(studio())
	for k, v := range map[string]string{
		model.LabelAppleChip: "m4-max", model.LabelAppleChipGeneration: "4", model.LabelMacOSVersion: "27",
		model.LabelVZRosetta: "true", model.LabelVZNested: "true", model.LabelThunderbolt: "true", model.LabelThunderboltFabric: "tb-a",
	} {
		if l[k] != v {
			t.Errorf("label %s = %q, want %q", k, l[k], v)
		}
	}
	for k, v := range map[string]string{
		model.AnnotationAppleModel: "Mac16,9", model.AnnotationAppleCores: "12P+4E",
		model.AnnotationThunderboltInterface: "bridge0", model.AnnotationThunderboltAddresses: "169.254.20.1",
	} {
		if a[k] != v {
			t.Errorf("annotation %s = %q, want %q", k, a[k], v)
		}
	}
	// A fabric name without a Thunderbolt address is not published.
	i := studio()
	i.ThunderboltAddresses = nil
	if _, ok := Labels("studio-1", i)[model.LabelThunderboltFabric]; ok {
		t.Fatal("fabric label without a Thunderbolt address")
	}
	meta := metadataPatch("studio-1", i)["metadata"].(map[string]any)
	if v, ok := meta["labels"].(map[string]any)[model.LabelThunderbolt]; !ok || v != nil {
		t.Fatalf("a Thunderbolt label that went away must be removed: %v", meta["labels"])
	}
	if meta["labels"].(map[string]any)[model.LabelAppleChip] != "m4-max" {
		t.Fatal("chip label")
	}
}

func network(name, uid string, members ...model.MacNetworkMember) model.MacNetwork {
	n := model.MacNetwork{Metadata: model.ObjectMeta{Name: name, Namespace: "ai", UID: uid}}
	n.Status = model.MacNetworkStatus{Phase: model.MacNetworkPhaseReady, Name: model.MacNetworkName("ai", name), SubnetIndex: 192, Port: 48192, MTU: 1400, Members: members}
	return n
}

type netsAPI []model.MacNetwork

func (n netsAPI) ListMacNetworks(context.Context) ([]model.MacNetwork, error) { return n, nil }

func TestOverlayForThisMac(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	shard := network("shard", "uid-1",
		model.MacNetworkMember{Node: "studio-1", Hosts: [2]int{2, 31}, Address: "169.254.20.1"},
		model.MacNetworkMember{Node: "studio-2", Hosts: [2]int{32, 61}, Address: "169.254.20.2"})
	prev := 0
	shard.Status.KeyEpoch, shard.Status.PreviousKeyEpoch = 1, &prev
	other := network("other", "uid-2", model.MacNetworkMember{Node: "studio-2", Hosts: [2]int{2, 31}, Address: "10.0.0.2"})

	entries, keys := BuildOverlay("studio-1", []model.MacNetwork{shard, other}, secret, "/k")
	name := shard.Status.Name
	e, ok := entries[name]
	if len(entries) != 1 || !ok || e.Subnet != 192 || e.Hosts != [2]int{2, 31} || e.Listen != "0.0.0.0:48192" ||
		len(e.Peers) != 1 || e.Peers[0] != "169.254.20.2:48192" || e.KeyFile != "/k/"+name+".key" || e.MTU != 1400 {
		t.Fatalf("entries = %+v", entries)
	}
	alone, _ := BuildOverlay("studio-2", []model.MacNetwork{other}, secret, "/k")
	if raw, _ := json.Marshal(alone[other.Status.Name]); !strings.Contains(string(raw), `"peers":[]`) {
		t.Fatalf("a Mac alone in a network must write an empty peers list, not null: %s", raw)
	}
	want := "1 " + DeriveKey(secret, "uid-1", 1) + "\n0 " + DeriveKey(secret, "uid-1", 0) + "\n"
	if keys[name] != want || len(DeriveKey(secret, "uid-1", 1)) != 64 || DeriveKey(secret, "uid-1", 1) == DeriveKey(secret, "uid-2", 1) {
		t.Fatalf("key = %q", keys[name])
	}

	// Written next to another owner's network, which stays; a Kairon network this Mac left goes.
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay.json")
	stale := model.MacNetworkName("ai", "gone")
	if err := os.MkdirAll(filepath.Join(dir, "overlay-keys"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{stale + ".key", "acme-net.key"} {
		if err := os.WriteFile(filepath.Join(dir, "overlay-keys", f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seed := `{"networks": {"acme-net": {"subnet": 128}, "` + stale + `": {"subnet": 200}}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	s := OverlaySync{API: netsAPI{shard, other}, Node: "studio-1", Path: path, Secret: secret}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var file struct {
		Networks map[string]json.RawMessage `json:"networks"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Networks) != 2 || file.Networks["acme-net"] == nil || file.Networks[name] == nil {
		t.Fatalf("overlay.json = %s", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "overlay-keys", stale+".key")); !os.IsNotExist(err) {
		t.Fatal("stale Kairon key kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "overlay-keys", "acme-net.key")); err != nil {
		t.Fatal("another owner's key removed")
	}
	st, err := os.Stat(filepath.Join(dir, "overlay-keys", name+".key"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	entries, keys = BuildOverlay("studio-1", []model.MacNetwork{shard, other}, secret, filepath.Join(dir, "overlay-keys"))
	if changed, err := WriteOverlay(path, entries, keys); err != nil || changed {
		t.Fatalf("second write changed=%v err=%v", changed, err)
	}
}
