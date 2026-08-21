package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/sonalys/animeman/pkg/v1/torrentclient"
)

// fakeTorrentClient implements TorrentClient for tests. List returns torrentsAfterCalls[n]
// on its n-th call (0-indexed), clamped to the last entry once calls run past the slice —
// lets a test script "the torrent shows up after N polls" without real waiting logic.
type fakeTorrentClient struct {
	torrentsAfterCalls [][]torrentclient.Torrent
	calls              int
}

func (f *fakeTorrentClient) List(_ context.Context, _ *torrentclient.ListTorrentConfig) ([]torrentclient.Torrent, error) {
	i := f.calls
	if i >= len(f.torrentsAfterCalls) {
		i = len(f.torrentsAfterCalls) - 1
	}
	f.calls++
	return f.torrentsAfterCalls[i], nil
}

func (f *fakeTorrentClient) AddTorrent(_ context.Context, _ *torrentclient.AddTorrentConfig) error {
	return nil
}

func (f *fakeTorrentClient) AddTorrentTags(_ context.Context, _ []string, _ []string) error {
	return nil
}

func newTestController(client TorrentClient) *Controller {
	return &Controller{
		dep: Dependencies{
			TorrentClient: client,
			Config:        Config{Category: "Animes"},
		},
		verifyFailures: newVerifyFailureTracker(),
	}
}

func Test_hasAllTags(t *testing.T) {
	tests := []struct {
		name string
		have []string
		want []string
		out  bool
	}{
		{"exact match", []string{"!show", "s1e1"}, []string{"!show", "s1e1"}, true},
		{"have extra tags", []string{"!show", "s1e1", "extra"}, []string{"!show", "s1e1"}, true},
		{"missing one", []string{"!show"}, []string{"!show", "s1e1"}, false},
		{"missing all", []string{}, []string{"!show", "s1e1"}, false},
		{"empty want always matches", []string{"!show"}, []string{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasAllTags(tt.have, tt.want); got != tt.out {
				t.Errorf("hasAllTags(%v, %v) = %v, want %v", tt.have, tt.want, got, tt.out)
			}
		})
	}
}

func Test_verifyTorrentAdded_appearsImmediately(t *testing.T) {
	client := &fakeTorrentClient{
		torrentsAfterCalls: [][]torrentclient.Torrent{
			{{Hash: "abc", Tags: []string{"!show", "s1e1"}}},
		},
	}
	c := newTestController(client)

	if err := c.verifyTorrentAdded(context.Background(), []string{"!show", "s1e1"}); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if client.calls != 1 {
		t.Errorf("expected exactly 1 List call, got %d", client.calls)
	}
}

func Test_verifyTorrentAdded_appearsAfterAFewPolls(t *testing.T) {
	origInterval := torrentAddVerifyInterval
	torrentAddVerifyInterval = time.Millisecond
	defer func() { torrentAddVerifyInterval = origInterval }()

	client := &fakeTorrentClient{
		torrentsAfterCalls: [][]torrentclient.Torrent{
			{},                                                      // not there yet
			{{Hash: "other", Tags: []string{"!show", "s1e2"}}},      // a different episode, doesn't count
			{{Hash: "abc", Tags: []string{"!show", "s1e1", "1080"}}}, // there, extra tag doesn't matter
		},
	}
	c := newTestController(client)

	if err := c.verifyTorrentAdded(context.Background(), []string{"!show", "s1e1"}); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if client.calls != 3 {
		t.Errorf("expected exactly 3 List calls, got %d", client.calls)
	}
}

func Test_verifyTorrentAdded_neverAppears(t *testing.T) {
	origTimeout, origInterval := torrentAddVerifyTimeout, torrentAddVerifyInterval
	torrentAddVerifyTimeout = 5 * time.Millisecond
	torrentAddVerifyInterval = time.Millisecond
	defer func() { torrentAddVerifyTimeout, torrentAddVerifyInterval = origTimeout, origInterval }()

	client := &fakeTorrentClient{
		torrentsAfterCalls: [][]torrentclient.Torrent{{}}, // never has it
	}
	c := newTestController(client)

	err := c.verifyTorrentAdded(context.Background(), []string{"!show", "s1e1"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func Test_verifyTorrentAdded_respectsContextCancellation(t *testing.T) {
	origInterval := torrentAddVerifyInterval
	torrentAddVerifyInterval = time.Second // long enough that the test would hang without cancellation
	defer func() { torrentAddVerifyInterval = origInterval }()

	client := &fakeTorrentClient{
		torrentsAfterCalls: [][]torrentclient.Torrent{{}},
	}
	c := newTestController(client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.verifyTorrentAdded(ctx, []string{"!show", "s1e1"})
	if err == nil {
		t.Fatal("expected an error from cancelled context, got nil")
	}
}

// A zero-value tags.Tag (parser.Parse's default when it can't extract season/episode
// info) String()s to "" — same as any other tag field that ends up empty. Requiring a
// literal "" tag to appear in qBittorrent's response would either always fail (if
// qBittorrent strips empty tags rather than storing them) or match unreliably, so
// verifyTorrentAdded is expected to skip verification entirely rather than produce that.
func Test_verifyTorrentAdded_skipsWhenNoUsableTags(t *testing.T) {
	client := &fakeTorrentClient{
		torrentsAfterCalls: [][]torrentclient.Torrent{{}}, // nothing in the client at all
	}
	c := newTestController(client)

	if err := c.verifyTorrentAdded(context.Background(), []string{"", ""}); err != nil {
		t.Fatalf("expected verification to be skipped (nil error), got %v", err)
	}
	if client.calls != 0 {
		t.Errorf("expected no List calls when there's nothing usable to check, got %d", client.calls)
	}
}

func Test_verifyTorrentAdded_ignoresEmptyTagsButUsesRealOnes(t *testing.T) {
	client := &fakeTorrentClient{
		torrentsAfterCalls: [][]torrentclient.Torrent{
			{{Hash: "abc", Tags: []string{"!show"}}}, // qBittorrent never stored the empty tag
		},
	}
	c := newTestController(client)

	if err := c.verifyTorrentAdded(context.Background(), []string{"!show", ""}); err != nil {
		t.Fatalf("expected the non-empty tag alone to verify successfully, got %v", err)
	}
}
