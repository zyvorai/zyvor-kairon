// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Prometheus struct {
	URL    string
	Token  string
	HTTP   *http.Client
	MaxAge time.Duration
}

func (p *Prometheus) query(ctx context.Context, query string) (map[string]float64, error) {
	if p == nil || p.URL == "" {
		return nil, fmt.Errorf("live metrics backend is not configured")
	}
	endpoint := strings.TrimRight(p.URL, "/") + "/api/v1/query?" + url.Values{"query": {query}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("metrics redirect refused") }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics HTTP %d", resp.StatusCode)
	}
	var out struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if out.Status != "success" || out.Data.ResultType != "vector" {
		return nil, fmt.Errorf("metrics query did not return a vector")
	}
	values := map[string]float64{}
	for _, sample := range out.Data.Result {
		if len(sample.Value) != 2 {
			return nil, fmt.Errorf("invalid metric sample")
		}
		var rawValue string
		if err := json.Unmarshal(sample.Value[1], &rawValue); err != nil {
			return nil, err
		}
		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("invalid metric value")
		}
		name := sample.Metric["machine"]
		if name == "" {
			continue
		}
		if _, exists := values[name]; exists {
			return nil, fmt.Errorf("duplicate samples for Machine %s", name)
		}
		values[name] = value
	}
	return values, nil
}
func (p *Prometheus) CPU(ctx context.Context, namespace string, now time.Time) (map[string]float64, error) {
	cpu, err := p.query(ctx, `kairon_machine_resource_usage{namespace=`+strconv.Quote(namespace)+`,resource="cpu_percent"}`)
	if err != nil {
		return nil, err
	}
	timestamps, err := p.query(ctx, `kairon_machine_usage_sample_timestamp_seconds{namespace=`+strconv.Quote(namespace)+`}`)
	if err != nil {
		return nil, err
	}
	age := p.MaxAge
	if age == 0 {
		age = 30 * time.Second
	}
	for name := range cpu {
		sample, ok := timestamps[name]
		if !ok || float64(now.Unix())-sample > age.Seconds() || sample > float64(now.Unix())+5 {
			delete(cpu, name)
		}
	}
	return cpu, nil
}
