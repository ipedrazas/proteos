package dev_test

import (
	"testing"

	"github.com/ipedrazas/proteos/nodeagent/internal/driver"
	"github.com/ipedrazas/proteos/nodeagent/internal/driver/conformance"
)

// TestConformance runs the shared driver-contract suite against the dev driver.
// The dev driver is the reference implementation of the contract — it is what
// the HTTP-layer and control-plane e2e tests run against — so holding it and
// the kubernetes driver to the same suite is what stops a new backend from
// quietly diverging.
//
// SupportsHibernateSnapshot is true here: unlike a pod, the dev driver writes
// (fake) snapshot metadata on hibernate, so the suite must not assert its
// absence.
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Suite{
		NewDriver: func(t *testing.T) driver.Driver {
			d, _ := newDriver(t, "")
			return d
		},
		Spec: func(id string) driver.VMSpec {
			return driver.VMSpec{
				MachineID: id,
				Vcpus:     2,
				MemMiB:    2048,
				KernelRef: "vmlinux-test",
				RootfsRef: "rootfs-test",
			}
		},
		SupportsHibernateSnapshot: true,
	})
}
