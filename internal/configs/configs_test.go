package configs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func Test_RSSConfig_Validate_preferredSources(t *testing.T) {
	base := func() *RSSConfig {
		return &RSSConfig{Type: RSSTypeNyaa, PollFrequency: time.Minute}
	}

	t.Run("no preferred sources: delay left untouched", func(t *testing.T) {
		c := base()
		require.NoError(t, c.Validate())
		require.Zero(t, c.PreferredSourcesDelay)
	})

	t.Run("preferred sources without delay: defaults to 24h", func(t *testing.T) {
		c := base()
		c.PreferredSources = []string{"Erai-raws"}
		require.NoError(t, c.Validate())
		require.Equal(t, 24*time.Hour, c.PreferredSourcesDelay)
	})

	t.Run("preferred sources with explicit delay: kept", func(t *testing.T) {
		c := base()
		c.PreferredSources = []string{"Erai-raws"}
		c.PreferredSourcesDelay = 6 * time.Hour
		require.NoError(t, c.Validate())
		require.Equal(t, 6*time.Hour, c.PreferredSourcesDelay)
	})

	t.Run("negative delay is rejected", func(t *testing.T) {
		c := base()
		c.PreferredSources = []string{"Erai-raws"}
		c.PreferredSourcesDelay = -time.Second
		require.Error(t, c.Validate())
	})
}
