// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var guestUserRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,31}\$?$`)

func validGuestUser(u string) error {
	if !guestUserRe.MatchString(u) {
		return fmt.Errorf("--user %q is not a valid guest account name", u)
	}
	return nil
}

// sshKey is one authorized_keys line plus its base64 blob, which is what
// duplicates and removals match on (comments and options may differ).
type sshKey struct {
	Line string
	Blob string
}

var sshKeyTypes = []string{"ssh-", "ecdsa-", "sk-ssh-", "sk-ecdsa-"}

// parseSSHKeys reads public keys from text, one per line; blank lines and
// # comments are skipped. Leading authorized_keys options are allowed.
func parseSSHKeys(text string) ([]sshKey, error) {
	var keys []sshKey
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		blob := ""
		for i := 0; i+1 < len(fields); i++ {
			if hasAnyPrefix(fields[i], sshKeyTypes) {
				blob = fields[i+1]
				break
			}
		}
		if blob == "" {
			return nil, fmt.Errorf("line %d: not an SSH public key (want e.g. \"ssh-ed25519 AAAA... comment\")", n+1)
		}
		if _, err := base64.StdEncoding.DecodeString(blob); err != nil {
			return nil, fmt.Errorf("line %d: key data is not base64", n+1)
		}
		keys = append(keys, sshKey{Line: line, Blob: blob})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no SSH public keys given")
	}
	return keys, nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// guestHomeSh resolves $u's home directory into $h, also on guests without getent.
const guestHomeSh = `h=$(getent passwd "$u" 2>/dev/null | cut -d: -f6)
[ -n "$h" ] || h=$(awk -F: -v u="$u" '$1==u{print $6}' /etc/passwd)
[ -n "$h" ] || { echo "no such user: $u" >&2; exit 3; }
`

// addKeysSh takes: user, then (line, blob) pairs. Prints how many keys it added.
const addKeysSh = `set -e
u="$1"; shift
` + guestHomeSh + `g=$(id -gn "$u")
d="$h/.ssh"; f="$d/authorized_keys"
mkdir -p "$d"; touch "$f"
if [ -s "$f" ] && [ -n "$(tail -c1 "$f")" ]; then echo >> "$f"; fi
n=0
while [ $# -ge 2 ]; do
  if ! grep -qF -- "$2" "$f"; then printf '%s\n' "$1" >> "$f"; n=$((n+1)); fi
  shift 2
done
chmod 700 "$d"; chmod 600 "$f"; chown "$u:$g" "$d" "$f"
if command -v restorecon >/dev/null 2>&1; then restorecon -R "$d" >/dev/null 2>&1 || true; fi
echo "$n"`

// removeKeysSh takes: user, then blobs. Rewrites the file in place so its
// owner, mode and SELinux label stay. Prints how many keys it removed.
const removeKeysSh = `set -e
u="$1"; shift
` + guestHomeSh + `f="$h/.ssh/authorized_keys"
[ -f "$f" ] || { echo 0; exit 0; }
t=$(mktemp)
trap 'rm -f "$t" "$t.n"' EXIT
cp "$f" "$t"
n=0
for b in "$@"; do
  if grep -qF -- "$b" "$t"; then
    grep -vF -- "$b" "$t" > "$t.n" || true
    mv "$t.n" "$t"
    n=$((n+1))
  fi
done
cat "$t" > "$f"
echo "$n"`

func keyScriptArgs(script, user string, keys []sshKey, withLines bool) []string {
	args := []string{"-c", script, "sh", user}
	for _, k := range keys {
		if withLines {
			args = append(args, k.Line)
		}
		args = append(args, k.Blob)
	}
	return args
}

// runKeyScript runs one of the authorized_keys scripts and returns the
// count it prints.
func runKeyScript(ctx context.Context, ns, name string, args []string) (int, error) {
	res, err := guestRun(ctx, ns, name, map[string]any{"path": "/bin/sh", "args": args})
	if err != nil {
		return 0, err
	}
	if res.ExitCode != 0 {
		return 0, fmt.Errorf("guest exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	var n int
	if _, err := fmt.Sscan(strings.TrimSpace(res.Stdout), &n); err != nil {
		return 0, fmt.Errorf("unexpected guest output %q", strings.TrimSpace(res.Stdout))
	}
	return n, nil
}

// readSecret prompts on the terminal without echo. Replaced in tests.
var readSecret = func(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("stdin is not a terminal; pipe the password with --password-stdin")
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

func promptNewPassword(user string) (string, error) {
	p1, err := readSecret(fmt.Sprintf("New password for %s: ", user))
	if err != nil {
		return "", err
	}
	p2, err := readSecret("Retype password: ")
	if err != nil {
		return "", err
	}
	if p1 != p2 {
		return "", fmt.Errorf("passwords do not match")
	}
	return p1, nil
}

func readPasswordLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// setGuestPassword stages "user:password" in a root-only temp file over
// the guest file channel, then feeds it to chpasswd and removes it, so the
// password never appears on a command line in the guest or in transit logs.
func setGuestPassword(ctx context.Context, ns, name, user, password string) error {
	if password == "" {
		return fmt.Errorf("empty password")
	}
	if strings.ContainsAny(password, "\r\n") {
		return fmt.Errorf("password must not contain a newline")
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	path := "/run/kairon-cred-" + hex.EncodeToString(rnd[:])
	mode := uint32(0o600)
	put := map[string]any{
		"path":          path,
		"contentBase64": base64.StdEncoding.EncodeToString([]byte(user + ":" + password + "\n")),
		"mode":          mode,
	}
	if _, err := uiRaw(ctx, http.MethodPost, machinePath(ns, name, "agent-file/put"), put); err != nil {
		return fmt.Errorf("stage password file: %w", err)
	}
	cmd := fmt.Sprintf(`f=%s; chpasswd < "$f"; rc=$?; rm -f "$f"; exit $rc`, path)
	raw, err := uiRaw(ctx, http.MethodPost, machinePath(ns, name, "agent-exec"), map[string]any{"command": cmd, "timeoutSeconds": 30})
	if err != nil {
		_, _ = uiRaw(ctx, http.MethodPost, machinePath(ns, name, "agent-exec"), map[string]any{"command": "rm -f " + path, "timeoutSeconds": 10})
		return err
	}
	var res execResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("chpasswd exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func newCredentialsCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "credentials",
		Aliases: []string{"creds"},
		Short:   "Manage SSH keys and passwords inside a running Linux guest (admin)",
		Long: `Changes guest accounts in place, without a reboot, through kairon-ui.
Needs KAIRON_UI_URL (and KAIRON_UI_TOKEN) and an admin account.

  add-ssh-key / remove-ssh-key  edit ~USER/.ssh/authorized_keys through the
                                guest agent (spec.guestAgent.enabled)
  set-password                  stages USER:PASSWORD in a root-only file through
                                the FluxVM guest agent (spec.guestAgent.console)
                                and pipes it into chpasswd; the password is never
                                put on a command line`,
	}

	var user, key, file string
	readKeys := func(cmd *cobra.Command) ([]sshKey, error) {
		if err := validGuestUser(user); err != nil {
			return nil, err
		}
		switch {
		case key != "" && file != "":
			return nil, fmt.Errorf("use --key or --file, not both")
		case key != "":
			return parseSSHKeys(key)
		case file == "-":
			b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
			if err != nil {
				return nil, err
			}
			return parseSSHKeys(string(b))
		case file != "":
			b, err := os.ReadFile(file)
			if err != nil {
				return nil, err
			}
			return parseSSHKeys(string(b))
		}
		return nil, fmt.Errorf("--key or --file is required")
	}
	keyFlags := func(c *cobra.Command) {
		c.Flags().StringVar(&user, "user", "", "guest account (required)")
		c.Flags().StringVar(&key, "key", "", "public key text, e.g. \"ssh-ed25519 AAAA... me@laptop\"")
		c.Flags().StringVar(&file, "file", "", "file with one or more public keys (- for stdin)")
		_ = c.MarkFlagRequired("user")
	}

	add := &cobra.Command{
		Use:               "add-ssh-key MACHINE --user USER (--key KEY | --file FILE)",
		Short:             "Append public keys to the user's authorized_keys (skips ones already there)",
		Example:           "  $ kaironctl credentials add-ssh-key web-1 --user ubuntu --file ~/.ssh/id_ed25519.pub",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			keys, err := readKeys(cmd)
			if err != nil {
				return err
			}
			n, err := runKeyScript(cmd.Context(), opts.Namespace, args[0], keyScriptArgs(addKeysSh, user, keys, true))
			if err != nil {
				return err
			}
			okf("added %d of %d key(s) for %s on %s/%s", n, len(keys), user, opts.Namespace, args[0])
			return nil
		},
	}
	keyFlags(add)

	remove := &cobra.Command{
		Use:               "remove-ssh-key MACHINE --user USER (--key KEY | --file FILE)",
		Short:             "Remove public keys from the user's authorized_keys",
		Example:           "  $ kaironctl credentials remove-ssh-key web-1 --user ubuntu --key \"$(cat old.pub)\"",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			keys, err := readKeys(cmd)
			if err != nil {
				return err
			}
			n, err := runKeyScript(cmd.Context(), opts.Namespace, args[0], keyScriptArgs(removeKeysSh, user, keys, false))
			if err != nil {
				return err
			}
			okf("removed %d of %d key(s) for %s on %s/%s", n, len(keys), user, opts.Namespace, args[0])
			return nil
		},
	}
	keyFlags(remove)

	var pwUser string
	var fromStdin bool
	setPw := &cobra.Command{
		Use:   "set-password MACHINE --user USER [--password-stdin]",
		Short: "Set a guest account's password (prompts without echo)",
		Example: `  $ kaironctl credentials set-password web-1 --user ubuntu
  $ pass show web-1 | kaironctl credentials set-password web-1 --user ubuntu --password-stdin`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validGuestUser(pwUser); err != nil {
				return err
			}
			var pw string
			var err error
			if fromStdin {
				pw, err = readPasswordLine(cmd.InOrStdin())
			} else {
				pw, err = promptNewPassword(pwUser)
			}
			if err != nil {
				return err
			}
			if err := setGuestPassword(cmd.Context(), opts.Namespace, args[0], pwUser, pw); err != nil {
				return err
			}
			okf("password set for %s on %s/%s", pwUser, opts.Namespace, args[0])
			return nil
		},
	}
	setPw.Flags().StringVar(&pwUser, "user", "", "guest account (required)")
	setPw.Flags().BoolVar(&fromStdin, "password-stdin", false, "read the password from the first line of stdin")
	_ = setPw.MarkFlagRequired("user")

	cmd.AddCommand(add, remove, setPw)
	return cmd
}
