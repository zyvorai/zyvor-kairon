// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package charts embeds the Kairon Helm chart into the kaironctl binary
// so `kaironctl install` works without a checkout of ./charts/kairon.
package charts

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ChartFS is the charts/kairon tree (Chart.yaml, values.yaml, templates/, crds/).
//
//go:embed all:kairon
var ChartFS embed.FS

const root = "kairon"

// WriteToDir materializes the embedded chart under dest (created if needed).
func WriteToDir(dest string) (string, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	err := fs.WalkDir(ChartFS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := ChartFS.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return "", fmt.Errorf("extract embedded chart: %w", err)
	}
	return dest, nil
}

// HasChart reports whether the embed includes Chart.yaml.
func HasChart() bool {
	_, err := ChartFS.Open(root + "/Chart.yaml")
	return err == nil
}
