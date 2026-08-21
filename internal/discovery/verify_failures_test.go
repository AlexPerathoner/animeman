package discovery

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_verifyFailureTracker_skipsAfterMaxFailures(t *testing.T) {
	tr := newVerifyFailureTracker()
	tags := []string{"!show", "s1e1"}

	for i := 1; i < maxVerifyFailuresBeforeSkip; i++ {
		count, shouldSkip := tr.recordFailure(tags)
		require.Falsef(t, shouldSkip, "failure %d: expected not to skip yet", i)
		assert.Equalf(t, i, count, "failure %d: unexpected count", i)
	}

	count, shouldSkip := tr.recordFailure(tags)
	require.Truef(t, shouldSkip, "failure %d: expected shouldSkip=true at the threshold", maxVerifyFailuresBeforeSkip)
	assert.Equal(t, maxVerifyFailuresBeforeSkip, count)
}

func Test_verifyFailureTracker_successResetsCount(t *testing.T) {
	tr := newVerifyFailureTracker()
	tags := []string{"!show", "s1e1"}

	tr.recordFailure(tags)
	tr.recordFailure(tags)
	tr.recordSuccess(tags)

	count, shouldSkip := tr.recordFailure(tags)
	assert.False(t, shouldSkip, "expected not to skip right after a reset")
	assert.Equal(t, 1, count, "expected count to restart at 1 after a success")
}

func Test_verifyFailureTracker_pastThreshold(t *testing.T) {
	tr := newVerifyFailureTracker()
	tags := []string{"!show", "s1e1"}

	require.False(t, tr.pastThreshold(tags), "expected a never-seen tag set to not be past threshold")

	for i := 0; i < maxVerifyFailuresBeforeSkip-1; i++ {
		tr.recordFailure(tags)
	}
	require.False(t, tr.pastThreshold(tags), "expected not past threshold with one failure left before the limit")

	tr.recordFailure(tags)
	require.True(t, tr.pastThreshold(tags), "expected past threshold once the limit is reached")

	// pastThreshold must not itself mutate state.
	assert.True(t, tr.pastThreshold(tags), "expected pastThreshold to be idempotent")
}

func Test_verifyFailureTracker_tracksTagSetsIndependently(t *testing.T) {
	tr := newVerifyFailureTracker()

	tr.recordFailure([]string{"!show", "s1e1"})
	tr.recordFailure([]string{"!show", "s1e1"})
	count, _ := tr.recordFailure([]string{"!show", "s1e2"})

	assert.Equal(t, 1, count, "expected a different tag set to have its own independent count")
}
