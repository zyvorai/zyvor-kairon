// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package rehearsal

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// CheckBridge runs on the VM host before creating/starting a rehearsal VM.
// The bridge has no host IP, only TAP ports, and an nftables host input/output
// drop guard. A trusted host administrator must reserve it for one run.
func CheckBridge(ctx context.Context, name string) error {
	if runtime.GOOS != "linux" || !bridgeName.MatchString(name) {
		return fmt.Errorf("rehearsal isolation requires Linux and a krh- bridge")
	}
	root := filepath.Join("/sys/class/net", name)
	if _, err := os.Stat(filepath.Join(root, "bridge")); err != nil {
		return fmt.Errorf("rehearsal bridge: %w", err)
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return err
	}
	if len(addresses) != 0 {
		return fmt.Errorf("rehearsal bridge must have no IPv4 or IPv6 address")
	}
	ports, err := os.ReadDir(filepath.Join(root, "brif"))
	if err != nil {
		return err
	}
	for _, port := range ports {
		flags, err := os.ReadFile(filepath.Join("/sys/class/net", port.Name(), "tun_flags"))
		if err != nil {
			return fmt.Errorf("bridge port %s is not a TAP: %w", port.Name(), err)
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(flags)), 0, 32)
		if err != nil || n&2 == 0 {
			return fmt.Errorf("bridge port %s is not a TAP", port.Name())
		}
	}
	table := "krh_" + strings.TrimPrefix(name, "krh-")
	raw, err := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", table).Output()
	if err != nil {
		return fmt.Errorf("cannot verify rehearsal nftables guard: %w", err)
	}
	return validateGuard(raw, name, table)
}

// Validate the actual nft JSON, not command output substring matches. Each
// base chain must contain exactly the unconditional-on-interface drop rule.
func validateGuard(raw []byte, bridge, table string) error {
	var doc struct {
		NFTables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	chains := map[string]bool{}
	rules := map[string]int{}
	for _, entry := range doc.NFTables {
		if b, ok := entry["chain"]; ok {
			var c struct {
				Family, Table, Name, Type, Hook, Policy string
				Prio                                    int
			}
			if err := json.Unmarshal(b, &c); err != nil {
				return err
			}
			if c.Family != "inet" || c.Table != table || (c.Name != "input" && c.Name != "output") || c.Type != "filter" || c.Hook != c.Name || c.Prio != -300 || c.Policy != "accept" {
				return fmt.Errorf("unexpected rehearsal guard chain")
			}
			chains[c.Name] = true
		}
		if b, ok := entry["rule"]; ok {
			var rule struct {
				Family, Table, Chain string
				Expr                 []map[string]json.RawMessage
			}
			if err := json.Unmarshal(b, &rule); err != nil {
				return err
			}
			if rule.Family != "inet" || rule.Table != table || (rule.Chain != "input" && rule.Chain != "output") || len(rule.Expr) != 2 {
				return fmt.Errorf("unexpected rehearsal guard rule")
			}
			var match struct {
				Op    string
				Left  struct{ Meta struct{ Key string } }
				Right string
			}
			if err := json.Unmarshal(rule.Expr[0]["match"], &match); err != nil {
				return err
			}
			key := "iifname"
			if rule.Chain == "output" {
				key = "oifname"
			}
			if match.Op != "==" || match.Left.Meta.Key != key || match.Right != bridge {
				return fmt.Errorf("invalid rehearsal interface drop match")
			}
			if _, ok := rule.Expr[1]["drop"]; !ok {
				return fmt.Errorf("rehearsal guard must drop")
			}
			rules[rule.Chain]++
		}
	}
	if !chains["input"] || !chains["output"] || rules["input"] != 1 || rules["output"] != 1 {
		return fmt.Errorf("missing rehearsal host input/output guard")
	}
	return nil
}
