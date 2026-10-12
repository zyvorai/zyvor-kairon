// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package csinode

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// nqnPrefix names every NVMe subsystem this driver provisions; the volume
// name follows it, so DeleteVolume can recover the backing file.
const nqnPrefix = "nqn.2026-01.dev.zyvor.kairon:"

// nvmetPortIDBase keeps this driver's nvmet port ids clear of ports another
// kernel-target user may have created (nvmetcli and most guides start at 1).
const nvmetPortIDBase = 7400

// nvmeTransport is the CSI-facing name of an NVMe-oF transport.
type nvmeTransport string

const (
	transportISCSI    = "iscsi"
	transportNVMeTCP  = nvmeTransport("nvme-tcp")
	transportNVMeRDMA = nvmeTransport("nvme-rdma")
)

// kernel is the nvmet/nvme-fabrics transport name ("tcp", "rdma").
func (t nvmeTransport) kernel() string {
	return strings.TrimPrefix(string(t), "nvme-")
}

func parseNVMeTransport(s string) (nvmeTransport, error) {
	switch nvmeTransport(s) {
	case transportNVMeTCP, transportNVMeRDMA:
		return nvmeTransport(s), nil
	}
	return "", fmt.Errorf("transport %q is not nvme-tcp or nvme-rdma", s)
}

// NVMePortal is one address the controller exports subsystems on.
type NVMePortal struct {
	Transport nvmeTransport
	Host      string
	Port      string
}

func (p NVMePortal) addr() string { return net.JoinHostPort(p.Host, p.Port) }

// ParseNVMePortals parses the controller's -nvme-portals flag:
// comma-separated "tcp:HOST:PORT" or "rdma:HOST:PORT" entries. Several
// entries of one transport are several paths to every volume.
func ParseNVMePortals(s string) ([]NVMePortal, error) {
	var out []NVMePortal
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		kind, addr, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, fmt.Errorf("nvme portal %q must be tcp:HOST:PORT or rdma:HOST:PORT", raw)
		}
		t, err := parseNVMeTransport("nvme-" + kind)
		if err != nil {
			return nil, fmt.Errorf("nvme portal %q: %w", raw, err)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("nvme portal %q: %w", raw, err)
		}
		if net.ParseIP(host) == nil {
			return nil, fmt.Errorf("nvme portal %q: the kernel target needs an IP address, not a hostname", raw)
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return nil, fmt.Errorf("nvme portal %q: bad port", raw)
		}
		out = append(out, NVMePortal{Transport: t, Host: host, Port: port})
	}
	return out, nil
}

// nvmetClient drives the kernel NVMe target (nvmet) through configfs
// directly -- the same files nvmetcli writes, without needing it installed.
// Root is /sys/kernel/config/nvmet; tests point it at a temp directory.
type nvmetClient struct {
	Root string

	// rmdir removes a configfs group; nil means os.Remove. configfs drops
	// a group's attribute files with it, a plain directory in tests does not.
	rmdir func(string) error
}

func (c *nvmetClient) remove(path string) error {
	if c.rmdir != nil {
		return c.rmdir(path)
	}
	return os.Remove(path)
}

func (c *nvmetClient) subsystemDir(nqn string) string {
	return filepath.Join(c.Root, "subsystems", nqn)
}

func (c *nvmetClient) portDir(id int) string {
	return filepath.Join(c.Root, "ports", strconv.Itoa(id))
}

func readAttr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeAttr(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// mkdir creates a configfs group; an existing one is fine.
func mkdir(path string) error {
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create %s: %w", path, err)
	}
	return nil
}

// ensurePort creates nvmet port id for p. A port's address can't change
// while subsystems are linked to it, so an existing port with a different
// address is an error rather than something to rewrite.
func (c *nvmetClient) ensurePort(id int, p NVMePortal) error {
	dir := c.portDir(id)
	if err := mkdir(dir); err != nil {
		return fmt.Errorf("is the nvmet module loaded? %w", err)
	}
	// configfs creates the default groups itself; MkdirAll leaves them be.
	if err := os.MkdirAll(filepath.Join(dir, "subsystems"), 0o755); err != nil {
		return err
	}
	family := "ipv4"
	if ip := net.ParseIP(p.Host); ip != nil && ip.To4() == nil {
		family = "ipv6"
	}
	want := [][2]string{
		{"addr_trtype", p.Transport.kernel()},
		{"addr_adrfam", family},
		{"addr_traddr", p.Host},
		{"addr_trsvcid", p.Port},
	}
	for _, kv := range want {
		path := filepath.Join(dir, kv[0])
		cur := readAttr(path)
		if cur == kv[1] {
			continue
		}
		if cur != "" && len(c.linkedSubsystems(id)) > 0 {
			return fmt.Errorf("nvmet port %d already serves %s=%s; refusing to change it to %s", id, kv[0], cur, kv[1])
		}
		if err := writeAttr(path, kv[1]); err != nil {
			return err
		}
	}
	return nil
}

func (c *nvmetClient) linkedSubsystems(id int) []string {
	ents, err := os.ReadDir(filepath.Join(c.portDir(id), "subsystems"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// namespaceUUID is a stable namespace UUID derived from the NQN, so a
// re-created subsystem presents the same identity to hosts.
func namespaceUUID(nqn string) string {
	h := sha256.Sum256([]byte(nqn))
	h[6] = (h[6] & 0x0f) | 0x50
	h[8] = (h[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// ensureSubsystem creates subsystem nqn with namespace 1 on devicePath (a
// file or block device) and enables it. Any host may connect: nvmet's host
// allow-list and DH-CHAP are not configured by this driver.
func (c *nvmetClient) ensureSubsystem(nqn, devicePath string) error {
	dir := c.subsystemDir(nqn)
	if err := mkdir(dir); err != nil {
		return fmt.Errorf("is the nvmet module loaded? %w", err)
	}
	if err := writeAttr(filepath.Join(dir, "attr_allow_any_host"), "1"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "namespaces"), 0o755); err != nil {
		return err
	}
	ns := filepath.Join(dir, "namespaces", "1")
	if err := mkdir(ns); err != nil {
		return err
	}
	if readAttr(filepath.Join(ns, "enable")) == "1" {
		if cur := readAttr(filepath.Join(ns, "device_path")); cur != devicePath {
			return fmt.Errorf("subsystem %s namespace 1 already serves %s, not %s", nqn, cur, devicePath)
		}
		return nil
	}
	if err := writeAttr(filepath.Join(ns, "device_path"), devicePath); err != nil {
		return err
	}
	if err := writeAttr(filepath.Join(ns, "device_uuid"), namespaceUUID(nqn)); err != nil {
		return err
	}
	return writeAttr(filepath.Join(ns, "enable"), "1")
}

// linkPort exports subsystem nqn on port id.
func (c *nvmetClient) linkPort(id int, nqn string) error {
	link := filepath.Join(c.portDir(id), "subsystems", nqn)
	if _, err := os.Lstat(link); err == nil {
		return nil
	}
	if err := os.Symlink(c.subsystemDir(nqn), link); err != nil {
		return fmt.Errorf("export %s on nvmet port %d: %w", nqn, id, err)
	}
	return nil
}

// unlinkPort stops exporting nqn on port id; hosts connected through it
// lose that path.
func (c *nvmetClient) unlinkPort(id int, nqn string) error {
	err := os.Remove(filepath.Join(c.portDir(id), "subsystems", nqn))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unexport %s from nvmet port %d: %w", nqn, id, err)
	}
	return nil
}

// deleteSubsystem unexports nqn from every port, disables and removes its
// namespace, then the subsystem. Idempotent.
func (c *nvmetClient) deleteSubsystem(nqn string) error {
	ports, _ := os.ReadDir(filepath.Join(c.Root, "ports"))
	for _, p := range ports {
		id, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		if err := c.unlinkPort(id, nqn); err != nil {
			return err
		}
	}
	dir := c.subsystemDir(nqn)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	ns := filepath.Join(dir, "namespaces", "1")
	if _, err := os.Stat(ns); err == nil {
		if readAttr(filepath.Join(ns, "enable")) == "1" {
			if err := writeAttr(filepath.Join(ns, "enable"), "0"); err != nil {
				return err
			}
		}
		if err := c.remove(ns); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove namespace of %s: %w", nqn, err)
		}
	}
	if err := c.remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove subsystem %s: %w", nqn, err)
	}
	return nil
}

// revalidateSize tells nvmet the namespace's backing file grew, so
// connected hosts get a namespace-changed notice and see the new size.
func (c *nvmetClient) revalidateSize(nqn string) error {
	return writeAttr(filepath.Join(c.subsystemDir(nqn), "namespaces", "1", "revalidate_size"), "1")
}

// nameFromNQN recovers CreateVolume's name from an NQN this driver made.
func nameFromNQN(nqn string) (string, bool) {
	if !strings.HasPrefix(nqn, nqnPrefix) {
		return "", false
	}
	return strings.TrimPrefix(nqn, nqnPrefix), true
}
