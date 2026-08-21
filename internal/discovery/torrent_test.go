package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/sonalys/animeman/pkg/v1/torrentclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			assert.Equal(t, tt.out, hasAllTags(tt.have, tt.want))
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

	err := c.verifyTorrentAdded(context.Background(), "!show", []string{"!show", "s1e1"})
	require.NoError(t, err)
	assert.Equal(t, 1, client.calls, "expected exactly 1 List call")
}

func Test_verifyTorrentAdded_appearsAfterAFewPolls(t *testing.T) {
	origInterval := torrentAddVerifyInterval
	torrentAddVerifyInterval = time.Millisecond
	defer func() { torrentAddVerifyInterval = origInterval }()

	client := &fakeTorrentClient{
		torrentsAfterCalls: [][]torrentclient.Torrent{
			{}, // not there yet
			{{Hash: "other", Tags: []string{"!show", "s1e2"}}},       // a different episode, doesn't count
			{{Hash: "abc", Tags: []string{"!show", "s1e1", "1080"}}}, // there, extra tag doesn't matter
		},
	}
	c := newTestController(client)

	err := c.verifyTorrentAdded(context.Background(), "!show", []string{"!show", "s1e1"})
	require.NoError(t, err)
	assert.Equal(t, 3, client.calls, "expected exactly 3 List calls")
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

	err := c.verifyTorrentAdded(context.Background(), "!show", []string{"!show", "s1e1"})
	assert.Error(t, err)
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

	err := c.verifyTorrentAdded(ctx, "!show", []string{"!show", "s1e1"})
	assert.Error(t, err)
}

func Test_verifyTorrentAdded_ignoresEmptyTagsButUsesRealOnes(t *testing.T) {
	client := &fakeTorrentClient{
		torrentsAfterCalls: [][]torrentclient.Torrent{
			{{Hash: "abc", Tags: []string{"!show"}}}, // qBittorrent never stored the empty tag
		},
	}
	c := newTestController(client)

	err := c.verifyTorrentAdded(context.Background(), "!show", []string{"!show", ""})
	assert.NoError(t, err, "expected the non-empty tag alone to verify successfully")
}
