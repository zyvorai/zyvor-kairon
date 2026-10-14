// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/mcp"
)

const (
	maxScreenshotBytes = 32 << 20
	maxTypeChars       = 4096
	maxInputActions    = 256
	mcpScreenWidth     = 1280
)

// inputActions are FluxVM's display input actions (fluxvm-apple screen.rs).
var inputActions = []string{"type", "key", "move", "click", "double_click", "right_click", "middle_click", "down", "up", "drag", "scroll"}

var inputModifiers = []string{"shift", "control", "option", "command"}

type sessionScreen struct {
	PNG           []byte
	Width, Height string
	Blank         bool
}

// sessionScreenshot reads a Mac session's guest display as a PNG; maxWidth
// scales it down (0: full size).
func sessionScreenshot(ctx context.Context, ns, name string, maxWidth int) (sessionScreen, error) {
	var out sessionScreen
	if uiBaseURL() == "" {
		return out, fmt.Errorf("KAIRON_UI_URL is not set; this command goes through kairon-ui")
	}
	verb := "screenshot"
	if maxWidth > 0 {
		verb += "?max_width=" + strconv.Itoa(maxWidth)
	}
	resp, err := uiRequest(ctx, http.MethodGet, sessionPath(ns, name, verb), nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return out, uiError(resp)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		return out, fmt.Errorf("screenshot: expected image/png, got %q", ct)
	}
	png, err := io.ReadAll(io.LimitReader(resp.Body, maxScreenshotBytes+1))
	if err != nil {
		return out, err
	}
	if len(png) > maxScreenshotBytes {
		return out, fmt.Errorf("screenshot is over %d bytes; pass a smaller max width", maxScreenshotBytes)
	}
	out.PNG = png
	out.Width = resp.Header.Get("X-Fluxvm-Screen-Width")
	out.Height = resp.Header.Get("X-Fluxvm-Screen-Height")
	out.Blank = resp.Header.Get("X-Fluxvm-Screen-Blank") == "true"
	return out, nil
}

func (s sessionScreen) caption() string {
	c := fmt.Sprintf("%sx%s PNG", dash(s.Width), dash(s.Height))
	if s.Blank {
		c += " (the display is blank: the guest may still be booting or has no graphical console)"
	}
	return c
}

// inputAction builds one FluxVM input action from CLI words: the action,
// then its text, key or coordinates.
func inputAction(words []string, mods []string, screenWidth int) (map[string]any, error) {
	if len(words) == 0 {
		return nil, fmt.Errorf("name an action: %s", strings.Join(inputActions, ", "))
	}
	action := strings.ReplaceAll(words[0], "-", "_")
	if !slices.Contains(inputActions, action) {
		return nil, fmt.Errorf("unknown action %q (want %s)", words[0], strings.Join(inputActions, ", "))
	}
	args := words[1:]
	a := map[string]any{"action": action}
	nums := func(names ...string) error {
		if len(args) != len(names) {
			return fmt.Errorf("%s needs %s", action, strings.ToUpper(strings.Join(names, " ")))
		}
		for i, n := range names {
			v, err := strconv.ParseFloat(args[i], 64)
			if err != nil || v < 0 {
				return fmt.Errorf("%s: %s must be a non-negative pixel coordinate", action, strings.ToUpper(n))
			}
			a[n] = v
		}
		return nil
	}
	switch action {
	case "type":
		text := strings.Join(args, " ")
		if text == "" || len(text) > maxTypeChars {
			return nil, fmt.Errorf("type needs 1-%d characters of text", maxTypeChars)
		}
		a["text"] = text
	case "key":
		if len(args) != 1 {
			return nil, fmt.Errorf("key needs one KEY (a character, or enter, tab, escape, backspace, up, f1, ...)")
		}
		a["key"] = args[0]
	case "drag":
		if err := nums("x", "y", "to_x", "to_y"); err != nil {
			return nil, err
		}
	case "scroll":
		if len(args) < 1 || len(args) > 2 {
			return nil, fmt.Errorf("scroll needs DY [DX] (wheel notches; positive scrolls down and right)")
		}
		for i, n := range []string{"dy", "dx"}[:len(args)] {
			v, err := strconv.Atoi(args[i])
			if err != nil {
				return nil, fmt.Errorf("scroll: %s must be an integer", strings.ToUpper(n))
			}
			a[n] = v
		}
	default:
		if err := nums("x", "y"); err != nil {
			return nil, err
		}
	}
	if len(mods) > 0 {
		if action != "key" && action != "click" {
			return nil, fmt.Errorf("--mod applies to key and click only")
		}
		for _, m := range mods {
			if !slices.Contains(inputModifiers, m) {
				return nil, fmt.Errorf("unknown modifier %q (want %s)", m, strings.Join(inputModifiers, ", "))
			}
		}
		a["modifiers"] = mods
	}
	if screenWidth > 0 && action != "type" && action != "key" {
		a["screen_width"] = screenWidth
	}
	return a, nil
}

// mcpInputAction maps one session_input action from the tool's camelCase
// arguments to FluxVM's body, refusing unknown fields.
func mcpInputAction(in map[string]any, screenWidth any) (map[string]any, error) {
	keys := map[string]string{"action": "action", "text": "text", "key": "key", "modifiers": "modifiers", "x": "x", "y": "y",
		"toX": "to_x", "toY": "to_y", "dx": "dx", "dy": "dy", "screenWidth": "screen_width"}
	out := map[string]any{}
	for k, v := range in {
		fk, ok := keys[k]
		if !ok {
			return nil, fmt.Errorf("unknown input field %q", k)
		}
		out[fk] = v
	}
	action, _ := out["action"].(string)
	if !slices.Contains(inputActions, action) {
		return nil, fmt.Errorf("action must be one of %s", strings.Join(inputActions, ", "))
	}
	if text, _ := out["text"].(string); len(text) > maxTypeChars {
		return nil, fmt.Errorf("text is limited to %d characters", maxTypeChars)
	}
	if _, set := out["screen_width"]; !set && screenWidth != nil && action != "type" && action != "key" {
		out["screen_width"] = screenWidth
	}
	return out, nil
}

// sessionInputBody is the POST input body for session_input's arguments:
// one action, or a list under actions.
func sessionInputBody(args map[string]any) (any, error) {
	width := args["screenWidth"]
	list, hasList := args["actions"]
	single := map[string]any{}
	for k, v := range args {
		switch k {
		case "namespace", "name", "actions", "screenshot":
		default:
			single[k] = v
		}
	}
	if hasList {
		if _, ok := single["action"]; ok {
			return nil, fmt.Errorf("give either action or actions, not both")
		}
		items, ok := list.([]any)
		if !ok || len(items) == 0 || len(items) > maxInputActions {
			return nil, fmt.Errorf("actions must be a list of 1-%d actions", maxInputActions)
		}
		actions := make([]map[string]any, 0, len(items))
		for i, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("actions[%d] must be an object", i)
			}
			a, err := mcpInputAction(m, width)
			if err != nil {
				return nil, fmt.Errorf("actions[%d]: %w", i, err)
			}
			actions = append(actions, a)
		}
		return map[string]any{"actions": actions}, nil
	}
	if _, ok := single["action"]; !ok {
		return nil, fmt.Errorf("give an action (or a list of actions)")
	}
	delete(single, "screenWidth")
	return mcpInputAction(single, width)
}

// redactTypedText replaces session_input's typed text with its length, so
// what an agent types into the guest (perhaps a password) stays out of the
// MCP audit log, as it stays out of FluxVM's ledger.
func redactTypedText(args json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return args
	}
	redact := func(a map[string]any) {
		if t, ok := a["text"].(string); ok {
			a["text"] = fmt.Sprintf("[%d characters]", len(t))
		}
	}
	redact(m)
	if list, ok := m["actions"].([]any); ok {
		for _, it := range list {
			if a, ok := it.(map[string]any); ok {
				redact(a)
			}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return out
}

func newSessionScreenshotCmd(opts *Options) *cobra.Command {
	var file string
	var maxWidth int
	c := &cobra.Command{
		Use:   "screenshot NAME",
		Short: "Save a PNG of a Mac session's guest display",
		Long: `Reads the guest display of a session on a Mac node (FluxVM's vz backend)
through kairon-ui. Needs a FluxVM newer than 0.4.8.`,
		Example: `  $ kaironctl session screenshot demo
  $ kaironctl session screenshot demo -o screen.png --max-width 1280`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			shot, err := sessionScreenshot(ctx, opts.Namespace, args[0], maxWidth)
			if err != nil {
				return err
			}
			if file == "" {
				file = args[0] + "-" + time.Now().Format("20060102-150405") + ".png"
			}
			if err := os.WriteFile(file, shot.PNG, 0o600); err != nil {
				return err
			}
			okf("%s (%s)", file, shot.caption())
			return nil
		},
	}
	c.Flags().StringVarP(&file, "file", "o", "", "output PNG (default NAME-TIMESTAMP.png)")
	c.Flags().IntVar(&maxWidth, "max-width", 0, "scale the image down to this width")
	return c
}

func newSessionInputCmd(opts *Options) *cobra.Command {
	var mods []string
	var screenWidth int
	var actionsFile, shotFile string
	c := &cobra.Command{
		Use:   "input NAME ACTION [ARGS...]",
		Short: "Send keyboard or mouse input to a Mac session's guest display",
		Long: `Actions: type TEXT, key KEY, move|click|double-click|right-click|middle-click|down|up X Y,
drag X Y TO_X TO_Y, scroll DY [DX]. Coordinates are pixels of a screenshot from
the top left; pass --screen-width when the screenshot was scaled down. --actions-file
sends a JSON list of FluxVM actions (or {"actions": [...]}; - reads stdin) in order.
Typed text does not go into the session ledger.`,
		Example: `  $ kaironctl session input demo click 640 400 --screen-width 1280
  $ kaironctl session input demo type -- 'make test'
  $ kaironctl session input demo key enter
  $ kaironctl session input demo key q --mod command --screenshot after.png`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var body any
			switch {
			case actionsFile != "":
				if len(args) > 1 {
					return fmt.Errorf("give an action or --actions-file, not both")
				}
				b, err := readFileOrStdin(actionsFile)
				if err != nil {
					return err
				}
				var list []map[string]any
				var wrapped struct {
					Actions []map[string]any `json:"actions"`
				}
				if json.Unmarshal(b, &list) != nil {
					if err := json.Unmarshal(b, &wrapped); err != nil {
						return fmt.Errorf("%s: want a JSON list of actions or {\"actions\": [...]}: %w", actionsFile, err)
					}
					list = wrapped.Actions
				}
				if len(list) == 0 || len(list) > maxInputActions {
					return fmt.Errorf("%s: send 1-%d actions", actionsFile, maxInputActions)
				}
				body = map[string]any{"actions": list}
			default:
				a, err := inputAction(args[1:], mods, screenWidth)
				if err != nil {
					return err
				}
				body = a
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			raw, err := sessionSend(ctx, opts.Namespace, args[0], http.MethodPost, "input", body)
			if err != nil {
				return err
			}
			if shotFile == "" {
				return printRaw(raw)
			}
			time.Sleep(500 * time.Millisecond)
			shot, err := sessionScreenshot(ctx, opts.Namespace, args[0], screenWidth)
			if err != nil {
				return err
			}
			if err := os.WriteFile(shotFile, shot.PNG, 0o600); err != nil {
				return err
			}
			okf("%s (%s)", shotFile, shot.caption())
			return nil
		},
	}
	c.Flags().StringArrayVar(&mods, "mod", nil, "modifier held for key or click: shift, control, option, command (repeatable)")
	c.Flags().IntVar(&screenWidth, "screen-width", 0, "width of the screenshot the coordinates refer to")
	c.Flags().StringVar(&actionsFile, "actions-file", "", "JSON list of actions to send in order (- for stdin)")
	c.Flags().StringVar(&shotFile, "screenshot", "", "save a screenshot to this PNG after the input")
	return c
}

func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	}
	return os.ReadFile(path)
}

// sessionScreenTools are session_screenshot and session_input.
func sessionScreenTools(nsDefault string) []mcp.Tool {
	nsProp := mcp.String("Kubernetes namespace; defaults to " + nsOrDefault(nsDefault))
	widthProp := mcp.Integer("scale the screenshot down to this width (default 1280)", 64, 8192)
	shoot := func(ctx context.Context, r sessionRef, width int) ([]mcp.Block, error) {
		shot, err := sessionScreenshot(ctx, r.ns(nsDefault), r.Name, width)
		if err != nil {
			return nil, err
		}
		return []mcp.Block{mcp.ImageBlock("image/png", shot.PNG), mcp.TextBlock(shot.caption())}, nil
	}
	return []mcp.Tool{
		{
			Name: "session_screenshot",
			Description: "Screenshot of a Mac AgentSession's guest display, as an image. Coordinates for session_input are pixels " +
				"of this image from the top left; pass its width as screenWidth.",
			Schema: mcp.Object(map[string]any{"namespace": nsProp, "name": mcp.String("AgentSession name"), "maxWidth": widthProp}, "name"),
			CallBlocks: func(ctx context.Context, raw json.RawMessage) ([]mcp.Block, error) {
				var a struct {
					sessionRef
					MaxWidth int `json:"maxWidth"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return nil, err
				}
				ctx, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				return shoot(ctx, a.sessionRef, cmp.Or(a.MaxWidth, mcpScreenWidth))
			},
		},
		{
			Name: "session_input",
			Description: "Keyboard and mouse input to a Mac AgentSession's guest display: type, key, move, click, double_click, " +
				"right_click, middle_click, down, up, drag, scroll; actions runs a list in order (same fields). Typed text is " +
				"kept out of the session ledger and the audit log. screenshot returns the display afterwards.",
			Write: true,
			Schema: mcp.Object(map[string]any{
				"namespace":   nsProp,
				"name":        mcp.String("AgentSession name"),
				"action":      mcp.String("input action", inputActions...),
				"text":        mcp.String("text to type (printable ASCII, newline, tab)"),
				"key":         mcp.String("key to press: a character, or enter, tab, escape, backspace, delete, up, down, left, right, home, end, page_up, page_down, f1-f12"),
				"modifiers":   map[string]any{"type": "array", "items": mcp.String("modifier", inputModifiers...)},
				"x":           map[string]any{"type": "number"},
				"y":           map[string]any{"type": "number"},
				"toX":         map[string]any{"type": "number"},
				"toY":         map[string]any{"type": "number"},
				"dx":          map[string]any{"type": "integer"},
				"dy":          map[string]any{"type": "integer"},
				"screenWidth": mcp.Integer("width of the screenshot the coordinates refer to", 1, 8192),
				"actions":     map[string]any{"type": "array", "maxItems": maxInputActions, "items": map[string]any{"type": "object"}},
				"screenshot":  map[string]any{"type": "boolean", "description": "return a screenshot after the input"},
			}, "name"),
			CallBlocks: func(ctx context.Context, raw json.RawMessage) ([]mcp.Block, error) {
				var args map[string]any
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.UseNumber()
				if err := dec.Decode(&args); err != nil {
					return nil, err
				}
				var r sessionRef
				_ = json.Unmarshal(raw, &r)
				if r.Name == "" {
					return nil, fmt.Errorf("name is required")
				}
				body, err := sessionInputBody(args)
				if err != nil {
					return nil, err
				}
				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				out, err := sessionSend(ctx, r.ns(nsDefault), r.Name, http.MethodPost, "input", body)
				if err != nil {
					return nil, err
				}
				blocks := []mcp.Block{mcp.TextBlock(strings.TrimSpace(string(out)))}
				if shot, _ := args["screenshot"].(bool); shot {
					time.Sleep(500 * time.Millisecond)
					width := mcpScreenWidth
					if n, ok := args["screenWidth"].(json.Number); ok {
						if v, err := n.Int64(); err == nil && v > 0 {
							width = int(v)
						}
					}
					img, err := shoot(ctx, r, width)
					if err != nil {
						return nil, err
					}
					blocks = append(img, blocks...)
				}
				return blocks, nil
			},
		},
	}
}
