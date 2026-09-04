package discovery

import (
	"testing"
	"time"

	"github.com/sonalys/animeman/internal/integrations/nyaa"
	"github.com/sonalys/animeman/internal/parser"
	"github.com/sonalys/animeman/internal/tags"
	"github.com/sonalys/animeman/pkg/v1/animelist"
	"github.com/stretchr/testify/require"
)

func newFilterData() *FilterData {
	return &FilterData{DiscardReason: make(map[DiscardReason]uint)}
}

func spEntry(source string, season int, episode float64, pub time.Time) parser.ParsedNyaa {
	return parser.ParsedNyaa{
		ExtractedMetadata: parser.Metadata{
			Source: source,
			Tag:    tags.SeasonEpisode(season, episode),
		},
		NyaaTorrent: nyaa.Item{PubDate: pub.Format(time.RFC1123Z)},
	}
}

func sources(list []parser.ParsedNyaa) []string {
	out := make([]string, len(list))
	for i, e := range list {
		out[i] = e.ExtractedMetadata.Source
	}
	return out
}

func Test_applySourcePriority(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	fresh := now.Add(-2 * time.Hour)  // within a 24h grace
	stale := now.Add(-30 * time.Hour) // past a 24h grace
	preferred := []string{"Erai-raws"}
	const grace = 24 * time.Hour

	t.Run("preferred release wins immediately, drops the rest for that episode", func(t *testing.T) {
		in := []parser.ParsedNyaa{
			spEntry("ASW", 1, 1, fresh),
			spEntry("Erai-raws", 1, 1, fresh),
			spEntry("Judas", 1, 1, fresh),
		}
		fd := newFilterData()
		got := applySourcePriority(in, preferred, grace, now, fd)
		require.Equal(t, []string{"Erai-raws"}, sources(got))
		require.Equal(t, uint(2), fd.DiscardReason[DiscardReasonSourceNotPreferred])
	})

	t.Run("only non-preferred, still within grace: episode is held", func(t *testing.T) {
		in := []parser.ParsedNyaa{spEntry("ASW", 1, 1, fresh)}
		fd := newFilterData()
		got := applySourcePriority(in, preferred, grace, now, fd)
		require.Empty(t, got)
		require.Equal(t, uint(1), fd.DiscardReason[DiscardReasonAwaitingPreferredSource])
	})

	t.Run("only non-preferred, grace elapsed: fall back to it", func(t *testing.T) {
		in := []parser.ParsedNyaa{
			spEntry("ASW", 1, 1, stale),
			spEntry("Judas", 1, 1, fresh),
		}
		got := applySourcePriority(in, preferred, grace, now, newFilterData())
		require.ElementsMatch(t, []string{"ASW", "Judas"}, sources(got))
	})

	t.Run("grace is measured from the oldest non-preferred release", func(t *testing.T) {
		// newest is fresh, but the oldest has been up long enough -> release the hold
		in := []parser.ParsedNyaa{
			spEntry("ASW", 1, 1, fresh),
			spEntry("Judas", 1, 1, stale),
		}
		got := applySourcePriority(in, preferred, grace, now, newFilterData())
		require.Len(t, got, 2)
	})

	t.Run("a held episode blocks every later episode this scan", func(t *testing.T) {
		in := []parser.ParsedNyaa{
			spEntry("Erai-raws", 1, 1, fresh), // preferred -> kept
			spEntry("ASW", 1, 2, fresh),       // only non-preferred, within grace -> held
			spEntry("Erai-raws", 1, 3, fresh), // preferred, but earlier hole -> must not pass
		}
		fd := newFilterData()
		got := applySourcePriority(in, preferred, grace, now, fd)
		require.Equal(t, []string{"Erai-raws"}, sources(got))
		require.Equal(t, tags.SeasonEpisode(1, 1), got[0].ExtractedMetadata.Tag)
		require.Equal(t, uint(2), fd.DiscardReason[DiscardReasonAwaitingPreferredSource])
	})

	t.Run("earlier fallback resolved, later preferred still flows", func(t *testing.T) {
		in := []parser.ParsedNyaa{
			spEntry("ASW", 1, 1, stale),       // grace elapsed -> fallback taken
			spEntry("Erai-raws", 1, 2, fresh), // preferred -> taken
		}
		got := applySourcePriority(in, preferred, grace, now, newFilterData())
		require.Equal(t, []string{"ASW", "Erai-raws"}, sources(got))
	})

	t.Run("source match is case-insensitive", func(t *testing.T) {
		in := []parser.ParsedNyaa{spEntry("erai-RAWS", 1, 1, fresh)}
		got := applySourcePriority(in, preferred, grace, now, newFilterData())
		require.Len(t, got, 1)
	})

	t.Run("zero delay accepts non-preferred right away", func(t *testing.T) {
		in := []parser.ParsedNyaa{spEntry("ASW", 1, 1, fresh)}
		got := applySourcePriority(in, preferred, 0, now, newFilterData())
		require.Equal(t, []string{"ASW"}, sources(got))
	})

	t.Run("multi-episode batches pass through untouched", func(t *testing.T) {
		batch := spEntry("ASW", 1, 1, fresh)
		batch.ExtractedMetadata.Tag = tags.Tag{Seasons: []int{1}, Episodes: []float64{1, 12}}
		require.True(t, batch.ExtractedMetadata.Tag.IsMultiEpisode())
		got := applySourcePriority([]parser.ParsedNyaa{batch}, preferred, grace, now, newFilterData())
		require.Len(t, got, 1)
	})
}

func Test_filterRelevantResults_sourcePriorityOff(t *testing.T) {
	now := time.Now()
	parsed := []parser.ParsedNyaa{
		spEntry("ASW", 1, 1, now.Add(-time.Hour)),
	}
	// empty preferredSources => behaves exactly as before, non-preferred passes through
	got := filterRelevantResults(animelist.Entry{}, parsed, tags.Zero, newFilterData(), Config{})
	require.Len(t, got, 1)
}
