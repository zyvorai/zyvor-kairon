// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Adapter is Kairon's stable local extension point for hypervisor-specific
// migration support. It speaks HTTP over a root-owned Unix socket. FluxVM does
// not currently expose this contract; deploying an adapter is therefore an
// explicit capability rather than an assumed FluxVM API.
type Adapter struct {
	Socket string
	HTTP   *http.Client
}

func NewAdapter(socket string) *Adapter {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &Adapter{Socket: socket, HTTP: &http.Client{Transport: transport, Timeout: 20 * time.Second}}
}

func (a *Adapter) Prepare(ctx context.Context, session Session) (PrepareResult, error) {
	var out PrepareResult
	err := a.do(ctx, http.MethodPost, "/v1/destination/prepare", PrepareRequest{Session: session}, &out)
	return out, err
}
func (a *Adapter) Commit(ctx context.Context, session Session) error {
	return a.do(ctx, http.MethodPost, "/v1/destination/"+url.PathEscape(session.ID)+"/commit", map[string]any{}, nil)
}
func (a *Adapter) Abort(ctx context.Context, session Session) error {
	return a.do(ctx, http.MethodPost, "/v1/destination/"+url.PathEscape(session.ID)+"/abort", map[string]any{}, nil)
}
func (a *Adapter) Start(ctx context.Context, req SourceRequest) (TransferStatus, error) {
	var out TransferStatus
	err := a.do(ctx, http.MethodPost, "/v1/source/start", req, &out)
	return out, err
}
func (a *Adapter) Status(ctx context.Context, session Session, transferID string) (TransferStatus, error) {
	var out TransferStatus
	path := "/v1/source/" + url.PathEscape(session.ID) + "/" + url.PathEscape(transferID)
	err := a.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}
func (a *Adapter) AbortSource(ctx context.Context, session Session, transferID string) error {
	path := "/v1/source/" + url.PathEscape(session.ID) + "/" + url.PathEscape(transferID) + "/abort"
	return a.do(ctx, http.MethodPost, path, map[string]any{}, nil)
}

// Capabilities reports the adapter's optional features. An adapter that
// predates GET /v1/capabilities (404) has none.
func (a *Adapter) Capabilities(ctx context.Context) (Capabilities, error) {
	var out Capabilities
	err := a.do(ctx, http.MethodGet, "/v1/capabilities", nil, &out)
	var se *adapterStatusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return Capabilities{}, nil
	}
	return out, err
}

func (a *Adapter) sourcePath(session Session, transferID, action string) string {
	return "/v1/source/" + url.PathEscape(session.ID) + "/" + url.PathEscape(transferID) + "/" + action
}

func (a *Adapter) Tune(ctx context.Context, session Session, transferID string, req TuneRequest) (TransferStatus, error) {
	var out TransferStatus
	err := a.do(ctx, http.MethodPost, a.sourcePath(session, transferID, "tune"), req, &out)
	return out, err
}

func (a *Adapter) StartPostCopy(ctx context.Context, session Session, transferID string) (TransferStatus, error) {
	var out TransferStatus
	err := a.do(ctx, http.MethodPost, a.sourcePath(session, transferID, "postcopy"), map[string]any{}, &out)
	return out, err
}

func (a *Adapter) Finish(ctx context.Context, session Session, transferID string) error {
	return a.do(ctx, http.MethodPost, a.sourcePath(session, transferID, "finish"), map[string]any{}, nil)
}

// Abort satisfies SourceDriver. Destination abort uses the same method name,
// so SourceDriver is implemented through SourceAdapter below.
type SourceAdapter struct{ Adapter *Adapter }

func (a SourceAdapter) Start(ctx context.Context, req SourceRequest) (TransferStatus, error) {
	return a.Adapter.Start(ctx, req)
}
func (a SourceAdapter) Status(ctx context.Context, s Session, id string) (TransferStatus, error) {
	return a.Adapter.Status(ctx, s, id)
}
func (a SourceAdapter) Abort(ctx context.Context, s Session, id string) error {
	return a.Adapter.AbortSource(ctx, s, id)
}
func (a SourceAdapter) Capabilities(ctx context.Context) (Capabilities, error) {
	return a.Adapter.Capabilities(ctx)
}
func (a SourceAdapter) Tune(ctx context.Context, s Session, id string, req TuneRequest) (TransferStatus, error) {
	return a.Adapter.Tune(ctx, s, id, req)
}
func (a SourceAdapter) StartPostCopy(ctx context.Context, s Session, id string) (TransferStatus, error) {
	return a.Adapter.StartPostCopy(ctx, s, id)
}
func (a SourceAdapter) Finish(ctx context.Context, s Session, id string) error {
	return a.Adapter.Finish(ctx, s, id)
}

type adapterStatusError struct {
	path string
	code int
	body string
}

func (e *adapterStatusError) Error() string {
	return fmt.Sprintf("migration adapter %s: HTTP %d: %s", e.path, e.code, e.body)
}

func (a *Adapter) do(ctx context.Context, method, path string, body any, out any) error {
	if strings.TrimSpace(a.Socket) == "" {
		return ErrUnsupported
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("migration adapter %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &adapterStatusError{path: path, code: resp.StatusCode, body: strings.TrimSpace(string(data))}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode migration adapter response: %w", err)
		}
	}
	return nil
}
