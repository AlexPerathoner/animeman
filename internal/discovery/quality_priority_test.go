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

func qEntry(title string, season int, episode float64, pub time.Time) parser.ParsedNyaa {
	return parser.ParsedNyaa{
		ExtractedMetadata: parser.Metadata{Tag: tags.SeasonEpisode(season, episode)},
		NyaaTorrent:       nyaa.Item{Title: title, PubDate: pub.Format(time.RFC1123Z)},
	}
}

func titles(list []parser.ParsedNyaa) []string {
	out := make([]string, len(list))
	for i, e := range list {
		out[i] = e.NyaaTorrent.Title
	}
	return out
}

func Test_isPreferredQuality(t *testing.T) {
	pref := isPreferredQuality([]string{"HEVC"})
	require.True(t, pref(qEntry("[Judas] Youjo Senki - S02E09 [1080p][HEVC x265 10bit]", 2, 9, time.Now())))
	require.True(t, pref(qEntry("[Erai-raws] Youjo Senki II - 08 [1080p CR WEBRip hevc AAC]", 2, 8, time.Now())))
	require.False(t, pref(qEntry("[Erai-raws] Youjo Senki II - 09 [1080p CR WEB-DL AVC AAC]", 2, 9, time.Now())))
}

func Test_qualityPriority_viaApplyPriority(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	fresh := now.Add(-2 * time.Hour)
	stale := now.Add(-30 * time.Hour)
	pref := isPreferredQuality([]string{"HEVC"})
	const grace = 24 * time.Hour

	run := func(in []parser.ParsedNyaa, fd *FilterData) []parser.ParsedNyaa {
		return applyPriority(in, pref, grace, now, fd,
			DiscardReasonQualityNotPreferred, DiscardReasonAwaitingPreferredQuality)
	}

	t.Run("HEVC present wins, AVC dropped", func(t *testing.T) {
		fd := newFilterData()
		got := run([]parser.ParsedNyaa{
			qEntry("[Erai-raws] Show II - 09 [1080p AVC]", 2, 9, fresh),
			qEntry("[Judas] Show - S02E09 [1080p][HEVC x265]", 2, 9, fresh),
		}, fd)
		require.Equal(t, []string{"[Judas] Show - S02E09 [1080p][HEVC x265]"}, titles(got))
		require.Equal(t, uint(1), fd.DiscardReason[DiscardReasonQualityNotPreferred])
	})

	t.Run("only AVC within grace: held", func(t *testing.T) {
		fd := newFilterData()
		got := run([]parser.ParsedNyaa{qEntry("[Erai-raws] Show II - 09 [1080p AVC]", 2, 9, fresh)}, fd)
		require.Empty(t, got)
		require.Equal(t, uint(1), fd.DiscardReason[DiscardReasonAwaitingPreferredQuality])
	})

	t.Run("only AVC, grace elapsed: accept it", func(t *testing.T) {
		got := run([]parser.ParsedNyaa{qEntry("[Erai-raws] Show II - 09 [1080p AVC]", 2, 9, stale)}, newFilterData())
		require.Len(t, got, 1)
	})
}

func Test_filterRelevantResults_qualityPriority(t *testing.T) {
	now := time.Now()
	cfg := Config{
		PreferredQualities:      []string{"HEVC"},
		PreferredQualitiesDelay: 24 * time.Hour,
	}

	// ep 9: only a fresh 1080p AVC release -> held until the grace elapses
	held := filterRelevantResults(animelist.Entry{}, []parser.ParsedNyaa{
		qEntry("[Erai-raws] Show II - 09 [1080p AVC]", 2, 9, now.Add(-time.Hour)),
	}, tags.SeasonEpisode(2, 8), newFilterData(), cfg)
	require.Empty(t, held)

	// same release, now 30h old -> accepted
	taken := filterRelevantResults(animelist.Entry{}, []parser.ParsedNyaa{
		qEntry("[Erai-raws] Show II - 09 [1080p AVC]", 2, 9, now.Add(-30*time.Hour)),
	}, tags.SeasonEpisode(2, 8), newFilterData(), cfg)
	require.Len(t, taken, 1)

	// with no preferredQualities set, the AVC release passes straight through
	off := filterRelevantResults(animelist.Entry{}, []parser.ParsedNyaa{
		qEntry("[Erai-raws] Show II - 09 [1080p AVC]", 2, 9, now.Add(-time.Hour)),
	}, tags.SeasonEpisode(2, 8), newFilterData(), Config{})
	require.Len(t, off, 1)
}
