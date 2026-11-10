// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"net/http"
	"sort"
	"strings"
)

func (s *Server) withActionAuthorization(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := usernameFromContext(r.Context())
		admin := s.isAdminIdentity(r.Context(), user)
		if s.NamespaceScopingEnabled && user == "" {
			writeError(w, http.StatusForbidden, "namespace scoping requires a named session")
			return
		}
		// Nodes and runtime/node proxy routes expose data across namespaces.
		if s.NamespaceScopingEnabled && !admin && (strings.HasPrefix(r.URL.Path, "/api/v1/nodes") || strings.HasPrefix(r.URL.Path, "/api/v1/atlas")) {
			writeError(w, http.StatusForbidden, "node operations require administrator access")
			return
		}
		account, found := s.findUser(user)
		if found && account.Role != "" && account.Role != "operator" && !admin {
			if account.Role != "viewer" || (r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.HasPrefix(r.URL.Path, "/api/v1/auth/")) || viewerForbiddenRead(r) {
				writeError(w, http.StatusForbidden, "role does not allow this action")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	allowed := map[string]bool{}
	user := usernameFromContext(r.Context())
	if !s.NamespaceScopingEnabled || s.isAdminIdentity(r.Context(), user) {
		machines, err := s.Kube.ListMachines(r.Context())
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		for _, m := range machines {
			allowed[m.Namespace()] = true
		}
		allowed["default"] = true
	} else {
		if u, ok := s.findUser(user); ok {
			for _, ns := range u.Namespaces {
				allowed[ns] = true
			}
		}
		if s.OIDC != nil {
			for _, g := range groupsFromContext(r.Context()) {
				for _, ns := range s.OIDC.NamespaceGroups[g] {
					allowed[ns] = true
				}
			}
		}
	}
	names := make([]string, 0, len(allowed))
	for ns := range allowed {
		names = append(names, ns)
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, names)
}

// viewerForbiddenRead reports GET routes a read-only "viewer" must not use even
// though they do not change cluster state: a raw TCP tunnel into a guest (it
// also wakes hibernated VMs) and a full disk-image download expose far more than
// status and listings.
func viewerForbiddenRead(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasSuffix(p, "/portforward") ||
		(strings.HasPrefix(p, "/api/v1/exports/") && strings.HasSuffix(p, "/download"))
}
