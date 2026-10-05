package discovery

import (
	"testing"
	"time"

	"github.com/sonalys/animeman/internal/integrations/nyaa"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nyaaItem(title, hash string, seeders int) nyaa.Item {
	return nyaa.Item{
		Title:    title,
		InfoHash: hash,
		Seeders:  seeders,
		PubDate:  time.Now().Add(-time.Hour).Format(time.RFC1123Z),
		Size:     "1.4 GiB",
		Trusted:  "Yes",
	}
}

func TestBuildCandidates(t *testing.T) {
	entry := watching("Tsuihou Sareta Tensei Juukishi")
	items := []nyaa.Item{
		nyaaItem("[SubsPlease] Tsuihou Sareta Tensei Juukishi - 05 (1080p) [ABCD].mkv", "a", 300),
		nyaaItem("[Erai-raws] Tsuihou Sareta Tensei Juukishi - 05 [720p][HEVC][Multiple Subtitle].mkv", "b", 40),
		nyaaItem("[Erai-raws] Tsuihou Sareta Tensei Juukishi - 05 [1080p][HEVC x265][Multiple Subtitle].mkv", "c", 90),
		nyaaItem("[Erai-raws] Tsuihou Sareta Tensei Juukishi - 04 [1080p][HEVC][Multiple Subtitle].mkv", "d", 99), // other ep
		nyaaItem("[Judas] Tsuihou Sareta Tensei Juukishi - 01-12 (Batch) [1080p][HEVC x265]", "e", 99),       // batch
		nyaaItem("[Erai-raws] Some Other Show - 05 [1080p][HEVC].mkv", "f", 99),                              // other show
	}
	cfg := Config{Sources: []string{"Erai-raws"}, Qualitites: []string{"HEVC", "1080"}, PreferredQualities: []string{"HEVC"}}

	got := buildCandidates(entry, items, 5, cfg)
	require.Len(t, got, 3)

	// filter matches first (resolution desc), then the SubsPlease release outside rssConfig.sources
	assert.Equal(t, []string{"c", "b", "a"}, []string{got[0].ID, got[1].ID, got[2].ID})
	assert.True(t, got[0].MatchesFilters)
	assert.False(t, got[2].MatchesFilters)

	assert.Equal(t, "Erai-raws", got[0].Source)
	assert.Equal(t, 1080, got[0].Resolution)
	assert.Equal(t, "hevc", got[0].Codec)
	assert.True(t, got[0].PreferredQuality)
	assert.Equal(t, "SubsPlease", got[2].Source)
	assert.Equal(t, "", got[2].Codec)
	assert.False(t, got[2].PreferredQuality)
	assert.True(t, got[2].Trusted)
}

func TestDetectCodec(t *testing.T) {
	for title, want := range map[string]string{
		"[A] X - 01 [1080p][HEVC x265 10bit]": "hevc",
		"[A] X - 01 (1080p) [x264]":           "h264",
		"[A] X - 01 [1080p AV1 Opus]":         "av1",
		"[A] X - 01 [1080p AVC]":              "h264",
		"[A] X - 01 (1080p)":                  "",
	} {
		assert.Equal(t, want, detectCodec(title), title)
	}
}
