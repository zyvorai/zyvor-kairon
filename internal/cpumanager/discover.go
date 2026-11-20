// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package cpumanager proves a node's pinnable CPU set from sysfs and
// kubelet cpu_manager_state. An operator label is not proof. Discovery
// fails closed when the online set, the kubelet state, or the reserved
// set is missing, so kairon-node does not publish
// kairon.zyvor.dev/pinnable-cpus.
package cpumanager

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

const (
	// AnnSource marks a pinnable-cpus label this agent published.
	// An operator label without this annotation is left alone.
	AnnSource = "kairon.zyvor.dev/pinnable-cpus-source"
	// SourceDiscovered is the only value this agent writes.
	SourceDiscovered = "discovered"
)

// Input is the files and reserved set for one node.
type Input struct {
	OnlinePath string
	StatePath  string
	// Reserved is the cpuset kept off the pinnable set (OS, kubelet,
	// kairon-node). Required: kubelet's reserved CPUs are not in
	// cpu_manager_state, and defaultCpuSet is the shared pool, not a
	// reservation.
	Reserved string
}

// Result is a cpuset list safe to publish, or Refused when the node
// cannot prove a set.
type Result struct {
	CPUs    []uint32
	List    string
	Refused string
}

type cpuManagerState struct {
	Entries map[string]map[string]string `json:"entries"`
}

// Discover returns online CPUs minus kubelet exclusive pod allocations
// minus Reserved. defaultCpuSet is ignored: it is every CPU not
// exclusively assigned, so subtracting it would leave nothing. An
// unreadable or unparseable input refuses rather than guessing.
func Discover(in Input) (Result, error) {
	if strings.TrimSpace(in.Reserved) == "" {
		return Result{Refused: "reserved cpus not configured (--reserved-cpus)"}, nil
	}
	onlineRaw, err := os.ReadFile(in.OnlinePath)
	if err != nil {
		return Result{Refused: fmt.Sprintf("online cpus: %v", err)}, nil
	}
	online, err := model.ParseCPUList(strings.TrimSpace(string(onlineRaw)))
	if err != nil || len(online) == 0 {
		return Result{Refused: "online cpus missing or unparseable"}, nil
	}
	taken := map[uint32]struct{}{}
	if err := add(taken, in.Reserved); err != nil {
		return Result{Refused: fmt.Sprintf("reserved cpus: %v", err)}, nil
	}
	stateRaw, err := os.ReadFile(in.StatePath)
	if err != nil {
		return Result{Refused: fmt.Sprintf("cpu_manager_state: %v", err)}, nil
	}
	var st cpuManagerState
	if err := json.Unmarshal(stateRaw, &st); err != nil {
		return Result{Refused: fmt.Sprintf("cpu_manager_state: %v", err)}, nil
	}
	for pod, containers := range st.Entries {
		for container, set := range containers {
			if err := add(taken, set); err != nil {
				return Result{Refused: fmt.Sprintf("cpu_manager_state entry %s/%s: %v", pod, container, err)}, nil
			}
		}
	}
	free := make([]uint32, 0, len(online))
	for _, cpu := range online {
		if _, ok := taken[cpu]; ok {
			continue
		}
		free = append(free, cpu)
	}
	sort.Slice(free, func(i, j int) bool { return free[i] < free[j] })
	list, err := model.FormatCPULabel(free)
	if err != nil {
		return Result{Refused: err.Error()}, nil
	}
	return Result{CPUs: free, List: list}, nil
}

// LabelPatch publishes or clears the pinnable label. owned is true when
// this agent already set AnnSource. An operator label (present, not owned)
// is not overwritten. nil means no patch.
func LabelPatch(current, source, list string, owned bool, refused bool) map[string]any {
	if current != "" && !owned {
		return nil
	}
	if refused || list == "" {
		if current == "" && source == "" {
			return nil
		}
		return map[string]any{"metadata": map[string]any{
			"labels":      map[string]any{model.PinnableCPUsLabel: nil},
			"annotations": map[string]any{AnnSource: nil},
		}}
	}
	if current == list && source == SourceDiscovered {
		return nil
	}
	return map[string]any{"metadata": map[string]any{
		"labels":      map[string]any{model.PinnableCPUsLabel: list},
		"annotations": map[string]any{AnnSource: SourceDiscovered},
	}}
}

func add(dst map[uint32]struct{}, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	cpus, err := model.ParseCPUList(raw)
	if err != nil {
		return err
	}
	for _, c := range cpus {
		dst[c] = struct{}{}
	}
	return nil
}
