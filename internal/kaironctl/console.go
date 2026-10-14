// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/model"
)

// consoleEscape is Ctrl+], the same detach key as virtctl console and telnet.
const consoleEscape byte = 0x1d

// consoleTicket asks kairon-ui for a single-use console ticket of kind
// "text" or "vnc".
func consoleTicket(ctx context.Context, namespace, machine, kind string) (string, error) {
	raw, err := uiRaw(ctx, http.MethodPost, machinePath(namespace, machine, "console/ticket")+"?kind="+url.QueryEscape(kind), nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode console ticket: %w", err)
	}
	if out.Ticket == "" {
		return "", fmt.Errorf("kairon-ui returned no console ticket")
	}
	return out.Ticket, nil
}

// dialConsole opens a console WebSocket of kind "text" or "vnc". cols and
// rows size the text console; zero leaves FluxVM's 80x24 default.
func dialConsole(ctx context.Context, namespace, machine, kind string, cols, rows int) (net.Conn, error) {
	ticket, err := consoleTicket(ctx, namespace, machine, kind)
	if err != nil {
		return nil, fmt.Errorf("console %s/%s: %w", namespace, machine, err)
	}
	q := url.Values{"ticket": {ticket}}
	if cols > 0 && rows > 0 {
		q.Set("cols", strconv.Itoa(cols))
		q.Set("rows", strconv.Itoa(rows))
	}
	conn, err := dialUIWebSocket(ctx, machinePath(namespace, machine, "console"), q, false)
	if err != nil {
		return nil, fmt.Errorf("console %s/%s: %w", namespace, machine, err)
	}
	return websocket.NetConn(context.Background(), conn, websocket.MessageBinary), nil
}

// consoleInfo is what the console header shows about the Machine.
type consoleInfo struct {
	Machine   string
	Namespace string
	Node      string
	Phase     string
	Backend   string
	IP        string
	CPU       string
	Memory    string
	Log       string
}

func consoleInfoFor(m model.Machine, namespace, name string) consoleInfo {
	backend := m.Spec.Runtime.Backend
	if backend == "" {
		backend = "auto"
	}
	return consoleInfo{
		Machine:   name,
		Namespace: namespace,
		Node:      m.Status.NodeName,
		Phase:     m.Status.Phase,
		Backend:   backend,
		IP:        m.Status.GuestIP,
		CPU:       m.Spec.Resources.CPU,
		Memory:    m.Spec.Resources.Memory,
	}
}

type frameSeg struct{ text, color string }

func frameRow(w io.Writer, inner int, segs ...frameSeg) string {
	var b strings.Builder
	n := 0
	for _, s := range segs {
		n += utf8.RuneCountInString(s.text)
		b.WriteString(style.Paint(w, s.color, s.text))
	}
	if n < inner {
		b.WriteString(strings.Repeat(" ", inner-n))
	}
	border := style.Paint(w, style.DarkGrey, "│")
	return border + b.String() + border
}

func frameRule(w io.Writer, inner int, left, right string) string {
	return style.Paint(w, style.DarkGrey, left+strings.Repeat("─", inner)+right)
}

func fit(s string, n int) string {
	if s == "" {
		s = "-"
	}
	if utf8.RuneCountInString(s) <= n {
		return s + strings.Repeat(" ", n-utf8.RuneCountInString(s))
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func phaseColor(phase string) string {
	switch phase {
	case "Running":
		return style.BrightGrn
	case "Failed", "Error", "NeedsRecovery":
		return style.BrightRed
	case "", "-":
		return style.Grey
	case "Paused", "Halted", "Hibernated", "Stopped":
		return style.BrightCyan
	default:
		return style.BrightYel
	}
}

// renderConsoleFrame draws a macOS-window style header: traffic-light dots,
// the Machine as the window title, and its placement underneath.
func renderConsoleFrame(w io.Writer, info consoleInfo, width int) string {
	width = max(48, min(width, 76))
	inner := width - 2
	title := info.Machine + " — " + info.Namespace
	const dots = 7 // " ● ● ● "
	pad := max(1, (inner-utf8.RuneCountInString(title))/2-dots)
	if dots+pad+utf8.RuneCountInString(title) > inner {
		title = strings.TrimRight(fit(title, inner-dots-pad), " ")
	}
	col := (inner - 2) / 2
	label := 9
	val := col - label
	field := func(name, value, color string) []frameSeg {
		return []frameSeg{{fit(name, label), style.Grey}, {fit(value, val), color}}
	}
	row := func(a, b []frameSeg) string {
		segs := append([]frameSeg{{"  ", ""}}, a...)
		return frameRow(w, inner, append(segs, b...)...)
	}
	resources := strings.TrimSpace(strings.Join([]string{cpuLabel(info.CPU), info.Memory}, " "))
	lines := []string{
		frameRule(w, inner, "╭", "╮"),
		frameRow(w, inner,
			frameSeg{" ", ""}, frameSeg{"●", style.BrightRed}, frameSeg{" ", ""},
			frameSeg{"●", style.BrightYel}, frameSeg{" ", ""}, frameSeg{"●", style.BrightGrn},
			frameSeg{" " + strings.Repeat(" ", pad), ""}, frameSeg{title, style.White}),
		frameRule(w, inner, "├", "┤"),
		row(field("phase", info.Phase, phaseColor(info.Phase)), field("node", info.Node, style.SoftCyan)),
		row(field("backend", info.Backend, style.SoftYellow), field("ip", info.IP, style.SoftGreen)),
		row(field("size", resources, style.White), field("console", "text", style.SoftCyan)),
		frameRule(w, inner, "╰", "╯"),
	}
	hint := "  " + style.Paint(w, style.BrightCyan, "Ctrl+]") + style.Paint(w, style.Grey, " to detach")
	if info.Log != "" {
		hint += style.Paint(w, style.DarkGrey, "  ·  ") + style.Paint(w, style.Grey, "logging to ") + style.Paint(w, style.White, info.Log)
	}
	return strings.Join(append(lines, hint), "\n") + "\n"
}

func cpuLabel(cpu string) string {
	if cpu == "" {
		return ""
	}
	return cpu + " vCPU"
}

// consoleEnd is why a console session ended.
type consoleEnd int

const (
	endRemote consoleEnd = iota
	endDetached
	endInput
	endCancelled
)

// runConsoleSession relays in to conn and conn to out until the guest side
// closes, the user types the escape byte, in ends, or ctx is cancelled.
func runConsoleSession(ctx context.Context, conn io.ReadWriter, in io.Reader, out io.Writer, escape byte) consoleEnd {
	remote := make(chan struct{})
	local := make(chan consoleEnd, 1)
	go func() {
		_, _ = io.Copy(out, conn)
		close(remote)
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if i := indexByte(chunk, escape); i >= 0 {
					if i > 0 {
						_, _ = conn.Write(chunk[:i])
					}
					local <- endDetached
					return
				}
				if _, werr := conn.Write(chunk); werr != nil {
					local <- endRemote
					return
				}
			}
			if err != nil {
				local <- endInput
				return
			}
		}
	}()
	select {
	case <-remote:
		return endRemote
	case why := <-local:
		return why
	case <-ctx.Done():
		return endCancelled
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func newConsoleCmd(opts *Options) *cobra.Command {
	var logPath string
	var noFrame bool
	cmd := &cobra.Command{
		Use:   "console MACHINE",
		Short: "Open an interactive text console on a Machine",
		Long: `Attach your terminal to a running Machine's text console, through kairon-ui and
the node relay. Like virtctl console; press Ctrl+] to detach.

The text console is FluxVM's guest agent console, so the Machine needs
spec.guestAgent.console. Needs KAIRON_UI_URL (and KAIRON_UI_TOKEN when
kairon-ui authenticates). Access follows the console rules:
kairon.zyvor.dev/console-allowed-users and, when enabled, RBAC on
machines/console.

--log appends everything the console prints to FILE.`,
		Example: `  $ kaironctl console web-1
  $ kaironctl console -n prod db-1 --log db-1-console.log`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, ns := args[0], opts.Namespace
			info := consoleInfo{Machine: name, Namespace: ns, Log: logPath}
			if kc, err := newKubeClient(); err == nil {
				if m, err := kc.GetMachine(cmd.Context(), ns, name); err == nil {
					if m.Status.Phase != "Running" {
						return fmt.Errorf("machine/%s is %s; the console needs a Running Machine", name, orDash(m.Status.Phase))
					}
					if !m.Spec.GuestAgent.Console {
						return fmt.Errorf("machine/%s has no text console: set spec.guestAgent.console (or use kaironctl vnc for the display)", name)
					}
					info = consoleInfoFor(m, ns, name)
					info.Log = logPath
				}
			}
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
			defer cancel()

			stdinFd, stdoutFd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
			tty := term.IsTerminal(stdinFd)
			cols, rows := 0, 0
			width := 64
			if w, h, err := term.GetSize(stdoutFd); err == nil {
				cols, rows, width = w, h, w
			}
			conn, err := dialConsole(ctx, ns, name, "text", cols, rows)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()

			var out io.Writer = os.Stdout
			if logPath != "" {
				f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				_, _ = fmt.Fprintf(f, "\n# kaironctl console %s/%s %s\n", ns, name, time.Now().UTC().Format(time.RFC3339))
				out = io.MultiWriter(os.Stdout, f)
			}
			if !noFrame {
				fmt.Fprint(os.Stdout, renderConsoleFrame(os.Stdout, info, width))
			}
			restore := func() {}
			if tty {
				state, err := term.MakeRaw(stdinFd)
				if err != nil {
					return fmt.Errorf("raw terminal: %w", err)
				}
				restore = func() { _ = term.Restore(stdinFd, state) }
				defer restore()
			}
			start := time.Now()
			why := runConsoleSession(ctx, conn, os.Stdin, out, consoleEscape)
			restore()
			fmt.Fprint(os.Stdout, consoleGoodbye(os.Stdout, why, ns, name, time.Since(start)))
			return nil
		},
	}
	cmd.Flags().StringVar(&logPath, "log", "", "append the console output to FILE")
	cmd.Flags().BoolVar(&noFrame, "no-frame", false, "skip the header box")
	return cmd
}

func consoleGoodbye(w io.Writer, why consoleEnd, ns, name string, d time.Duration) string {
	verb, color := "Connection closed by the guest", style.SoftYellow
	switch why {
	case endDetached:
		verb, color = "Detached from", style.SoftGreen
	case endInput:
		verb, color = "Input ended; left", style.Grey
	case endCancelled:
		verb, color = "Interrupted; left", style.SoftRed
	}
	return "\r\n" + style.Paint(w, color, "● ") + style.Paint(w, style.White, verb+" "+ns+"/"+name) +
		style.Paint(w, style.Grey, " after "+d.Round(time.Second).String()) + "\r\n"
}

// vncViewer picks the command that opens a VNC viewer on host:port: the
// built-in Screen Sharing app on macOS, else remote-viewer or a TigerVNC/
// TightVNC-style vncviewer from PATH. ok is false when none is available.
func vncViewer(goos, host string, port int, lookPath func(string) (string, error)) (name string, args []string, ok bool) {
	hp := net.JoinHostPort(host, strconv.Itoa(port))
	if goos == "darwin" {
		return "open", []string{"vnc://" + hp}, true
	}
	if p, err := lookPath("remote-viewer"); err == nil {
		return p, []string{"vnc://" + hp}, true
	}
	if p, err := lookPath("vncviewer"); err == nil {
		return p, []string{host + "::" + strconv.Itoa(port)}, true
	}
	return "", nil, false
}

func newVNCCmd(opts *Options) *cobra.Command {
	var proxyOnly bool
	var address string
	var port int
	cmd := &cobra.Command{
		Use:   "vnc MACHINE",
		Short: "Open a Machine's graphical display in a VNC viewer",
		Long: `Proxy a running Machine's VNC display (qemu backend) to a local port and open a
viewer on it, like virtctl vnc. On macOS that is the built-in Screen Sharing
app; elsewhere remote-viewer or vncviewer from PATH.

--proxy-only only starts the local proxy and prints its address, for any
other viewer. Each viewer connection gets its own single-use console ticket.
Needs KAIRON_UI_URL (and KAIRON_UI_TOKEN). If Screen Sharing refuses a display
without a password, use --proxy-only with another viewer.`,
		Example: `  $ kaironctl vnc win-1
  $ kaironctl vnc win-1 --proxy-only --port 5901`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, ns := args[0], opts.Namespace
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			ln, err := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(port)))
			if err != nil {
				return fmt.Errorf("listen: %w", err)
			}
			defer func() { _ = ln.Close() }()
			go func() {
				<-ctx.Done()
				_ = ln.Close()
			}()
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					go func() {
						defer func() { _ = c.Close() }()
						up, err := dialConsole(ctx, ns, name, "vnc", 0, 0)
						if err != nil {
							style.Warnf("%v", err)
							return
						}
						defer func() { _ = up.Close() }()
						pipe(c, up)
					}()
				}
			}()
			lport := ln.Addr().(*net.TCPAddr).Port
			host := address
			if ip := net.ParseIP(address); ip == nil || ip.IsUnspecified() {
				host = "127.0.0.1"
			}
			style.Log(style.EmojiOK, "VNC proxy for %s/%s on %s (Ctrl+C to stop)", ns, name, net.JoinHostPort(host, strconv.Itoa(lport)))
			if !proxyOnly {
				viewer, vargs, ok := vncViewer(runtime.GOOS, host, lport, exec.LookPath)
				if !ok {
					style.Warnf("no VNC viewer found (install remote-viewer or vncviewer); point one at %s", net.JoinHostPort(host, strconv.Itoa(lport)))
				} else {
					c := exec.CommandContext(ctx, viewer, vargs...)
					c.Stdout, c.Stderr = os.Stderr, os.Stderr
					if err := c.Start(); err != nil {
						return fmt.Errorf("start %s: %w", viewer, err)
					}
					if runtime.GOOS != "darwin" {
						if err := c.Wait(); err != nil && !errors.Is(ctx.Err(), context.Canceled) {
							style.Warnf("%s: %v", viewer, err)
						}
						return nil
					}
					_ = c.Wait()
				}
			}
			<-ctx.Done()
			return nil
		},
	}
	cmd.Flags().BoolVar(&proxyOnly, "proxy-only", false, "only run the local proxy; do not start a viewer")
	cmd.Flags().StringVar(&address, "address", "127.0.0.1", "local address to listen on")
	cmd.Flags().IntVar(&port, "port", 0, "local port (0 picks a free one)")
	return cmd
}
