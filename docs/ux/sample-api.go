// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

//go:build ignore

// sample-api is a tiny fake Kubernetes API server with sample Kairon data, used
// only to capture dashboard screenshots and clips (see docs/ux/README.md). It
// is NOT a conformance fake: it answers the list/get/patch/delete/create calls
// kairon-ui makes, keeps Machine power state in memory so clips can show
// actions taking effect, and nothing else.
//
//	go run docs/ux/sample-api.go -listen 127.0.0.1:18999
//	KAIRON_KUBE_URL=http://127.0.0.1:18999 kairon-ui -listen 127.0.0.1:18082 -web-dir web/dist
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type obj = map[string]any

var (
	mu    sync.Mutex
	store = map[string][]obj{} // plural -> objects
)

func meta(ns, name string, age time.Duration) obj {
	m := obj{"name": name, "creationTimestamp": time.Now().Add(-age).UTC().Format(time.RFC3339)}
	if ns != "" {
		m["namespace"] = ns
	}
	return m
}

func machine(ns, name, node, phase string, cpu, mem string, cpuPct int, memGiB float64, backend string, age time.Duration) obj {
	power := map[string]string{"Running": "Running", "Paused": "Paused", "Halted": "Halted", "Stopped": "Stopped", "Pending": "Running", "Failed": "Running"}[phase]
	spec := obj{
		"image":      obj{"path": "/var/lib/fluxvm/images/" + strings.SplitN(name, "-", 2)[0] + ".qcow2"},
		"resources":  obj{"cpu": cpu, "memory": mem},
		"runtime":    obj{"backend": backend},
		"network":    obj{"mode": "tap"},
		"powerState": power,
		"guestAgent": obj{"enabled": true, "console": true},
		"priority":   0,
	}
	if node != "" {
		spec["nodeName"] = node
	}
	status := obj{"phase": phase}
	if node != "" {
		status["nodeName"] = node
	}
	if phase == "Running" || phase == "Paused" {
		status["guestIP"] = fmt.Sprintf("10.42.%d.%d", len(ns)%7+1, 10+len(name)*3%200)
		status["resourceUsage"] = obj{"cpuPercent": float64(cpuPct), "memoryBytes": int64(memGiB * (1 << 30))}
	}
	if phase == "Failed" {
		status["message"] = "FluxVM: runtime exited with code 137"
	}
	return obj{"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine", "metadata": meta(ns, name, age), "spec": spec, "status": status}
}

func node(name, ip string, ready, cordoned bool) obj {
	cond := "True"
	if !ready {
		cond = "False"
	}
	spec := obj{}
	if cordoned {
		spec["unschedulable"] = true
		spec["taints"] = []obj{{"key": "node.kubernetes.io/unschedulable", "effect": "NoSchedule"}}
	}
	return obj{"metadata": meta("", name, 90*24*time.Hour), "spec": spec, "status": obj{
		"conditions": []obj{{"type": "Ready", "status": cond}},
		"addresses":  []obj{{"type": "InternalIP", "address": ip}},
	}}
}

func seed() {
	h := time.Hour
	store["machines"] = []obj{
		machine("default", "web-1", "kairon-n1", "Running", "2", "4Gi", 12, 2.1, "qemu", 12*d),
		machine("default", "web-2", "kairon-n2", "Running", "2", "4Gi", 14, 2.4, "qemu", 12*d),
		machine("default", "web-3", "kairon-n3", "Running", "2", "4Gi", 10, 1.9, "qemu", 12*d),
		machine("default", "db-primary", "kairon-n2", "Running", "8", "32Gi", 22, 24.6, "qemu", 41*d),
		machine("default", "db-replica", "kairon-n3", "Running", "8", "32Gi", 13, 22.1, "qemu", 41*d),
		machine("default", "cache-1", "kairon-n1", "Running", "4", "16Gi", 8, 9.7, "cloud-hypervisor", 8*d),
		machine("default", "ci-runner-7", "kairon-n4", "Paused", "4", "8Gi", 0, 3.2, "firecracker", 3*h),
		machine("default", "batch-nightly", "kairon-n1", "Halted", "16", "64Gi", 0, 0, "qemu", 19*d),
		machine("default", "build-box", "", "Pending", "4", "8Gi", 0, 0, "qemu", 2*time.Minute),
		machine("prod", "api-1", "kairon-n2", "Running", "4", "8Gi", 18, 5.3, "qemu", 27*d),
		machine("prod", "api-2", "kairon-n3", "Running", "4", "8Gi", 16, 5.0, "qemu", 27*d),
		machine("prod", "edge-gw", "kairon-n4", "Running", "2", "2Gi", 6, 1.1, "cloud-hypervisor", 60*d),
		machine("staging", "api-1", "kairon-n1", "Running", "2", "4Gi", 4, 1.6, "qemu", 5*d),
		machine("staging", "legacy-erp", "kairon-n3", "Failed", "4", "16Gi", 0, 0, "qemu", 70*d),
		machine("staging", "scratch-vm", "kairon-n4", "Stopped", "1", "1Gi", 0, 0, "firecracker", 9*d),
	}
	store["nodes"] = []obj{
		node("kairon-n1", "10.0.0.11", true, false),
		node("kairon-n2", "10.0.0.12", true, false),
		node("kairon-n3", "10.0.0.13", true, false),
		node("kairon-n4", "10.0.0.14", true, true),
	}
	store["machinemigrations"] = []obj{
		{"metadata": meta("default", "db-replica-rebalance", 4*time.Minute), "spec": obj{"machineName": "db-replica", "strategy": "live", "targetNode": "kairon-n1", "mode": "precopy", "bandwidthMbps": 2000, "maxDowntimeMs": 300},
			"status": obj{"phase": "Running", "sourceNode": "kairon-n3", "targetNode": "kairon-n1", "effectiveStrategy": "live", "transferPhase": "precopy", "backend": "qemu", "ramTransferred": int64(14) << 30, "ramRemaining": int64(8) << 30, "ramTotal": int64(22) << 30, "dataPlaneEncrypted": true}},
		{"metadata": meta("default", "web-2-move", 3*h), "spec": obj{"machineName": "web-2", "strategy": "cold", "targetNode": "kairon-n2"},
			"status": obj{"phase": "Succeeded", "sourceNode": "kairon-n1", "targetNode": "kairon-n2", "effectiveStrategy": "cold", "totalTimeMs": 41200, "downtimeMs": 38100, "backend": "qemu"}},
		{"metadata": meta("default", "batch-nightly-evac", 26*h), "spec": obj{"machineName": "batch-nightly", "strategy": "live", "targetNode": "kairon-n3"},
			"status": obj{"phase": "NeedsRecovery", "sourceNode": "kairon-n4", "targetNode": "kairon-n3", "message": "destination session lost after cut-over; operator diagnosis required",
				"recovery": obj{"sourceRuntimeStatus": "Paused", "destinationSessionPhase": "Lost", "destinationRuntimeFound": false}}},
	}
	store["machinesnapshots"] = []obj{
		{"metadata": meta("default", "db-primary-pre-upgrade", 5*h), "spec": obj{"machineName": "db-primary"}, "status": obj{"phase": "Ready", "readyToUse": true}},
		{"metadata": meta("default", "web-1-nightly-0300", 9*h), "spec": obj{"machineName": "web-1"}, "status": obj{"phase": "Ready", "readyToUse": true}},
		{"metadata": meta("default", "web-2-nightly-0300", 9*h), "spec": obj{"machineName": "web-2"}, "status": obj{"phase": "Ready", "readyToUse": true}},
		{"metadata": meta("default", "web-3-hourly", 40*time.Minute), "spec": obj{"machineName": "web-3"}, "status": obj{"phase": "Creating"}},
	}
	store["machinesnapshotschedules"] = []obj{
		{"metadata": meta("default", "nightly-web", 30*d), "spec": obj{"selector": obj{"tier": "web"}, "intervalSeconds": 86400, "keepLast": 7},
			"status": obj{"lastRunTime": time.Now().Add(-9 * h).UTC().Format(time.RFC3339), "lastRunSnapshotCount": 3, "nextRunTime": time.Now().Add(15 * h).UTC().Format(time.RFC3339)}},
		{"metadata": meta("prod", "hourly-api", 14*d), "spec": obj{"selector": obj{"app": "api"}, "intervalSeconds": 3600, "keepLast": 24},
			"status": obj{"lastRunTime": time.Now().Add(-40 * time.Minute).UTC().Format(time.RFC3339), "lastRunSnapshotCount": 2, "nextRunTime": time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339)}},
	}
	store["machinesnapshotrestores"] = []obj{
		{"metadata": meta("default", "restore-web-1", 2*d), "spec": obj{"snapshotName": "web-1-nightly-0300", "targetClaimName": "web-1-restored"}, "status": obj{"phase": "Succeeded", "restoredClaimName": "web-1-restored"}},
	}
	store["machinequotas"] = []obj{
		{"metadata": meta("default", "team-a", 60*d), "spec": obj{"maxMachines": 20, "maxTotalCpu": "96", "maxTotalMemory": "256Gi"}, "status": obj{"usedMachines": 9, "usedTotalCpuCores": 56, "usedTotalMemoryMiB": 188 * 1024}},
		{"metadata": meta("staging", "staging-cap", 30*d), "spec": obj{"maxMachines": 6, "maxTotalCpu": "24", "maxTotalMemory": "64Gi"}, "status": obj{"usedMachines": 3, "usedTotalCpuCores": 7, "usedTotalMemoryMiB": 21 * 1024}},
	}
	store["machinedisruptionbudgets"] = []obj{
		{"metadata": meta("default", "api-pdb", 27*d), "spec": obj{"selector": obj{"app": "api"}, "minAvailable": "1"}, "status": obj{"expectedMachines": 2, "currentHealthy": 2, "desiredHealthy": 1, "disruptionsAllowed": 1}},
	}
	store["machinesets"] = []obj{
		{"metadata": meta("default", "web", 12*d), "spec": obj{"replicas": 3, "strategy": "RollingUpdate", "maxUnavailable": "1"}, "status": obj{"replicas": 3, "readyReplicas": 3, "updatedReplicas": 3}},
		{"metadata": meta("prod", "api", 27*d), "spec": obj{"replicas": 2, "strategy": "RollingUpdate", "maxUnavailable": "1"}, "status": obj{"replicas": 2, "readyReplicas": 2, "updatedReplicas": 2}},
	}
	store["machineinstancetypes"] = []obj{
		{"metadata": meta("default", "small", 90*d), "spec": obj{"resources": obj{"cpu": "1", "memory": "2Gi"}}},
		{"metadata": meta("default", "general", 90*d), "spec": obj{"resources": obj{"cpu": "4", "memory": "8Gi", "maxCpu": "8", "maxMemory": "16Gi"}}},
		{"metadata": meta("default", "db-pinned", 90*d), "spec": obj{"resources": obj{"cpu": "8", "memory": "32Gi", "hugepages": true, "cpuPinning": true, "numaNode": 0}}},
	}
	store["migrationpolicies"] = []obj{
		{"metadata": meta("default", "business-hours", 40*d), "spec": obj{"selector": obj{"tier": "web"}, "bandwidthMbps": 1000, "maxConcurrent": 2}, "status": obj{"activeMigrations": 1}},
	}
	store["machinenetworkpolicies"] = []obj{
		{"metadata": meta("default", "api-egress", 27*d), "spec": obj{"selector": obj{"app": "api"}, "policy": obj{"defaultAllow": false, "allowCidrs": []string{"10.42.0.0/16"}, "allowPorts": []string{"443/tcp", "5432/tcp"}, "maxEgressMbps": 500}},
			"status": obj{"phase": "Applied", "observedMachines": 2, "effectiveSynced": true}},
		{"metadata": meta("default", "audit-only", 6*d), "spec": obj{"selector": obj{"env": "staging"}, "policy": obj{"defaultAllow": true, "auditMode": true}}, "status": obj{"phase": "Applied", "observedMachines": 2, "effectiveSynced": true}},
	}
	store["networksecuritygroups"] = []obj{
		{"metadata": meta("default", "web-tier", 27*d), "spec": obj{"groupName": "web-tier", "priority": 100, "description": "Public HTTPS in, app tier out", "policy": obj{"defaultAllow": false, "allowPorts": []string{"443/tcp"}}}, "status": obj{"phase": "Applied", "identity": 4107}},
		{"metadata": meta("default", "db-tier", 27*d), "spec": obj{"groupName": "db-tier", "priority": 200, "description": "Postgres from app tier only", "policy": obj{"defaultAllow": false, "allowPorts": []string{"5432/tcp"}}}, "status": obj{"phase": "Applied", "identity": 4108}},
	}
	store["machinepools"] = []obj{
		{"metadata": meta("default", "warm-small", 20*d), "spec": obj{"replicas": 4}, "status": obj{"replicas": 4, "readyReplicas": 3, "claimed": 1}},
	}
	store["machineclaims"] = []obj{
		{"metadata": meta("default", "ci-job-4821", 15*time.Minute), "spec": obj{"poolName": "warm-small", "reclaimPolicy": "Delete", "ttlSeconds": 3600}, "status": obj{"phase": "Bound", "machineName": "warm-small-2", "bindMillis": 180}},
	}
	store["machinebackups"] = []obj{
		{"metadata": meta("default", "db-primary-nightly", 7*h), "spec": obj{"machineName": "db-primary", "quiesce": "freeze"}, "status": obj{"phase": "Succeeded", "nodeName": "kairon-n2", "completionTime": time.Now().Add(-7 * h).UTC().Format(time.RFC3339)}},
		{"metadata": meta("prod", "api-1-weekly", 3*d), "spec": obj{"machineName": "api-1"}, "status": obj{"phase": "Succeeded", "nodeName": "kairon-n2", "completionTime": time.Now().Add(-3 * d).UTC().Format(time.RFC3339)}},
	}
	store["machinebackuprestores"] = []obj{
		{"metadata": meta("default", "restore-db-drill", 2*d), "spec": obj{"backupName": "db-primary-nightly", "machineName": "db-drill"}, "status": obj{"phase": "Succeeded", "machineName": "db-drill"}},
	}
}

const d = 24 * time.Hour

// phaseFor maps a spec.powerState to the phase the real controller would settle on.
func phaseFor(power string) string {
	switch power {
	case "Stopped":
		return "Stopped"
	case "Paused":
		return "Paused"
	case "Halted":
		return "Halted"
	}
	return "Running"
}

func nsOf(o obj) string {
	m, _ := o["metadata"].(obj)
	s, _ := m["namespace"].(string)
	return s
}
func nameOf(o obj) string {
	m, _ := o["metadata"].(obj)
	s, _ := m["name"].(string)
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func handle(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	p := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(p, "/")

	// /api/v1/nodes and /api/v1/namespaces (core group).
	if p == "api/v1/nodes" {
		writeJSON(w, obj{"items": store["nodes"]})
		return
	}
	if p == "api/v1/namespaces" {
		seen := map[string]bool{}
		var items []obj
		for _, m := range store["machines"] {
			if ns := nsOf(m); !seen[ns] {
				seen[ns] = true
				items = append(items, obj{"metadata": obj{"name": ns}})
			}
		}
		sort.Slice(items, func(i, j int) bool { return nameOf(items[i]) < nameOf(items[j]) })
		writeJSON(w, obj{"items": items})
		return
	}

	// /apis/kairon.zyvor.dev/v1[/namespaces/NS]/PLURAL[/NAME[/status]]
	if len(parts) < 4 || parts[0] != "apis" || parts[1] != "kairon.zyvor.dev" {
		http.NotFound(w, r)
		return
	}
	rest := parts[3:]
	ns := ""
	if rest[0] == "namespaces" && len(rest) >= 3 {
		ns, rest = rest[1], rest[2:]
	}
	plural := rest[0]
	name := ""
	if len(rest) > 1 {
		name = rest[1]
	}
	items := store[plural]

	switch r.Method {
	case http.MethodGet:
		if name != "" {
			for _, o := range items {
				if nameOf(o) == name && (ns == "" || nsOf(o) == ns) {
					writeJSON(w, o)
					return
				}
			}
			http.NotFound(w, r)
			return
		}
		out := []obj{}
		for _, o := range items {
			if ns == "" || nsOf(o) == ns {
				out = append(out, o)
			}
		}
		writeJSON(w, obj{"items": out})
	case http.MethodPatch:
		var patch obj
		_ = json.NewDecoder(r.Body).Decode(&patch)
		for _, o := range items {
			if nameOf(o) != name || nsOf(o) != ns {
				continue
			}
			if spec, ok := patch["spec"].(obj); ok {
				cur, _ := o["spec"].(obj)
				for k, v := range spec {
					cur[k] = v
				}
				if ps, ok := spec["powerState"].(string); ok && plural == "machines" {
					st, _ := o["status"].(obj)
					st["phase"] = phaseFor(ps)
				}
			}
			writeJSON(w, o)
			return
		}
		http.NotFound(w, r)
	case http.MethodDelete:
		kept := items[:0:0]
		for _, o := range items {
			if !(nameOf(o) == name && nsOf(o) == ns) {
				kept = append(kept, o)
			}
		}
		store[plural] = kept
		writeJSON(w, obj{"status": "Success"})
	case http.MethodPost:
		var o obj
		_ = json.NewDecoder(r.Body).Decode(&o)
		m, _ := o["metadata"].(obj)
		if m == nil {
			m = obj{}
			o["metadata"] = m
		}
		if ns != "" {
			m["namespace"] = ns
		}
		m["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
		if plural == "machines" {
			o["status"] = obj{"phase": "Pending"}
			go func(name, ns string) { // settle to Running shortly after, like a real start
				time.Sleep(2500 * time.Millisecond)
				mu.Lock()
				defer mu.Unlock()
				for _, x := range store["machines"] {
					if nameOf(x) == name && nsOf(x) == ns {
						x["status"] = obj{"phase": "Running", "nodeName": "kairon-n1", "guestIP": "10.42.1.99", "resourceUsage": obj{"cpuPercent": 3.0, "memoryBytes": int64(512 << 20)}}
					}
				}
			}(nameOf(o), ns)
		}
		store[plural] = append(store[plural], o)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, o)
	default:
		writeJSON(w, obj{})
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18999", "listen address (keep it on loopback)")
	flag.Parse()
	seed()
	http.HandleFunc("/", handle)
	fmt.Println("sample-api on", *listen)
	if err := http.ListenAndServe(*listen, nil); err != nil {
		panic(err)
	}
}
