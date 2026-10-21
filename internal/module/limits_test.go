package module

import "testing"

func TestPoolLimitsOnlyLowerModuleValues(t *testing.T) {
	pool := &PoolSpec{TestConcurrency: 50, MaxJobsPerInstance: 800}

	for _, tc := range []struct {
		name     string
		limits   PoolLimits
		wantConc int
		wantJobs int
	}{
		{"zero keeps module values", PoolLimits{}, 50, 800},
		{"positive caps lower", PoolLimits{TestConcurrency: 4, MaxJobsPerInstance: 128}, 4, 128},
		{"above module value keeps module value", PoolLimits{TestConcurrency: 100, MaxJobsPerInstance: 1000}, 50, 800},
		{"partial cap", PoolLimits{TestConcurrency: 4}, 4, 800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := newPoolChecker(Module{Name: "x", Pool: pool}, tc.limits)
			if got.testConcurrency() != tc.wantConc {
				t.Fatalf("testConcurrency = %d, want %d", got.testConcurrency(), tc.wantConc)
			}
			if got.maxJobsPerInstance() != tc.wantJobs {
				t.Fatalf("maxJobsPerInstance = %d, want %d", got.maxJobsPerInstance(), tc.wantJobs)
			}
		})
	}
}

func TestNewCheckerWithLimitsAppliesToPooledModulesOnly(t *testing.T) {
	m := Module{Name: "x", Pool: &PoolSpec{TestConcurrency: 50, MaxJobsPerInstance: 800}}
	pc, ok := NewCheckerWithLimits(m, PoolLimits{TestConcurrency: 2, MaxJobsPerInstance: 3}).(*PoolChecker)
	if !ok {
		t.Fatal("pooled module did not produce a *PoolChecker")
	}
	if pc.testConcurrency() != 2 || pc.maxJobsPerInstance() != 3 {
		t.Fatalf("limits not applied: concurrency=%d jobs=%d", pc.testConcurrency(), pc.maxJobsPerInstance())
	}

	plain := NewCheckerWithLimits(Module{Name: "plain"}, PoolLimits{TestConcurrency: 2})
	if _, ok := plain.(Checker); !ok {
		t.Fatalf("non-pooled module produced %T, want Checker", plain)
	}
}
