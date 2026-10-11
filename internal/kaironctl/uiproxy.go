// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/kube"
)

type uiOpts struct {
	Namespace string
	Service   string
	SvcPort   int
	Port      int
	NoOpen    bool
	// BackendHTTP reaches a kairon-ui that was installed with ui.insecureHttp
	// (plain HTTP). The default is HTTPS, which is what the chart serves.
	BackendHTTP bool
}

func newUICmd() *cobra.Command {
	o := &uiOpts{Namespace: "kairon-system", Service: "kairon-ui", SvcPort: 18082}
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the Kairon dashboard through the Kubernetes API server",
		Long: `Start a local proxy on 127.0.0.1 that forwards to the kairon-ui Service through the
API server's service proxy, then open it in a browser. Your kubeconfig credentials
stay in kaironctl; the dashboard still asks you to sign in.

The hop from kaironctl to the API server and on to the dashboard is HTTPS (the
dashboard serves HTTPS by default; pass --backend-http only for an install with
ui.insecureHttp). The proxy itself listens on 127.0.0.1 and is never reachable from
another machine.

No port-forward, Ingress or cluster-internal address is needed. Stop with Ctrl+C.`,
		Example: `  $ kaironctl ui
  $ kaironctl ui --port 8088 --no-open
  $ kaironctl ui --context staging -n kairon-system`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer cancel()
			addr, stop, err := startUIProxy(ctx, kc, o)
			if err != nil {
				return err
			}
			defer stop()
			// Loopback only: the browser-to-kaironctl hop never leaves this
			// machine, the same as kubectl proxy and port-forward.
			link := "http://" + addr
			style.Log(style.EmojiOK, "Dashboard proxy ready at %s (Ctrl+C to stop)", link)
			style.Log(style.EmojiInfo, "Sign in as admin; see docs/getting-started.md for the generated password")
			if !o.NoOpen {
				if err := openBrowser(link); err != nil {
					style.Warnf("could not open a browser: %v (open %s yourself)", err, link)
				}
			}
			<-ctx.Done()
			return nil
		},
	}
	cmd.Flags().StringVarP(&o.Namespace, "namespace", "n", o.Namespace, "namespace where kairon-ui runs")
	cmd.Flags().StringVar(&o.Service, "service", o.Service, "kairon-ui Service name")
	cmd.Flags().IntVar(&o.SvcPort, "service-port", o.SvcPort, "kairon-ui Service port")
	cmd.Flags().IntVar(&o.Port, "port", 0, "local port (default: pick a free one)")
	cmd.Flags().BoolVar(&o.NoOpen, "no-open", false, "do not open a browser")
	cmd.Flags().BoolVar(&o.BackendHTTP, "backend-http", false, "the dashboard serves plain HTTP (installed with ui.insecureHttp); default is HTTPS")
	return cmd
}

// startUIProxy serves a loopback reverse proxy to the kairon-ui Service via the
// API server's service proxy and returns its address and a stop function.
//
// Every request is rewritten under one fixed service-proxy prefix, so the
// proxy cannot be used to reach other API server paths with the operator's
// credentials, and requests whose Host is not the loopback address are
// refused (DNS-rebinding protection).
func startUIProxy(ctx context.Context, kc *kube.Client, o *uiOpts) (string, func(), error) {
	target, err := url.Parse(kc.BaseURL)
	if err != nil {
		return "", nil, err
	}
	scheme := "https"
	if o.BackendHTTP {
		scheme = "http"
	}
	prefix := fmt.Sprintf("/api/v1/namespaces/%s/services/%s:%s:%d/proxy", url.PathEscape(o.Namespace), scheme, url.PathEscape(o.Service), o.SvcPort)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", o.Port))
	if err != nil {
		return "", nil, fmt.Errorf("listen: %w", err)
	}
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			r := pr.Out
			clean := path.Clean("/" + strings.TrimPrefix(pr.In.URL.Path, "/"))
			r.URL.Scheme = target.Scheme
			r.URL.Host = target.Host
			r.URL.Path = strings.TrimRight(target.Path, "/") + prefix + clean
			r.URL.RawPath = ""
			r.Host = target.Host
			if kc.Token != "" {
				r.Header.Set("Authorization", "Bearer "+kc.Token)
			} else {
				r.Header.Del("Authorization")
			}
		},
		Transport: kc.HTTP.Transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "kaironctl ui: "+err.Error(), http.StatusBadGateway)
		},
	}
	allowedHosts := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true, "[::1]:" + port: true}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allowedHosts[r.Host] {
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
			rp.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	stop := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return addr, stop, nil
}

func openBrowser(link string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	return cmd.Start()
}
