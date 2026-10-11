package model

import (
	"strings"
	"testing"
)

func elasticSpec(mem, min string) MachineSpec {
	var s MachineSpec
	s.Resources.Memory = mem
	s.Resources.ElasticMemory = &ElasticMemorySpec{Min: min}
	return s
}

func TestValidateElasticMemory(t *testing.T) {
	cases := []struct {
		name string
		spec MachineSpec
		want string
	}{
		{"not elastic", MachineSpec{}, ""},
		{"valid", elasticSpec("4Gi", "1Gi"), ""},
		{"min equals memory", elasticSpec("4Gi", "4Gi"), ""},
		{"min above memory", elasticSpec("2Gi", "3Gi"), "more than"},
		{"min missing", elasticSpec("2Gi", ""), "elasticMemory.min"},
		{"min zero", elasticSpec("2Gi", "0"), "elasticMemory.min"},
		{"hugepages", func() MachineSpec {
			s := elasticSpec("2Gi", "1Gi")
			s.Resources.Hugepages = true
			return s
		}(), "hugepages"},
		{"devices", func() MachineSpec {
			s := elasticSpec("2Gi", "1Gi")
			s.Devices = []MachineDevice{{Name: "gpu", Class: DeviceClassGPU}}
			return s
		}(), "passthrough"},
		{"apple backend", func() MachineSpec {
			s := elasticSpec("2Gi", "1Gi")
			s.Runtime.Backend = "vz"
			return s
		}(), "qemu backend"},
	}
	for _, c := range cases {
		err := ValidateElasticMemory(c.spec)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want error containing %q", c.name, err, c.want)
		}
	}
}

func TestValidateElasticMemoryUpdate(t *testing.T) {
	plain := MachineSpec{}
	plain.Resources.Memory = "2Gi"
	if err := ValidateElasticMemoryUpdate(plain, elasticSpec("2Gi", "1Gi")); err == nil {
		t.Fatal("adding elasticMemory to an existing Machine was allowed")
	}
	if err := ValidateElasticMemoryUpdate(elasticSpec("2Gi", "1Gi"), plain); err == nil {
		t.Fatal("removing elasticMemory was allowed")
	}
	if err := ValidateElasticMemoryUpdate(elasticSpec("2Gi", "1Gi"), elasticSpec("2Gi", "512Mi")); err != nil {
		t.Fatalf("changing min: %v", err)
	}
}

func TestElasticMinMiB(t *testing.T) {
	if v, ok := elasticSpec("4Gi", "1536Mi").ElasticMinMiB(); !ok || v != 1536 {
		t.Fatalf("got %d %v", v, ok)
	}
	if _, ok := (MachineSpec{}).ElasticMinMiB(); ok {
		t.Fatal("non-elastic Machine reported a minimum")
	}
	var e *ElasticMemorySpec
	if !e.FreePageReportingEnabled() {
		t.Fatal("free page reporting should default on")
	}
	off := false
	if (&ElasticMemorySpec{FreePageReporting: &off}).FreePageReportingEnabled() {
		t.Fatal("explicit false ignored")
	}
}
