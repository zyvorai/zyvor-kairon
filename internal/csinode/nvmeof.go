// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package csinode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Volume context keys for an NVMe-oF volume.
const (
	volumeAttrTransport = "transport" // nvme-tcp or nvme-rdma (absent: iscsi)
	volumeAttrNQN       = "nqn"
	volumeAttrPortals   = "portals" // comma-separated host:port, one per path
)

const nvmeVolumeIDPrefix = "nvme"

// nvmeConfig is one NVMe-oF namespace: subsystem NQN, namespace 1, reached
// over every portal (each one a path the kernel's native multipath merges).
type nvmeConfig struct {
	Transport nvmeTransport
	NQN       string
	Portals   []string
	FSType    string
}

func parseNVMeConfig(volumeContext map[string]string) (nvmeConfig, error) {
	t, err := parseNVMeTransport(volumeContext[volumeAttrTransport])
	if err != nil {
		return nvmeConfig{}, err
	}
	nqn := volumeContext[volumeAttrNQN]
	if !strings.HasPrefix(nqn, "nqn.") {
		return nvmeConfig{}, fmt.Errorf("volume_context %q must be an NVMe qualified name (nqn....)", volumeAttrNQN)
	}
	portals, err := splitPortals(volumeContext[volumeAttrPortals])
	if err != nil {
		return nvmeConfig{}, err
	}
	return nvmeConfig{Transport: t, NQN: nqn, Portals: portals, FSType: defaultFSType}, nil
}

func splitPortals(s string) ([]string, error) {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(p); err != nil {
			return nil, fmt.Errorf("portal %q must be host:port: %w", p, err)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("volume_context %q must list at least one host:port", volumeAttrPortals)
	}
	return out, nil
}

// encodeNVMeVolumeID is "nvme|<transport>|<nqn>|<portal>,<portal>" -- enough
// for NodeUnstageVolume (which gets no volume_context) to disconnect.
func encodeNVMeVolumeID(cfg nvmeConfig) string {
	return strings.Join([]string{nvmeVolumeIDPrefix, string(cfg.Transport), cfg.NQN, strings.Join(cfg.Portals, ",")}, "|")
}

func decodeNVMeVolumeID(id string) (nvmeConfig, error) {
	parts := strings.Split(id, "|")
	if len(parts) != 4 || parts[0] != nvmeVolumeIDPrefix {
		return nvmeConfig{}, fmt.Errorf("volume_id %q is not a Kairon NVMe-oF volume handle (expected %q)", id, "nvme|<transport>|<nqn>|<portal>[,<portal>...]")
	}
	return parseNVMeConfig(map[string]string{volumeAttrTransport: parts[1], volumeAttrNQN: parts[2], volumeAttrPortals: parts[3]})
}

// volumeRef is a decoded volume_id of either backend.
type volumeRef struct {
	iscsi *iscsiConfig
	nvme  *nvmeConfig
}

func decodeAnyVolumeID(id string) (volumeRef, error) {
	if strings.HasPrefix(id, nvmeVolumeIDPrefix+"|") {
		c, err := decodeNVMeVolumeID(id)
		if err != nil {
			return volumeRef{}, err
		}
		return volumeRef{nvme: &c}, nil
	}
	c, err := decodeVolumeID(id)
	if err != nil {
		return volumeRef{}, err
	}
	return volumeRef{iscsi: &c}, nil
}

// nvmeHost is the NVMe-oF initiator: it writes connect strings to
// /dev/nvme-fabrics and reads controller state from sysfs -- what nvme-cli
// does, without needing it installed. Every path is a field so tests can
// point them at a temp directory.
type nvmeHost struct {
	FabricsDev    string // /dev/nvme-fabrics
	SysNVMe       string // /sys/class/nvme
	SysSubsystem  string // /sys/class/nvme-subsystem
	MultipathFile string // /sys/module/nvme_core/parameters/multipath
	HostNQNFile   string // /etc/nvme/hostnqn
	HostIDFile    string // /etc/nvme/hostid
	MachineIDFile string // /etc/machine-id
	DevDir        string // /dev

	// connectFn writes one connect string; nil means FabricsDev.
	connectFn func(opts string) error
}

func newNVMeHost() *nvmeHost {
	return &nvmeHost{
		FabricsDev:    "/dev/nvme-fabrics",
		SysNVMe:       "/sys/class/nvme",
		SysSubsystem:  "/sys/class/nvme-subsystem",
		MultipathFile: "/sys/module/nvme_core/parameters/multipath",
		HostNQNFile:   "/etc/nvme/hostnqn",
		HostIDFile:    "/etc/nvme/hostid",
		MachineIDFile: "/etc/machine-id",
		DevDir:        "/dev",
	}
}

// hostNQN is /etc/nvme/hostnqn, else one derived from /etc/machine-id so it
// stays the same across restarts of this plugin.
func (h *nvmeHost) hostNQN() (string, error) {
	if s := readAttr(h.HostNQNFile); strings.HasPrefix(s, "nqn.") {
		return s, nil
	}
	id := readAttr(h.MachineIDFile)
	if len(id) != 32 {
		return "", fmt.Errorf("no host NQN: %s is missing and %s is not a 32-hex machine id", h.HostNQNFile, h.MachineIDFile)
	}
	return fmt.Sprintf("nqn.2014-08.org.nvmexpress:uuid:%s-%s-%s-%s-%s", id[0:8], id[8:12], id[12:16], id[16:20], id[20:32]), nil
}

// hostID is the host identifier sent with hostnqn: /etc/nvme/hostid, else
// the UUID of a "uuid:" host NQN, else one derived from the NQN. The kernel
// otherwise pairs hostnqn with its own default hostid, which it already
// holds under a different NQN, and refuses the connect.
func (h *nvmeHost) hostID(hostnqn string) string {
	if s := readAttr(h.HostIDFile); uuidPattern.MatchString(s) {
		return s
	}
	if _, u, ok := strings.Cut(hostnqn, ":uuid:"); ok && uuidPattern.MatchString(u) {
		return u
	}
	return namespaceUUID(hostnqn)
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// nvmePath is one controller (path) of a connected subsystem.
type nvmePath struct {
	Controller string // nvme0
	Transport  string // tcp, rdma
	TrAddr     string
	TrSvcID    string
	State      string // live, connecting, resetting, deleting, ...
	ANAState   string // optimized, non-optimized, inaccessible, ... ("" without ANA)
}

func (p nvmePath) addr() string { return net.JoinHostPort(p.TrAddr, p.TrSvcID) }

// parseAddress reads a controller's sysfs "address" line
// ("traddr=10.0.0.1,trsvcid=4420,src_addr=...").
func parseAddress(s string) (traddr, trsvcid string) {
	for _, kv := range strings.Split(strings.TrimSpace(s), ",") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "traddr":
			traddr = v
		case "trsvcid":
			trsvcid = v
		}
	}
	return traddr, trsvcid
}

var (
	pathNamespacePattern = regexp.MustCompile(`^nvme\d+c\d+n\d+$`)
	headNamespacePattern = regexp.MustCompile(`^nvme\d+n\d+$`)
)

// paths lists every controller connected to nqn, sorted by name.
func (h *nvmeHost) paths(nqn string) []nvmePath {
	ents, err := os.ReadDir(h.SysNVMe)
	if err != nil {
		return nil
	}
	var out []nvmePath
	for _, e := range ents {
		dir := filepath.Join(h.SysNVMe, e.Name())
		if readAttr(filepath.Join(dir, "subsysnqn")) != nqn {
			continue
		}
		traddr, trsvcid := parseAddress(readAttr(filepath.Join(dir, "address")))
		p := nvmePath{
			Controller: e.Name(),
			Transport:  readAttr(filepath.Join(dir, "transport")),
			TrAddr:     traddr,
			TrSvcID:    trsvcid,
			State:      readAttr(filepath.Join(dir, "state")),
		}
		if subs, err := os.ReadDir(dir); err == nil {
			for _, s := range subs {
				if pathNamespacePattern.MatchString(s.Name()) {
					p.ANAState = readAttr(filepath.Join(dir, s.Name(), "ana_state"))
					break
				}
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Controller < out[j].Controller })
	return out
}

func (h *nvmeHost) writeConnect(opts string) error {
	if h.connectFn != nil {
		return h.connectFn(opts)
	}
	f, err := os.OpenFile(h.FabricsDev, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s (is the nvme-fabrics module loaded?): %w", h.FabricsDev, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(opts); err != nil {
		return err
	}
	return nil
}

// connect opens a path to cfg.NQN on every portal not already connected and
// waits for the namespace's block device. One live path is enough to
// succeed (NVMePathCondition reports the others); none is an error.
// ctrl_loss_tmo=-1 keeps a lost path reconnecting instead of giving up, so
// a returning portal rejoins on its own and I/O queues rather than fails
// while no path is up.
func (h *nvmeHost) connect(ctx context.Context, cfg nvmeConfig) (string, error) {
	hostnqn, err := h.hostNQN()
	if err != nil {
		return "", err
	}
	if len(cfg.Portals) > 1 && readAttr(h.MultipathFile) == "N" {
		return "", fmt.Errorf("volume has %d paths but native NVMe multipath is off (nvme_core.multipath=N); enable it or use one portal", len(cfg.Portals))
	}
	existing := map[string]bool{}
	for _, p := range h.paths(cfg.NQN) {
		if p.Transport == cfg.Transport.kernel() && p.State != "deleting" {
			existing[p.addr()] = true
		}
	}
	var errs []error
	for _, portal := range cfg.Portals {
		if existing[portal] {
			continue
		}
		host, port, _ := net.SplitHostPort(portal)
		opts := fmt.Sprintf("nqn=%s,transport=%s,traddr=%s,trsvcid=%s,hostnqn=%s,hostid=%s,ctrl_loss_tmo=-1",
			cfg.NQN, cfg.Transport.kernel(), host, port, hostnqn, h.hostID(hostnqn))
		if err := h.writeConnect(opts); err != nil {
			errs = append(errs, fmt.Errorf("connect %s via %s %s: %w", cfg.NQN, cfg.Transport, portal, err))
		}
	}
	if len(errs) == len(cfg.Portals) {
		return "", errors.Join(errs...)
	}
	deadline := time.Now().Add(deviceWaitTimeout)
	for {
		if dev := h.namespaceDevice(cfg.NQN); dev != "" {
			return dev, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no block device for %s within %s of connecting (%v)", cfg.NQN, deviceWaitTimeout, errors.Join(errs...))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(deviceWaitInterval):
		}
	}
}

// namespaceDevice is the /dev path of nqn's namespace: the multipath head
// (/dev/nvmeXnY, listed under the subsystem) or, with multipath off, the
// single controller's namespace.
func (h *nvmeHost) namespaceDevice(nqn string) string {
	subs, _ := os.ReadDir(h.SysSubsystem)
	for _, s := range subs {
		dir := filepath.Join(h.SysSubsystem, s.Name())
		if readAttr(filepath.Join(dir, "subsysnqn")) != nqn {
			continue
		}
		if dev := firstMatch(dir, headNamespacePattern); dev != "" {
			return filepath.Join(h.DevDir, dev)
		}
	}
	for _, p := range h.paths(nqn) {
		if dev := firstMatch(filepath.Join(h.SysNVMe, p.Controller), headNamespacePattern); dev != "" {
			return filepath.Join(h.DevDir, dev)
		}
	}
	return ""
}

func firstMatch(dir string, re *regexp.Regexp) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range ents {
		if re.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// disconnect deletes every controller of nqn. Idempotent.
func (h *nvmeHost) disconnect(nqn string) error {
	var errs []error
	for _, p := range h.paths(nqn) {
		err := writeAttr(filepath.Join(h.SysNVMe, p.Controller, "delete_controller"), "1")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rescan asks every controller of nqn to re-read its namespaces (a grown
// namespace's new size).
func (h *nvmeHost) rescan(nqn string) error {
	paths := h.paths(nqn)
	if len(paths) == 0 {
		return fmt.Errorf("no controller is connected to %s", nqn)
	}
	var errs []error
	for _, p := range paths {
		if p.State != "live" {
			continue
		}
		if err := writeAttr(filepath.Join(h.SysNVMe, p.Controller, "rescan_controller"), "1"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// NVMePathCondition reports the paths this host has to the NVMe-oF volume
// volumeID: abnormal unless every portal has a live path, and a one-line
// summary. ok is false for a volume that isn't NVMe-oF. CSI v1.13's Go
// bindings carry no volume condition, so kairon-node reads this itself.
func NVMePathCondition(volumeID string) (abnormal bool, message string, ok bool) {
	ref, err := decodeAnyVolumeID(volumeID)
	if err != nil || ref.nvme == nil {
		return false, "", false
	}
	abnormal, message = newNVMeHost().pathCondition(*ref.nvme)
	return abnormal, message, true
}

// pathCondition summarizes nqn's paths: abnormal unless every expected
// portal has a live path.
func (h *nvmeHost) pathCondition(cfg nvmeConfig) (abnormal bool, message string) {
	paths := h.paths(cfg.NQN)
	usable := 0
	var parts []string
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p.addr()] = true
		// A live controller whose ANA group is inaccessible, persistent-loss or
		// changing carries no I/O.
		if p.State == "live" && (p.ANAState == "" || p.ANAState == "optimized" || p.ANAState == "non-optimized") {
			usable++
		}
		s := fmt.Sprintf("%s %s %s %s", p.Controller, p.Transport, p.addr(), p.State)
		if p.ANAState != "" {
			s += " " + p.ANAState
		}
		parts = append(parts, s)
	}
	for _, portal := range cfg.Portals {
		if !seen[portal] {
			parts = append(parts, fmt.Sprintf("%s %s not connected", cfg.Transport.kernel(), portal))
		}
	}
	msg := fmt.Sprintf("%d of %d paths usable: %s", usable, len(cfg.Portals), strings.Join(parts, "; "))
	return usable < len(cfg.Portals), msg
}
