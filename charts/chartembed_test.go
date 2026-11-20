// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package charts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasChart(t *testing.T) {
	if !HasChart() {
		t.Fatal("embedded chart missing Chart.yaml")
	}
}

func TestWriteToDir(t *testing.T) {
	dir := t.TempDir()
	out, err := WriteToDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "Chart.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "values.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "templates", "all.yaml")); err != nil {
		t.Fatal(err)
	}
}
