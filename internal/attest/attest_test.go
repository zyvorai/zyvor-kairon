// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package attest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sevabi "github.com/google/go-sev-guest/abi"
	sevpb "github.com/google/go-sev-guest/proto/sevsnp"
	sevtest "github.com/google/go-sev-guest/testing"
	sevverify "github.com/google/go-sev-guest/verify"
	sevdata "github.com/google/go-sev-guest/verify/testdata"
	sevtrust "github.com/google/go-sev-guest/verify/trust"
	tdxabi "github.com/google/go-tdx-guest/abi"
	tdxpb "github.com/google/go-tdx-guest/proto/tdx"
	tdxdata "github.com/google/go-tdx-guest/testing/testdata"
	tdxverify "github.com/google/go-tdx-guest/verify"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func sampleSNPVerifier() *Verifier {
	getter := sevtest.SimpleGetter(map[string][]byte{
		"https://kdsintf.amd.com/vcek/v1/Milan/cert_chain": sevtrust.AskArkMilanVcekBytes,
		"https://kdsintf.amd.com/vcek/v1/Milan/3ac3fe21e13fb0990eb28a802e3fb6a29483a6b0753590c951bdd3b8e53786184ca39e359669a2b76a1936776b564ea464cdce40c05f63c9b610c5068b006b5d?blSPL=2&teeSPL=0&snpSPL=5&ucodeSPL=68": sevdata.VcekBytes,
	})
	return &Verifier{SNP: &sevverify.Options{
		Getter:  getter,
		Product: &sevpb.SevProduct{Name: sevpb.SevProduct_SEV_PRODUCT_MILAN, MachineStepping: &wrapperspb.UInt32Value{Value: 0}},
	}}
}

func reportDataOf(t *testing.T, raw []byte) [64]byte {
	rep, err := sevabi.ReportToProto(raw)
	if err != nil {
		t.Fatal(err)
	}
	var want [64]byte
	copy(want[:], rep.GetReportData())
	return want
}

func TestVerifySNPSampleReport(t *testing.T) {
	raw := sevdata.AttestationBytes[:sevabi.ReportSize]
	v := sampleSNPVerifier()
	if err := v.Verify(context.Background(), KindSNP, raw, reportDataOf(t, raw)); err == nil || err.Error() != "sev-snp guest policy allows debug" {
		t.Fatalf("the sample is a debug guest; want only the policy refusal, got %v", err)
	}
	v.AllowDebug = true
	if err := v.Verify(context.Background(), KindSNP, raw, reportDataOf(t, raw)); err != nil {
		t.Fatalf("genuine report rejected: %v", err)
	}
	if err := v.Verify(context.Background(), KindSNP, raw, ReportData("uid-1", "n1")); err == nil || !strings.Contains(err.Error(), "report_data") {
		t.Fatalf("nonce mismatch accepted: %v", err)
	}
	tampered := append([]byte(nil), raw...)
	tampered[0x50] ^= 0xff
	if err := v.Verify(context.Background(), KindSNP, tampered, reportDataOf(t, tampered)); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("tampered report accepted: %v", err)
	}
	if err := v.Verify(context.Background(), KindSNP, raw[:100], [64]byte{}); err == nil {
		t.Fatal("short report accepted")
	}
}

func TestVerifyTDXSampleQuote(t *testing.T) {
	v := &Verifier{TDX: &tdxverify.Options{Now: time.Date(2023, time.July, 1, 1, 0, 0, 0, time.UTC)}}
	q, err := tdxabi.QuoteToProto(tdxdata.RawQuote)
	if err != nil {
		t.Fatal(err)
	}
	var want [64]byte
	copy(want[:], q.(*tdxpb.QuoteV4).GetTdQuoteBody().GetReportData())
	if err := v.Verify(context.Background(), KindTDX, tdxdata.RawQuote, want); err != nil {
		t.Fatalf("genuine quote rejected: %v", err)
	}
	if err := v.Verify(context.Background(), KindTDX, tdxdata.RawQuote, ReportData("uid-1", "n1")); err == nil || !strings.Contains(err.Error(), "report_data") {
		t.Fatal("nonce mismatch accepted")
	}
	tampered := append([]byte(nil), tdxdata.RawQuote...)
	tampered[200] ^= 0xff
	if err := v.Verify(context.Background(), KindTDX, tampered, want); err == nil {
		t.Fatal("tampered quote accepted")
	}
}

func TestReportDataBindsUIDAndNonce(t *testing.T) {
	a := ReportData("uid-1", "n1")
	if a == ReportData("uid-2", "n1") || a == ReportData("uid-1", "n2") || a != ReportData("uid-1", "n1") {
		t.Fatal("report data must depend on uid and nonce only")
	}
	if n := NewNonce(); len(n) != 64 || n == NewNonce() {
		t.Fatal(n)
	}
	if err := (&Verifier{}).Verify(context.Background(), "sev", nil, a); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestCachingGetter(t *testing.T) {
	calls := 0
	g := &cachingGetter{inner: getterFunc(func(string) ([]byte, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("second fetch")
		}
		return []byte("cert"), nil
	}), ttl: time.Hour}
	for i := 0; i < 3; i++ {
		if b, err := g.Get("u"); err != nil || string(b) != "cert" {
			t.Fatal(b, err)
		}
	}
}

type getterFunc func(string) ([]byte, error)

func (f getterFunc) Get(url string) ([]byte, error) { return f(url) }
