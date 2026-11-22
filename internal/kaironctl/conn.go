// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/rest"

	"github.com/zyvorai/kairon/internal/kube"
)

// kubeFlags carry the global --kubeconfig / --context selection. They are
// package state because the Helm SDK path (restConfigFromEnv) and the REST
// client path (newKubeClient) must agree on which cluster they talk to.
var kubeFlags struct {
	Kubeconfig string
	Context    string
}

// explicitKubeTarget reports whether the operator named a kubeconfig or
// context on the command line; that always wins over environment defaults.
func explicitKubeTarget() bool {
	return kubeFlags.Kubeconfig != "" || kubeFlags.Context != ""
}

// newKubeClient returns the REST client every kaironctl verb uses.
//
// Selection order:
//  1. --kubeconfig / --context (any kubeconfig auth: certs, tokens, exec plugins)
//  2. KAIRON_KUBE_URL (+ KAIRON_KUBE_TOKEN / KAIRON_KUBE_CA / KAIRON_KUBE_INSECURE)
//  3. in-cluster service account
//  4. the default kubeconfig ($KUBECONFIG, ~/.kube/config)
//
// Steps 2 and 3 keep using the stdlib client in internal/kube; steps 1 and 4
// reuse the same client-go configuration loading the Helm SDK path already
// uses, so `kaironctl install` and `kaironctl get` always target one cluster.
// client-go stays inside internal/kaironctl: the controller, node and UI
// binaries do not link it.
func newKubeClient() (*kube.Client, error) {
	if !explicitKubeTarget() && (os.Getenv("KAIRON_KUBE_URL") != "" || os.Getenv("KUBERNETES_SERVICE_HOST") != "") {
		return kube.FromEnvironment()
	}
	cfg, err := restConfigFromEnv()
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w (set --kubeconfig, KUBECONFIG, KAIRON_KUBE_URL, or run in cluster)", err)
	}
	cfg.Timeout = 20 * time.Second
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	host := strings.TrimRight(cfg.Host, "/")
	if host == "" {
		return nil, fmt.Errorf("kubernetes config has no server address")
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	return &kube.Client{BaseURL: host, HTTP: hc}, nil
}

// extractConnFlags removes --kubeconfig/--context (space or = form) from the
// argument list of a DisableFlagParsing command and records them, so
// `kaironctl get machines --context prod` works like the root-level form.
func extractConnFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--kubeconfig" || a == "--context":
			if i+1 < len(args) {
				setKubeFlag(strings.TrimPrefix(a, "--"), args[i+1])
				i++
				continue
			}
			out = append(out, a)
		case strings.HasPrefix(a, "--kubeconfig="):
			setKubeFlag("kubeconfig", strings.TrimPrefix(a, "--kubeconfig="))
		case strings.HasPrefix(a, "--context="):
			setKubeFlag("context", strings.TrimPrefix(a, "--context="))
		default:
			out = append(out, a)
		}
	}
	return out
}

func setKubeFlag(name, value string) {
	if name == "kubeconfig" {
		kubeFlags.Kubeconfig = value
		return
	}
	kubeFlags.Context = value
}
