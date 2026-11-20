// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package llm is a minimal client for OpenAI-compatible chat completion
// endpoints (OpenAI, Ollama, vLLM, LiteLLM). It only ever returns text;
// callers validate what it says before anything is shown or applied.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Message is one chat turn. Role is system, user or assistant.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client calls POST {URL}/chat/completions.
type Client struct {
	// URL is the API base, e.g. https://api.openai.com/v1 or
	// http://localhost:11434/v1 for Ollama.
	URL    string
	APIKey string
	Model  string
	HTTP   *http.Client
}

// FromEnv reads KAIRON_LLM_URL, KAIRON_LLM_API_KEY and KAIRON_LLM_MODEL.
// It returns nil when URL or model is unset, so AI features stay off.
func FromEnv() *Client {
	u := strings.TrimSpace(os.Getenv("KAIRON_LLM_URL"))
	model := strings.TrimSpace(os.Getenv("KAIRON_LLM_MODEL"))
	if u == "" || model == "" {
		return nil
	}
	return &Client{URL: u, APIKey: strings.TrimSpace(os.Getenv("KAIRON_LLM_API_KEY")), Model: model}
}

// ErrNotConfigured is returned by callers when no client is set up.
var ErrNotConfigured = errors.New("no LLM configured: set KAIRON_LLM_URL and KAIRON_LLM_MODEL (and KAIRON_LLM_API_KEY if the endpoint needs one)")

const maxResponseBytes = 1 << 20

// ChatJSON asks for a JSON object matching schema. Endpoints that reject
// json_schema output are retried once with plain JSON mode.
func (c *Client) ChatJSON(ctx context.Context, messages []Message, schemaName string, schema map[string]any) (string, error) {
	if c == nil {
		return "", ErrNotConfigured
	}
	format := map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": schemaName, "schema": schema}}
	out, status, err := c.complete(ctx, messages, format)
	if err != nil && status == http.StatusBadRequest {
		out, _, err = c.complete(ctx, messages, map[string]any{"type": "json_object"})
	}
	return out, err
}

func (c *Client) complete(ctx context.Context, messages []Message, format map[string]any) (string, int, error) {
	body, err := json.Marshal(map[string]any{
		"model":           c.Model,
		"messages":        messages,
		"temperature":     0,
		"response_format": format,
	})
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("llm: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", resp.StatusCode, fmt.Errorf("llm: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, fmt.Errorf("llm: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", resp.StatusCode, fmt.Errorf("llm: decode response: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", resp.StatusCode, errors.New("llm: empty response")
	}
	return stripFences(out.Choices[0].Message.Content), resp.StatusCode, nil
}

// stripFences removes a ```json fence some local models add in JSON mode.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimPrefix(s, "json")
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}
