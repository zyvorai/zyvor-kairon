// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// ovfSystem is one VirtualSystem. Tags carry no namespace, so ovf:, rasd:
// and vmw: prefixes all match.
type ovfSystem struct {
	ID   string `xml:"id,attr"`
	Name string `xml:"Name"`
	OS   struct {
		OSType      string `xml:"osType,attr"`
		Description string `xml:"Description"`
	} `xml:"OperatingSystemSection"`
	Items   []ovfItem `xml:"VirtualHardwareSection>Item"`
	Configs []struct {
		Key   string `xml:"key,attr"`
		Value string `xml:"value,attr"`
	} `xml:"VirtualHardwareSection>Config"`
	Props []ovfProp `xml:"ProductSection>Property"`
}

type ovfProp struct {
	Key string `xml:"key,attr"`
}

// ovfCollection is a VirtualSystemCollection (a vApp); it may nest.
type ovfCollection struct {
	ID       string          `xml:"id,attr"`
	Name     string          `xml:"Name"`
	Startup  []ovfStartItem  `xml:"StartupSection>Item"`
	Props    []ovfProp       `xml:"ProductSection>Property"`
	Systems  []ovfSystem     `xml:"VirtualSystem"`
	Children []ovfCollection `xml:"VirtualSystemCollection"`
}

type ovfStartItem struct {
	ID    string `xml:"id,attr"`
	Order string `xml:"order,attr"`
}

// ovfEnvelope is the subset of an OVF descriptor kaironctl reads.
type ovfEnvelope struct {
	XMLName     xml.Name        `xml:"Envelope"`
	Collections []ovfCollection `xml:"VirtualSystemCollection"`
	Systems     []ovfSystem     `xml:"VirtualSystem"`
	Disks       []struct {
		ID string `xml:"diskId,attr"`
	} `xml:"DiskSection>Disk"`
}

type ovfItem struct {
	ResourceType    string `xml:"ResourceType"`
	VirtualQuantity string `xml:"VirtualQuantity"`
	AllocationUnits string `xml:"AllocationUnits"`
	HostResource    string `xml:"HostResource"`
}

// ovfInfo is what the import uses from an OVF.
type ovfInfo struct {
	Name      string
	VCPUs     int
	MemoryMiB int64
	Firmware  string
	OS        string
	NICs      int
	Disks     int
}

// parseOVF reads a single-VM OVF. A multi-VM OVF is an error here; use
// parseOVFPackage for those.
func parseOVF(data []byte) (ovfInfo, error) {
	var env ovfEnvelope
	if err := xml.Unmarshal(data, &env); err != nil {
		return ovfInfo{}, fmt.Errorf("parse OVF: %w", err)
	}
	if len(env.Collections) > 0 || len(env.Systems) > 1 {
		return ovfInfo{}, errors.New("multi-VM OVF (VirtualSystemCollection or several VirtualSystem entries): use parseOVFPackage")
	}
	var sys ovfSystem
	if len(env.Systems) == 1 {
		sys = env.Systems[0]
	}
	info := systemInfo(sys)
	info.Disks = len(env.Disks)
	return info, nil
}

// systemInfo maps one VirtualSystem to an ovfInfo; Disks is left for the caller.
func systemInfo(sys ovfSystem) ovfInfo {
	info := ovfInfo{Name: sys.Name, OS: sys.OS.OSType}
	if info.Name == "" {
		info.Name = sys.ID
	}
	if info.OS == "" {
		info.OS = sys.OS.Description
	}
	for _, it := range sys.Items {
		q, _ := strconv.ParseInt(strings.TrimSpace(it.VirtualQuantity), 10, 64)
		switch strings.TrimSpace(it.ResourceType) {
		case "3":
			info.VCPUs = int(q)
		case "4":
			units := strings.ReplaceAll(strings.ToLower(it.AllocationUnits), " ", "")
			switch units {
			case "byte*2^30", "gigabytes":
				info.MemoryMiB = q << 10
			case "byte*2^10", "kilobytes":
				info.MemoryMiB = q >> 10
			case "byte":
				info.MemoryMiB = q >> 20
			default: // byte*2^20, MegaBytes, or unset
				info.MemoryMiB = q
			}
		case "10":
			info.NICs++
		}
	}
	for _, c := range sys.Configs {
		if c.Key == "firmware" {
			info.Firmware = strings.ToLower(c.Value)
		}
	}
	return info
}

// scanOVA streams an OVA once, hashing every byte and pulling out the OVF.
// Nothing is written to disk.
func scanOVA(r io.Reader) (ovf []byte, digest string, size int64, err error) {
	h := sha256.New()
	counter := &countingWriter{}
	tee := io.TeeReader(r, io.MultiWriter(h, counter))
	tr := tar.NewReader(tee)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", 0, fmt.Errorf("read OVA: %w", err)
		}
		if ovf == nil && strings.HasSuffix(strings.ToLower(hdr.Name), ".ovf") {
			if ovf, err = io.ReadAll(io.LimitReader(tr, 8<<20)); err != nil {
				return nil, "", 0, err
			}
		}
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return nil, "", 0, err
	}
	if ovf == nil {
		return nil, "", 0, errors.New("OVA has no .ovf descriptor")
	}
	return ovf, "sha256:" + hex.EncodeToString(h.Sum(nil)), counter.n, nil
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func openImportSource(ctx context.Context, source string) (io.ReadCloser, error) {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("GET %s: HTTP %d", source, resp.StatusCode)
		}
		return resp.Body, nil
	}
	return os.Open(source)
}

// cmdImport implements `kaironctl import ova SOURCE`.
func cmdImport(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 2 || strings.ToLower(args[0]) != "ova" {
		fatal(fmt.Errorf("usage: kaironctl import ova SOURCE [--name NAME] [--url URL] [--no-repair] [--dry-run] [create flags]"))
	}
	source := args[1]
	fs := flag.NewFlagSet("import ova", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	name := fs.String("name", "", "Machine name (default: the OVF VM name)")
	url := fs.String("url", "", "http(s) URL nodes download the OVA from (default: SOURCE when it is a URL; required for a local file)")
	digest := fs.String("sha256", "", "OVA digest; with --cpu and --memory set, skips reading the OVA")
	noRepair := fs.Bool("no-repair", false, "convert only; skip the offline virtio repair")
	dryRun := fs.Bool("dry-run", false, "print the Machine instead of creating it")
	specFn, _ := machineSpecFromFlags(fs)
	_ = fs.Parse(args[2:])
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	isURL := strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
	if *url == "" {
		if !isURL {
			fatal(fmt.Errorf("--url is required for a local OVA: nodes download the image themselves, so put the file on an http(s) server and pass its URL"))
		}
		*url = source
	}

	var info ovfInfo
	sum := *digest
	if sum == "" || !set["cpu"] || !set["memory"] {
		rc, err := openImportSource(ctx, source)
		if err != nil {
			fatal(err)
		}
		ovf, d, size, err := scanOVA(rc)
		_ = rc.Close()
		if err != nil {
			fatal(err)
		}
		infos, warns, err := parseOVFPackage(ovf)
		if err != nil {
			fatal(err)
		}
		if sum != "" && sum != d {
			fatal(fmt.Errorf("--sha256 %s does not match the OVA (%s)", sum, d))
		}
		sum = d
		if len(infos) > 1 {
			importOVAMulti(ctx, infos, warns, multiImportOpts{
				source: source, url: *url, namespace: *ns, name: *name,
				noRepair: *noRepair, size: size, sum: d, set: set, specFn: specFn,
			})
			return
		}
		info = infos[0]
		fmt.Fprintf(os.Stderr, "read %s: %d bytes, %s, OVF %q: %d vCPU, %d MiB, %d disk(s), %d NIC(s)\n",
			source, size, sum, info.Name, info.VCPUs, info.MemoryMiB, info.Disks, info.NICs)
	}

	spec := specFn()
	if !set["cpu"] && info.VCPUs > 0 {
		spec.Resources.CPU = strconv.Itoa(info.VCPUs)
	}
	if !set["memory"] && info.MemoryMiB > 0 {
		spec.Resources.Memory = strconv.FormatInt(info.MemoryMiB, 10) + "Mi"
	}
	spec.Image = model.ImageSpec{
		Digest: sum,
		Source: &model.ImageSource{HTTPURL: *url, Format: "ova", Repair: !*noRepair},
	}
	machineName := *name
	if machineName == "" {
		machineName = resourceName(info.Name)
	}
	if machineName == "" {
		fatal(fmt.Errorf("--name is required (the OVF has no VM name)"))
	}
	m := model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{Name: machineName, Namespace: *ns, Labels: map[string]string{"kairon.zyvor.dev/imported-from": "ova"}},
		Spec:     spec,
	}
	if info.Firmware == "efi" {
		fmt.Fprintln(os.Stderr, "note: the source VM boots UEFI; schedule it on a node whose FluxVM sets qemu_ovmf_code")
	}
	if info.Disks > 1 {
		fmt.Fprintf(os.Stderr, "note: the OVA has %d disks; only the boot disk is attached\n", info.Disks)
	}
	if *dryRun {
		b, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(b))
		return
	}
	out, err := kc.CreateMachine(ctx, *ns, m)
	if err != nil {
		fatal(err)
	}
	okf("machine/%s created from %s (kairon-node converts and repairs it before first boot)", out.Metadata.Name, *url)
}
