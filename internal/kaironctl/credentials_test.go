// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl me@laptop"

func TestParseSSHKeys(t *testing.T) {
	keys, err := parseSSHKeys("# mine\n\n" + testKey + "\nfrom=\"10.0.0.0/8\" ecdsa-sha2-nistp256 AAAAE2VjZHNh other\n")
	if err != nil || len(keys) != 2 {
		t.Fatalf("%v %v", keys, err)
	}
	if keys[0].Blob != "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl" || keys[1].Blob != "AAAAE2VjZHNh" {
		t.Fatalf("blobs %+v", keys)
	}
	for _, bad := range []string{"", "hello world", "ssh-rsa not-base64!"} {
		if _, err := parseSSHKeys(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestValidGuestUser(t *testing.T) {
	for _, u := range []string{"ubuntu", "_svc", "a.b-c", "machine$"} {
		if validGuestUser(u) != nil {
			t.Fatalf("%q rejected", u)
		}
	}
	for _, u := range []string{"", "root;id", "a b", "-x", strings.Repeat("a", 40)} {
		if validGuestUser(u) == nil {
			t.Fatalf("%q accepted", u)
		}
	}
}

// TestAuthorizedKeysScripts runs the real scripts against a fake home so
// the add/dedupe/remove behaviour is checked, not just the wire shape.
func TestAuthorizedKeysScripts(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	passwd := filepath.Join(home, "passwd")
	user := os.Getenv("USER")
	if user == "" {
		t.Skip("no USER")
	}
	if err := os.WriteFile(passwd, []byte(user+":x:1:1::"+home+":/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Point the home lookup at the fake passwd file.
	fix := func(s string) string {
		s = strings.Replace(s, `getent passwd "$u" 2>/dev/null`, `grep "^$u:" `+passwd, 1)
		return strings.Replace(s, "/etc/passwd", passwd, 1)
	}
	run := func(script string, keys []sshKey, withLines bool) string {
		args := keyScriptArgs(fix(script), user, keys, withLines)
		out, err := exec.Command("sh", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	keys, _ := parseSSHKeys(testKey + "\nssh-rsa AAAAB3NzaC1yc2E= second")
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(ak), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ak, []byte("ssh-rsa AAAAexisting old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := run(addKeysSh, keys, true); n != "2" {
		t.Fatalf("added %s", n)
	}
	if n := run(addKeysSh, keys[:1], true); n != "0" {
		t.Fatalf("duplicate added: %s", n)
	}
	b, _ := os.ReadFile(ak)
	if want := "ssh-rsa AAAAexisting old\n" + testKey + "\nssh-rsa AAAAB3NzaC1yc2E= second\n"; string(b) != want {
		t.Fatalf("authorized_keys:\n%s", b)
	}
	if n := run(removeKeysSh, keys[:1], false); n != "1" {
		t.Fatalf("removed %s", n)
	}
	b, _ = os.ReadFile(ak)
	if strings.Contains(string(b), "me@laptop") || !strings.Contains(string(b), "second") {
		t.Fatalf("after remove:\n%s", b)
	}
	if fi, _ := os.Stat(ak); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestSetPasswordKeepsSecretOffCommandLine(t *testing.T) {
	type call struct {
		path string
		body map[string]any
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		calls = append(calls, call{r.URL.Path, b})
		if strings.HasSuffix(r.URL.Path, "/agent-exec") {
			_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "tok")

	root := NewRootCmd(nil)
	root.SetIn(strings.NewReader("s3cret pass\n"))
	root.SetArgs([]string{"credentials", "set-password", "web-1", "-n", "prod", "--user", "ubuntu", "--password-stdin"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].path != "/api/v1/machines/prod/web-1/agent-file/put" || calls[1].path != "/api/v1/machines/prod/web-1/agent-exec" {
		t.Fatalf("calls %+v", calls)
	}
	content, _ := base64.StdEncoding.DecodeString(calls[0].body["contentBase64"].(string))
	if string(content) != "ubuntu:s3cret pass\n" || calls[0].body["mode"] != float64(0o600) {
		t.Fatalf("staged %q %v", content, calls[0].body["mode"])
	}
	staged := calls[0].body["path"].(string)
	command := calls[1].body["command"].(string)
	if strings.Contains(command, "s3cret") || !strings.Contains(command, staged) || !strings.Contains(command, "chpasswd") {
		t.Fatalf("command %q", command)
	}
}

func TestAddSSHKeyCommand(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": "1\n"})
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)

	root := NewRootCmd(nil)
	root.SetArgs([]string{"creds", "add-ssh-key", "web-1", "--user", "ubuntu", "--key", testKey})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	args, _ := body["args"].([]any)
	if body["path"] != "/bin/sh" || len(args) != 6 || args[3] != "ubuntu" || args[4] != testKey {
		t.Fatalf("body %v", body)
	}
}
