// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

type Record struct {
	IDValue string `json:"id,omitempty"`
	UUID    string `json:"uuid,omitempty"`
	Name    string `json:"name,omitempty"`
	Status  string `json:"status,omitempty"`
	GuestIP string `json:"guest_ip,omitempty"`
	TapName string `json:"tap_name,omitempty"`
	// Disk is the real qcow2/raw path FluxVM currently has open for this
	// VM -- distinct from the Machine's original spec.image.path, which
	// only names the base image a create() request provisioned from. A
	// real migration adapter needs this exact, live path (storage
	// contract v1: shared storage, no disk copy) to build a
	// MigrationReceiverRequest on the target FluxVM.
	Disk string `json:"disk,omitempty"`
	// Workspace is the per-VM directory FluxVM allocates on the VM host --
	// e.g. its VNC socket lives at Workspace+"/vnc.sock" (see
	// internal/consoleproxy). Already present on FluxVM's own VmRecord and
	// returned by GET /v1/vms/{id}; Kairon just wasn't capturing it before.
	Workspace string `json:"workspace,omitempty"`
	// Request mirrors just the fields of FluxVM's own CreateVmRequest a
	// real migration adapter needs to reconstruct a topology-matching
	// receiver spec -- not the full request shape, which Kairon has no
	// other use for.
	Request struct {
		VCPUs     uint32 `json:"vcpus,omitempty"`
		MemoryMiB uint64 `json:"memory_mib,omitempty"`
		Network   struct {
			MAC string `json:"mac,omitempty"`
			// Extra are the VM's extra NICs; an entry with no TapName
			// is a slot freed by an unplug.
			Extra []struct {
				Bridge  string `json:"bridge,omitempty"`
				MAC     string `json:"mac,omitempty"`
				TapName string `json:"tap_name,omitempty"`
			} `json:"extra,omitempty"`
		} `json:"network,omitempty"`
		// Cdroms are the VM's CD-ROM drives; an empty Path is an ejected
		// drive (the device stays so the guest's layout doesn't change).
		Cdroms []Cdrom `json:"cdroms,omitempty"`
		// VFIODevices are host PCI devices passed through to the guest.
		VFIODevices []string `json:"vfio_devices,omitempty"`
	} `json:"request,omitempty"`
	// Hibernation is set while FluxVM holds the VM's saved state; the next
	// Start resumes it.
	Hibernation *Hibernation `json:"hibernation,omitempty"`
}

func (r Record) ID() string {
	if r.IDValue != "" {
		return r.IDValue
	}
	return r.UUID
}

// CloudInitSpec mirrors FluxVM's own cloud-init NoCloud datasource fields
// (fluxvm-core/src/model.rs CloudInitSpec) that Kairon exposes.
type CloudInitSpec struct {
	Hostname          string          `json:"hostname,omitempty"`
	User              string          `json:"user,omitempty"`
	SSHAuthorizedKeys []string        `json:"ssh_authorized_keys,omitempty"`
	Packages          []string        `json:"packages,omitempty"`
	RunCmd            []string        `json:"runcmd,omitempty"`
	StaticNetwork     bool            `json:"static_network,omitempty"`
	WriteFiles        []CloudInitFile `json:"write_files,omitempty"`
}

// CloudInitFile mirrors FluxVM's own CloudInitFile exactly (fluxvm-core's
// cloud-init write_files module) -- drops a file into the guest before
// first boot, e.g. a systemd unit or an app config, with no custom image
// build needed.
type CloudInitFile struct {
	Path        string `json:"path"`
	Content     string `json:"content"`
	Permissions string `json:"permissions,omitempty"`
}

// StorageCephRBDInPlace makes FluxVM open an existing RBD image as the boot
// disk (no clone, never deleted by FluxVM).
const StorageCephRBDInPlace = "ceph-rbd-in-place"

// StorageShared boots a raw file or block device in place: no overlay, no
// copy, writes go straight to it.
const StorageShared = "shared"

type CreateRequest struct {
	Name    string `json:"name"`
	Tenant  string `json:"tenant,omitempty"`
	Backend string `json:"backend"`
	Image   string `json:"image"`
	Storage string `json:"storage,omitempty"`
	// RootDiskIO is FluxVM's root_disk_io (QEMU).
	RootDiskIO *model.DiskIO `json:"root_disk_io,omitempty"`
	// SharedMemory boots with shareable guest memory, which hot-adding a
	// vhost-user-blk disk needs.
	SharedMemory bool   `json:"shared_memory,omitempty"`
	Kernel       string `json:"kernel,omitempty"`
	VCPUs        uint32 `json:"vcpus"`
	MemoryMiB    uint64 `json:"memory_mib"`
	// MaxVCPUs/MaxMemoryMiB request more CPU/DIMM hotplug headroom than
	// FluxVM's own default -- see model.ResourceSpec.MaxCPU/.MaxMemory's
	// own doc comment. nil lets FluxVM pick its default, exactly as
	// before this field existed.
	MaxVCPUs     *uint32        `json:"max_vcpus,omitempty"`
	MaxMemoryMiB *uint64        `json:"max_memory_mib,omitempty"`
	Network      map[string]any `json:"network,omitempty"`
	CloudInit    *CloudInitSpec `json:"cloud_init,omitempty"`
	PodUID       string         `json:"pod_uid,omitempty"`
	TTLSeconds   int64          `json:"ttl_seconds,omitempty"`
	VFIODevices  []string       `json:"vfio_devices,omitempty"`
	Qga          *QgaSpec       `json:"qga,omitempty"`
	Agent        *AgentSpec     `json:"agent,omitempty"`
	// NUMANode/CPUSet/Hugepages mirror FluxVM's own CreateVmRequest
	// fields of the same name (fluxvm-core/src/model.rs) exactly --
	// QEMU-backend-only there (silently ignored for Cloud Hypervisor/
	// Firecracker), enforced instead at CreateWithVFIO so an operator
	// gets a clear error rather than a silent no-op.
	NUMANode  *int   `json:"numa_node,omitempty"`
	CPUSet    string `json:"cpuset,omitempty"`
	Hugepages bool   `json:"hugepages,omitempty"`
	// NUMA binds guest NUMA nodes to host nodes, from
	// spec.topologyAllocation (QEMU only).
	NUMA []NUMANode `json:"numa,omitempty"`
	// SecureBoot/TPM mirror FluxVM's own CreateVmRequest fields of the same
	// name (fluxvm-core/src/model.rs) exactly -- enforced at
	// buildCreateRequest against the same backend restrictions FluxVM's own
	// scheduler enforces server-side (secure_boot: qemu only; tpm: qemu or
	// cloud-hypervisor), so an operator gets a clear error at the same
	// place NUMA/CPUSet/Hugepages already do rather than a 400 relayed from
	// FluxVM with no Kairon-side context. Neither field carries an OVMF
	// firmware/vars-template path -- that's a FluxVM node-level config
	// default (Config.qemu_ovmf_code/.qemu_ovmf_vars_template), not
	// something Kairon manages or has a Machine-spec field for yet.
	SecureBoot bool `json:"secure_boot,omitempty"`
	TPM        bool `json:"tpm,omitempty"`
	// SharedFolders maps Machine spec.volumes[1+] (PVC host dirs) into
	// FluxVM virtiofs shares — QEMU-only. See docs/guides/machine-storage.md.
	SharedFolders []SharedFolder `json:"shared_folders,omitempty"`
	// DataDisks are per-VM qcow2 overlays on shared images, present from
	// the first boot (QEMU only): an imported OVA's extra disks.
	DataDisks []DataDisk `json:"data_disks,omitempty"`
	// DiskSizeGiB grows the root disk at creation (spec.image.diskSize).
	DiskSizeGiB *uint64 `json:"disk_size_gib,omitempty"`
	// Cdroms are read-only install media (spec.cdroms), QEMU only. Needs a
	// FluxVM that knows the field; an older one ignores it.
	Cdroms []Cdrom `json:"cdroms,omitempty"`
	// Balloon adds a virtio-balloon (spec.resources.elasticMemory), QEMU
	// only.
	Balloon *BalloonSpec `json:"balloon,omitempty"`
	// Apple carries spec.apple (vz only).
	Apple *AppleRequest `json:"apple,omitempty"`
}

// Cdrom is one FluxVM CreateVmRequest.cdroms entry.
type Cdrom struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// DataDisk is one FluxVM CreateVmRequest.data_disks entry.
type DataDisk struct {
	Name    string `json:"name"`
	Backing string `json:"backing"`
}

// SharedFolder mirrors FluxVM's CreateVmRequest.shared_folders entry.
type SharedFolder struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// QgaSpec mirrors FluxVM's own qemu-guest-agent (virtio-serial) opt-in --
// adds the channel to the VM's QEMU command line so kairon-node can later
// call FluxVM's /qga/* endpoints against it.
type QgaSpec struct {
	Enabled bool `json:"enabled"`
}

// AgentSpec mirrors FluxVM's own bespoke vsock guest agent opt-in
// (fluxvm-core's AgentSpec) -- a completely different channel from
// QgaSpec above: it requires FluxVM's own proprietary fluxvm-guest-agent
// binary installed and running inside the guest image (not the standard,
// widely-available qemu-guest-agent package QgaSpec/spec.guestAgent.enabled
// needs), and is what actually backs the interactive text console
// (internal/consoleproxy's text-console relay). Port/Token are
// deliberately left unset here: FluxVM defaults the vsock port itself and,
// when Token is empty on an enabled request, generates a random one and
// burns it into the guest's own disk before boot -- Kairon never has to
// generate, store, or rotate this secret itself.
type AgentSpec struct {
	Enabled bool `json:"enabled"`
}

func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	return c.doWith(ctx, c.HTTP, method, path, body)
}

func (c *Client) doWith(ctx context.Context, hc *http.Client, method, path string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fluxvm %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// importTimeout bounds POST /v1/images/import: a multi-disk OVA conversion
// plus an initramfs rebuild takes minutes, not the default 20s.
const importTimeout = 30 * time.Minute

// ImportResult is the part of FluxVM's import response kairon-node uses.
type ImportResult struct {
	Image      string   `json:"image"`
	ExtraDisks []string `json:"extra_disks"`
	Repair     *struct {
		OSType   string   `json:"os_type"`
		Distro   string   `json:"distro"`
		Actions  []string `json:"actions"`
		Warnings []string `json:"warnings"`
	} `json:"repair"`
}

// ImportImage converts source (a host path FluxVM can read: OVA, OVF,
// VMDK, VHD(X), qcow2) into raw disks named name, optionally repairing the
// boot disk for virtio.
func (c *Client) ImportImage(ctx context.Context, source, name string, repair bool) (*ImportResult, error) {
	hc := *c.HTTP
	hc.Timeout = importTimeout
	data, err := c.doWith(ctx, &hc, http.MethodPost, "/v1/images/import", map[string]any{"source": source, "name": name, "repair": repair})
	if err != nil {
		return nil, err
	}
	var out ImportResult
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode fluxvm import response: %w", err)
	}
	if out.Image == "" {
		return nil, fmt.Errorf("fluxvm import returned no image path")
	}
	return &out, nil
}

// backupTimeout bounds a full-disk backup or restore copy.
const backupTimeout = 2 * time.Hour

// Backup is one entry of FluxVM's backup list or a backup response.
type Backup struct {
	Name      string `json:"name"`
	VMName    string `json:"vm_name"`
	CreatedAt string `json:"created_at"`
	SizeBytes int64  `json:"size_bytes"`
	Quiesced  bool   `json:"quiesced"`
	Disks     []struct {
		Name string `json:"name"`
	} `json:"disks"`
}

// BackupVM writes the VM's root and FluxVM-owned data disks to FluxVM's
// backups directory under name. quiesce is auto, required or never.
func (c *Client) BackupVM(ctx context.Context, id, name, quiesce string) (Backup, error) {
	hc := *c.HTTP
	hc.Timeout = backupTimeout
	data, err := c.doWith(ctx, &hc, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/backup", map[string]any{"name": name, "all_disks": true, "quiesce": quiesce})
	if err != nil {
		return Backup{}, err
	}
	var out Backup
	if err := json.Unmarshal(data, &out); err != nil {
		return Backup{}, fmt.Errorf("decode fluxvm backup response: %w", err)
	}
	return out, nil
}

func (c *Client) ListBackups(ctx context.Context) ([]Backup, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/backups", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []Backup `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode fluxvm backup list: %w", err)
	}
	return out.Items, nil
}

// DeleteBackup deletes a backup; a backup that is already gone is not an
// error.
func (c *Client) DeleteBackup(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/backups/"+url.PathEscape(name), nil)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		return nil
	}
	return err
}

// RestoreBackup copies backup name's disks into the stopped VM id and
// returns the disks it restored.
func (c *Client) RestoreBackup(ctx context.Context, id, name string) ([]string, error) {
	hc := *c.HTTP
	hc.Timeout = backupTimeout
	data, err := c.doWith(ctx, &hc, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/restore-backup", map[string]any{"name": name})
	if err != nil {
		return nil, err
	}
	var out struct {
		Restored []string `json:"restored"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode fluxvm restore response: %w", err)
	}
	return out.Restored, nil
}

func (c *Client) Ready(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/readyz", nil)
	if err == nil {
		return nil
	}
	// FluxVM 0.2.x exposed /healthz only; /readyz arrived later. Treat a
	// 404 on /readyz as "use the older probe" so kairon-node stays ready
	// against both generations without requiring a FluxVM upgrade first.
	if strings.Contains(err.Error(), "HTTP 404") {
		_, err = c.do(ctx, http.MethodGet, "/healthz", nil)
	}
	return err
}

func (c *Client) LookupByName(ctx context.Context, name string) (*Record, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms?name="+url.QueryEscape(name), nil)
	if err != nil {
		return nil, err
	}
	var arr []Record
	if json.Unmarshal(data, &arr) == nil {
		if len(arr) == 0 {
			return nil, nil
		}
		return &arr[0], nil
	}
	var wrapper struct {
		Items []Record `json:"items"`
		VMs   []Record `json:"vms"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil {
		if len(wrapper.Items) > 0 {
			return &wrapper.Items[0], nil
		}
		if len(wrapper.VMs) > 0 {
			return &wrapper.VMs[0], nil
		}
		return nil, nil
	}
	var one Record
	if err := json.Unmarshal(data, &one); err != nil {
		return nil, fmt.Errorf("decode FluxVM lookup: %w", err)
	}
	if one.ID() == "" && one.Name == "" {
		return nil, nil
	}
	return &one, nil
}

func (c *Client) Get(ctx context.Context, id string) (*Record, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetRaw returns FluxVM's VM record exactly as served, for handing to
// another FluxVM (a migration receiver launched from it) without dropping
// fields Record doesn't model.
func (c *Client) GetRaw(ctx context.Context, id string) (json.RawMessage, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("fluxvm returned an invalid VM record for %s", id)
	}
	return json.RawMessage(data), nil
}

// buildCreateRequest maps a Machine spec onto FluxVM's own CreateVmRequest
// wire shape -- the one place this translation happens, shared by
// CreateWithVFIO (POST /v1/vms) and CreateSandboxForMachine
// (POST /v1/sandboxes' embedded spec field), so the two creation paths
// can never drift apart on what a given Machine field maps to.
func buildCreateRequest(m model.Machine, defaultBackend string, vfioDevices []string, sharedFolders []SharedFolder) (CreateRequest, error) {
	cpu, err := model.ParseVCPUs(m.Spec.Resources.CPU)
	if err != nil {
		return CreateRequest{}, err
	}
	mem, err := model.ParseMemoryMiB(m.Spec.Resources.Memory)
	if err != nil {
		return CreateRequest{}, err
	}
	var maxVCPUs *uint32
	if m.Spec.Resources.MaxCPU != "" {
		v, err := model.ParseVCPUs(m.Spec.Resources.MaxCPU)
		if err != nil {
			return CreateRequest{}, fmt.Errorf("spec.resources.maxCpu: %w", err)
		}
		maxVCPUs = &v
	}
	var maxMemoryMiB *uint64
	if m.Spec.Resources.MaxMemory != "" {
		v, err := model.ParseMemoryMiB(m.Spec.Resources.MaxMemory)
		if err != nil {
			return CreateRequest{}, fmt.Errorf("spec.resources.maxMemory: %w", err)
		}
		// QEMU refuses maxmem equal to the boot size once DIMM slots are
		// configured. At or below Memory, let FluxVM pick its default.
		if v > mem {
			maxMemoryMiB = &v
		}
	}
	backend := m.Spec.Runtime.Backend
	if backend == "" || backend == "auto" {
		backend = defaultBackend
	}
	if backend == "" {
		backend = "qemu"
	}
	if backend == BackendApple {
		if err := checkAppleBackend(m, cpu, mem, maxVCPUs, maxMemoryMiB, vfioDevices); err != nil {
			return CreateRequest{}, err
		}
	} else if m.Spec.Apple != nil {
		return CreateRequest{}, fmt.Errorf("spec.apple needs the vz backend; Machine resolves to backend %q", backend)
	}
	if r := m.Spec.Resources; (r.NUMANode != nil || r.CPUSet != "" || r.Hugepages) && backend != "qemu" {
		return CreateRequest{}, fmt.Errorf("spec.resources.numaNode/cpuSet/hugepages require the qemu backend (FluxVM only supports them there); Machine resolves to backend %q", backend)
	}
	numa, err := numaFromAllocation(m.Spec.TopologyAllocation, cpu, mem)
	if err != nil {
		return CreateRequest{}, err
	}
	if (len(numa) > 0 || len(m.Spec.Devices) > 0) && backend != "qemu" {
		return CreateRequest{}, fmt.Errorf("spec.topology and spec.devices require the qemu backend; Machine resolves to backend %q", backend)
	}
	var balloon *BalloonSpec
	if e := m.Spec.Resources.ElasticMemory; e != nil {
		if backend != "qemu" {
			return CreateRequest{}, fmt.Errorf("spec.resources.elasticMemory requires the qemu backend; Machine resolves to backend %q", backend)
		}
		if len(vfioDevices) > 0 || m.Spec.Resources.Hugepages {
			return CreateRequest{}, fmt.Errorf("spec.resources.elasticMemory cannot be combined with VFIO devices or hugepages: their guest memory is pinned")
		}
		balloon = &BalloonSpec{FreePageReporting: e.FreePageReportingEnabled(), DeflateOnOOM: true}
	}
	if len(sharedFolders) > 0 && backend != "qemu" {
		return CreateRequest{}, fmt.Errorf("spec.volumes beyond [0] (virtiofs data volumes) require the qemu backend; Machine resolves to backend %q", backend)
	}
	// secureBoot/tpm backend restrictions mirror FluxVM's own scheduler-side
	// enforcement exactly (fluxvm-scheduler's secure_boot_or_tpm_backend_error,
	// confirmed against FluxVM's own source, not assumed): secureBoot is
	// QEMU-only, permanently -- Cloud Hypervisor's own --firmware is a
	// single opaque file with no documented separate vars store to enroll
	// Secure Boot keys into, so claiming support there would be dishonest,
	// not just unimplemented. tpm is qemu or cloud-hypervisor (both dial a
	// real swtpm-backed socket); firecracker/flux-vm have no vTPM device at
	// all. Enforced here too, not just relayed from FluxVM's own 400, so an
	// operator gets the same clear, Kairon-side error the NUMA/CPUSet check
	// above already gives instead of a bare HTTP failure.
	if m.Spec.Security.SecureBoot && backend != "qemu" {
		return CreateRequest{}, fmt.Errorf("spec.security.secureBoot requires the qemu backend (FluxVM only supports Secure Boot there); Machine resolves to backend %q", backend)
	}
	if m.Spec.Security.TPM && backend != "qemu" && backend != "cloud-hypervisor" {
		return CreateRequest{}, fmt.Errorf("spec.security.tpm requires the qemu or cloud-hypervisor backend; Machine resolves to backend %q", backend)
	}
	tenant := m.Namespace()
	network := BuildNetworkMap(m.EffectiveNetwork())
	if m.Spec.Network.VirtualNetwork != nil {
		var vs *model.MachineVirtualNetworkStatus
		if m.Status.Network != nil {
			vs = m.Status.Network.VirtualNetwork
		}
		if vs == nil || vs.VNI == 0 || vs.IP == "" {
			return CreateRequest{}, fmt.Errorf("spec.network.virtualNetwork %q: no address allocated yet", m.Spec.Network.VirtualNetwork.Name)
		}
		network["vnet"] = VnetAttach{VNI: vs.VNI, IP: vs.IP, PrefixLen: vs.PrefixLength, Gateway: vs.Gateway}
	}
	image := m.Spec.Image.Path
	if m.Spec.Image.CatalogName != "" {
		// FluxVM's own CreateVmRequest.image field already accepts a
		// catalog alias interchangeably with a raw path -- passed through
		// as-is, never fenced against --image-root (it was never a
		// filesystem path Kairon itself resolved).
		image = m.Spec.Image.CatalogName
	}
	payload := CreateRequest{
		Name:          m.RuntimeName(),
		Tenant:        tenant,
		Backend:       backend,
		Image:         image,
		Kernel:        m.Spec.Runtime.Kernel,
		VCPUs:         cpu,
		MemoryMiB:     mem,
		MaxVCPUs:      maxVCPUs,
		MaxMemoryMiB:  maxMemoryMiB,
		Network:       network,
		TTLSeconds:    m.Spec.TTLSeconds,
		VFIODevices:   vfioDevices,
		PodUID:        m.Spec.Network.PodUID,
		NUMANode:      m.Spec.Resources.NUMANode,
		CPUSet:        m.Spec.Resources.CPUSet,
		Hugepages:     m.Spec.Resources.Hugepages,
		NUMA:          numa,
		SecureBoot:    m.Spec.Security.SecureBoot,
		TPM:           m.Spec.Security.TPM,
		SharedFolders: sharedFolders,
		Balloon:       balloon,
		Apple:         appleRequest(m),
	}
	if m.Spec.Image.DiskSize != "" {
		gib, err := model.ParseDiskSizeGiB(m.Spec.Image.DiskSize)
		if err != nil {
			return CreateRequest{}, fmt.Errorf("spec.image.diskSize: %w", err)
		}
		payload.DiskSizeGiB = &gib
	}
	if len(m.Spec.Cdroms) > 0 {
		if backend != "qemu" {
			return CreateRequest{}, fmt.Errorf("spec.cdroms requires the qemu backend (FluxVM attaches install media only there); Machine resolves to backend %q", backend)
		}
		for _, cd := range m.Spec.Cdroms {
			if cd.Path == "" {
				return CreateRequest{}, fmt.Errorf("spec.cdroms %q has no resolved path", cd.Name)
			}
			payload.Cdroms = append(payload.Cdroms, Cdrom{Name: cd.Name, Path: cd.Path})
		}
	}
	if m.Spec.GuestAgent.Enabled {
		payload.Qga = &QgaSpec{Enabled: true}
	}
	if m.Spec.GuestAgent.Console {
		payload.Agent = &AgentSpec{Enabled: true}
	}
	if s := m.Spec.Image.Storage; s != "" {
		if backend != "qemu" {
			return CreateRequest{}, fmt.Errorf("storage %s requires the qemu backend; Machine resolves to backend %q", s, backend)
		}
		if payload.Agent != nil {
			return CreateRequest{}, fmt.Errorf("spec.guestAgent.console needs FluxVM to inject an agent token into the disk, which storage %s does not support", s)
		}
		payload.Storage = s
	}
	if io := m.Spec.Image.IO; io != nil {
		if backend != "qemu" {
			return CreateRequest{}, fmt.Errorf("spec.image.io requires the qemu backend; Machine resolves to backend %q", backend)
		}
		payload.RootDiskIO = io
	}
	for _, d := range m.Spec.Disks {
		if d.IO != nil && d.IO.Bus == "vhost-user-blk" {
			payload.SharedMemory = true
		}
	}
	ci := m.Spec.CloudInit
	if m.Spec.Network.StaticNetwork || ci.Hostname != "" || ci.User != "" || len(ci.SSHAuthorizedKeys) > 0 || len(ci.Packages) > 0 || len(ci.RunCmd) > 0 || len(ci.WriteFiles) > 0 {
		var writeFiles []CloudInitFile
		for _, f := range ci.WriteFiles {
			writeFiles = append(writeFiles, CloudInitFile{Path: f.Path, Content: f.Content, Permissions: f.Permissions})
		}
		payload.CloudInit = &CloudInitSpec{
			Hostname:          ci.Hostname,
			User:              ci.User,
			SSHAuthorizedKeys: ci.SSHAuthorizedKeys,
			Packages:          ci.Packages,
			RunCmd:            ci.RunCmd,
			StaticNetwork:     m.Spec.Network.StaticNetwork,
			WriteFiles:        writeFiles,
		}
	}
	return payload, nil
}

func (c *Client) Create(ctx context.Context, m model.Machine, defaultBackend string) (*Record, error) {
	return c.CreateWithVFIO(ctx, m, defaultBackend, nil, nil, nil)
}

func (c *Client) CreateWithVFIO(ctx context.Context, m model.Machine, defaultBackend string, vfioDevices []string, sharedFolders []SharedFolder, dataDisks []DataDisk) (*Record, error) {
	payload, err := buildCreateRequest(m, defaultBackend, vfioDevices, sharedFolders)
	if err != nil {
		return nil, err
	}
	payload.DataDisks = dataDisks
	data, err := c.do(ctx, http.MethodPost, "/v1/vms", payload)
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("decode FluxVM create: %w", err)
	}
	return &rec, nil
}

// HotplugCPU adds addVCPUs to a running VM's vCPU count without a reboot,
// returning the realized total. QEMU-only on FluxVM's side; fails clearly
// (not silently) once the VM's max_vcpus headroom is exhausted.
func (c *Client) HotplugCPU(ctx context.Context, id string, addVCPUs uint32) (uint32, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/hotplug/cpu", map[string]any{"add_vcpus": addVCPUs})
	if err != nil {
		return 0, err
	}
	var out struct {
		VCPUs uint32 `json:"vcpus"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, fmt.Errorf("decode hotplug cpu response: %w", err)
	}
	return out.VCPUs, nil
}

// HotplugMemory adds addMemoryMiB to a running VM's live memory without a
// reboot, returning the new total (boot-time memory plus every hot-added
// DIMM so far, not just what this call added).
func (c *Client) HotplugMemory(ctx context.Context, id string, addMemoryMiB uint64) (uint64, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/hotplug/memory", map[string]any{"add_memory_mib": addMemoryMiB})
	if err != nil {
		return 0, err
	}
	var out struct {
		MemoryMiB uint64 `json:"memory_mib"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, fmt.Errorf("decode hotplug memory response: %w", err)
	}
	return out.MemoryMiB, nil
}

// Disk is one entry of GET /v1/vms/{id}/disks.
type Disk struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Format string `json:"format"`
}

// ListDisks returns the VM's root and data disks.
func (c *Client) ListDisks(ctx context.Context, id string) ([]Disk, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/disks", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []Disk `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode disk list: %w", err)
	}
	return out.Items, nil
}

// AttachDisk attaches the existing image file or block device at path as
// data disk name, hot-adding it when the VM is running.
func (c *Client) AttachDisk(ctx context.Context, id, name, path string, io *model.DiskIO) error {
	body := map[string]any{"name": name, "path": path}
	if io != nil {
		body["io"] = io
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/disks", body)
	return err
}

// DetachDisk unplugs data disk name. For a path-attached disk FluxVM only
// removes its link, never the source.
func (c *Client) DetachDisk(ctx context.Context, id, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/vms/"+url.PathEscape(id)+"/disks/"+url.PathEscape(name), nil)
	return err
}

// HotplugNIC hot-adds a bridged virtio-net NIC with the given MAC.
func (c *Client) HotplugNIC(ctx context.Context, id, bridge, mac string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/hotplug/nic", map[string]any{"bridge": bridge, "mac": mac})
	return err
}

// UnplugNIC hot-removes the extra NIC with this MAC and deletes its tap.
func (c *Client) UnplugNIC(ctx context.Context, id, mac string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/hotplug/nic/unplug", map[string]any{"mac": mac})
	return err
}

// SetResourceLimits applies a partial cgroup v2 resource-control patch to a
// running VM's own VMM process cgroup -- FluxVM's POST /v1/vms/{id}/resources,
// backend-agnostic (it operates on the cgroup the VMM process runs in, not a
// backend-specific API, unlike HotplugCPU/HotplugMemory above). Only the
// fields set in limits are touched; a nil field leaves that cgroup control
// exactly as it already was. Returns FluxVM's own error unmodified when the
// VM isn't running (no cgroup exists yet) or the cgroup call itself fails.
func (c *Client) SetResourceLimits(ctx context.Context, id string, limits model.ResourceLimits) error {
	payload := map[string]any{}
	if limits.CPUQuotaPercent != nil {
		payload["cpu_quota_percent"] = *limits.CPUQuotaPercent
	}
	if limits.MemoryMaxBytes != nil {
		payload["memory_max_bytes"] = *limits.MemoryMaxBytes
	}
	if limits.IOWeight != nil {
		payload["io_weight"] = *limits.IOWeight
	}
	if limits.PIDsMax != nil {
		payload["pids_max"] = *limits.PIDsMax
	}
	if len(limits.CPUSetCPUs) > 0 {
		payload["cpuset_cpus"] = limits.CPUSetCPUs
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/resources", payload)
	return err
}

// Stats is a running VM's live cgroup-derived resource usage --
// FluxVM's GET /v1/vms/{id}/stats, backend-agnostic (cgroup-based, like
// SetResourceLimits above, not a backend-specific API).
type Stats struct {
	// CPUUsagePercent is a percentage of one core, averaged over the
	// VMM process's entire lifetime (not an instantaneous rate) -- can
	// exceed 100 for a multi-vCPU Machine using more than one core's
	// worth of time.
	CPUUsagePercent  float64 `json:"cpuUsagePercent"`
	MemoryUsageBytes uint64  `json:"memoryUsageBytes"`
	DiskReadBytes    uint64  `json:"diskReadBytes"`
	DiskWriteBytes   uint64  `json:"diskWriteBytes"`
}

// GetStats reads a running VM's current resource usage.
func (c *Client) GetStats(ctx context.Context, id string) (*Stats, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/stats", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		CPUUsagePercent  float64 `json:"cpu_usage_percent"`
		MemoryUsageBytes uint64  `json:"memory_usage_bytes"`
		DiskReadBytes    uint64  `json:"disk_read_bytes"`
		DiskWriteBytes   uint64  `json:"disk_write_bytes"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode vm stats: %w", err)
	}
	return &Stats{
		CPUUsagePercent:  out.CPUUsagePercent,
		MemoryUsageBytes: out.MemoryUsageBytes,
		DiskReadBytes:    out.DiskReadBytes,
		DiskWriteBytes:   out.DiskWriteBytes,
	}, nil
}

// Pause suspends a running VM's guest CPUs via FluxVM's real QMP
// stop/cont (or the equivalent on Cloud Hypervisor/Firecracker --
// backend-agnostic, unlike hotplug/NUMA/hugepages) -- RAM and device
// state stay fully resident, the guest simply stops executing, distinct
// from Delete/Stopped (which tears the runtime down entirely) or the
// cgroup-freezer-based fsfreeze QGA already wraps (which freezes the
// guest's own filesystem I/O, not its CPUs, and needs the guest to
// cooperate). Returns the updated Record (its Status now "Paused").
func (c *Client) Pause(ctx context.Context, id string) (*Record, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/pause", nil)
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("decode pause response: %w", err)
	}
	return &rec, nil
}

// Resume reverses Pause -- FluxVM's real QMP cont (or backend
// equivalent), resuming guest CPU execution exactly where it left off.
func (c *Client) Resume(ctx context.Context, id string) (*Record, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/resume", nil)
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("decode resume response: %w", err)
	}
	return &rec, nil
}

// EjectCdrom removes the medium from a CD-ROM drive (live when the VM is
// running) and records the drive as empty. Needs FluxVM with
// POST /v1/vms/{id}/cdroms/{name}/eject; ejecting an empty drive is a no-op.
func (c *Client) EjectCdrom(ctx context.Context, id, name string) (*Record, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/cdroms/"+url.PathEscape(name)+"/eject", nil)
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("decode cdrom eject response: %w", err)
	}
	return &rec, nil
}

func (c *Client) Delete(ctx context.Context, id string) error {
	return c.deleteIdempotent(ctx, "/v1/vms/"+url.PathEscape(id), "/v1/vms", id)
}

// deleteIdempotent implements the idempotent-404 raw DELETE shared by
// Client.Delete (Machine deletion) and DeleteNetworkGroup
// (internal/fluxvm/network.go's own doc comment explains the
// idempotent-404 behavior itself): hand-rolled rather than routed
// through the shared do() helper specifically to get at the raw status
// code, since do() treats every non-2xx (404 included) as an error.
// path is the full request path (already url.PathEscape'd by the
// caller); route and resourceID are used only to format the error
// message the same way each caller previously did.
func (c *Client) deleteIdempotent(ctx context.Context, path, route, resourceID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	// Some FluxVM builds answer DELETE of an already-removed VM with
	// HTTP 400 {"error":"VM not found"} instead of 404. Deletion is
	// idempotent, so treat that as success; otherwise the Machine
	// finalizer is never released.
	if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(string(data)), "not found") {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("fluxvm DELETE %s/%s: HTTP %d: %s", route, resourceID, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}
