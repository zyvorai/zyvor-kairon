// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

const exportTicketKind = "export"

type createExportRequest struct {
	Name        string `json:"name"`
	MachineName string `json:"machineName"`
	Format      string `json:"format"`
	TTL         string `json:"ttl"`
}

func (s *Server) handleListExports(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineExportsNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if items == nil {
		items = []model.MachineExport{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleCreateExport(w http.ResponseWriter, r *http.Request) {
	var req createExportRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	spec := model.MachineExportSpec{MachineName: req.MachineName, Format: req.Format, TTL: req.TTL}
	if err := model.ValidateMachineExportSpec(spec); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ns := namespaceParam(r)
	if req.Name == "" {
		req.Name = resourceName(req.MachineName + "-export-" + time.Now().UTC().Format("20060102-150405"))
	}
	out, err := s.Kube.CreateMachineExport(r.Context(), ns, model.MachineExport{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineExport},
		Metadata: model.ObjectMeta{Name: req.Name, Namespace: ns},
		Spec:     spec,
	})
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleDeleteExport(w http.ResponseWriter, r *http.Request) {
	if err := s.Kube.DeleteMachineExport(r.Context(), r.PathValue("namespace"), r.PathValue("name")); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleExportLink issues a single-use URL for a browser download, which
// can't carry the Authorization header.
func (s *Server) handleExportLink(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	if _, ok := s.authorizeExport(w, r, namespace, name, usernameFromContext(r.Context())); !ok {
		return
	}
	ticket, err := s.issueConsoleTicket(r.Context(), usernameFromContext(r.Context()), namespace, exportTicketKind+"/"+name, exportTicketKind)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate download link")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": "/api/v1/export-links/" + url.PathEscape(ticket)})
}

func (s *Server) handleExportDownload(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	s.streamExport(w, r, namespace, name, usernameFromContext(r.Context()))
}

func (s *Server) handleExportLinkDownload(w http.ResponseWriter, r *http.Request) {
	username, namespace, ticketName, kind, ok := s.consumeConsoleTicket(r.Context(), r.PathValue("ticket"))
	if !ok || kind != exportTicketKind || len(ticketName) <= len(exportTicketKind)+1 || ticketName[:len(exportTicketKind)+1] != exportTicketKind+"/" {
		writeError(w, http.StatusUnauthorized, "invalid or expired download link")
		return
	}
	s.streamExport(w, r, namespace, ticketName[len(exportTicketKind)+1:], username)
}

// authorizeExport loads a Ready export and applies the same per-Machine
// check as the console: a disk image is at least as sensitive.
func (s *Server) authorizeExport(w http.ResponseWriter, r *http.Request, namespace, name, username string) (model.MachineExport, bool) {
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "export download needs the node relay (console relay) to be enabled")
		return model.MachineExport{}, false
	}
	e, err := s.Kube.GetMachineExport(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return e, false
	}
	m, err := s.Kube.GetMachine(r.Context(), namespace, e.Spec.MachineName)
	if err != nil {
		writeUpstreamError(w, err)
		return e, false
	}
	if !s.consoleAuthorized(r.Context(), m, username) {
		writeError(w, http.StatusForbidden, "not authorized to export this machine's disk")
		return e, false
	}
	if e.Status.Phase != "Ready" || e.Status.NodeName == "" {
		writeError(w, http.StatusConflict, fmt.Sprintf("export is not Ready (phase %q)", e.Status.Phase))
		return e, false
	}
	return e, true
}

func (s *Server) streamExport(w http.ResponseWriter, r *http.Request, namespace, name, username string) {
	e, ok := s.authorizeExport(w, r, namespace, name, username)
	if !ok {
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), e.Status.NodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	scheme, hc := "http", &http.Client{}
	if s.ConsoleTLS != nil {
		scheme = "https"
		hc.Transport = &http.Transport{TLSClientConfig: s.ConsoleTLS}
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/export/%s/%s", scheme, nodeAddr, s.ConsolePort, url.PathEscape(namespace), url.PathEscape(name))
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	resp, err := hc.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "connect to node relay: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		writeError(w, http.StatusBadGateway, fmt.Sprintf("node relay: HTTP %d: %s", resp.StatusCode, data))
		return
	}
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "X-Kairon-Disk-Format"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", e.Spec.MachineName+"."+e.Status.Format))
	if s.Log != nil {
		s.Log.Info("uiapi export download", "username", username, "namespace", namespace, "export", name, "range", r.Header.Get("Range"))
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
