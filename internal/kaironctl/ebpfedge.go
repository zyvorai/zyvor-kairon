// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/ebpfedge"
	"github.com/zyvorai/kairon/internal/kaironctl/style"
)

func newNetworkEdgeCmds(opts *Options) []*cobra.Command {
	return []*cobra.Command{
		newNetworkIdentityCmd(opts),
		newNetworkCaptureCmd(opts),
	}
}

func newNetworkIdentityCmd(opts *Options) *cobra.Command {
	var machine string
	cmd := &cobra.Command{
		Use:   "identity",
		Short: "Print the stable eBPF identity for a Machine",
		Long: `Identity is FNV-1a of namespace and name. It does not change when
the guest IP changes, so MachineNetworkPolicy survives live migration.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				machine = args[0]
			}
			if machine == "" {
				return fmt.Errorf("machine name required")
			}
			ns := opts.Namespace
			if ns == "" {
				ns = "default"
			}
			id := ebpfedge.StableIdentity(ns, machine)
			style.Log(style.EmojiOK, "identity %d for %s/%s", id, ns, machine)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&machine, "machine", "", "Machine name")
	return cmd
}

func newNetworkCaptureCmd(opts *Options) *cobra.Command {
	var seconds int
	var filter, output string
	cmd := &cobra.Command{
		Use:   "capture MACHINE",
		Short: "Capture packets on a Machine's VM edge (max 30s)",
		Long: `Asks FluxVM to run a bounded packet capture (tcpdump) on the Machine's
dataplane interface. Needs KAIRON_UI_URL (and KAIRON_UI_TOKEN when kairon-ui
requires one); without it the session is only printed. With --output, waits
for the capture to finish and writes the pcap there. Seconds above 30 are
rejected.`,
		Example: `  kaironctl network capture web --seconds 10 --filter "udp port 53" --output dns.pcap`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := opts.Namespace
			if ns == "" {
				ns = "default"
			}
			session, err := ebpfedge.NewCapture(ns, args[0], filter, seconds, time.Now().UTC())
			if err != nil {
				return err
			}
			style.Log(style.EmojiOK, "capture %s/%s for %ds token=%s", session.Namespace, session.Machine, session.Seconds, session.Token)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "expires %s\n", session.ExpiresAt.Format(time.RFC3339))
			if session.Filter != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "filter %s\n", session.Filter)
			}
			posted, err := postCapture(cmd.Context(), ns, args[0], session)
			if err != nil {
				return err
			}
			if output == "" {
				return nil
			}
			if !posted {
				return fmt.Errorf("--output needs KAIRON_UI_URL")
			}
			n, err := downloadCapture(cmd.Context(), ns, args[0], session, output)
			if err != nil {
				return err
			}
			style.Log(style.EmojiOK, "wrote %d bytes to %s", n, output)
			return nil
		},
	}
	cmd.Flags().IntVar(&seconds, "seconds", 15, "capture length, 1-30")
	cmd.Flags().StringVar(&filter, "filter", "", "optional tcpdump filter expression")
	cmd.Flags().StringVarP(&output, "output", "o", "", "wait for the capture and write the pcap to this file")
	return cmd
}

func uiRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	base := uiBaseURL()
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token := uiToken(false); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return uiDo(req)
}

// postCapture reports whether the session was sent; without KAIRON_UI_URL
// it is only printed.
func postCapture(ctx context.Context, namespace, machine string, session ebpfedge.CaptureSession) (bool, error) {
	if strings.TrimSpace(os.Getenv("KAIRON_UI_URL")) == "" {
		return false, nil
	}
	body, err := json.Marshal(session)
	if err != nil {
		return false, err
	}
	resp, err := uiRequest(ctx, http.MethodPost, fmt.Sprintf("/api/v1/machines/%s/%s/network-capture", namespace, machine), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("capture post: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	style.Log(style.EmojiOK, "capture started")
	return true, nil
}

// downloadCapture waits out the capture, then polls until FluxVM stops
// answering 409 (still running) and writes the pcap.
func downloadCapture(ctx context.Context, namespace, machine string, session ebpfedge.CaptureSession, output string) (int64, error) {
	path := fmt.Sprintf("/api/v1/machines/%s/%s/network-capture/%s", namespace, machine, session.Token)
	wait := time.Duration(session.Seconds) * time.Second
	deadline := time.Now().Add(wait + 30*time.Second)
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(wait):
		}
		resp, err := uiRequest(ctx, http.MethodGet, path, nil)
		if err != nil {
			return 0, err
		}
		switch resp.StatusCode {
		case http.StatusOK:
			defer func() { _ = resp.Body.Close() }()
			f, err := os.Create(output)
			if err != nil {
				return 0, err
			}
			n, err := io.Copy(f, resp.Body)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			return n, err
		case http.StatusConflict:
			_ = resp.Body.Close()
			if time.Now().After(deadline) {
				return 0, fmt.Errorf("capture %s is still running after %s", session.Token, wait+30*time.Second)
			}
			wait = time.Second
		default:
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return 0, fmt.Errorf("capture download: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		}
	}
}
