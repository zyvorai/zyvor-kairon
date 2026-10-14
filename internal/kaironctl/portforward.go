// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
)

// dialPortForward opens one TCP stream to port in a Machine's guest through
// kairon-ui's port-forward route.
func dialPortForward(ctx context.Context, namespace, machine string, port int) (net.Conn, error) {
	path := machinePath(namespace, machine, "portforward")
	conn, err := dialUIWebSocket(ctx, path, url.Values{"port": {strconv.Itoa(port)}}, true)
	if err != nil {
		return nil, fmt.Errorf("port-forward %s/%s:%d: %w", namespace, machine, port, err)
	}
	return websocket.NetConn(context.Background(), conn, websocket.MessageBinary), nil
}

// dialUIWebSocket opens a WebSocket to a kairon-ui route. withToken sends
// KAIRON_UI_TOKEN; routes authorized by a single-use ticket in the query
// (console) do not need it.
func dialUIWebSocket(ctx context.Context, path string, query url.Values, withToken bool) (*websocket.Conn, error) {
	base := uiBaseURL()
	if base == "" {
		return nil, fmt.Errorf("set KAIRON_UI_URL (and KAIRON_UI_TOKEN, plus KAIRON_UI_CA for a self-signed dashboard certificate) to reach kairon-ui")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("KAIRON_UI_URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = query.Encode()
	client, err := uiHTTPClient()
	if err != nil {
		return nil, err
	}
	opts := &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{}}
	if token := uiToken(false); withToken && token != "" {
		opts.HTTPHeader.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.Dial(ctx, u.String(), opts)
	if resp != nil && resp.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if err != nil && len(body) > 0 {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(-1)
	return conn, nil
}

func pipe(a, b io.ReadWriter) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

// parsePortSpec parses [LOCAL:]REMOTE; LOCAL defaults to REMOTE, and 0
// picks a free port.
func parsePortSpec(spec string) (local, remote int, err error) {
	l, r, found := strings.Cut(spec, ":")
	if !found {
		r = l
	}
	remote, err = strconv.Atoi(r)
	if err != nil || remote < 1 || remote > 65535 {
		return 0, 0, fmt.Errorf("remote port %q: must be 1-65535", r)
	}
	if !found {
		return remote, remote, nil
	}
	local, err = strconv.Atoi(l)
	if err != nil || local < 0 || local > 65535 {
		return 0, 0, fmt.Errorf("local port %q: must be 0-65535", l)
	}
	return local, remote, nil
}

func newPortForwardCmd(opts *Options) *cobra.Command {
	var address string
	var useStdio bool
	cmd := &cobra.Command{
		Use:   "port-forward MACHINE [LOCAL:]REMOTE",
		Short: "Forward a local port to a TCP port in a Machine's guest",
		Long: `Forward connections on a local port to a TCP port inside a Machine, through
kairon-ui and the node relay. Like virtctl port-forward, no Service, Ingress or
route to the guest network is needed.

Needs KAIRON_UI_URL (and KAIRON_UI_TOKEN when kairon-ui authenticates) and the
console relay on the nodes. A Machine on user-mode networking is reached
through its spec.network.forwards, so the guest port must be forwarded there.
Access follows the console rules: kairon.zyvor.dev/console-allowed-users
restricts who may connect.

--stdio relays a single connection over stdin/stdout, for ssh's ProxyCommand.`,
		Example: `  $ kaironctl port-forward web-1 8080:80
  $ kaironctl port-forward db-1 5432 --address 127.0.0.1
  $ ssh -o ProxyCommand='kaironctl port-forward --stdio web-1 22' ubuntu@web-1`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			machine := args[0]
			local, remote, err := parsePortSpec(args[1])
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if useStdio {
				conn, err := dialPortForward(ctx, opts.Namespace, machine, remote)
				if err != nil {
					return err
				}
				defer func() { _ = conn.Close() }()
				pipe(conn, stdio{})
				return nil
			}
			ln, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(local)))
			if err != nil {
				return fmt.Errorf("listen: %w", err)
			}
			defer func() { _ = ln.Close() }()
			style.Log(style.EmojiOK, "Forwarding %s -> %s/%s:%d (Ctrl+C to stop)", ln.Addr(), opts.Namespace, machine, remote)
			go func() {
				<-ctx.Done()
				_ = ln.Close()
			}()
			for {
				c, err := ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
				go func() {
					defer func() { _ = c.Close() }()
					up, err := dialPortForward(ctx, opts.Namespace, machine, remote)
					if err != nil {
						style.Warnf("%v", err)
						return
					}
					defer func() { _ = up.Close() }()
					pipe(c, up)
				}()
			}
		},
	}
	cmd.Flags().StringVar(&address, "address", "127.0.0.1", "local address to listen on")
	cmd.Flags().BoolVar(&useStdio, "stdio", false, "relay one connection over stdin/stdout instead of listening")
	return cmd
}

// proxyCommand is the ssh ProxyCommand that tunnels to port on machine.
// %h stays for ssh to expand to the destination host (the Machine name).
func proxyCommand(namespace string, port int) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s -n %s port-forward --stdio %%h %d", shellQuote(self), shellQuote(namespace), port), nil
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;#~%") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func hostKeyAlias(namespace, machine string) string {
	return machine + "." + namespace + ".kairon"
}

// sshArgs builds the ssh command line: ProxyCommand through kaironctl and a
// per-namespace host key alias, so two Machines with the same name in
// different namespaces keep separate known_hosts entries.
func sshArgs(namespace, target string, port int, extra []string) ([]string, error) {
	pc, err := proxyCommand(namespace, port)
	if err != nil {
		return nil, err
	}
	machine := target
	if _, host, ok := strings.Cut(target, "@"); ok {
		machine = host
	}
	args := []string{"-o", "ProxyCommand=" + pc, "-o", "HostKeyAlias=" + hostKeyAlias(namespace, machine)}
	args = append(args, extra...)
	return append(args, target), nil
}

var scpRemoteRE = regexp.MustCompile(`^(?:[^@/:]+@)?([a-z0-9]([-a-z0-9.]*[a-z0-9])?):`)

// scpArgs rewrites nothing in the operands; it only adds the ProxyCommand,
// and a host key alias when every remote operand names the same Machine.
func scpArgs(namespace string, port int, operands []string) ([]string, error) {
	pc, err := proxyCommand(namespace, port)
	if err != nil {
		return nil, err
	}
	machines := map[string]bool{}
	for _, op := range operands {
		if m := scpRemoteRE.FindStringSubmatch(op); m != nil {
			machines[m[1]] = true
		}
	}
	if len(machines) == 0 {
		return nil, fmt.Errorf("no remote operand: write one as [USER@]MACHINE:PATH")
	}
	args := []string{"-o", "ProxyCommand=" + pc}
	if len(machines) == 1 {
		for m := range machines {
			args = append(args, "-o", "HostKeyAlias="+hostKeyAlias(namespace, m))
		}
	}
	return append(args, operands...), nil
}

func runTool(name string, args []string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%s not found in PATH", name)
	}
	c := exec.Command(path, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

func newSSHCmd(opts *Options) *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "ssh [USER@]MACHINE [-- SSH-ARGS...]",
		Short: "SSH into a Machine through kairon-ui",
		Long: `Run the system ssh client against a Machine, tunnelled by kaironctl port-forward
--stdio. Arguments after -- go to ssh unchanged (before the destination).
Same requirements as kaironctl port-forward.`,
		Example: `  $ kaironctl ssh ubuntu@web-1
  $ kaironctl ssh -n prod root@db-1 -- -i ~/.ssh/lab -L 5432:localhost:5432`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sargs, err := sshArgs(opts.Namespace, args[0], port, args[1:])
			if err != nil {
				return err
			}
			return runTool("ssh", sargs)
		},
	}
	cmd.Flags().IntVarP(&port, "port", "p", 22, "SSH port in the guest")
	return cmd
}

func newSCPCmd(opts *Options) *cobra.Command {
	var port int
	var recursive bool
	cmd := &cobra.Command{
		Use:   "scp SRC... DST",
		Short: "Copy files to or from a Machine through kairon-ui",
		Long: `Run the system scp client with a ProxyCommand through kaironctl port-forward
--stdio. Write remote operands as [USER@]MACHINE:PATH.`,
		Example: `  $ kaironctl scp ./app.tar ubuntu@web-1:/tmp/
  $ kaironctl scp -r ubuntu@web-1:/var/log/app ./logs`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sargs, err := scpArgs(opts.Namespace, port, args)
			if err != nil {
				return err
			}
			if recursive {
				sargs = append([]string{"-r"}, sargs...)
			}
			return runTool("scp", sargs)
		},
	}
	cmd.Flags().IntVarP(&port, "port", "P", 22, "SSH port in the guest")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "copy directories recursively")
	return cmd
}
