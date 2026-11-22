// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// atlasPathRE is the allowlist of Atlas gateway GET resources the dashboard
// may read through the proxy. Anything else is a 404: the browser never gets a
// generic window onto the storage control plane.
var atlasPathRE = regexp.MustCompile(`^(clusters|metrics/summary|pools|osds|alerts|volumes|snapshots|backups|schedules|jobs|events|` +
	`clusters/[A-Za-z0-9_.-]+/health|dr/mirrors/[A-Za-z0-9_.-]+/status)$`)

// atlasOwnerPrefix is the owner Kairon stamps on every volume it asks Atlas
// for ("kairon/machine/<ns>/<name>"); the proxy shows only those, so a shared
// Atlas gateway's other products' volumes never leak into this dashboard.
const atlasOwnerPrefix = "kairon/"

func (s *Server) atlasEnabled() bool { return s.AtlasURL != "" }

// handleAtlas is a read-only, allowlisted proxy to the Atlas storage gateway
// (GET /api/v1/atlas/{path...}). The Atlas bearer token stays server-side.
func (s *Server) handleAtlas(w http.ResponseWriter, r *http.Request) {
	if !s.atlasEnabled() {
		writeError(w, http.StatusNotImplemented, "atlas is not configured on this server (set KAIRON_UI_ATLAS_URL)")
		return
	}
	// Atlas data spans tenants; with namespace scoping on, only admins see it
	// (same rule as the /api/v1/nodes routes in actionauth.go).
	if s.NamespaceScopingEnabled && !s.isAdminIdentity(r.Context(), usernameFromContext(r.Context())) {
		writeError(w, http.StatusForbidden, "storage views require administrator access")
		return
	}
	path := r.PathValue("path")
	if !atlasPathRE.MatchString(path) {
		writeError(w, http.StatusNotFound, "unknown atlas resource")
		return
	}
	target := strings.TrimRight(s.AtlasURL, "/") + "/api/atlas/v1/" + path
	if q := r.URL.RawQuery; q != "" {
		if _, err := url.ParseQuery(q); err != nil {
			writeError(w, http.StatusBadRequest, "invalid query")
			return
		}
		target += "?" + q
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Accept", "application/json")
	if s.AtlasToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.AtlasToken)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "atlas unreachable: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "atlas response: "+err.Error())
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Surface Atlas's own status (401 bad token, 404, ...) but never its body
		// verbatim beyond the error text.
		writeError(w, http.StatusBadGateway, "atlas returned HTTP "+resp.Status)
		return
	}
	if path == "volumes" {
		body = filterAtlasVolumes(body)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// filterAtlasVolumes keeps only entries owned by Kairon. It understands a bare
// array or an object holding the array under items/volumes; any other shape is
// returned unchanged (nothing to filter safely).
func filterAtlasVolumes(raw []byte) []byte {
	keep := func(v any) bool {
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		owner, _ := m["owner"].(string)
		return strings.HasPrefix(owner, atlasOwnerPrefix)
	}
	filter := func(list []any) []any {
		out := make([]any, 0, len(list))
		for _, v := range list {
			if keep(v) {
				out = append(out, v)
			}
		}
		return out
	}
	var arr []any
	if json.Unmarshal(raw, &arr) == nil {
		b, _ := json.Marshal(filter(arr))
		return b
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"items", "volumes"} {
			if list, ok := obj[k].([]any); ok {
				obj[k] = filter(list)
				b, _ := json.Marshal(obj)
				return b
			}
		}
	}
	return raw
}
