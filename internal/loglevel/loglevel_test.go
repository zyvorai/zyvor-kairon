// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package loglevel

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type fakeCM struct {
	data map[string]string
	err  error
}

func (f fakeCM) GetConfigMap(context.Context, string, string) (model.ConfigMap, error) {
	return model.ConfigMap{Data: f.data}, f.err
}

func TestResolve(t *testing.T) {
	data := map[string]string{"controller": "debug", "node": "warn", "node.n2": "error"}
	cases := []struct {
		component, node string
		want            slog.Level
	}{
		{"controller", "", slog.LevelDebug},
		{"ui", "", slog.LevelInfo},
		{"node", "n1", slog.LevelWarn},
		{"node", "n2", slog.LevelError},
	}
	for _, c := range cases {
		got, err := Resolve(data, c.component, c.node)
		if err != nil || got != c.want {
			t.Errorf("%s/%s = %v, %v; want %v", c.component, c.node, got, err, c.want)
		}
	}
	if _, err := Resolve(map[string]string{"ui": "loud"}, "ui", ""); err == nil {
		t.Error("bad level accepted")
	}
}

func TestApply(t *testing.T) {
	ctx := context.Background()
	lv := new(slog.LevelVar)
	if changed, err := Apply(ctx, fakeCM{data: map[string]string{"ui": "debug"}}, "ns", "ui", "", lv); err != nil || !changed || lv.Level() != slog.LevelDebug {
		t.Fatalf("apply debug: %v %v %v", changed, err, lv.Level())
	}
	if changed, _ := Apply(ctx, fakeCM{data: map[string]string{"ui": "debug"}}, "ns", "ui", "", lv); changed {
		t.Fatal("unchanged level reported as changed")
	}
	if _, err := Apply(ctx, fakeCM{err: errors.New("forbidden")}, "ns", "ui", "", lv); err == nil || lv.Level() != slog.LevelDebug {
		t.Fatalf("read error must keep level: %v %v", err, lv.Level())
	}
	notFound := &kube.APIError{StatusCode: 404}
	if changed, err := Apply(ctx, fakeCM{err: notFound}, "ns", "ui", "", lv); err != nil || !changed || lv.Level() != slog.LevelInfo {
		t.Fatalf("missing ConfigMap must reset to info: %v %v %v", changed, err, lv.Level())
	}
}
