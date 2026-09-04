package discovery

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sonalys/animeman/internal/integrations/nyaa"
)

type (
	Dependencies struct {
		NYAA            *nyaa.API
		AnimeListClient AnimeListSource
		TorrentClient   TorrentClient
		Config          Config
	}

	Controller struct {
		dep             Dependencies
		intervalTracker *IntervalTracker
		verifyFailures  *verifyFailureTracker
		// trigger short-circuits the poll ticker when the API requests a rescan.
		trigger chan struct{}
	}
)

func New(dep Dependencies) *Controller {
	return &Controller{
		dep:             dep,
		intervalTracker: NewIntervalTracker(dep.Config.PollFrequency),
		verifyFailures:  newVerifyFailureTracker(),
		trigger:         make(chan struct{}, 1),
	}
}

func (c *Controller) Start(ctx context.Context) error {
	log.Info().Msgf("starting polling with frequency %s", c.dep.Config.PollFrequency.String())

	if c.dep.Config.APIAddr != "" {
		go c.serveAPI(ctx)
	}

	ticker := time.NewTicker(c.dep.Config.PollFrequency)
	defer ticker.Stop()

	for {
		if err := c.RunDiscovery(ctx); err != nil {
			log.Error().Msgf("discovery scan failed: %s", err)
		}

		select {
		case <-ticker.C:
		case <-c.trigger:
			log.Info().Msg("rescan triggered via API")
		case <-ctx.Done():
			log.Info().Msgf("stopping discovery: %s", ctx.Err())
			return nil
		}
	}
}
