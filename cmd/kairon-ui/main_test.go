// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crypto/tls"
	"golang.org/x/crypto/bcrypt"
	"log/slog"
	"net"
	"net/http"
)

func TestLoadUsersParsesConfiguredAccounts(t *testing.T) {
	users, err := loadUsers(`[{"username":"alice","passwordHash":"$2a$10$abc"}]`, "")
	if err != nil {
		t.Fatalf("loadUsers: %v", err)
	}
	if len(users) != 1 || users[0].Username != "alice" || users[0].PasswordHash != "$2a$10$abc" {
		t.Fatalf("unexpected users: %+v", users)
	}
}

func TestLoadUsersHashesDefaultAdminPassword(t *testing.T) {
	users, err := loadUsers("", "generated-password")
	if err != nil {
		t.Fatalf("loadUsers: %v", err)
	}
	if len(users) != 1 || users[0].Username != "admin" {
		t.Fatalf("expected one admin user, got %+v", users)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(users[0].PasswordHash), []byte("generated-password")); err != nil {
		t.Fatalf("expected the stored hash to verify against the plaintext password: %v", err)
	}
	if users[0].PasswordHash == "generated-password" {
		t.Fatal("password must be hashed, not stored in plaintext")
	}
}

func TestLoadUsersCombinesConfiguredAndDefaultAdmin(t *testing.T) {
	users, err := loadUsers(`[{"username":"alice","passwordHash":"$2a$10$abc"}]`, "generated-password")
	if err != nil {
		t.Fatalf("loadUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected both accounts, got %+v", users)
	}
}

func TestLoadUsersRejectsMalformedJSON(t *testing.T) {
	if _, err := loadUsers("not json", ""); err == nil {
		t.Fatal("expected an error for malformed $KAIRON_UI_USERS_JSON")
	}
}

// TestLoadUsersParsesNamespaces proves ui.auth.users[].namespaces
// round-trips through $KAIRON_UI_USERS_JSON into uiapi.User.Namespaces --
// the same JSON field the Helm chart's users.json Secret carries (see
// charts/kairon/templates/all.yaml's $usersJSONValue).
func TestLoadUsersParsesNamespaces(t *testing.T) {
	users, err := loadUsers(`[{"username":"bob","passwordHash":"$2a$10$abc","namespaces":["team-bob","shared"]}]`, "")
	if err != nil {
		t.Fatalf("loadUsers: %v", err)
	}
	if len(users) != 1 || len(users[0].Namespaces) != 2 || users[0].Namespaces[0] != "team-bob" || users[0].Namespaces[1] != "shared" {
		t.Fatalf("expected namespaces [team-bob shared] to round-trip, got %+v", users)
	}
}

func TestLoadNamespaceGroupsEmptyInputReturnsNil(t *testing.T) {
	groups, err := loadNamespaceGroups("")
	if err != nil {
		t.Fatalf("loadNamespaceGroups: %v", err)
	}
	if groups != nil {
		t.Fatalf("expected a nil map for empty input, got %+v", groups)
	}
}

// TestLoadNamespaceGroupsParsesListIntoMap proves
// $KAIRON_UI_OIDC_NAMESPACE_GROUPS's own wire shape (a JSON array of
// {group, namespaces}, mirroring ui.oidc.namespaceGroups' Helm values
// shape exactly -- see charts/kairon/templates/all.yaml) parses into the
// map[string][]string shape uiapi.OIDCAuth.NamespaceGroups actually
// consults at request time, including merging two entries for the same
// group.
func TestLoadNamespaceGroupsParsesListIntoMap(t *testing.T) {
	raw := `[{"group":"team-bob","namespaces":["team-bob","shared"]},{"group":"team-bob","namespaces":["extra"]},{"group":"team-carol","namespaces":["team-carol"]}]`
	groups, err := loadNamespaceGroups(raw)
	if err != nil {
		t.Fatalf("loadNamespaceGroups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 distinct groups, got %+v", groups)
	}
	if want := []string{"team-bob", "shared", "extra"}; !equalStrings(groups["team-bob"], want) {
		t.Fatalf("expected team-bob's two entries to merge into %v, got %v", want, groups["team-bob"])
	}
	if want := []string{"team-carol"}; !equalStrings(groups["team-carol"], want) {
		t.Fatalf("expected %v, got %v", want, groups["team-carol"])
	}
}

func TestLoadNamespaceGroupsRejectsMalformedJSON(t *testing.T) {
	if _, err := loadNamespaceGroups("not json"); err == nil {
		t.Fatal("expected an error for malformed $KAIRON_UI_OIDC_NAMESPACE_GROUPS")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHashPasswordPrintsAVerifiableBcryptHash(t *testing.T) {
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	code := hashPassword([]string{"my-test-password"})
	_ = w.Close()
	os.Stdout = orig
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	hash := strings.TrimSpace(buf.String())
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("my-test-password")); err != nil {
		t.Fatalf("printed hash does not verify: %v", err)
	}
}

func TestHashPasswordRequiresExactlyOneArg(t *testing.T) {
	if code := hashPassword(nil); code != 2 {
		t.Fatalf("expected usage error (2) for no args, got %d", code)
	}
	if code := hashPassword([]string{"a", "b"}); code != 2 {
		t.Fatalf("expected usage error (2) for too many args, got %d", code)
	}
}

// writeSelfSignedCAPEM generates a throwaway self-signed certificate and
// writes it PEM-encoded to a temp file, purely to give consoleTLSConfig a
// real, parseable CA to load -- not a working TLS identity.
func writeSelfSignedCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "kairon-test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	return path
}

func TestConsoleTLSConfigEmptyPathMeansNoTLS(t *testing.T) {
	cfg, err := consoleTLSConfig("", "")
	if err != nil {
		t.Fatalf("expected no error for an empty path, got %v", err)
	}
	if cfg != nil {
		t.Fatal("expected a nil TLS config when no CA is configured")
	}
}

func TestConsoleTLSConfigLoadsARealCA(t *testing.T) {
	path := writeSelfSignedCAPEM(t)
	cfg, err := consoleTLSConfig(path, "")
	if err != nil {
		t.Fatalf("consoleTLSConfig: %v", err)
	}
	if cfg == nil || cfg.RootCAs == nil {
		t.Fatal("expected a non-nil TLS config with a populated RootCAs pool")
	}
}

func TestConsoleTLSConfigRejectsMissingOrInvalidFile(t *testing.T) {
	if _, err := consoleTLSConfig("/nonexistent/ca.pem", ""); err == nil {
		t.Fatal("expected an error for a nonexistent CA file")
	}
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write garbage file: %v", err)
	}
	if _, err := consoleTLSConfig(garbage, ""); err == nil {
		t.Fatal("expected an error for a CA file with no real certificates")
	}
}

func TestLoginHint(t *testing.T) {
	t.Setenv("KAIRON_UI_LOGIN_HINT", "")
	t.Setenv("KAIRON_UI_DEFAULT_ADMIN_PASSWORD", "something-else-123")
	if got := loginHint(); got != "" {
		t.Fatalf("custom password must not be advertised, got %q", got)
	}
	t.Setenv("KAIRON_UI_DEFAULT_ADMIN_PASSWORD", "Admin@321")
	if got := loginHint(); !strings.Contains(got, "admin / Admin@321") {
		t.Fatalf("default password should be advertised, got %q", got)
	}
	t.Setenv("KAIRON_UI_LOGIN_HINT", "Ask ops")
	if got := loginHint(); got != "Ask ops" {
		t.Fatalf("explicit hint wins, got %q", got)
	}
}

func TestWellKnownPasswordOnlyForTheChartDefault(t *testing.T) {
	t.Setenv("KAIRON_UI_DEFAULT_ADMIN_PASSWORD", "Admin@321")
	if wellKnownPassword() == "" {
		t.Fatal("chart default should be reported")
	}
	t.Setenv("KAIRON_UI_DEFAULT_ADMIN_PASSWORD", "something-else-123")
	if wellKnownPassword() != "" {
		t.Fatal("a custom password must not be reported")
	}
}

func TestResolveTransport(t *testing.T) {
	cases := []struct {
		name         string
		cert, key    string
		insecureHTTP bool
		want         transportMode
		wantErr      bool
	}{
		{"default is HTTPS with a generated certificate", "", "", false, modeEphemeral, false},
		{"certificate files", "/c.pem", "/k.pem", false, modeCertFiles, false},
		{"explicit opt-out", "", "", true, modeHTTP, false},
		{"cert without key", "/c.pem", "", false, 0, true},
		{"key without cert", "", "/k.pem", false, 0, true},
		{"certificate contradicts opt-out", "/c.pem", "/k.pem", true, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveTransport(c.cert, c.key, c.insecureHTTP)
			if (err != nil) != c.wantErr || (!c.wantErr && got != c.want) {
				t.Errorf("resolveTransport(%q,%q,%v) = %v, %v; want %v, err=%v", c.cert, c.key, c.insecureHTTP, got, err, c.want, c.wantErr)
			}
		})
	}
	if modeEphemeral.scheme() != "https" || modeCertFiles.scheme() != "https" || modeHTTP.scheme() != "http" {
		t.Error("only -insecure-http may report the http scheme")
	}
}

func TestEphemeralTLSServesHTTPSAndIsSelfSigned(t *testing.T) {
	cfg, leaf, err := newEphemeralTLS("kairon.example.test,10.9.8.7")
	if err != nil {
		t.Fatal(err)
	}
	if !isSelfSigned(leaf) {
		t.Error("the generated certificate must report itself as self-signed (images are then marked insecureSkipTLSVerify)")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2+", cfg.MinVersion)
	}
	var hasDNS, hasIP, hasLoopback bool
	for _, n := range leaf.DNSNames {
		hasDNS = hasDNS || n == "kairon.example.test"
	}
	for _, ip := range leaf.IPAddresses {
		hasIP = hasIP || ip.String() == "10.9.8.7"
		hasLoopback = hasLoopback || ip.String() == "127.0.0.1"
	}
	if !hasDNS || !hasIP || !hasLoopback {
		t.Errorf("SANs dns=%v ips=%v; want the extras and loopback", leaf.DNSNames, leaf.IPAddresses)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }),
		TLSConfig:         cfg,
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer func() { _ = srv.Close() }()

	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	resp, err := client.Get("https://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("a client that trusts the certificate must connect: %v", err)
	}
	_ = resp.Body.Close()

	if r, err := http.Get("http://" + ln.Addr().String()); err == nil {
		_ = r.Body.Close()
		if r.StatusCode < 400 {
			t.Errorf("plain HTTP on the TLS port must not succeed, got %d", r.StatusCode)
		}
	}
}

func TestIsSelfSignedDistinguishesAnIssuedCertificate(t *testing.T) {
	_, leaf, err := newEphemeralTLS("")
	if err != nil {
		t.Fatal(err)
	}
	if !isSelfSigned(leaf) {
		t.Fatal("ephemeral certificate should be self-signed")
	}
	if isSelfSigned(nil) {
		t.Error("nil is not self-signed")
	}
}

func TestLeafFromFileReadsTheFirstCertificate(t *testing.T) {
	certPath, _, want := writeServerCert(t)
	got, err := leafFromFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Error("leafFromFile returned a different certificate")
	}
	if _, err := leafFromFile(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Error("a missing file must be an error")
	}
}

// writeServerCert writes a self-signed ECDSA leaf for 127.0.0.1 and returns
// the cert and key paths plus the parsed certificate for use as a root.
func writeServerCert(t *testing.T) (certPath, keyPath string, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kairon-ui"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, parsed
}

func TestNewServerTLSServesHTTPSAndRefusesOldTLS(t *testing.T) {
	certPath, keyPath, leaf := writeServerCert(t)
	cfg, _, err := newServerTLS(slog.New(slog.NewTextHandler(io.Discard, nil)), certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	// A real listener: httptest's StartTLS injects its own certificate when
	// Certificates is empty, which would hide the GetCertificate path.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }),
		TLSConfig:         cfg,
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer func() { _ = srv.Close() }()
	url := "https://" + ln.Addr().String()

	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("HTTPS request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" || resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("body=%q tls=%+v", body, resp.TLS)
	}

	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS11}}}
	if oldResp, err := old.Get(url); err == nil {
		_ = oldResp.Body.Close()
		t.Error("a TLS 1.1 client must be refused")
	}
}

func TestNewServerTLSFailsOnABadPair(t *testing.T) {
	certPath, _, _ := writeServerCert(t)
	if _, _, err := newServerTLS(slog.New(slog.NewTextHandler(io.Discard, nil)), certPath, "/does/not/exist.pem"); err == nil {
		t.Error("a missing key must be a startup failure")
	}
}

func TestEphemeralSANsAreDeduplicated(t *testing.T) {
	dns, ips := ephemeralSANs("localhost,127.0.0.1,::1,extra.test,extra.test")
	count := func(want string) int {
		n := 0
		for _, d := range dns {
			if d == want {
				n++
			}
		}
		for _, ip := range ips {
			if ip.String() == want {
				n++
			}
		}
		return n
	}
	for _, name := range []string{"localhost", "127.0.0.1", "::1", "extra.test"} {
		if got := count(name); got != 1 {
			t.Errorf("%s appears %d times, want 1 (dns=%v ips=%v)", name, got, dns, ips)
		}
	}
}

func TestConsoleTLSConfigPinsTheServerName(t *testing.T) {
	path := writeSelfSignedCAPEM(t)
	cfg, err := consoleTLSConfig(path, "kairon-node")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "kairon-node" {
		t.Errorf("ServerName = %q; a fixed name lets one certificate serve every node IP", cfg.ServerName)
	}
	if cfg.MinVersion < tls.VersionTLS12 || cfg.RootCAs == nil {
		t.Errorf("config must verify against the CA with TLS 1.2+: %+v", cfg)
	}
	cfg, err = consoleTLSConfig(path, "")
	if err != nil || cfg.ServerName != "" {
		t.Errorf("an empty name verifies the dialled host: %+v %v", cfg, err)
	}
}
