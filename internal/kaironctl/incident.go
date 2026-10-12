// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/kube"
)

// incidentPoll is how often a running node bundle is checked.
var incidentPoll = time.Second

type incidentOpts struct {
	Deep       bool
	Pcap       time.Duration
	PcapFilter string
	MemoryDump bool
	NoRedact   bool
	LogLines   int
	Output     string
	Timeout    time.Duration
}

// incidentManifest is manifest.json at the top of a deep bundle.
type incidentManifest struct {
	Kind        string                    `json:"kind"`
	Namespace   string                    `json:"namespace"`
	Machine     string                    `json:"machine"`
	UID         string                    `json:"uid,omitempty"`
	Node        string                    `json:"node,omitempty"`
	RuntimeID   string                    `json:"runtimeID,omitempty"`
	CollectedAt time.Time                 `json:"collectedAt"`
	Kaironctl   string                    `json:"kaironctl"`
	Redacted    bool                      `json:"redacted"`
	Cluster     []string                  `json:"clusterFiles"`
	Skipped     []string                  `json:"skipped,omitempty"`
	NodeBundle  *fluxvm.DiagnosticsBundle `json:"nodeBundle,omitempty"`
	MemoryDump  string                    `json:"memoryDumpFetch,omitempty"`
}

func newDiagnoseCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diagnose",
		Short: "Diagnose a Machine; --deep collects an incident bundle",
	}
	cmd.AddCommand(newDiagnoseMachineCmd(opts), newDiagnoseMemoryDumpCmd(opts))
	return cmd
}

func newDiagnoseMachineCmd(opts *Options) *cobra.Command {
	o := &incidentOpts{}
	c := &cobra.Command{
		Use:   "machine NAME",
		Short: "Rank likely causes for a Machine; --deep writes an incident bundle (tar.gz)",
		Long: `Without --deep, prints the ranked causes from the Machine's phase,
conditions and events (the same as "kaironctl agent diagnose --no-ai").

With --deep, writes one tar.gz with the cluster side (Machine, events,
migrations, Node, NodeTopology, the ranked causes) under cluster/ and the
node side under node/: FluxVM's QMP state, cgroup and PSI files, console
log, dataplane status, flows, drops, tc and BPF state, host memory, and with
--pcap a packet capture. The node side needs KAIRON_UI_URL and an
administrator KAIRON_UI_TOKEN.

--memory-dump also has FluxVM dump guest memory (QEMU, ELF). The guest is
paused while it is written. The dump stays on the node; the manifest gives
its size and sha256, and "kaironctl diagnose memory-dump" downloads it.

Secrets are fingerprinted or dropped by default: cloud-init, tokens,
passwords and secret env. --no-redact keeps them.`,
		Example: `  kaironctl diagnose machine web
  kaironctl diagnose machine web --deep --pcap 10s -o web-incident.tar.gz
  kaironctl diagnose machine db --deep --memory-dump`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), o.Timeout)
			defer cancel()
			if !o.Deep {
				d, err := diagnoseRef(ctx, kc, opts.Namespace, "machine/"+args[0], false)
				if err != nil {
					return err
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(d)
			}
			if o.Pcap < 0 || o.Pcap > 30*time.Second {
				return fmt.Errorf("--pcap must be 0-30s")
			}
			if o.Output == "" {
				o.Output = fmt.Sprintf("kairon-incident-%s-%s-%s.tar.gz", opts.Namespace, args[0], time.Now().UTC().Format("20060102T150405Z"))
			}
			f, err := os.OpenFile(o.Output, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			m, err := writeIncident(ctx, kc, f, opts.Namespace, args[0], o, opts.Version)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				_ = os.Remove(o.Output)
				return err
			}
			style.Log(style.EmojiOK, "wrote %s", o.Output)
			for _, s := range m.Skipped {
				style.Log(style.EmojiWarn, "not collected: %s", s)
			}
			if nb := m.NodeBundle; nb != nil && nb.MemoryDump != nil {
				style.Log(style.EmojiOK, "memory dump on %s: %s (%d MiB, guest paused %dms)", m.Node, nb.MemoryDump.Path, nb.MemoryDump.SizeBytes>>20, nb.MemoryDump.PausedMs)
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "fetch it with: %s\n", m.MemoryDump)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&o.Deep, "deep", false, "collect an incident bundle instead of printing the diagnosis")
	c.Flags().DurationVar(&o.Pcap, "pcap", 0, "packet capture length on the Machine's dataplane interface, up to 30s")
	c.Flags().StringVar(&o.PcapFilter, "pcap-filter", "", "tcpdump filter for --pcap")
	c.Flags().BoolVar(&o.MemoryDump, "memory-dump", false, "dump guest memory on the node (QEMU; pauses the guest while it is written)")
	c.Flags().BoolVar(&o.NoRedact, "no-redact", false, "keep secrets (cloud-init, tokens, passwords) in the bundle")
	c.Flags().IntVar(&o.LogLines, "log-lines", 0, "console log lines from the node (default 2000, at most 20000)")
	c.Flags().StringVarP(&o.Output, "output", "o", "", "bundle file (default kairon-incident-<ns>-<name>-<UTC time>.tar.gz)")
	c.Flags().DurationVar(&o.Timeout, "timeout", 20*time.Minute, "give up after this long")
	return c
}

func newDiagnoseMemoryDumpCmd(opts *Options) *cobra.Command {
	var output string
	c := &cobra.Command{
		Use:     "memory-dump MACHINE TOKEN",
		Short:   "Download the guest memory dump of an incident bundle and check its sha256",
		Example: `  kaironctl diagnose memory-dump db 3f9c0a1b2c3d4e5f -o db.elf`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if uiBaseURL() == "" {
				return fmt.Errorf("needs KAIRON_UI_URL")
			}
			if output == "" {
				output = fmt.Sprintf("%s-%s.elf", args[0], args[1])
			}
			n, sum, err := downloadMemoryDump(cmd.Context(), opts.Namespace, args[0], args[1], output)
			if err != nil {
				return err
			}
			style.Log(style.EmojiOK, "wrote %d bytes to %s (sha256 %s)", n, output, sum)
			return nil
		},
	}
	c.Flags().StringVarP(&output, "output", "o", "", "dump file (default MACHINE-TOKEN.elf)")
	return c
}

// writeIncident writes a deep bundle to w. A part that cannot be read is
// listed in manifest.json and README.txt rather than failing the bundle.
func writeIncident(ctx context.Context, kc *kube.Client, w io.Writer, ns, name string, o *incidentOpts, version string) (*incidentManifest, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	b := &bundleWriter{tw: tw, when: time.Now().UTC()}
	redact := !o.NoRedact
	m := &incidentManifest{
		Kind:        "KaironIncidentBundle",
		Namespace:   ns,
		Machine:     name,
		CollectedAt: b.when,
		Kaironctl:   version,
		Redacted:    redact,
	}
	clean := func(raw []byte) []byte {
		if redact {
			return redactJSON(raw)
		}
		return raw
	}
	add := func(file string, data []byte, err error) error {
		if err == nil {
			m.Cluster = append(m.Cluster, file)
		}
		return b.addOptional(file, data, err)
	}

	machine, err := kc.GetMachine(ctx, ns, name)
	if err != nil {
		return nil, fmt.Errorf("machine %s/%s: %w", ns, name, err)
	}
	m.UID = machine.Metadata.UID
	m.Node = machine.Status.NodeName
	m.RuntimeID = machine.Status.RuntimeID

	raw, err := kubeGetObject(ctx, kc, "/apis/kairon.zyvor.dev/v1/namespaces/"+url.PathEscape(ns)+"/machines/"+url.PathEscape(name))
	if err := add("cluster/machine.json", clean(raw), err); err != nil {
		return nil, err
	}
	events, err := kc.ListEventsFor(ctx, ns, "Machine", name)
	if err := add("cluster/events.json", jsonIndent(events), err); err != nil {
		return nil, err
	}
	migrations, err := kc.ListMachineMigrations(ctx)
	var mine []any
	for _, mm := range migrations {
		if mm.Namespace() == ns && mm.Spec.MachineName == name {
			mine = append(mine, mm)
		}
	}
	if err := add("cluster/migrations.json", clean(jsonIndent(mine)), err); err != nil {
		return nil, err
	}
	d, err := diagnoseRef(ctx, kc, ns, "machine/"+name, false)
	if err := add("cluster/diagnosis.json", jsonIndent(d), err); err != nil {
		return nil, err
	}
	if m.Node != "" {
		raw, err := kubeGetObject(ctx, kc, "/api/v1/nodes/"+url.PathEscape(m.Node))
		if err := add("cluster/node.json", clean(raw), err); err != nil {
			return nil, err
		}
		raw, err = kubeGetObject(ctx, kc, "/apis/kairon.zyvor.dev/v1/nodetopologies/"+url.PathEscape(m.Node))
		if err := add("cluster/nodetopology.json", raw, err); err != nil {
			return nil, err
		}
	}

	switch {
	case m.RuntimeID == "" || m.Node == "":
		b.skip = append(b.skip, "node/: the Machine has no runtime on a node")
	case uiBaseURL() == "":
		b.skip = append(b.skip, "node/: KAIRON_UI_URL is not set")
	default:
		req := fluxvm.DiagnosticsRequest{
			PcapSeconds: int(o.Pcap / time.Second),
			PcapFilter:  o.PcapFilter,
			MemoryDump:  o.MemoryDump,
			Redact:      redact,
			LogLines:    o.LogLines,
		}
		job, err := collectNodeBundle(ctx, ns, name, req, b)
		if job != nil {
			m.NodeBundle = job
			if job.MemoryDump != nil {
				m.MemoryDump = fmt.Sprintf("kaironctl diagnose memory-dump %s %s -n %s", name, job.Token, ns)
			}
		}
		if err != nil {
			b.skip = append(b.skip, "node/: "+err.Error())
		}
	}

	m.Skipped = b.skip
	if err := b.add("manifest.json", jsonIndent(m)); err != nil {
		return nil, err
	}
	readme := "Kairon incident bundle for Machine " + ns + "/" + name + "\n\n" +
		"cluster/  Machine, events, migrations, Node, NodeTopology and the ranked causes\n" +
		"node/     FluxVM's bundle from the Machine's node (see node/manifest.json)\n\n"
	if redact {
		readme += "Redacted: secret-like keys, Machine spec.cloudInit, and on the node side secret-looking JSON values and text after password=/token:/Bearer.\n" +
			"Not redacted: secrets printed to the console without such a key word, packet captures, memory dumps.\n"
	} else {
		readme += "NOT redacted (--no-redact): this bundle may hold credentials.\n"
	}
	if len(b.skip) > 0 {
		readme += "\nNot collected:\n- " + strings.Join(b.skip, "\n- ") + "\n"
	}
	if err := b.add("README.txt", []byte(readme)); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return m, gz.Close()
}

// collectNodeBundle starts a FluxVM bundle through kairon-ui, waits for it
// and copies its files under node/. The job is returned once known, also on
// a later error.
func collectNodeBundle(ctx context.Context, ns, name string, req fluxvm.DiagnosticsRequest, b *bundleWriter) (*fluxvm.DiagnosticsBundle, error) {
	base := fmt.Sprintf("/api/v1/machines/%s/%s/diagnostics", url.PathEscape(ns), url.PathEscape(name))
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var job fluxvm.DiagnosticsBundle
	if err := uiCall(ctx, http.MethodPost, base, bytes.NewReader(body), &job); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	if req.MemoryDump {
		style.Log(style.EmojiOK, "collecting on the node, with a memory dump (the guest pauses while it is written)")
	}
	for job.State == "running" {
		select {
		case <-ctx.Done():
			return &job, fmt.Errorf("bundle %s still running: %w", job.Token, ctx.Err())
		case <-time.After(incidentPoll):
		}
		if err := uiCall(ctx, http.MethodGet, base+"/"+url.PathEscape(job.Token), nil, &job); err != nil {
			return &job, fmt.Errorf("poll: %w", err)
		}
	}
	if job.State != "done" {
		return &job, fmt.Errorf("bundle %s %s: %s", job.Token, job.State, job.Error)
	}
	resp, err := uiRequest(ctx, http.MethodGet, base+"/"+url.PathEscape(job.Token)+"/bundle", nil)
	if err != nil {
		return &job, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &job, fmt.Errorf("download: %w", uiError(resp))
	}
	tmp, err := os.CreateTemp("", "kairon-node-bundle-*.tar.gz")
	if err != nil {
		return &job, err
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return &job, fmt.Errorf("download: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); job.SHA256 != "" && got != job.SHA256 {
		return &job, fmt.Errorf("bundle sha256 %s, FluxVM reported %s", got, job.SHA256)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return &job, err
	}
	if err := copyTarUnder(tmp, "node/", b); err != nil {
		return &job, fmt.Errorf("copy: %w", err)
	}
	return &job, nil
}

// copyTarUnder copies every regular file of a tar.gz stream into b under
// prefix.
func copyTarUnder(r io.Reader, prefix string, b *bundleWriter) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || strings.Contains(hdr.Name, "..") || strings.HasPrefix(hdr.Name, "/") {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 256<<20))
		if err != nil {
			return err
		}
		if err := b.add(prefix+hdr.Name, data); err != nil {
			return err
		}
	}
	return nil
}

func downloadMemoryDump(ctx context.Context, ns, name, token, output string) (int64, string, error) {
	base := fmt.Sprintf("/api/v1/machines/%s/%s/diagnostics/%s", url.PathEscape(ns), url.PathEscape(name), url.PathEscape(token))
	var job fluxvm.DiagnosticsBundle
	if err := uiCall(ctx, http.MethodGet, base, nil, &job); err != nil {
		return 0, "", err
	}
	if job.MemoryDump == nil {
		return 0, "", fmt.Errorf("bundle %s has no memory dump", token)
	}
	resp, err := uiRequest(ctx, http.MethodGet, base+"/memory-dump", nil)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("download: %w", uiError(resp))
	}
	f, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if sum != job.MemoryDump.SHA256 {
		return n, sum, fmt.Errorf("sha256 %s does not match the node's %s", sum, job.MemoryDump.SHA256)
	}
	return n, sum, nil
}

// uiCall sends a kairon-ui request and decodes a 2xx JSON answer into out.
func uiCall(ctx context.Context, method, path string, body io.Reader, out any) error {
	resp, err := uiRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return uiError(resp)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func jsonIndent(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return []byte(fmt.Sprintf("%q", err.Error()))
	}
	return b
}
