package backupjob

import (
	"testing"
	"time"
)

// This is the exact failure a user hit in the field: a fast LAN transfer
// showing "790 hours remaining" at 0%. The cumulative-average version of
// this code computed rate as bytes-done / elapsed-since-upload-started,
// so the very first sample - a small file whose transfer time is mostly
// fixed per-request overhead, not bandwidth - permanently dragged the
// average down for the rest of a large transfer. A rolling window must
// instead reflect only the most recent throughput.
func TestETACheckpointReflectsRecentRateNotTheSlowFirstSample(t *testing.T) {
	c := etaCheckpoint{at: time.Unix(0, 0)}
	total := int64(1_000_000_000) // 1 GB

	// First sample: a tiny file that took the full window to land - a
	// terrible apparent rate if it were allowed to set the tone.
	eta := c.update(time.Unix(0, 0).Add(etaWindow), 1024, total)
	if eta <= 0 {
		t.Fatalf("eta = %d, attendu une estimation positive après le premier échantillon lent", eta)
	}
	slowFirstETA := eta

	// Second window: real LAN throughput (100 MB in 3s = ~33 MB/s). The
	// reported ETA must reflect this recent rate, not stay anchored near
	// the pessimistic first sample.
	fastAt := time.Unix(0, 0).Add(2 * etaWindow)
	eta = c.update(fastAt, 1024+100_000_000, total)
	if eta >= slowFirstETA {
		t.Fatalf("eta = %d après un débit réel rapide, attendu nettement inférieur à l'estimation du premier échantillon lent (%d)", eta, slowFirstETA)
	}
	// 900MB left at ~33MB/s is on the order of 27s, nowhere near hours.
	if eta > 120 {
		t.Fatalf("eta = %ds, attendu un ordre de grandeur de quelques dizaines de secondes pour ce débit", eta)
	}
}

// Within a single window, the ETA must hold steady at the last completed
// window's estimate rather than resetting to zero or recomputing from a
// near-zero elapsed time on every single file.
func TestETACheckpointHoldsSteadyWithinAWindow(t *testing.T) {
	c := etaCheckpoint{at: time.Unix(0, 0)}
	total := int64(1_000_000)

	first := c.update(time.Unix(0, 0).Add(etaWindow), 500_000, total)
	if first <= 0 {
		t.Fatalf("eta = %d, attendu une estimation positive après la première fenêtre", first)
	}

	// Well within the next window - must return the same value, not jitter.
	second := c.update(time.Unix(0, 0).Add(etaWindow+time.Second), 600_000, total)
	if second != first {
		t.Fatalf("eta = %d au milieu d'une fenêtre, attendu la valeur figée de la fenêtre précédente (%d)", second, first)
	}
}

// No estimate should be reported until enough time has passed to trust a
// rate at all - and once done reaches total, there's nothing left to
// estimate, so the checkpoint must not report a stale positive ETA.
func TestETACheckpointStaysZeroBeforeFirstWindowAndOnceDone(t *testing.T) {
	c := etaCheckpoint{at: time.Unix(0, 0)}
	total := int64(1_000_000)

	if eta := c.update(time.Unix(0, 0).Add(etaWindow/2), 900_000, total); eta != 0 {
		t.Fatalf("eta = %d avant la première fenêtre complète, attendu 0", eta)
	}

	if eta := c.update(time.Unix(0, 0).Add(etaWindow), total, total); eta != 0 {
		t.Fatalf("eta = %d une fois le transfert terminé, attendu 0 (rien à estimer)", eta)
	}
}
