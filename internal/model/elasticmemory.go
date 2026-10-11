package model

import "fmt"

// ValidateElasticMemory checks spec.resources.elasticMemory against the
// rest of the spec. The CRD's CEL rules cover the structural part for
// clients that skip the webhook.
func ValidateElasticMemory(s MachineSpec) error {
	e := s.Resources.ElasticMemory
	if e == nil {
		return nil
	}
	switch s.Runtime.Backend {
	case "", "auto", "qemu":
	default:
		return fmt.Errorf("spec.resources.elasticMemory needs the qemu backend, not %q", s.Runtime.Backend)
	}
	if s.Resources.Hugepages {
		return fmt.Errorf("spec.resources.elasticMemory cannot be combined with hugepages: hugepage memory is pinned")
	}
	if len(s.Devices) > 0 || len(s.DeviceClaims) > 0 {
		return fmt.Errorf("spec.resources.elasticMemory cannot be combined with passthrough devices: VFIO pins guest memory")
	}
	minMiB, err := ParseMemoryMiB(e.Min)
	if err != nil {
		return fmt.Errorf("spec.resources.elasticMemory.min: %v", err)
	}
	if s.Resources.Memory != "" {
		mem, err := ParseMemoryMiB(s.Resources.Memory)
		if err == nil && minMiB > mem {
			return fmt.Errorf("spec.resources.elasticMemory.min %s is more than spec.resources.memory %s", e.Min, s.Resources.Memory)
		}
	}
	return nil
}

// ValidateElasticMemoryUpdate refuses turning elasticMemory on or off on an
// existing Machine: the balloon device is added at boot, and the scheduler
// would otherwise count only the minimum for a VM that cannot be reclaimed.
// Changing min is allowed.
func ValidateElasticMemoryUpdate(old, cur MachineSpec) error {
	if (old.Resources.ElasticMemory == nil) != (cur.Resources.ElasticMemory == nil) {
		return fmt.Errorf("spec.resources.elasticMemory cannot be added or removed after creation; recreate the Machine")
	}
	return ValidateElasticMemory(cur)
}

// ElasticMinMiB returns the guaranteed memory of an elastic-memory Machine
// and true, or 0 and false when the Machine is not elastic or min does not
// parse.
func (s MachineSpec) ElasticMinMiB() (uint64, bool) {
	e := s.Resources.ElasticMemory
	if e == nil {
		return 0, false
	}
	v, err := ParseMemoryMiB(e.Min)
	if err != nil || v == 0 {
		return 0, false
	}
	return v, true
}
