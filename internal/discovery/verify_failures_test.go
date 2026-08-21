package discovery

import "testing"

func Test_verifyFailureTracker_skipsAfterMaxFailures(t *testing.T) {
	tr := newVerifyFailureTracker()
	tags := []string{"!show", "s1e1"}

	for i := 1; i < maxVerifyFailuresBeforeSkip; i++ {
		count, shouldSkip := tr.recordFailure(tags)
		if shouldSkip {
			t.Fatalf("failure %d: expected not to skip yet, got shouldSkip=true", i)
		}
		if count != i {
			t.Errorf("failure %d: expected count %d, got %d", i, i, count)
		}
	}

	count, shouldSkip := tr.recordFailure(tags)
	if !shouldSkip {
		t.Fatalf("failure %d: expected shouldSkip=true at the threshold", maxVerifyFailuresBeforeSkip)
	}
	if count != maxVerifyFailuresBeforeSkip {
		t.Errorf("expected count %d, got %d", maxVerifyFailuresBeforeSkip, count)
	}
}

func Test_verifyFailureTracker_successResetsCount(t *testing.T) {
	tr := newVerifyFailureTracker()
	tags := []string{"!show", "s1e1"}

	tr.recordFailure(tags)
	tr.recordFailure(tags)
	tr.recordSuccess(tags)

	count, shouldSkip := tr.recordFailure(tags)
	if shouldSkip {
		t.Fatal("expected not to skip right after a reset")
	}
	if count != 1 {
		t.Errorf("expected count to restart at 1 after a success, got %d", count)
	}
}

func Test_verifyFailureTracker_pastThreshold(t *testing.T) {
	tr := newVerifyFailureTracker()
	tags := []string{"!show", "s1e1"}

	if tr.pastThreshold(tags) {
		t.Fatal("expected a never-seen tag set to not be past threshold")
	}

	for i := 0; i < maxVerifyFailuresBeforeSkip-1; i++ {
		tr.recordFailure(tags)
	}
	if tr.pastThreshold(tags) {
		t.Fatal("expected not past threshold with one failure left before the limit")
	}

	tr.recordFailure(tags)
	if !tr.pastThreshold(tags) {
		t.Fatal("expected past threshold once the limit is reached")
	}

	// pastThreshold must not itself mutate state.
	if !tr.pastThreshold(tags) {
		t.Fatal("expected pastThreshold to be idempotent")
	}
}

func Test_verifyFailureTracker_tracksTagSetsIndependently(t *testing.T) {
	tr := newVerifyFailureTracker()

	tr.recordFailure([]string{"!show", "s1e1"})
	tr.recordFailure([]string{"!show", "s1e1"})
	count, _ := tr.recordFailure([]string{"!show", "s1e2"})

	if count != 1 {
		t.Errorf("expected a different tag set to have its own independent count, got %d", count)
	}
}
