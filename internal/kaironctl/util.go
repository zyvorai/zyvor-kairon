// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"fmt"
	"os"
	"strings"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
)

func resourceName(s string) string {
	s = strings.ToLower(s)
	s = invalidResourceName.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

func okf(format string, args ...any) {
	fmt.Println(style.Success(os.Stdout, format, args...))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
