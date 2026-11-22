// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package kaironctl is kaironctl's Cobra command tree and every
// subcommand implementation, factored out of cmd/kaironctl so
// cmd/kubectl-kairon can be a second, real binary sharing the exact same
// logic (a kubectl plugin -- `kubectl kairon ARGS` invokes
// `kubectl-kairon ARGS`).
//
// Deliberate dependency exceptions (see docs/DEPENDENCIES.md):
//   - github.com/spf13/cobra — hierarchical help / completion
//   - helm.sh/helm/v3 — install/upgrade/uninstall of the embedded chart
//
// kairon-controller and kairon-node remain Go-stdlib-only.
package kaironctl

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

// stringSliceFlag collects a repeatable flag (e.g. --ssh-key a --ssh-key b)
// into an ordered slice, since the standard flag package has no built-in
// repeatable-flag type.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// parseKeyValues parses a repeated "key=value" flag (e.g. --label/
// --selector) into a map -- the map-flag counterpart to parseForwards's
// "hostPort:guestPort" splitting below. Returns a nil map for an empty
// input, matching how an unset map-shaped JSON field already behaves.
func parseKeyValues(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid key=value %q: want key=value", p)
		}
		out[k] = v
	}
	return out, nil
}

// parseForwards parses "hostPort:guestPort[/proto]" specs, matching FluxVM's
// SLIRP hostfwd syntax (protocol defaults to tcp).
func parseForwards(specs []string) ([]model.PortForward, error) {
	var out []model.PortForward
	for _, s := range specs {
		orig := s
		proto := "tcp"
		if idx := strings.LastIndex(s, "/"); idx >= 0 {
			proto = s[idx+1:]
			s = s[:idx]
		}
		hostStr, guestStr, ok := strings.Cut(s, ":")
		if !ok {
			return nil, fmt.Errorf("invalid --forward %q: want hostPort:guestPort[/proto]", orig)
		}
		hostPort, err := strconv.ParseUint(hostStr, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid --forward %q: bad host port: %w", orig, err)
		}
		guestPort, err := strconv.ParseUint(guestStr, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid --forward %q: bad guest port: %w", orig, err)
		}
		out = append(out, model.PortForward{HostPort: uint16(hostPort), GuestPort: uint16(guestPort), Protocol: proto})
	}
	return out, nil
}

func nsFlag(args []string) (string, []string) {
	ns := "default"
	var out []string
	for i := 0; i < len(args); i++ {
		if (args[i] == "-n" || args[i] == "--namespace") && i+1 < len(args) {
			ns = args[i+1]
			i++
			continue
		}
		out = append(out, args[i])
	}
	return ns, out
}

// selectorFilter narrows items to those whose labels (as returned by the
// caller-supplied accessor) satisfy every key=value pair in selector, via
// model.LabelsMatch -- the exact same "all pairs must match, an empty
// selector matches nothing" rule `kaironctl delete RESOURCE --selector`
// already applies for bulk delete, now shared by `kaironctl get`'s
// read-side equivalent. A generic helper rather than one filter function
// per kind (as cmdGet has a dozen of), since every kind's only difference
// here is how to reach its model.ObjectMeta.Labels -- Go generics can't
// express "any struct with a Metadata field" structurally, so the accessor
// closure stands in for that. An empty selector is a no-op (returns items
// unchanged) rather than the empty-matches-nothing rule below it, since
// unlike delete's --selector, get's is optional and its absence must keep
// meaning "list everything", exactly as it always has.
func selectorFilter[T any](items []T, selector map[string]string, labels func(T) map[string]string) []T {
	if len(selector) == 0 {
		return items
	}
	out := items[:0:0] // fresh backing array: never alias the caller's slice
	for _, it := range items {
		if model.LabelsMatch(labels(it), selector) {
			out = append(out, it)
		}
	}
	return out
}
