// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
)

// The helpers below are the read-only Kubernetes calls kaironctl diagnostics
// (doctor, logs, events, sysdump, ui) share. They sit on kubeGetJSON's
// approach: reuse the kube.Client's BaseURL, Token and HTTP transport (which
// carries kubeconfig auth when the client came from newKubeClient).

type podSummary struct {
	Name       string
	Phase      string
	Ready      bool
	Restarts   int
	Node       string
	IP         string
	Containers []string
	Labels     map[string]string
	// HealthPort is the containerPort named "health" (0 when absent); the
	// controller and node agent serve /healthz, /readyz and /metrics there.
	HealthPort int
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Name  string `json:"name"`
				Ports []struct {
					Name          string `json:"name"`
					ContainerPort int    `json:"containerPort"`
				} `json:"ports"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase             string `json:"phase"`
			PodIP             string `json:"podIP"`
			ContainerStatuses []struct {
				Ready        bool `json:"ready"`
				RestartCount int  `json:"restartCount"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// listPods lists pods in ns matching a label selector ("" = all).
func listPods(ctx context.Context, kc *kube.Client, ns, selector string) ([]podSummary, error) {
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods", url.PathEscape(ns))
	if selector != "" {
		path += "?labelSelector=" + url.QueryEscape(selector)
	}
	var pl podList
	if err := kubeGetJSON(ctx, kc, path, &pl); err != nil {
		return nil, err
	}
	out := make([]podSummary, 0, len(pl.Items))
	for _, it := range pl.Items {
		p := podSummary{Name: it.Metadata.Name, Phase: it.Status.Phase, Node: it.Spec.NodeName, IP: it.Status.PodIP, Labels: it.Metadata.Labels}
		for _, c := range it.Spec.Containers {
			p.Containers = append(p.Containers, c.Name)
			for _, port := range c.Ports {
				if port.Name == "health" {
					p.HealthPort = port.ContainerPort
				}
			}
		}
		p.Ready = len(it.Status.ContainerStatuses) > 0
		for _, cs := range it.Status.ContainerStatuses {
			p.Restarts += cs.RestartCount
			if !cs.Ready {
				p.Ready = false
			}
		}
		out = append(out, p)
	}
	return out, nil
}

type logOptions struct {
	Container string
	Tail      int           // 0 = server default (all)
	Since     time.Duration // 0 = no limit
	Previous  bool
	Follow    bool
}

// podLogs opens the log stream of one pod container. The caller closes the
// body. Follow streams use a client without the request timeout.
func podLogs(ctx context.Context, kc *kube.Client, ns, pod string, o logOptions) (io.ReadCloser, error) {
	q := url.Values{}
	if o.Container != "" {
		q.Set("container", o.Container)
	}
	if o.Tail > 0 {
		q.Set("tailLines", strconv.Itoa(o.Tail))
	}
	if o.Since > 0 {
		q.Set("sinceSeconds", strconv.Itoa(int(o.Since.Seconds())))
	}
	if o.Previous {
		q.Set("previous", "true")
	}
	if o.Follow {
		q.Set("follow", "true")
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", url.PathEscape(ns), url.PathEscape(pod))
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	hc := kc.HTTP
	if o.Follow {
		cp := *hc
		cp.Timeout = 0
		hc = &cp
	}
	resp, err := kubeStream(ctx, kc, hc, path)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// kubeStream issues an authenticated GET and returns the live response; a
// non-2xx status is turned into a *kube.APIError and the body closed.
func kubeStream(ctx context.Context, kc *kube.Client, hc *http.Client, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(kc.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	if kc.Token != "" {
		req.Header.Set("Authorization", "Bearer "+kc.Token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, &kube.APIError{Method: http.MethodGet, Path: path, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return resp, nil
}

type eventSummary struct {
	Namespace string    `json:"namespace"`
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Object    string    `json:"object"`
	Message   string    `json:"message"`
	Count     int       `json:"count"`
	Last      time.Time `json:"last"`
}

// listNamespaceEvents lists core/v1 Events in ns ("" = all namespaces), oldest first.
func listNamespaceEvents(ctx context.Context, kc *kube.Client, ns string) ([]eventSummary, error) {
	path := "/api/v1/events"
	if ns != "" {
		path = fmt.Sprintf("/api/v1/namespaces/%s/events", url.PathEscape(ns))
	}
	var raw struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Type           string `json:"type"`
			Reason         string `json:"reason"`
			Message        string `json:"message"`
			Count          int    `json:"count"`
			LastTimestamp  string `json:"lastTimestamp"`
			EventTime      string `json:"eventTime"`
			InvolvedObject struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"involvedObject"`
		} `json:"items"`
	}
	if err := kubeGetJSON(ctx, kc, path, &raw); err != nil {
		return nil, err
	}
	out := make([]eventSummary, 0, len(raw.Items))
	for _, it := range raw.Items {
		ev := eventSummary{
			Namespace: it.Metadata.Namespace, Type: it.Type, Reason: it.Reason, Message: it.Message, Count: it.Count,
			Object: it.InvolvedObject.Kind + "/" + it.InvolvedObject.Name,
		}
		for _, ts := range []string{it.LastTimestamp, it.EventTime} {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				ev.Last = t
				break
			}
		}
		out = append(out, ev)
	}
	sortEvents(out)
	return out, nil
}

func sortEvents(evs []eventSummary) {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Last.Before(evs[j].Last) })
}

// proxyGet reads path from a pod or service through the API server's proxy
// subresource (no port-forward or cluster-internal address needed).
// kind is "pods" or "services"; target is "name" or "name:port" (use
// "scheme:name:port" for https backends, per the API server proxy rules).
func proxyGet(ctx context.Context, kc *kube.Client, kind, ns, target, path string) ([]byte, error) {
	if kind != "pods" && kind != "services" {
		return nil, fmt.Errorf("proxy kind must be pods or services, got %q", kind)
	}
	p := fmt.Sprintf("/api/v1/namespaces/%s/%s/%s/proxy/%s", url.PathEscape(ns), kind, url.PathEscape(target), strings.TrimLeft(path, "/"))
	resp, err := kubeStream(ctx, kc, kc.HTTP, p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// kubeGetObject GETs any object path and returns the raw JSON.
func kubeGetObject(ctx context.Context, kc *kube.Client, path string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := kubeGetJSON(ctx, kc, path, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}
