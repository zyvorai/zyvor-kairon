// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
)

// waitCond is one --for condition: phase=X, ip, agent, ssh[=PORT],
// port=PORT or delete.
type waitCond struct {
	Kind  string
	Phase string
	Port  int
}

func (c waitCond) String() string {
	switch c.Kind {
	case "phase":
		return "phase=" + c.Phase
	case "ssh":
		if c.Port != 22 {
			return fmt.Sprintf("ssh=%d", c.Port)
		}
		return "ssh"
	case "port":
		return fmt.Sprintf("port=%d", c.Port)
	}
	return c.Kind
}

func parseWaitFor(s string) (waitCond, error) {
	key, val, hasVal := strings.Cut(strings.TrimSpace(s), "=")
	port := func(def int) (int, error) {
		if !hasVal {
			return def, nil
		}
		p, err := strconv.Atoi(val)
		if err != nil || p < 1 || p > 65535 {
			return 0, fmt.Errorf("--for %s: port must be 1-65535", s)
		}
		return p, nil
	}
	switch strings.ToLower(key) {
	case "phase":
		if val == "" {
			return waitCond{}, fmt.Errorf("--for phase=PHASE, e.g. phase=Running")
		}
		return waitCond{Kind: "phase", Phase: val}, nil
	case "ip", "agent", "delete":
		if hasVal {
			return waitCond{}, fmt.Errorf("--for %s takes no value", key)
		}
		return waitCond{Kind: strings.ToLower(key)}, nil
	case "ssh":
		p, err := port(22)
		return waitCond{Kind: "ssh", Port: p}, err
	case "port":
		if !hasVal {
			return waitCond{}, fmt.Errorf("--for port=PORT")
		}
		p, err := port(0)
		return waitCond{Kind: "port", Port: p}, err
	}
	return waitCond{}, fmt.Errorf("--for %q: want phase=PHASE, ip, agent, ssh[=PORT], port=PORT or delete", s)
}

// errWaitAbort wraps a probe result that can never turn into success.
var errWaitAbort = errors.New("giving up")

// waitProbe checks the condition once. state describes what it saw for
// progress lines; an error wrapping errWaitAbort stops the wait.
type waitProbe func(ctx context.Context) (done bool, state string, err error)

// waitLoop polls probe every interval until it succeeds, aborts, or
// timeout passes, calling onState whenever the observed state changes.
func waitLoop(ctx context.Context, timeout, interval time.Duration, probe waitProbe, onState func(string)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		done, state, err := probe(ctx)
		if err != nil && errors.Is(err, errWaitAbort) {
			return err
		}
		if err != nil {
			state = err.Error()
		}
		if done {
			return nil
		}
		if state != last && onState != nil {
			onState(state)
		}
		last = state
		select {
		case <-ctx.Done():
			if last == "" {
				last = "no state"
			}
			return fmt.Errorf("timed out after %s (last: %s)", timeout, last)
		case <-time.After(interval):
		}
	}
}

// sshBanner reports whether r starts an SSH server greeting.
func sshBanner(r io.Reader) bool {
	line, err := bufio.NewReader(io.LimitReader(r, 256)).ReadString('\n')
	return (err == nil || err == io.EOF) && strings.HasPrefix(line, "SSH-")
}

func waitProbeFor(c waitCond, ns, name string) (waitProbe, error) {
	switch c.Kind {
	case "phase", "ip", "delete":
		kc, err := newKubeClient()
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (bool, string, error) {
			m, err := kc.GetMachine(ctx, ns, name)
			if kube.IsNotFound(err) {
				return c.Kind == "delete", "not found", nil
			}
			if err != nil {
				return false, "", err
			}
			phase := orDash(m.Status.Phase)
			switch c.Kind {
			case "delete":
				return false, "phase " + phase, nil
			case "ip":
				return m.Status.GuestIP != "", "phase " + phase + ", no guest IP yet", nil
			}
			if strings.EqualFold(m.Status.Phase, c.Phase) {
				return true, phase, nil
			}
			if m.Status.Phase == "Failed" {
				return false, "", fmt.Errorf("%w: machine/%s is Failed: %s", errWaitAbort, name, orDash(m.Status.Message))
			}
			return false, "phase " + phase, nil
		}, nil
	case "agent":
		return func(ctx context.Context) (bool, string, error) {
			_, _, err := guestScript(ctx, ns, name, "true", "exit 0")
			if err != nil {
				return false, "guest agent: " + firstLine(err.Error()), nil
			}
			return true, "", nil
		}, nil
	case "ssh", "port":
		return func(ctx context.Context) (bool, string, error) {
			dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			conn, err := dialPortForward(dctx, ns, name, c.Port)
			if err != nil {
				return false, firstLine(err.Error()), nil
			}
			defer func() { _ = conn.Close() }()
			if c.Kind == "port" {
				return true, "", nil
			}
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			if !sshBanner(conn) {
				return false, fmt.Sprintf("port %d open but no SSH banner yet", c.Port), nil
			}
			return true, "", nil
		}, nil
	}
	return nil, fmt.Errorf("unknown condition %q", c.Kind)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func newWaitCmd(opts *Options) *cobra.Command {
	var forFlag string
	var timeout, interval time.Duration
	cmd := &cobra.Command{
		Use:   "wait MACHINE --for CONDITION [--timeout 5m]",
		Short: "Block until a Machine reaches a condition (for scripts and CI)",
		Long: `Polls until the condition holds, then exits 0. Exits non-zero on timeout, or
right away when waiting for a phase and the Machine goes Failed.

  phase=PHASE   status.phase matches (case-insensitive), e.g. phase=Running
  ip            the Machine reports a guest IP
  agent         the guest agent answers an exec (needs KAIRON_UI_URL)
  ssh[=PORT]    an SSH server greets through the port-forward tunnel
                (default port 22; needs KAIRON_UI_URL)
  port=PORT     a TCP port in the guest accepts connections (KAIRON_UI_URL)
  delete        the Machine no longer exists`,
		Example: `  $ kaironctl create web-1 --image /img/u.qcow2 && kaironctl wait web-1 --for phase=Running
  $ kaironctl wait web-1 --for ssh --timeout 10m && kaironctl ssh ubuntu@web-1
  $ kaironctl delete machine web-1 && kaironctl wait web-1 --for delete`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			cond, err := parseWaitFor(forFlag)
			if err != nil {
				return err
			}
			name, ns := args[0], opts.Namespace
			probe, err := waitProbeFor(cond, ns, name)
			if err != nil {
				return err
			}
			start := time.Now()
			err = waitLoop(cmd.Context(), timeout, interval, probe, func(state string) {
				fmt.Fprintf(os.Stderr, "… %s/%s %s: %s (%s)\n", ns, name, cond, state, time.Since(start).Round(time.Second))
			})
			if err != nil {
				return fmt.Errorf("wait %s/%s --for %s: %w", ns, name, cond, err)
			}
			okf("%s/%s %s (%s)", ns, name, cond, time.Since(start).Round(time.Second))
			return nil
		},
	}
	cmd.Flags().StringVar(&forFlag, "for", "phase=Running", "condition to wait for")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "give up after this long")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "time between checks")
	return cmd
}

// dashboardMachineURL links the dashboard's inspector for one Machine.
func dashboardMachineURL(base, ns, name string) string {
	return strings.TrimRight(base, "/") + "/#/machines/" + url.PathEscape(ns) + "/" + url.PathEscape(name)
}

func newOpenCmd(opts *Options) *cobra.Command {
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "open MACHINE",
		Short: "Open the Machine in the kairon-ui dashboard",
		Long: `Opens the dashboard's inspector for MACHINE in the default browser, using
KAIRON_UI_URL. Without KAIRON_UI_URL, run 'kaironctl ui' to reach the dashboard
through the API server instead.`,
		Example:           "  $ kaironctl open web-1\n  $ kaironctl open web-1 --print",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			base := uiBaseURL()
			if base == "" {
				return fmt.Errorf("KAIRON_UI_URL is not set; set it to the dashboard address, or run 'kaironctl ui' to open the dashboard through the API server")
			}
			link := dashboardMachineURL(base, opts.Namespace, args[0])
			if printOnly {
				fmt.Println(link)
				return nil
			}
			if err := openBrowser(link); err != nil {
				fmt.Println(link)
				return fmt.Errorf("could not open a browser: %w", err)
			}
			okf("opened %s", link)
			return nil
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the URL instead of opening it")
	return cmd
}
