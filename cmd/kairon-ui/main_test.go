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
	cfg, err := consoleTLSConfig("")
	if err != nil {
		t.Fatalf("expected no error for an empty path, got %v", err)
	}
	if cfg != nil {
		t.Fatal("expected a nil TLS config when no CA is configured")
	}
}

func TestConsoleTLSConfigLoadsARealCA(t *testing.T) {
	path := writeSelfSignedCAPEM(t)
	cfg, err := consoleTLSConfig(path)
	if err != nil {
		t.Fatalf("consoleTLSConfig: %v", err)
	}
	if cfg == nil || cfg.RootCAs == nil {
		t.Fatal("expected a non-nil TLS config with a populated RootCAs pool")
	}
}

func TestConsoleTLSConfigRejectsMissingOrInvalidFile(t *testing.T) {
	if _, err := consoleTLSConfig("/nonexistent/ca.pem"); err == nil {
		t.Fatal("expected an error for a nonexistent CA file")
	}
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write garbage file: %v", err)
	}
	if _, err := consoleTLSConfig(garbage); err == nil {
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

func TestValidateTLSFlagsIsAllOrNothing(t *testing.T) {
	cases := []struct {
		cert, key string
		want      bool
		wantErr   bool
	}{
		{"", "", false, false},
		{"/c.pem", "/k.pem", true, false},
		{"/c.pem", "", false, true},
		{"", "/k.pem", false, true},
	}
	for _, c := range cases {
		got, err := validateTLSFlags(c.cert, c.key)
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("validateTLSFlags(%q, %q) = %v, %v; want %v, err=%v", c.cert, c.key, got, err, c.want, c.wantErr)
		}
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
	if _, err := old.Get(url); err == nil {
		t.Error("a TLS 1.1 client must be refused")
	}
}

func TestNewServerTLSFailsOnABadPair(t *testing.T) {
	certPath, _, _ := writeServerCert(t)
	if _, _, err := newServerTLS(slog.New(slog.NewTextHandler(io.Discard, nil)), certPath, "/does/not/exist.pem"); err == nil {
		t.Error("a missing key must be a startup failure")
	}
}
