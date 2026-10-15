// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func newExportCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Download a Machine's disk or print its manifest",
	}
	cmd.AddCommand(newExportManifestCmd(opts), newExportMachineCmd(opts), newExportCreateCmd(opts), newExportListCmd(opts), newExportDownloadCmd(opts), newExportDeleteCmd(opts))
	return cmd
}

// createExport creates a MachineExport, reusing one that already exists
// under name. An empty name is derived from the Machine and the time.
func createExport(ctx context.Context, kc *kube.Client, ns, name string, spec model.MachineExportSpec) (string, error) {
	if name == "" {
		name = resourceName(spec.MachineName + "-export-" + time.Now().UTC().Format("20060102-150405"))
	}
	_, err := kc.CreateMachineExport(ctx, ns, model.MachineExport{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineExport},
		Metadata: model.ObjectMeta{Name: name, Namespace: ns},
		Spec:     spec,
	})
	switch {
	case kube.IsConflict(err):
		fmt.Fprintf(os.Stderr, "reusing machineexport/%s\n", name)
	case err != nil:
		return "", err
	default:
		fmt.Fprintf(os.Stderr, "machineexport/%s created\n", name)
	}
	return name, nil
}

// fetchExport waits for an export to be Ready and downloads it to output.
func fetchExport(ctx context.Context, kc *kube.Client, ns, name, output string, timeout time.Duration) error {
	base := uiBaseURL()
	if base == "" {
		return fmt.Errorf("KAIRON_UI_URL is not set; the download goes through kairon-ui")
	}
	e, err := waitExportReady(ctx, kc, ns, name, timeout, 2*time.Second)
	if err != nil {
		return err
	}
	client, err := uiHTTPClient()
	if err != nil {
		return err
	}
	n, err := downloadExport(ctx, client, base+e.Status.DownloadPath, uiToken(false), output, e.Status.SizeBytes)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d bytes (%s) to %s\n", n, e.Status.Format, output)
	return nil
}

func newExportCreateCmd(opts *Options) *cobra.Command {
	var format, ttl, name string
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "create MACHINE",
		Short: "Create a MachineExport without downloading it",
		Long: `Create a MachineExport for a Stopped or Halted Machine and print its name. Download
it later with "kaironctl export download NAME", from this machine or another,
until its TTL runs out. --wait blocks until the node is serving the disk.`,
		Example: `  $ kaironctl export create web-1 --format qcow2 --ttl 24h --name web-1-golden
  $ kaironctl export download web-1-golden -o web-1.qcow2`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec := model.MachineExportSpec{MachineName: args[0], Format: format, TTL: ttl}
			if err := model.ValidateMachineExportSpec(spec); err != nil {
				return err
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			name, err := createExport(ctx, kc, opts.Namespace, name, spec)
			if err != nil {
				return err
			}
			if wait {
				if _, err := waitExportReady(ctx, kc, opts.Namespace, name, timeout, 2*time.Second); err != nil {
					return err
				}
			}
			fmt.Println(name)
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "", "qcow2 or raw; empty keeps the disk's own format")
	cmd.Flags().StringVar(&ttl, "ttl", "", "how long the export stays downloadable (default 2h, max 168h)")
	cmd.Flags().StringVar(&name, "name", "", "MachineExport name")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the export is Ready")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long --wait waits")
	return cmd
}

func newExportListCmd(opts *Options) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List MachineExports in the namespace",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			items, err := kc.ListMachineExportsNamespace(cmd.Context(), opts.Namespace)
			if err != nil {
				return err
			}
			switch output {
			case "json", "yaml":
				raw, err := json.Marshal(model.MachineExportList{Items: items})
				if err != nil {
					return err
				}
				return printRawStructured(os.Stdout, raw, output)
			case "":
				writeExportTable(os.Stdout, items, time.Now())
				return nil
			}
			return fmt.Errorf("unknown output format %q (want json or yaml)", output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "json or yaml")
	return cmd
}

func writeExportTable(w io.Writer, items []model.MachineExport, now time.Time) {
	if len(items) == 0 {
		_, _ = fmt.Fprintln(w, "No MachineExports found.")
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Metadata.Name < items[j].Metadata.Name })
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tMACHINE\tPHASE\tFORMAT\tSIZE\tEXPIRES\tNODE")
	for _, e := range items {
		size := "-"
		if e.Status.SizeBytes > 0 {
			size = humanBytes(uint64(e.Status.SizeBytes))
		}
		expires := "-"
		if e.Status.ExpiresAt != nil {
			if d := e.Status.ExpiresAt.Sub(now); d > 0 {
				expires = "in " + d.Round(time.Minute).String()
			} else {
				expires = "expired"
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Metadata.Name, e.Spec.MachineName, style.Phase(w, orDash(e.Status.Phase)),
			orDash(e.Status.Format), size, expires, orDash(e.Status.NodeName))
	}
	_ = tw.Flush()
}

func newExportDownloadCmd(opts *Options) *cobra.Command {
	var output string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "download NAME -o FILE",
		Short: "Download an existing MachineExport",
		Long: `Wait for a MachineExport to be Ready and download it through kairon-ui. A partial
FILE is resumed. The export is left in place; delete it with "export delete".`,
		Example: `  $ kaironctl export download web-1-golden -o web-1.qcow2`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return fmt.Errorf("-o FILE is required (use - for stdout)")
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return fetchExport(ctx, kc, opts.Namespace, args[0], output, timeout)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "file to write, or - for stdout")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long to wait for the export to become Ready")
	return cmd
}

func newExportDeleteCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:     "delete NAME...",
		Aliases: []string{"rm"},
		Short:   "Delete MachineExports",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			for _, name := range args {
				if err := kc.DeleteMachineExport(cmd.Context(), opts.Namespace, name); err != nil {
					return err
				}
				okf("machineexport/%s deleted", name)
			}
			return nil
		},
	}
}

func newExportMachineCmd(opts *Options) *cobra.Command {
	var output, format, ttl, name string
	var keep bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "machine NAME -o FILE",
		Short: "Download a Stopped or Halted Machine's boot disk",
		Long: `Create a MachineExport, wait until the node holding the disk is serving it,
and download it through kairon-ui. The Machine must be Stopped or Halted; a
Stopped Machine needs a PersistentVolume boot disk, because FluxVM removes
its own disk with the VM.

Needs KAIRON_UI_URL (and KAIRON_UI_TOKEN when kairon-ui authenticates) and the
console relay on the nodes. If FILE already holds part of the disk, the
download resumes from where it stopped. The MachineExport is deleted
afterwards unless --keep is set.`,
		Example: `  $ kaironctl stop web-1
  $ kaironctl export machine web-1 -o web-1.qcow2 --format qcow2
  $ kaironctl export machine db-1 -o - --format raw | gzip > db-1.raw.gz`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			machine := args[0]
			if output == "" {
				return fmt.Errorf("-o FILE is required (use - for stdout)")
			}
			base := uiBaseURL()
			if base == "" {
				return fmt.Errorf("KAIRON_UI_URL is not set; the download goes through kairon-ui")
			}
			spec := model.MachineExportSpec{MachineName: machine, Format: format, TTL: ttl}
			if err := model.ValidateMachineExportSpec(spec); err != nil {
				return err
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			ns := opts.Namespace
			name, err = createExport(ctx, kc, ns, name, spec)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "waiting for the node to serve machineexport/%s\n", name)
			if err := fetchExport(ctx, kc, ns, name, output, timeout); err != nil {
				return err
			}
			if !keep {
				return kc.DeleteMachineExport(ctx, ns, name)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "file to write, or - for stdout")
	cmd.Flags().StringVar(&format, "format", "", "qcow2 or raw; empty keeps the disk's own format")
	cmd.Flags().StringVar(&ttl, "ttl", "", "how long the export stays downloadable (default 2h)")
	cmd.Flags().StringVar(&name, "name", "", "MachineExport name")
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the MachineExport in place after the download")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long to wait for the export to become Ready")
	return cmd
}

func waitExportReady(ctx context.Context, kc *kube.Client, ns, name string, timeout, every time.Duration) (model.MachineExport, error) {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		e, err := kc.GetMachineExport(ctx, ns, name)
		if err != nil {
			return e, err
		}
		switch e.Status.Phase {
		case "Ready":
			return e, nil
		case "Failed", "Expired":
			return e, fmt.Errorf("machineexport/%s %s: %s", name, e.Status.Phase, e.Status.Message)
		}
		if msg := e.Status.Phase + ": " + e.Status.Message; e.Status.Phase != "" && msg != last {
			fmt.Fprintln(os.Stderr, msg)
			last = msg
		}
		if time.Now().After(deadline) {
			return e, fmt.Errorf("machineexport/%s not Ready after %s (phase %q)", name, timeout, e.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return e, ctx.Err()
		case <-time.After(every):
		}
	}
}

// downloadExport writes the export to output, resuming a partial file.
func downloadExport(ctx context.Context, client *http.Client, url, token, output string, size int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	var w io.Writer = os.Stdout
	var offset int64
	if output != "-" {
		f, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}
		defer func() { _ = f.Close() }()
		if fi, err := f.Stat(); err == nil && fi.Size() > 0 && (size == 0 || fi.Size() < size) {
			offset = fi.Size()
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		} else if size > 0 && fi != nil && fi.Size() == size {
			return size, nil
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, err
		}
		w = f
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		if offset > 0 {
			if f, ok := w.(*os.File); ok {
				if err := f.Truncate(0); err != nil {
					return 0, err
				}
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return 0, err
				}
			}
			offset = 0
		}
	case http.StatusPartialContent:
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return 0, fmt.Errorf("download: HTTP %d: %s", resp.StatusCode, data)
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return offset + n, fmt.Errorf("download interrupted after %d bytes (rerun with the same --name to resume): %w", offset+n, err)
	}
	if f, ok := w.(*os.File); ok && output != "-" {
		if err := f.Truncate(offset + n); err != nil {
			return offset + n, err
		}
	}
	return offset + n, nil
}
