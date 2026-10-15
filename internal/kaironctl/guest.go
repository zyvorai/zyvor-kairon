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
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// uiRaw calls a kairon-ui route and returns the response body, which may
// be empty (DELETE).
func uiRaw(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	if uiBaseURL() == "" {
		return nil, fmt.Errorf("KAIRON_UI_URL is not set; this command goes through kairon-ui")
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	resp, err := uiRequest(ctx, method, path, r)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, uiError(resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func printRaw(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		fmt.Println("ok")
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		_, err = os.Stdout.Write(raw)
		return err
	}
	fmt.Println(string(jsonIndent(v)))
	return nil
}

func machinePath(ns, name, suffix string) string {
	return "/api/v1/machines/" + url.PathEscape(ns) + "/" + url.PathEscape(name) + "/" + suffix
}

func nodePath(node, suffix string) string {
	p := "/api/v1/nodes/" + url.PathEscape(node)
	if suffix != "" {
		p += "/" + suffix
	}
	return p
}

// uiRun is the RunE body shared by the thin route wrappers below.
func uiRun(method string, path func(args []string) (string, error), body func(args []string) (any, error)) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		p, err := path(args)
		if err != nil {
			return err
		}
		var b any
		if body != nil {
			if b, err = body(args); err != nil {
				return err
			}
		}
		raw, err := uiRaw(cmd.Context(), method, p, b)
		if err != nil {
			return err
		}
		return printRaw(raw)
	}
}

type execResult struct {
	ExitCode int64  `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func newGuestCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guest",
		Short: "Run guest and runtime operations on a Machine through kairon-ui",
		Long: `Operations that act inside or on a running Machine's VM, through kairon-ui and
the node relay. Needs KAIRON_UI_URL (and KAIRON_UI_TOKEN). The server applies
the same gates as the dashboard: some need an admin account, a guest agent, or a
Running Machine.`,
	}
	mp := func(suffix string) func([]string) (string, error) {
		return func(args []string) (string, error) { return machinePath(opts.Namespace, args[0], suffix), nil }
	}

	var timeout uint64
	var agent bool
	execCmd := &cobra.Command{
		Use:   "exec MACHINE -- COMMAND [ARGS...]",
		Short: "Run a command in the guest and print its output",
		Long: `Runs through FluxVM's sandbox exec channel by default (spec.guestAgent).
--agent uses the fluxvm guest agent command channel instead, which runs the
command line through the guest's shell. Exits with the guest command's exit code.`,
		Example: `  $ kaironctl guest exec web-1 -- uname -a
  $ kaironctl guest exec web-1 --agent -- 'df -h /'`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var t *uint64
			if timeout > 0 {
				t = &timeout
			}
			var body any = map[string]any{"path": args[1], "args": args[2:], "timeoutSeconds": t}
			suffix := "exec"
			if agent {
				suffix = "agent-exec"
				body = map[string]any{"command": strings.Join(args[1:], " "), "timeoutSeconds": t}
			}
			raw, err := uiRaw(cmd.Context(), http.MethodPost, machinePath(opts.Namespace, args[0], suffix), body)
			if err != nil {
				return err
			}
			var res execResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			_, _ = fmt.Fprint(os.Stdout, res.Stdout)
			fmt.Fprint(os.Stderr, res.Stderr)
			if res.ExitCode != 0 {
				os.Exit(int(res.ExitCode))
			}
			return nil
		},
	}
	execCmd.Flags().Uint64Var(&timeout, "timeout", 30, "seconds before the guest command is killed")
	execCmd.Flags().BoolVar(&agent, "agent", false, "use the fluxvm guest agent command channel")

	var protocol string
	fwOpen := &cobra.Command{
		Use:   "open MACHINE RULE PORT",
		Short: "Open a port in the guest firewall",
		Args:  cobra.ExactArgs(3),
		RunE: uiRun(http.MethodPost, mp("qga/firewall/open"), func(args []string) (any, error) {
			port, err := strconv.ParseUint(args[2], 10, 16)
			if err != nil || port == 0 {
				return nil, fmt.Errorf("port %q: must be 1-65535", args[2])
			}
			return map[string]any{"name": args[1], "port": port, "protocol": protocol}, nil
		}),
	}
	fwOpen.Flags().StringVar(&protocol, "protocol", "tcp", "tcp or udp")
	fw := &cobra.Command{Use: "firewall", Short: "Open or close guest firewall rules (guest agent)"}
	fw.AddCommand(fwOpen, &cobra.Command{
		Use:   "close MACHINE RULE",
		Short: "Remove a guest firewall rule",
		Args:  cobra.ExactArgs(2),
		RunE: uiRun(http.MethodPost, mp("qga/firewall/close"), func(args []string) (any, error) {
			return map[string]any{"name": args[1]}, nil
		}),
	})

	tagBody := func(args []string) (any, error) { return map[string]any{"tag": args[1]}, nil }
	simple := func(use, short, method, suffix string) *cobra.Command {
		return &cobra.Command{Use: use + " MACHINE", Short: short, Args: cobra.ExactArgs(1), RunE: uiRun(method, mp(suffix), nil)}
	}
	cmd.AddCommand(
		execCmd,
		fw,
		simple("fsfreeze-status", "Show the guest filesystem freeze state (guest agent)", http.MethodGet, "qga/fsfreeze-status"),
		simple("freeze", "Freeze the VM's cgroup (admin)", http.MethodPost, "freeze"),
		simple("thaw", "Thaw a frozen VM (admin)", http.MethodPost, "thaw"),
		simple("frozen", "Show whether the VM is frozen", http.MethodGet, "frozen"),
		simple("pressure", "Show the VM's CPU, memory and IO pressure (PSI)", http.MethodGet, "pressure"),
		simple("cpuset", "Show the host CPUs the VM runs on", http.MethodGet, "cpuset"),
		&cobra.Command{
			Use:   "vm-snapshot MACHINE TAG",
			Short: "Save the VM's full state (memory and devices) under TAG (admin)",
			Args:  cobra.ExactArgs(2),
			RunE:  uiRun(http.MethodPost, mp("vm-snapshot"), tagBody),
		},
		&cobra.Command{
			Use:   "vm-restore MACHINE TAG",
			Short: "Restore the VM state saved under TAG; restarts the VM in place (admin)",
			Args:  cobra.ExactArgs(2),
			RunE:  uiRun(http.MethodPost, mp("vm-restore-snapshot"), tagBody),
		},
	)
	cmd.AddCommand(newGuestInfoCmds(opts)...)
	return cmd
}

func newRuntimeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runtime",
		Short: "Manage a node's FluxVM runtime: sandboxes, templates, image catalog, warm pools",
		Long: `Per-node FluxVM administration through kairon-ui (the dashboard's node tools).
Needs KAIRON_UI_URL (and KAIRON_UI_TOKEN). Most writes need an admin account.`,
	}
	np := func(suffix string, extra ...int) func([]string) (string, error) {
		return func(args []string) (string, error) {
			s := suffix
			for _, i := range extra {
				s += "/" + url.PathEscape(args[i])
			}
			return nodePath(args[0], s), nil
		}
	}
	list := func(use, short, suffix string) *cobra.Command {
		return &cobra.Command{Use: use + " NODE", Short: short, Args: cobra.ExactArgs(1), RunE: uiRun(http.MethodGet, np(suffix), nil)}
	}

	templates := list("templates", "List or build sandbox templates", "templates")
	templates.AddCommand(&cobra.Command{
		Use:   "build NODE NAME IMAGE-REF",
		Short: "Build a sandbox template from an OCI image",
		Args:  cobra.ExactArgs(3),
		RunE: uiRun(http.MethodPost, np("templates"), func(args []string) (any, error) {
			return map[string]any{"name": args[1], "imageRef": args[2]}, nil
		}),
	})

	var format string
	catAdd := &cobra.Command{
		Use:   "add NODE NAME SOURCE",
		Short: "Add an image to the catalog from a URL or node-local path",
		Args:  cobra.ExactArgs(3),
		RunE: uiRun(http.MethodPost, np("catalog"), func(args []string) (any, error) {
			return map[string]any{"name": args[1], "source": args[2], "format": format}, nil
		}),
	}
	catAdd.Flags().StringVar(&format, "format", "", "qcow2 or raw; empty detects it")
	catalog := list("catalog", "List and manage the node's image catalog", "catalog")
	catalog.AddCommand(
		catAdd,
		&cobra.Command{Use: "delete NODE NAME", Short: "Remove a catalog entry", Args: cobra.ExactArgs(2), RunE: uiRun(http.MethodDelete, np("catalog", 1), nil)},
		&cobra.Command{Use: "rename NODE NAME NEW-NAME", Short: "Rename a catalog entry", Args: cobra.ExactArgs(3), RunE: uiRun(http.MethodPost, func(a []string) (string, error) {
			return nodePath(a[0], "catalog/"+url.PathEscape(a[1])+"/rename"), nil
		}, func(a []string) (any, error) { return map[string]any{"newName": a[2]}, nil })},
		&cobra.Command{Use: "clone NODE NAME TARGET", Short: "Copy a catalog entry", Args: cobra.ExactArgs(3), RunE: uiRun(http.MethodPost, func(a []string) (string, error) { return nodePath(a[0], "catalog/"+url.PathEscape(a[1])+"/clone"), nil }, func(a []string) (any, error) { return map[string]any{"targetName": a[2]}, nil })},
		&cobra.Command{Use: "export NODE NAME PATH", Short: "Write a catalog image to a node-local path", Args: cobra.ExactArgs(3), RunE: uiRun(http.MethodPost, func(a []string) (string, error) {
			return nodePath(a[0], "catalog/"+url.PathEscape(a[1])+"/export"), nil
		}, func(a []string) (any, error) { return map[string]any{"path": a[2]}, nil })},
		&cobra.Command{Use: "read-only NODE NAME true|false", Short: "Protect or unprotect a catalog entry", Args: cobra.ExactArgs(3), RunE: uiRun(http.MethodPost, func(a []string) (string, error) {
			return nodePath(a[0], "catalog/"+url.PathEscape(a[1])+"/read-only"), nil
		}, func(a []string) (any, error) {
			ro, err := strconv.ParseBool(a[2])
			return map[string]any{"readOnly": ro}, err
		})},
		&cobra.Command{Use: "clean NODE", Short: "Remove catalog downloads nothing references", Args: cobra.ExactArgs(1), RunE: uiRun(http.MethodPost, np("catalog/clean"), func([]string) (any, error) { return map[string]any{}, nil })},
	)

	var size int
	var templateFile, claimName string
	var ttl int64
	poolCreate := &cobra.Command{
		Use:   "create NODE NAME --size N --template FILE.json",
		Short: "Create a warm pool of pre-booted sandboxes",
		Args:  cobra.ExactArgs(2),
		RunE: uiRun(http.MethodPost, np("pools"), func(args []string) (any, error) {
			if templateFile == "" {
				return nil, fmt.Errorf("--template FILE.json is required (a FluxVM create request)")
			}
			b, err := os.ReadFile(templateFile)
			if err != nil {
				return nil, err
			}
			var tmpl map[string]any
			if err := json.Unmarshal(b, &tmpl); err != nil {
				return nil, fmt.Errorf("--template: %w", err)
			}
			return map[string]any{"name": args[1], "size": size, "template": tmpl}, nil
		}),
	}
	poolCreate.Flags().IntVar(&size, "size", 1, "number of warm VMs")
	poolCreate.Flags().StringVar(&templateFile, "template", "", "JSON file with the FluxVM create request each VM uses")
	poolClaim := &cobra.Command{
		Use:   "claim NODE POOL",
		Short: "Take a warm VM from a pool",
		Args:  cobra.ExactArgs(2),
		RunE: uiRun(http.MethodPost, func(a []string) (string, error) { return nodePath(a[0], "pools/"+url.PathEscape(a[1])+"/claim"), nil }, func([]string) (any, error) {
			body := map[string]any{}
			if claimName != "" {
				body["name"] = claimName
			}
			if ttl > 0 {
				body["ttlSeconds"] = ttl
			}
			return body, nil
		}),
	}
	poolClaim.Flags().StringVar(&claimName, "name", "", "name for the claimed VM")
	poolClaim.Flags().Int64Var(&ttl, "ttl", 0, "seconds before the claimed VM is reclaimed")
	pools := list("pools", "List and manage warm pools", "pools")
	pools.AddCommand(
		poolCreate,
		poolClaim,
		&cobra.Command{Use: "get NODE NAME", Short: "Show a warm pool", Args: cobra.ExactArgs(2), RunE: uiRun(http.MethodGet, np("pools", 1), nil)},
		&cobra.Command{Use: "delete NODE NAME", Short: "Delete a warm pool and all its VMs, claimed ones included", Args: cobra.ExactArgs(2), RunE: uiRun(http.MethodDelete, np("pools", 1), nil)},
	)

	cmd.AddCommand(
		list("capabilities", "Show what the node's FluxVM supports", "capabilities"),
		list("sandboxes", "List sandboxes running on the node", "sandboxes"),
		templates,
		catalog,
		pools,
		&cobra.Command{
			Use:   "egress-check NODE HOST",
			Short: "Ask whether a sandbox may reach HOST under the node's egress policy",
			Args:  cobra.ExactArgs(2),
			RunE: uiRun(http.MethodPost, np("egress-check"), func(args []string) (any, error) {
				return map[string]any{"host": args[1]}, nil
			}),
		},
	)
	return cmd
}
