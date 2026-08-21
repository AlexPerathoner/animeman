package discovery

import (
	"strings"
	"sync"
)

// maxVerifyFailuresBeforeSkip bounds how many consecutive scans a single episode add is
// allowed to fail verification (see verifyTorrentAdded) before the discovery loop gives up
// on it and moves on to later episodes instead of blocking the whole entry on it forever.
//
// Retrying is what fixes the case that motivated verification in the first place — a
// transient failure (network hiccup, VPN flakiness) resolving itself by the next scan.
// But an episode whose nyaa release is genuinely dead (link 404s, nothing will ever fetch
// it) would otherwise stall every episode after it indefinitely, which is worse than the
// pre-verification behavior of silently leaving a gap and moving on. This bounds that
// downside: after this many consecutive failures the episode is skipped (logged at Error,
// so it's visible instead of silent) rather than blocking forever.
//
// In scan counts, not wall-clock time, since it's compared once per RunDiscovery pass —
// real elapsed time before giving up is roughly this times the configured poll frequency.
const maxVerifyFailuresBeforeSkip = 3

// verifyFailureTracker counts consecutive verification failures per torrent tag set,
// in-memory only — deliberately not persisted, matching IntervalTracker's same choice:
// losing this on restart just means one extra retry round after a restart, not a
// correctness problem, and animeman doesn't otherwise keep any state file.
type verifyFailureTracker struct {
	mu     sync.Mutex
	counts map[string]int
}

func newVerifyFailureTracker() *verifyFailureTracker {
	return &verifyFailureTracker{counts: make(map[string]int)}
}

// key must uniquely identify one specific episode add attempt — the full tag set (series
// + episode) already serves that purpose elsewhere in this package (see hasAllTags), so
// it's reused here rather than introducing a second identity scheme for the same episode.
func verifyFailureKey(tags []string) string {
	return strings.Join(tags, "|")
}

// recordFailure increments the failure count for tags and reports whether the caller
// should give up on it now (count reached maxVerifyFailuresBeforeSkip).
func (t *verifyFailureTracker) recordFailure(tags []string) (count int, shouldSkip bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := verifyFailureKey(tags)
	t.counts[key]++
	count = t.counts[key]
	return count, count >= maxVerifyFailuresBeforeSkip
}

// recordSuccess clears any failure history for tags — if the same tag set is ever
// attempted again later (e.g. after manual intervention), it starts from a clean count
// rather than skipping immediately because of an old, since-resolved failure streak.
func (t *verifyFailureTracker) recordSuccess(tags []string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.counts, verifyFailureKey(tags))
}

// pastThreshold reports whether tags already gave up on a prior scan, without recording
// anything itself. Meant to be checked before attempting an add at all — once past
// threshold, retrying costs a real add call plus verifyTorrentAdded's full poll timeout,
// every scan, forever, for something already known to keep failing; skip straight past it
// instead.
func (t *verifyFailureTracker) pastThreshold(tags []string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.counts[verifyFailureKey(tags)] >= maxVerifyFailuresBeforeSkip
}
