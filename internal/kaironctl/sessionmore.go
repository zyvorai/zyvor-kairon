// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
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
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// approvalRequired is FluxVM's 428 answer for a command that matched a
// policy.process.ask rule: it runs once an admin approves it and the caller
// repeats the request with the approval id.
type approvalRequired struct {
	Session   string `json:"-"`
	ID        string `json:"approval_id"`
	Kind      string `json:"kind"`
	Summary   string `json:"summary"`
	Matched   string `json:"matched"`
	ExpiresAt string `json:"expires_at"`
}

func (e *approvalRequired) Error() string {
	kind := e.Kind
	if kind == "" {
		kind = "command"
	}
	return fmt.Sprintf("this %s needs an admin's approval (rule %q); approval %s is pending until %s\n"+
		"  approve: kaironctl session approve %s %s\n"+
		"  then repeat with --approval-id %s, or wait for the decision with --approval-wait 2m",
		kind, e.Matched, e.ID, orDash(e.ExpiresAt), e.Session, e.ID, e.ID)
}

// sessionSend calls a session verb through kairon-ui like uiRaw, but turns
// a 428 into *approvalRequired. A nil body sends none.
func sessionSend(ctx context.Context, ns, name, method, verb string, body any) (json.RawMessage, error) {
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
	resp, err := uiRequest(ctx, method, sessionPath(ns, name, verb), r)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusPreconditionRequired {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		e := &approvalRequired{Session: name}
		if json.Unmarshal(raw, e) == nil && e.ID != "" {
			return nil, e
		}
		return nil, fmt.Errorf("kairon-ui: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, uiError(resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// approvalFlags carry an approval into exec and bg.
type approvalFlags struct {
	id   string
	wait time.Duration
}

func (a *approvalFlags) register(c *cobra.Command) {
	c.Flags().StringVar(&a.id, "approval-id", "", "run a command an admin approved (the id from the approval-required answer)")
	c.Flags().DurationVar(&a.wait, "approval-wait", 0, "hold the request open up to this long (at most 2m) for an admin's decision")
}

func (a approvalFlags) apply(body map[string]any) error {
	if a.id != "" {
		body["approval_id"] = a.id
	}
	if a.wait > 0 {
		if a.wait > 2*time.Minute {
			return fmt.Errorf("--approval-wait is at most 2m")
		}
		body["approval_wait_seconds"] = int64(a.wait / time.Second)
	}
	return nil
}

type sessionApproval struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Summary     string `json:"summary"`
	Matched     string `json:"matched"`
	State       string `json:"state"`
	RequestedAt string `json:"requested_at"`
	ExpiresAt   string `json:"expires_at"`
	Note        string `json:"note"`
}

func newSessionApprovalsCmd(opts *Options) *cobra.Command {
	var all, asJSON bool
	c := &cobra.Command{
		Use:   "approvals NAME",
		Short: "List the session's approval requests (pending only unless --all)",
		Long: `Commands matching the session's policy.process.ask rules wait for an admin.
Each request shows the command, the rule it matched and its state: pending,
approved, denied, consumed (approved and run) or expired.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := sessionSend(cmd.Context(), opts.Namespace, args[0], http.MethodGet, "approvals", nil)
			if err != nil {
				return err
			}
			if asJSON {
				return printRaw(raw)
			}
			var doc struct {
				Items []sessionApproval `json:"items"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				return err
			}
			printApprovals(os.Stdout, doc.Items, all)
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "include decided and expired requests")
	c.Flags().BoolVar(&asJSON, "json", false, "print the raw answer")
	return c
}

func printApprovals(w io.Writer, items []sessionApproval, all bool) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].RequestedAt > items[j].RequestedAt })
	_, _ = fmt.Fprintf(w, "ID\tSTATE\tKIND\tRULE\tCOMMAND\tEXPIRES\tNOTE\n")
	for _, a := range items {
		if !all && a.State != "pending" {
			continue
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.State, dash(a.Kind), dash(a.Matched),
			strings.ReplaceAll(dash(a.Summary), "\n", " "), dash(a.ExpiresAt), dash(a.Note))
	}
}

func newSessionDecideCmd(opts *Options, decision string) *cobra.Command {
	var note string
	short := "Approve a pending command so the agent's retry runs it (admin only)"
	if decision == "deny" {
		short = "Refuse a pending command (admin only)"
	}
	c := &cobra.Command{
		Use:   decision + " NAME APPROVAL",
		Short: short,
		Long: short + `.

kairon-ui records your user name at the front of the note FluxVM keeps in the
session's ledger. Operators who may run commands cannot decide approvals, so
an agent cannot approve its own request.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			if note != "" {
				body["note"] = note
			}
			raw, err := sessionSend(cmd.Context(), opts.Namespace, args[0], http.MethodPost, "approvals/"+url.PathEscape(args[1])+"/"+decision, body)
			if err != nil {
				return err
			}
			var a sessionApproval
			if json.Unmarshal(raw, &a) == nil && a.ID != "" {
				okf("approval %s %s: %s", a.ID, a.State, a.Summary)
				return nil
			}
			return printRaw(raw)
		},
	}
	c.Flags().StringVar(&note, "note", "", "reason kept with the decision")
	return c
}

const (
	inlinePutLimit   = 1 << 20
	uploadChunkBytes = 4 << 20
	maxUploadBytes   = 2 << 30
)

// sessionUpload sends a local file in 4 MiB chunks through FluxVM's
// resumable uploads: the guest checks the SHA-256 before the file appears.
// A chunk FluxVM already has (409) resumes at its offset; a failure after
// the upload opened aborts it so no parts are left in the guest.
func sessionUpload(ctx context.Context, ns, name, local, guest string, mode *uint64, parents bool, progress io.Writer) (json.RawMessage, error) {
	f, err := os.Open(local)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size <= 0 || size > maxUploadBytes {
		return nil, fmt.Errorf("%s: uploads are 1 byte to 2 GiB", local)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	body := map[string]any{"path": guest, "size": size, "sha256": hex.EncodeToString(h.Sum(nil)), "parents": parents}
	if mode != nil {
		body["mode"] = *mode
	}
	raw, err := sessionSend(ctx, ns, name, http.MethodPost, "uploads", body)
	if err != nil {
		return nil, err
	}
	var up struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &up); err != nil || up.ID == "" {
		return nil, fmt.Errorf("upload not opened: %s", raw)
	}
	verb := "uploads/" + url.PathEscape(up.ID)
	done := false
	defer func() {
		if !done {
			actx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = sessionSend(actx, ns, name, http.MethodDelete, verb, nil)
		}
	}()
	buf := make([]byte, uploadChunkBytes)
	var offset int64
	resyncs := 0
	for offset < size {
		n, err := f.ReadAt(buf[:min(int64(len(buf)), size-offset)], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		resp, err := uiRequest(ctx, http.MethodPut, sessionPath(ns, name, verb)+"?offset="+strconv.FormatInt(offset, 10), bytes.NewReader(buf[:n]))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusConflict && resyncs < 5 {
			_ = resp.Body.Close()
			resyncs++
			stRaw, err := sessionSend(ctx, ns, name, http.MethodGet, verb, nil)
			if err != nil {
				return nil, err
			}
			var cur struct {
				Received int64  `json:"received"`
				State    string `json:"state"`
			}
			if err := json.Unmarshal(stRaw, &cur); err != nil {
				return nil, err
			}
			if cur.State == "complete" {
				done = true
				return stRaw, nil
			}
			offset = cur.Received
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			err := uiError(resp)
			_ = resp.Body.Close()
			return nil, err
		}
		_ = resp.Body.Close()
		offset += int64(n)
		if progress != nil {
			_, _ = fmt.Fprintf(progress, "\ruploaded %d of %d bytes", offset, size)
		}
	}
	if progress != nil {
		_, _ = fmt.Fprintln(progress)
	}
	out, err := sessionSend(ctx, ns, name, http.MethodPost, verb+"/complete", nil)
	if err != nil {
		return nil, err
	}
	done = true
	return out, nil
}

func newSessionProbeCmd(opts *Options) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "probe NAME",
		Short: "Report which tools the guest image has and what the session features need",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := sessionSend(cmd.Context(), opts.Namespace, args[0], http.MethodGet, "probe", nil)
			if err != nil {
				return err
			}
			if asJSON {
				return printRaw(raw)
			}
			var doc struct {
				Tools    map[string]string `json:"tools"`
				System   map[string]string `json:"system"`
				ReadyFor map[string]bool   `json:"ready_for"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				return err
			}
			fmt.Print(probeSummary(doc.Tools, doc.System, doc.ReadyFor))
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print the raw answer")
	return c
}

func probeSummary(tools, system map[string]string, ready map[string]bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "system: %s %s\n", dash(system["arch"]), dash(system["kernel"]))
	names := make([]string, 0, len(tools))
	for t := range tools {
		names = append(names, t)
	}
	sort.Strings(names)
	fmt.Fprintf(&b, "tools: %s\n", dash(strings.Join(names, " ")))
	var yes, no []string
	for k, v := range ready {
		if v {
			yes = append(yes, k)
		} else {
			no = append(no, k)
		}
	}
	sort.Strings(yes)
	sort.Strings(no)
	fmt.Fprintf(&b, "ready for: %s\nnot ready for: %s\n", dash(strings.Join(yes, " ")), dash(strings.Join(no, " ")))
	return b.String()
}

func newSessionShellCmd(opts *Options) *cobra.Command {
	var plain bool
	c := &cobra.Command{
		Use:   "shell NAME [-- COMMAND]",
		Short: "Open an interactive terminal in the session (or run COMMAND on one)",
		Long: `A real terminal in the guest, in the workspace, over a WebSocket through
kairon-ui. The window size follows your terminal. FluxVM refuses a terminal
to a session with process or filesystem rules, because a shell would walk
around them. Exits with the shell's exit code.`,
		Example: `  $ kaironctl session shell demo
  $ kaironctl session shell demo -- htop`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
			defer cancel()
			stdinFd, stdoutFd := int(os.Stdin.Fd()), int(os.Stdout.Fd())
			cols, rows := 80, 24
			if w, h, err := term.GetSize(stdoutFd); err == nil {
				cols, rows = w, h
			}
			q := ptyQuery(cols, rows, strings.Join(args[1:], " "), plain, os.Getenv)
			conn, err := dialUIWebSocket(ctx, sessionPath(opts.Namespace, args[0], "pty"), q, true)
			if err != nil {
				return fmt.Errorf("shell %s/%s: %w", opts.Namespace, args[0], err)
			}
			defer func() { _ = conn.CloseNow() }()
			resize := make(chan [2]int, 1)
			stop := notifyWindowResize(func() {
				if w, h, err := term.GetSize(stdoutFd); err == nil {
					select {
					case resize <- [2]int{w, h}:
					default:
					}
				}
			})
			defer stop()
			restore := func() {}
			if term.IsTerminal(stdinFd) {
				state, err := term.MakeRaw(stdinFd)
				if err != nil {
					return fmt.Errorf("raw terminal: %w", err)
				}
				restore = func() { _ = term.Restore(stdinFd, state) }
			}
			code := runSessionPty(ctx, conn, os.Stdin, os.Stdout, resize)
			restore()
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&plain, "plain", false, "start a plain login shell without the colour defaults")
	return c
}

// ptyQuery is FluxVM's pty query: the size, the command, and the local
// terminal's TERM/COLORTERM/TERM_PROGRAM so guest tools pick the right
// features. Empty values are left out.
func ptyQuery(cols, rows int, command string, plain bool, getenv func(string) string) url.Values {
	q := url.Values{"cols": {strconv.Itoa(min(max(cols, 1), 500))}, "rows": {strconv.Itoa(min(max(rows, 1), 300))}}
	for key, val := range map[string]string{
		"command":              command,
		"term":                 getenv("TERM"),
		"colorterm":            getenv("COLORTERM"),
		"term_program":         getenv("TERM_PROGRAM"),
		"term_program_version": getenv("TERM_PROGRAM_VERSION"),
	} {
		if val != "" {
			q.Set(key, val)
		}
	}
	if plain {
		q.Set("plain", "true")
	}
	return q
}

// runSessionPty pipes in to the terminal and the terminal to out until the
// guest closes it, sending a resize frame per size on resize. It returns
// the shell's exit code from FluxVM's "exit=N" close reason, or 1 when the
// connection was lost.
func runSessionPty(ctx context.Context, conn *websocket.Conn, in io.Reader, out io.Writer, resize <-chan [2]int) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := in.Read(buf)
			if n > 0 && conn.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sz := <-resize:
				msg, _ := json.Marshal(map[string]any{"resize": map[string]int{"cols": min(max(sz[0], 1), 500), "rows": min(max(sz[1], 1), 300)}})
				_ = conn.Write(ctx, websocket.MessageText, msg)
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return ptyExitCode(err)
		}
		_, _ = out.Write(data)
	}
}

func ptyExitCode(err error) int {
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		return 1
	}
	if s, ok := strings.CutPrefix(ce.Reason, "exit="); ok {
		if n, err := strconv.Atoi(s); err == nil {
			if n < 0 || n > 255 {
				return 255
			}
			return n
		}
	}
	if ce.Code == websocket.StatusNormalClosure {
		return 0
	}
	return 1
}

func newSessionGrantCmd(opts *Options) *cobra.Command {
	var hosts, methods, paths []string
	var ttl time.Duration
	var valueEnv, fromSecret string
	c := &cobra.Command{
		Use:   "grant NAME SECRET-REF --host HOST...",
		Short: "Let the session use a credential it never sees (admin only)",
		Long: `FluxVM's egress proxy injects the credential as a header on the session's
requests to the named hosts; the guest never holds it. The grant ends with the
session, at --ttl, or on revoke-grant, and fork children start without it.

The header value (e.g. "Bearer ghp_...") comes from --value-env (a local
environment variable), --value-from-secret (SECRET/KEY of a Kubernetes Secret
in the namespace), or, with neither, the node's FLUXVM_SECRET_<REF> variable.
The value is never printed.`,
		Example: `  $ GH_AUTH="Bearer $(gh auth token)" kaironctl session grant demo github --host api.github.com --value-env GH_AUTH
  $ kaironctl session grant demo pypi --host upload.pypi.org --value-from-secret pypi-token/header --ttl 1h`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(hosts) == 0 {
				return fmt.Errorf("--host is required: the hosts the credential may be sent to")
			}
			if valueEnv != "" && fromSecret != "" {
				return fmt.Errorf("--value-env and --value-from-secret are exclusive")
			}
			body := map[string]any{"secret_ref": args[1], "hosts": hosts}
			if len(methods) > 0 {
				body["methods"] = methods
			}
			if len(paths) > 0 {
				body["paths"] = paths
			}
			if ttl > 0 {
				body["ttl_seconds"] = int64(ttl / time.Second)
			}
			switch {
			case valueEnv != "":
				v := os.Getenv(valueEnv)
				if v == "" {
					return fmt.Errorf("$%s is empty", valueEnv)
				}
				body["value"] = v
			case fromSecret != "":
				secret, key, ok := strings.Cut(fromSecret, "/")
				if !ok || secret == "" || key == "" {
					return fmt.Errorf("--value-from-secret wants SECRET/KEY")
				}
				kc, err := newKubeClient()
				if err != nil {
					return err
				}
				s, err := kc.GetSecret(cmd.Context(), opts.Namespace, secret)
				if err != nil {
					return err
				}
				v := strings.TrimSpace(string(s.Data[key]))
				if v == "" {
					return fmt.Errorf("secret %s/%s has no key %q", opts.Namespace, secret, key)
				}
				body["value"] = v
			}
			raw, err := sessionSend(cmd.Context(), opts.Namespace, args[0], http.MethodPost, "grants", body)
			if err != nil {
				return err
			}
			return printRaw(raw)
		},
	}
	c.Flags().StringArrayVar(&hosts, "host", nil, "host the credential may be sent to (repeatable; a domain also covers its subdomains)")
	c.Flags().StringArrayVar(&methods, "method", nil, "HTTP method it may be sent with (repeatable; default any)")
	c.Flags().StringArrayVar(&paths, "path", nil, "request path it may be sent to, * matches any run (repeatable; default any)")
	c.Flags().DurationVar(&ttl, "ttl", 0, "end the grant after this long (never later than the session)")
	c.Flags().StringVar(&valueEnv, "value-env", "", "local environment variable holding the header value")
	c.Flags().StringVar(&fromSecret, "value-from-secret", "", "SECRET/KEY of a Secret in the namespace holding the header value")
	return c
}

func newSessionRevokeGrantCmd(opts *Options) *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "revoke-grant NAME [GRANT]",
		Short: "Revoke one credential grant, or --all (admin only)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			verb := "grants"
			switch {
			case len(args) == 2 && !all:
				verb += "/" + url.PathEscape(args[1])
			case len(args) == 1 && all:
			default:
				return fmt.Errorf("name one GRANT or pass --all")
			}
			raw, err := sessionSend(cmd.Context(), opts.Namespace, args[0], http.MethodDelete, verb, nil)
			if err != nil {
				return err
			}
			return printRaw(raw)
		},
	}
	c.Flags().BoolVar(&all, "all", false, "revoke every grant of the session")
	return c
}
