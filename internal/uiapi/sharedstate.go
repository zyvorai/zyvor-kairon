// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

const (
	sharedStateRevocationPrefix     = "rev-"
	sharedStateLockoutPrefix        = "lock-"
	sharedStatePasswordChangePrefix = "pwc-"
	sharedStateTicketPrefix         = "tik-"
)

type sharedRevocationEntry struct {
	Expires time.Time `json:"expires"`
}

type sharedLockoutEntry struct {
	LockedUntil time.Time `json:"lockedUntil"`
}

type sharedPasswordChangeEntry struct {
	ChangedAt time.Time `json:"changedAt"`
}

// sharedTicketEntry carries the plaintext username, unlike the other three
// entry types -- handleConsole's audit log needs to attribute a session to
// a person even when the ticket was consumed on a different replica than
// the one that minted it (see console.go). A username isn't a secret; the
// ticket itself is, which is exactly why the *key* it's stored under is a
// one-way hash (see sharedSecretKey below), not the ticket value itself.
type sharedTicketEntry struct {
	Username string `json:"username"`
	// Namespace/Name bind the ticket to the one Machine it was issued
	// for -- handleConsole checks these against its own URL path params
	// before ever dialing anything, so a ticket minted for one Machine
	// can't be replayed against a different one's console endpoint within
	// its short TTL. Neither is a secret -- unlike Username (kept only
	// for audit attribution, see the doc comment above), these exist for
	// this check specifically, not for logging.
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Kind is "vnc" or "text" -- which consoleproxy relay route
	// handleConsole should dial, see consoleTicketState's own doc comment
	// in console.go.
	Kind    string    `json:"kind"`
	Expires time.Time `json:"expires"`
}

// sha256Hex is the general-purpose one-way hash behind every shared-state
// key that's derived from a real credential (a session token or console
// ticket) -- so a revoked/consumed value is never itself readable by
// anyone with "get configmaps" RBAC in kairon-ui's namespace, only
// checkable against a value they'd already have to already possess.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// sharedSecretKey builds a ConfigMap key for a real credential value --
// one-way hashed, see sha256Hex.
func sharedSecretKey(prefix, secret string) string {
	return prefix + sha256Hex(secret)
}

// sharedUsernameKey builds a ConfigMap key for a username -- hex-encoded,
// not hashed, since a username isn't a secret and this encoding needs to
// be reversible: mergeSharedConfigMap below has to recover the username
// to key kairon-ui's existing username-indexed local state
// (loginAttempts/passwordChangedAt) correctly. Hex-encoding (rather than
// using the username literally) only exists to guarantee a valid
// ConfigMap key regardless of what characters an operator's username
// happens to contain.
func sharedUsernameKey(prefix, username string) string {
	return prefix + hex.EncodeToString([]byte(username))
}

func usernameFromSharedKey(prefix, key string) (string, bool) {
	raw, err := hex.DecodeString(strings.TrimPrefix(key, prefix))
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func sharedRevocationKey(token string) string {
	return sharedSecretKey(sharedStateRevocationPrefix, token)
}
func sharedTicketKey(ticket string) string { return sharedSecretKey(sharedStateTicketPrefix, ticket) }
func sharedLockoutKey(username string) string {
	return sharedUsernameKey(sharedStateLockoutPrefix, username)
}
func sharedPasswordChangeKey(username string) string {
	return sharedUsernameKey(sharedStatePasswordChangePrefix, username)
}

// writeSharedState/deleteSharedState best-effort merge-patch one key of
// the shared ConfigMap -- a failure is logged and otherwise ignored, same
// posture as every other best-effort status update in this codebase (e.g.
// internal/controller/fencing.go's detectUnreachableNodes): this
// replica's own in-memory state (already updated by the caller before
// either of these runs) is unaffected either way, and a transient write
// failure heals itself on the next mutation or the next successful
// write. Both no-op when SharedStateConfigMapName is unset, the default --
// every mutation then behaves exactly as it did before this file existed.
func (s *Server) writeSharedState(ctx context.Context, key string, value any) {
	if s.SharedStateConfigMapName == "" {
		return
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	if err := s.Kube.PatchConfigMapData(ctx, s.SharedStateNamespace, s.SharedStateConfigMapName, map[string]any{key: string(encoded)}); err != nil && s.Log != nil {
		s.Log.Warn("uiapi shared-state write failed", "key", key, "error", err)
	}
}

func (s *Server) deleteSharedState(ctx context.Context, key string) {
	if s.SharedStateConfigMapName == "" {
		return
	}
	if err := s.Kube.PatchConfigMapData(ctx, s.SharedStateNamespace, s.SharedStateConfigMapName, map[string]any{key: nil}); err != nil && s.Log != nil {
		s.Log.Warn("uiapi shared-state delete failed", "key", key, "error", err)
	}
}

// RunSharedStateSync polls the shared ConfigMap and the Users-backing
// Secret on interval, for the life of the process -- the same "interval
// poll, not watch" posture kairon-controller/kairon-node already use (see
// ARCHITECTURE.md), rather than pulling in a client-go informer for this
// one feature; cmd/kairon-ui/main.go picks the actual interval and starts
// this in its own goroutine, the same shape internal/tlsreload.Watcher.Run
// already uses. A local mutation (revoke, failed login, password change,
// console ticket) still applies to this process's own in-memory state
// immediately and independently of this loop -- same-replica requests see
// it right away, same as a single kairon-ui replica always has.
// Cross-replica visibility lands within one interval, except console
// tickets, which need a synchronous fallback instead (see
// consumeConsoleTicket in console.go) since their own 30s TTL is shorter
// than any reasonable poll interval. No-ops entirely if neither
// SharedStateConfigMapName nor UsersSecretName is configured.
func (s *Server) RunSharedStateSync(ctx context.Context, interval time.Duration) {
	if s.SharedStateConfigMapName == "" && s.UsersSecretName == "" {
		return
	}
	s.syncSharedStateOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncSharedStateOnce(ctx)
		}
	}
}

func (s *Server) syncSharedStateOnce(ctx context.Context) {
	if s.SharedStateConfigMapName != "" {
		s.syncSharedConfigMap(ctx)
	}
	if s.UsersSecretName != "" {
		s.syncUsersFromSecret(ctx)
	}
}

// syncSharedConfigMap merges every entry of the shared ConfigMap into
// this replica's local state (see mergeSharedConfigMapEntry) and, in the
// same pass, prunes any entry whose own expiry has already passed --
// mirroring the lazy-delete-on-check behavior the purely local
// implementation already had (see revokeSession's original doc comment),
// so this never grows without bound. Console tickets are deliberately not
// part of this poll-and-merge pass -- see consumeConsoleTicket in
// console.go for why they need a synchronous, on-demand lookup instead.
func (s *Server) syncSharedConfigMap(ctx context.Context) {
	cm, err := s.Kube.GetConfigMap(ctx, s.SharedStateNamespace, s.SharedStateConfigMapName)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("uiapi shared-state sync failed", "error", err)
		}
		return
	}
	now := time.Now()
	expired := map[string]any{}
	for key, raw := range cm.Data {
		if !s.mergeSharedConfigMapEntry(key, raw, now) {
			expired[key] = nil
		}
	}
	if len(expired) == 0 {
		return
	}
	if err := s.Kube.PatchConfigMapData(ctx, s.SharedStateNamespace, s.SharedStateConfigMapName, expired); err != nil && s.Log != nil {
		s.Log.Warn("uiapi shared-state gc failed", "error", err)
	}
}

// mergeSharedConfigMapEntry merges one ConfigMap key/value into local
// state, returning false if it's expired (the caller prunes it) or
// unparseable/unrecognized (pruned the same way -- a key this version
// doesn't recognize is either stale or from a future version; either way
// there's nothing to merge and no reason to keep it around forever).
//
// Console tickets are the one category this deliberately does NOT merge
// into any local cache (see consumeConsoleTicket in console.go for why
// the shared ConfigMap has to stay the sole source of truth for them once
// sharing is enabled) -- but an unexpired one must still be preserved,
// not pruned, or a periodic sync landing inside a ticket's own 30s
// lifetime would delete a ticket nobody has had the chance to consume
// yet.
func (s *Server) mergeSharedConfigMapEntry(key, raw string, now time.Time) bool {
	switch {
	case strings.HasPrefix(key, sharedStateTicketPrefix):
		var e sharedTicketEntry
		return json.Unmarshal([]byte(raw), &e) == nil && now.Before(e.Expires)
	case strings.HasPrefix(key, sharedStateRevocationPrefix):
		var e sharedRevocationEntry
		if json.Unmarshal([]byte(raw), &e) != nil || now.After(e.Expires) {
			return false
		}
		s.revoked.Store(strings.TrimPrefix(key, sharedStateRevocationPrefix), e.Expires)
		return true
	case strings.HasPrefix(key, sharedStateLockoutPrefix):
		var e sharedLockoutEntry
		if json.Unmarshal([]byte(raw), &e) != nil || now.After(e.LockedUntil) {
			return false
		}
		username, ok := usernameFromSharedKey(sharedStateLockoutPrefix, key)
		if !ok {
			return false
		}
		s.mergeLockout(username, e.LockedUntil)
		return true
	case strings.HasPrefix(key, sharedStatePasswordChangePrefix):
		var e sharedPasswordChangeEntry
		if json.Unmarshal([]byte(raw), &e) != nil {
			return false
		}
		username, ok := usernameFromSharedKey(sharedStatePasswordChangePrefix, key)
		if !ok {
			return false
		}
		s.mergePasswordChangedAt(username, e.ChangedAt)
		return true
	default:
		return false
	}
}

// mergeLockout only ever moves a username's lockedUntil forward, never
// back -- safe regardless of poll timing, since every replica's own write
// already went out the moment it happened (see recordLoginResult in
// auth.go); this only fills in what a DIFFERENT replica decided. The
// failure *count* that led to it is deliberately not shared this way (see
// recordLoginResult's own doc comment for why) -- maxLoginAttempts is
// evaluated per-replica, only the resulting lockout itself is shared.
func (s *Server) mergeLockout(username string, lockedUntil time.Time) {
	s.withLoginState(username, func(st *loginAttemptState) {
		if lockedUntil.After(st.lockedUntil) {
			st.lockedUntil = lockedUntil
		}
	})
}

// mergePasswordChangedAt only ever moves a username's recorded reset time
// forward, same reasoning as mergeLockout.
func (s *Server) mergePasswordChangedAt(username string, changedAt time.Time) {
	if v, ok := s.passwordChangedAt.Load(username); ok {
		if existing, _ := v.(time.Time); !changedAt.After(existing) {
			return
		}
	}
	s.passwordChangedAt.Store(username, changedAt)
}

// syncUsersFromSecret re-reads UsersSecretName and replaces Server.Users
// if its content differs from what this replica already has in memory --
// without this, a password change persisted by a DIFFERENT replica's
// setPasswordHash would never be seen here, and this replica would keep
// authenticating logins against the old hash indefinitely (Users is
// otherwise only loaded once, at process startup). Concurrent password
// changes to two DIFFERENT usernames on two DIFFERENT replicas within the
// same sync interval can still race and have one silently overwrite the
// other -- persistUsers replaces the whole users.json blob, not a
// per-user key, so this sync can't protect against that. See
// docs/guides/kairon-ui-ha.md.
func (s *Server) syncUsersFromSecret(ctx context.Context) {
	secret, err := s.Kube.GetSecret(ctx, s.UsersSecretNamespace, s.UsersSecretName)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("uiapi users sync failed", "error", err)
		}
		return
	}
	key := s.UsersSecretKey
	if key == "" {
		key = "users.json"
	}
	raw, ok := secret.Data[key]
	if !ok {
		return
	}
	var users []User
	if err := json.Unmarshal(raw, &users); err != nil {
		if s.Log != nil {
			s.Log.Warn("uiapi users sync: invalid users.json in backing secret", "error", err)
		}
		return
	}
	s.usersMu.Lock()
	changed := !reflect.DeepEqual(s.Users, users)
	if changed {
		s.Users = users
	}
	s.usersMu.Unlock()
	if changed && s.Log != nil {
		s.Log.Info("uiapi users refreshed from backing secret", "count", len(users))
	}
}
