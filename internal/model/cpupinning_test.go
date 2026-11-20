// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import (
	"reflect"
	"testing"
)

func TestParseCPUListRange(t *testing.T) {
	got, err := ParseCPUList("2-5")
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{2, 3, 4, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseCPUListMixedRangesAndSingles(t *testing.T) {
	got, err := ParseCPUList("2,3,4-8,20")
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{2, 3, 4, 5, 6, 7, 8, 20}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseCPUListDeduplicatesAndSortsOutOfOrderInput(t *testing.T) {
	got, err := ParseCPUList("8,2,2-4")
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{2, 3, 4, 8}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseCPUListEmptyReturnsEmptyNotNil(t *testing.T) {
	got, err := ParseCPUList("")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %v, want an empty (non-nil) slice", got)
	}
}

func TestParseCPUListRejectsInvalidInput(t *testing.T) {
	for _, bad := range []string{"x", "2-", "-5", "5-2", "2--5", "2,x,5"} {
		if _, err := ParseCPUList(bad); err == nil {
			t.Errorf("ParseCPUList(%q): expected an error", bad)
		}
	}
}

func TestParseCPUListToleratesWhitespace(t *testing.T) {
	got, err := ParseCPUList(" 2, 3-4 , 6 ")
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{2, 3, 4, 6}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFormatCPULabelIsLabelSafeAndRoundTrips(t *testing.T) {
	in := []uint32{2, 3, 6, 7, 8, 9, 10, 11, 14}
	got, err := FormatCPULabel(in)
	if err != nil || got != "2-3_6-11_14" {
		t.Fatalf("got %q err %v, want 2-3_6-11_14", got, err)
	}
	back, err := ParseCPUList(got)
	if err != nil || !reflect.DeepEqual(back, in) {
		t.Fatalf("round trip got %v err %v", back, err)
	}
}

func TestFormatCPULabelRejectsOverLabelLimit(t *testing.T) {
	var sparse []uint32
	for c := uint32(0); c < 128; c += 2 {
		sparse = append(sparse, c)
	}
	if _, err := FormatCPULabel(sparse); err == nil {
		t.Fatal("expected an error for a value over 63 characters")
	}
}
