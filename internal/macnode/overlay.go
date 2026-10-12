// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package macnode

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

// OverlayNetwork is one entry of FluxVM's overlay.json (fluxvm_apple::vznet::NetworkOverlay).
type OverlayNetwork struct {
	Subnet  int      `json:"subnet"`
	Hosts   [2]int   `json:"hosts"`
	Listen  string   `json:"listen"`
	Peers   []string `json:"peers"`
	KeyFile string   `json:"key_file"`
	MTU     int      `json:"mtu,omitempty"`
}

// kaironNetwork matches model.MacNetworkName: entries and key files with other names belong to someone else (a
// FluxVM fleet central) and are left alone.
var kaironNetwork = regexp.MustCompile(`^k[0-9a-f]{8}(-[a-z0-9-]*)?$`)

// DeriveKey is a MacNetwork's key at epoch: HMAC-SHA256 of the network's UID and epoch under the fabric secret every
// member Mac holds. The Kubernetes API never sees a key.
func DeriveKey(secret []byte, uid string, epoch int) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("kairon-mac-network\x00" + uid + "\x00" + strconv.Itoa(epoch)))
	return hex.EncodeToString(m.Sum(nil))
}

// BuildOverlay is node's part of nets: the overlay.json entries and the key files' contents, by network name.
// keyDir is where the key files go.
func BuildOverlay(node string, nets []model.MacNetwork, secret []byte, keyDir string) (map[string]OverlayNetwork, map[string]string) {
	entries, keys := map[string]OverlayNetwork{}, map[string]string{}
	for _, n := range nets {
		st := n.Status
		me, ok := st.Member(node)
		if !ok || st.Phase != model.MacNetworkPhaseReady || st.Name == "" || st.Port == 0 || n.Metadata.UID == "" {
			continue
		}
		peers := []string{} // FluxVM's peers is a Vec: null fails the whole file
		for _, m := range st.Members {
			if m.Node == node {
				continue
			}
			if ip := net.ParseIP(m.Address); ip != nil && ip.To4() != nil {
				peers = append(peers, net.JoinHostPort(m.Address, strconv.Itoa(st.Port)))
			}
		}
		sort.Strings(peers)
		key := fmt.Sprintf("%d %s\n", st.KeyEpoch, DeriveKey(secret, n.Metadata.UID, st.KeyEpoch))
		if p := st.PreviousKeyEpoch; p != nil && *p != st.KeyEpoch {
			key += fmt.Sprintf("%d %s\n", *p, DeriveKey(secret, n.Metadata.UID, *p))
		}
		keys[st.Name] = key
		entries[st.Name] = OverlayNetwork{
			Subnet:  st.SubnetIndex,
			Hosts:   me.Hosts,
			Listen:  "0.0.0.0:" + strconv.Itoa(st.Port),
			Peers:   peers,
			KeyFile: filepath.Join(keyDir, st.Name+".key"),
			MTU:     st.MTU,
		}
	}
	return entries, keys
}

// WriteOverlay merges entries into the overlay.json at path (dropping Kairon networks this Mac left, keeping every
// other entry) and writes the key files next to it in overlay-keys/, mode 0600. It reports whether anything changed.
func WriteOverlay(path string, entries map[string]OverlayNetwork, keys map[string]string) (bool, error) {
	dir := filepath.Dir(path)
	keyDir := filepath.Join(dir, "overlay-keys")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		return false, err
	}
	changed := false
	for name, content := range keys {
		f := filepath.Join(keyDir, name+".key")
		if old, err := os.ReadFile(f); err == nil && string(old) == content {
			continue
		}
		if err := writeFile(f, []byte(content)); err != nil {
			return false, err
		}
		changed = true
	}
	dirents, err := os.ReadDir(keyDir)
	if err != nil {
		return false, err
	}
	for _, e := range dirents {
		stem, ok := strings.CutSuffix(e.Name(), ".key")
		if ok && kaironNetwork.MatchString(stem) && keys[stem] == "" {
			if err := os.Remove(filepath.Join(keyDir, e.Name())); err != nil {
				return false, err
			}
			changed = true
		}
	}

	file := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &file); err != nil {
			return false, fmt.Errorf("parsing %s: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return false, err
	}
	networks := map[string]any{}
	if old, ok := file["networks"].(map[string]any); ok {
		for name, v := range old {
			if !kaironNetwork.MatchString(name) {
				networks[name] = v
			}
		}
	}
	for name, e := range entries {
		networks[name] = e
	}
	file["networks"] = networks
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return false, err
	}
	if string(raw) == string(out) {
		return changed, nil
	}
	return true, writeFile(path, out)
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// OverlayAPI is the slice of kube.Client the overlay sync needs.
type OverlayAPI interface {
	ListMacNetworks(ctx context.Context) ([]model.MacNetwork, error)
}

// OverlaySync keeps this Mac's overlay.json in step with the MacNetworks it belongs to.
type OverlaySync struct {
	API    OverlayAPI
	Node   string
	Path   string
	Secret []byte
}

func (s OverlaySync) Sync(ctx context.Context) error {
	nets, err := s.API.ListMacNetworks(ctx)
	if err != nil {
		return fmt.Errorf("listing mac networks: %w", err)
	}
	entries, keys := BuildOverlay(s.Node, nets, s.Secret, filepath.Join(filepath.Dir(s.Path), "overlay-keys"))
	if _, err := WriteOverlay(s.Path, entries, keys); err != nil {
		return fmt.Errorf("writing %s: %w", s.Path, err)
	}
	return nil
}
