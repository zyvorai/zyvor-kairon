// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package attest verifies SEV-SNP attestation reports and TDX quotes and
// binds them to one Machine. Signature and certificate-chain checks are
// done by go-sev-guest and go-tdx-guest; this package adds the nonce
// binding and the policy the agent plane needs (no debug guests).
package attest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	sevabi "github.com/google/go-sev-guest/abi"
	sevpb "github.com/google/go-sev-guest/proto/sevsnp"
	sevverify "github.com/google/go-sev-guest/verify"
	sevtrust "github.com/google/go-sev-guest/verify/trust"
	tdxabi "github.com/google/go-tdx-guest/abi"
	tdxpb "github.com/google/go-tdx-guest/proto/tdx"
	tdxverify "github.com/google/go-tdx-guest/verify"
)

const (
	KindSNP = "sev-snp"
	KindTDX = "tdx"
)

// NewNonce returns a fresh random nonce for one attestation round.
func NewNonce() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ReportData is the 64 bytes the guest must put in REPORT_DATA. Binding
// the Machine UID means a report for one Machine cannot seal another,
// and the per-round nonce means an old report cannot be replayed.
func ReportData(uid, nonce string) [64]byte {
	return sha512.Sum512([]byte("kairon-attestation\x00" + uid + "\x00" + nonce))
}

// Verifier checks evidence. The zero value fetches missing SNP
// certificates from AMD KDS (cached) and verifies TDX quotes against the
// PCK chain embedded in the quote with Intel's root.
type Verifier struct {
	// SNP and TDX override library options; a copy is used per call.
	SNP *sevverify.Options
	TDX *tdxverify.Options
	// AllowDebug accepts debug-enabled guests, whose memory the host can
	// read. Lab use only.
	AllowDebug bool

	once   sync.Once
	getter sevtrust.HTTPSGetter
}

// Verify checks raw evidence of kind and that its REPORT_DATA is want.
// For sev-snp, raw is the 1184-byte report, optionally followed by the
// extended-report certificate table.
func (v *Verifier) Verify(ctx context.Context, kind string, raw []byte, want [64]byte) error {
	switch strings.ToLower(kind) {
	case KindSNP:
		return v.verifySNP(ctx, raw, want)
	case KindTDX:
		return v.verifyTDX(raw, want)
	}
	return fmt.Errorf("attestation kind %q is not sev-snp or tdx", kind)
}

func (v *Verifier) verifySNP(ctx context.Context, raw []byte, want [64]byte) error {
	if len(raw) < sevabi.ReportSize {
		return fmt.Errorf("sev-snp report is %d bytes, want at least %d", len(raw), sevabi.ReportSize)
	}
	var att *sevpb.Attestation
	if len(raw) == sevabi.ReportSize {
		rep, err := sevabi.ReportToProto(raw)
		if err != nil {
			return fmt.Errorf("sev-snp report: %w", err)
		}
		att = &sevpb.Attestation{Report: rep}
	} else {
		var err error
		if att, err = sevabi.ReportCertsToProto(raw); err != nil {
			return fmt.Errorf("sev-snp report: %w", err)
		}
	}
	opts := v.snpOptions()
	if att.GetCertificateChain().GetVcekCert() == nil && att.GetCertificateChain().GetVlekCert() == nil {
		full, err := sevverify.GetAttestationFromReportContext(ctx, att.Report, opts)
		if err != nil {
			return fmt.Errorf("sev-snp certificates: %w", err)
		}
		att = full
	}
	if err := sevverify.SnpAttestationContext(ctx, att, opts); err != nil {
		return fmt.Errorf("sev-snp signature: %w", err)
	}
	if !bytes.Equal(att.Report.GetReportData(), want[:]) {
		return errors.New("sev-snp report_data does not match this Machine's nonce")
	}
	pol, err := sevabi.ParseSnpPolicy(att.Report.GetPolicy())
	if err != nil {
		return fmt.Errorf("sev-snp policy: %w", err)
	}
	if pol.Debug && !v.AllowDebug {
		return errors.New("sev-snp guest policy allows debug")
	}
	return nil
}

func (v *Verifier) snpOptions() *sevverify.Options {
	v.once.Do(func() { v.getter = &cachingGetter{inner: sevtrust.DefaultHTTPSGetter(), ttl: 24 * time.Hour} })
	var opts sevverify.Options
	if v.SNP != nil {
		opts = *v.SNP
	}
	if opts.Getter == nil {
		opts.Getter = v.getter
	}
	return &opts
}

func (v *Verifier) verifyTDX(raw []byte, want [64]byte) error {
	q, err := tdxabi.QuoteToProto(raw)
	if err != nil {
		return fmt.Errorf("tdx quote: %w", err)
	}
	quote, ok := q.(*tdxpb.QuoteV4)
	if !ok {
		return fmt.Errorf("tdx quote type %T is not supported", q)
	}
	opts := tdxverify.DefaultOptions()
	if v.TDX != nil {
		o := *v.TDX
		opts = &o
	}
	if err := tdxverify.TdxQuote(quote, opts); err != nil {
		return fmt.Errorf("tdx signature: %w", err)
	}
	body := quote.GetTdQuoteBody()
	if !bytes.Equal(body.GetReportData(), want[:]) {
		return errors.New("tdx report_data does not match this Machine's nonce")
	}
	if a := body.GetTdAttributes(); len(a) > 0 && a[0]&1 != 0 && !v.AllowDebug {
		return errors.New("tdx guest has TD debug enabled")
	}
	return nil
}

// cachingGetter keeps KDS certificates; AMD rate-limits KDS and a VCEK
// only changes with a TCB update.
type cachingGetter struct {
	inner sevtrust.HTTPSGetter
	ttl   time.Duration
	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	body []byte
	at   time.Time
}

func (g *cachingGetter) Get(url string) ([]byte, error) {
	return g.GetContext(context.Background(), url)
}

func (g *cachingGetter) GetContext(ctx context.Context, url string) ([]byte, error) {
	g.mu.Lock()
	if c, ok := g.cache[url]; ok && time.Since(c.at) < g.ttl {
		g.mu.Unlock()
		return c.body, nil
	}
	g.mu.Unlock()
	var body []byte
	var err error
	if cg, ok := g.inner.(sevtrust.ContextHTTPSGetter); ok {
		body, err = cg.GetContext(ctx, url)
	} else {
		body, err = g.inner.Get(url)
	}
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.cache == nil {
		g.cache = map[string]cached{}
	}
	g.cache[url] = cached{body: body, at: time.Now()}
	g.mu.Unlock()
	return body, nil
}
