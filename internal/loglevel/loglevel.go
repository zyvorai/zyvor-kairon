// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package loglevel lets an operator change a running component's log level
// without a restart: kaironctl adm log-verbosity writes the
// kairon-log-levels ConfigMap and each component polls it into a
// slog.LevelVar.
package loglevel

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// ConfigMapName lives in the namespace Kairon is installed in. Keys are a
// component ("controller", "ui", "node") or "node.<NODE>" for one
// kairon-node; values are debug|info|warn|error. A missing key means info.
const ConfigMapName = "kairon-log-levels"

const PollInterval = 15 * time.Second

var Components = []string{"controller", "ui", "node"}

// Key is the ConfigMap key for component, narrowed to one node when node
// is set (kairon-node only).
func Key(component, node string) string {
	if component == "node" && node != "" {
		return "node." + node
	}
	return component
}

func Parse(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (debug|info|warn|error)", s)
}

// Resolve picks component's level from the ConfigMap data: the per-node
// key wins over the component key.
func Resolve(data map[string]string, component, node string) (slog.Level, error) {
	if v, ok := data[Key(component, node)]; ok && node != "" {
		return Parse(v)
	}
	return Parse(data[component])
}

// Namespace is KAIRON_LOG_LEVELS_NAMESPACE, else the pod's own namespace;
// empty outside a cluster, which disables polling.
func Namespace() string {
	if ns := os.Getenv("KAIRON_LOG_LEVELS_NAMESPACE"); ns != "" {
		return ns
	}
	b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type configMapGetter interface {
	GetConfigMap(ctx context.Context, ns, name string) (model.ConfigMap, error)
}

// Apply reads the ConfigMap once and sets lv. A missing ConfigMap resets
// to info; any other read error leaves lv as it is.
func Apply(ctx context.Context, kc configMapGetter, ns, component, node string, lv *slog.LevelVar) (changed bool, err error) {
	cm, err := kc.GetConfigMap(ctx, ns, ConfigMapName)
	if err != nil && !kube.IsNotFound(err) {
		return false, err
	}
	level, err := Resolve(cm.Data, component, node)
	if err != nil {
		return false, err
	}
	if lv.Level() == level {
		return false, nil
	}
	lv.Set(level)
	return true, nil
}

// Watch polls until ctx ends. Read errors are logged once until the next
// success so a missing RBAC grant does not flood the log.
func Watch(ctx context.Context, kc configMapGetter, ns, component, node string, lv *slog.LevelVar, log *slog.Logger) {
	if ns == "" {
		return
	}
	failing := false
	for {
		changed, err := Apply(ctx, kc, ns, component, node, lv)
		switch {
		case err != nil && !failing:
			failing = true
			log.Warn("log level: cannot read ConfigMap", "namespace", ns, "name", ConfigMapName, "error", err)
		case err == nil:
			failing = false
			if changed {
				log.Info("log level changed", "level", lv.Level().String())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(PollInterval):
		}
	}
}
