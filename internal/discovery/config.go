package discovery

import (
	"time"
)

type Config struct {
	SearchSuffix          string
	Sources               []string
	PreferredSources      []string
	PreferredSourcesDelay time.Duration
	Qualitites            []string
	Category              string
	RenameTorrent         bool
	DownloadPath          string
	CreateShowFolder      bool
	PollFrequency         time.Duration
	// APIAddr enables the rescan HTTP API (e.g. ":8091"); empty disables it.
	APIAddr  string
	APIToken string
}
