// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/internal/kube"
)

// extractOutput removes -o/--output VALUE (also -o=VALUE, --output=VALUE) from
// the argument list of a DisableFlagParsing command. The returned format is
// "" when the flag is absent, so default output stays byte-identical.
func extractOutput(args []string) (format string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" || a == "--output":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("flag %s needs an argument: json, yaml or name", a)
			}
			format = args[i+1]
			i++
		case strings.HasPrefix(a, "-o="):
			format = strings.TrimPrefix(a, "-o=")
		case strings.HasPrefix(a, "--output="):
			format = strings.TrimPrefix(a, "--output=")
		default:
			rest = append(rest, a)
		}
	}
	switch format {
	case "", "json", "yaml", "name":
		return format, rest, nil
	}
	return "", nil, fmt.Errorf("unknown output format %q (want json, yaml or name)", format)
}

// resourcePlural resolves every spelling `kaironctl get`/`describe`/`delete`
// accept to the CRD plural. core reports a core/v1 object (Node).
func resourcePlural(resource string) (plural string, core bool, ok bool) {
	switch strings.ToLower(resource) {
	case "machine", "machines", "vm", "vms":
		return "machines", false, true
	case "migration", "migrations", "machinemigration", "machinemigrations":
		return "machinemigrations", false, true
	case "snapshot", "snapshots", "machinesnapshot", "machinesnapshots":
		return "machinesnapshots", false, true
	case "restore", "restores", "machinesnapshotrestore", "machinesnapshotrestores":
		return "machinesnapshotrestores", false, true
	case "backup", "backups", "machinebackup", "machinebackups":
		return "machinebackups", false, true
	case "backuprestore", "backuprestores", "machinebackuprestore", "machinebackuprestores":
		return "machinebackuprestores", false, true
	case "quota", "quotas", "machinequota", "machinequotas":
		return "machinequotas", false, true
	case "budget", "budgets", "machinedisruptionbudget", "machinedisruptionbudgets":
		return "machinedisruptionbudgets", false, true
	case "machineset", "machinesets":
		return "machinesets", false, true
	case "machinepool", "machinepools":
		return "machinepools", false, true
	case "machineclaim", "machineclaims", "claim", "claims":
		return "machineclaims", false, true
	case "instancetype", "instancetypes", "machineinstancetype", "machineinstancetypes":
		return "machineinstancetypes", false, true
	case "migrationpolicy", "migrationpolicies":
		return "migrationpolicies", false, true
	case "snapshotschedule", "snapshotschedules", "machinesnapshotschedule", "machinesnapshotschedules":
		return "machinesnapshotschedules", false, true
	case "backuppolicy", "backuppolicies", "machinebackuppolicy", "machinebackuppolicies":
		return "machinebackuppolicies", false, true
	case "protectionplan", "protectionplans", "machineprotectionplan", "machineprotectionplans", "kmpp":
		return "machineprotectionplans", false, true
	case "macnetwork", "macnetworks", "kmn":
		return "macnetworks", false, true
	case "networkpolicy", "networkpolicies", "machinenetworkpolicy", "machinenetworkpolicies":
		return "machinenetworkpolicies", false, true
	case "securitygroup", "securitygroups", "networksecuritygroup", "networksecuritygroups":
		return "networksecuritygroups", false, true
	case "node", "nodes":
		return "nodes", true, true
	case "virtualnetwork", "virtualnetworks", "vnet", "vnets":
		return "virtualnetworks", false, true
	case "nodetopology", "nodetopologies", "knt":
		return "nodetopologies", false, true
	}
	return "", false, false
}

// listPath is the collection path of a resource in namespace ns.
func listPath(plural string, core bool, ns string) string {
	if core {
		return "/api/v1/" + plural
	}
	if plural == "nodetopologies" {
		return fmt.Sprintf("/apis/%s/v1/%s", crdGroup, plural)
	}
	return fmt.Sprintf("/apis/%s/v1/namespaces/%s/%s", crdGroup, url.PathEscape(ns), plural)
}

func objectPath(plural string, core bool, ns, name string) string {
	return listPath(plural, core, ns) + "/" + url.PathEscape(name)
}

func labelSelectorQuery(selector map[string]string) string {
	if len(selector) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(selector))
	for k, v := range selector {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return "?labelSelector=" + url.QueryEscape(strings.Join(pairs, ","))
}

// printRawStructured writes a raw API object as indented JSON or YAML.
func printRawStructured(w io.Writer, raw json.RawMessage, format string) error {
	switch format {
	case "yaml":
		b, err := yaml.JSONToYAML(raw)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	default:
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	}
}

// getStructured implements `kaironctl get RESOURCE -o json|yaml|name`.
func getStructured(ctx context.Context, kc *kube.Client, w io.Writer, resource, ns string, selector map[string]string, format string) error {
	plural, core, ok := resourcePlural(resource)
	if !ok {
		return fmt.Errorf("unknown resource %q", resource)
	}
	raw, err := kubeGetObject(ctx, kc, listPath(plural, core, ns)+labelSelectorQuery(selector))
	if err != nil {
		return err
	}
	if format != "name" {
		return printRawStructured(w, raw, format)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	prefix := plural + "." + crdGroup
	if core {
		prefix = "node"
	}
	for _, it := range list.Items {
		_, _ = fmt.Fprintf(w, "%s/%s\n", prefix, it.Metadata.Name)
	}
	return nil
}

// describeStructured implements `kaironctl describe RESOURCE NAME -o json|yaml`:
// the object as stored, for every kind including the ones whose default
// describe is a derived view.
func describeStructured(ctx context.Context, kc *kube.Client, w io.Writer, resource, ns, name, format string) error {
	plural, core, ok := resourcePlural(resource)
	if !ok {
		return fmt.Errorf("unknown resource %q", resource)
	}
	raw, err := kubeGetObject(ctx, kc, objectPath(plural, core, ns, name))
	if err != nil {
		return err
	}
	if format == "name" {
		format = "json"
	}
	return printRawStructured(w, raw, format)
}
