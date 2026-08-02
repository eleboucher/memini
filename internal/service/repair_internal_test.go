package service

import (
	"testing"
	"time"
)

// TestRepairLeaseCoversBatch pins the invariant that keeps a slow batch from
// being claimed twice: the lease protecting a claimed batch has to outlive the
// whole batch. Three lines, and it stops a future tuning change from silently
// enabling concurrent duplicate repairs against the embedder.
func TestRepairLeaseCoversBatch(t *testing.T) {
	worst := time.Duration(repairBatch) * repairTimeout
	if repairLeaseTTL <= worst {
		t.Fatalf("repairLeaseTTL (%v) must exceed repairBatch*repairTimeout (%v), "+
			"or a slow batch can be re-claimed while it is still running",
			repairLeaseTTL, worst)
	}
}

// TestRepairBackoffIsBoundedAndJittered pins the retry schedule's two
// properties: it never exceeds the cap, and it is jittered so an outage's worth
// of rows do not all retry in the same instant against the backend that just
// failed.
func TestRepairBackoffIsBoundedAndJittered(t *testing.T) {
	for _, attempts := range []int{0, 1, 2, 5, 12, 50} {
		for range 50 {
			d := repairBackoff(attempts)
			if d <= 0 {
				t.Fatalf("repairBackoff(%d) = %v, want a positive delay", attempts, d)
			}
			if d > repairBackoffCap {
				t.Fatalf("repairBackoff(%d) = %v, want at most the %v cap", attempts, d, repairBackoffCap)
			}
		}
	}
	// Full jitter means repeated calls at the same attempt count must differ;
	// an unjittered schedule would return the same value every time.
	seen := map[time.Duration]bool{}
	for range 40 {
		seen[repairBackoff(6)] = true
	}
	if len(seen) < 2 {
		t.Fatal("repairBackoff returned a constant delay; without jitter every row degraded " +
			"in one outage retries in the same instant")
	}
}
