package discovery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sonalys/animeman/internal/integrations/nyaa"
	"github.com/sonalys/animeman/internal/parser"
	"github.com/sonalys/animeman/internal/tags"
	"github.com/sonalys/animeman/internal/utils"
	"github.com/sonalys/animeman/pkg/v1/animelist"
	"github.com/sonalys/animeman/pkg/v1/torrentclient"
)

// RunDiscovery controls the discovery routine,
// fetching entries from your anime list and looking for updates in Nyaa.si
// After finding updates, it will verify episode collision and dispatch it to your torrent client.
func (c *Controller) RunDiscovery(ctx context.Context) error {
	t1 := time.Now()

	log.
		Debug().
		Msgf("discovery started")

	ctx = log.Logger.WithContext(ctx)

	if err := c.TorrentRegenerateTags(ctx); err != nil {
		return fmt.Errorf("updating qBittorrent entries: %w", err)
	}

	entries, err := c.dep.AnimeListClient.GetCurrentlyWatching(ctx)
	if err != nil {
		return fmt.Errorf("fetching anime list: %w", err)
	}

	scannedCount := 0
	skippedCount := 0

	// API-requested rescans: read and cleared once per pass, overriding the adaptive interval.
	forcedKeys, forceAll := c.intervalTracker.consumePending()
	if forceAll {
		log.Debug().Msg("forced rescan of all shows this pass")
	}

	for _, entry := range entries {
		forced := forceAll || forcedKeys[getShowKey(entry.Titles)]

		// Check if this show should be scanned based on adaptive intervals
		if !forced && !c.intervalTracker.ShouldScanNow(entry) {
			skippedCount++
			log.
				Trace().
				Str("title", selectIdealTitle(entry.Titles)).
				Time("nextScanAt", c.intervalTracker.GetNextScanTime(entry)).
				Msgf("skipping entry: not due for scan yet")
			continue
		}

		logger := log.Logger.
			With().
			Str("title", selectIdealTitle(entry.Titles)).
			Logger()

		ctx := logger.WithContext(ctx)

		logger.
			Trace().
			Msgf("starting discovery for entry")

		foundNew, err := c.DiscoverEntry(ctx, entry)
		if errors.Is(err, torrentclient.ErrUnauthorized) || errors.Is(err, context.Canceled) {
			return fmt.Errorf("failed to digest entry: %w", err)
		}

		// Update the interval tracker with the scan results
		nextScanAt := c.intervalTracker.UpdateState(entry, foundNew)

		scannedCount++

		logger.
			Debug().
			Bool("foundNew", foundNew).
			Time("nextScanAt", nextScanAt).
			Msgf("discovery finished for entry")
	}

	log.
		Debug().
		Int("scanned", scannedCount).
		Int("skipped", skippedCount).
		Dur("duration", time.Since(t1)).
		Msg("discovery finished")

	return nil
}

// filterEpisodes will only return ParsedNyaa entries that are more recent than the given latestTag.
// excludeBatch is used when a show is airing or you have already downloaded some episodes of the season.
// excludeBatch avoids downloading a batch for episodes which you already have.
func filterEpisodes(
	results []parser.ParsedNyaa,
	initialTag tags.Tag,
	filterData *FilterData,
) ([]parser.ParsedNyaa, tags.Tag) {
	out := make([]parser.ParsedNyaa, 0, len(results))

	var latestDetectedTag tags.Tag

	for _, nyaaEntry := range results {
		currentTag := nyaaEntry.ExtractedMetadata.Tag

		if tagCompare(currentTag, initialTag) <= 0 || tagCompare(currentTag, latestDetectedTag) <= 0 {
			filterData.DiscardReason[DiscardReasonOlderEpisode]++
			continue
		}

		if !latestDetectedTag.IsZero() {
			if latestDetectedTag.IsMultiEpisode() && latestDetectedTag.Contains(currentTag) {
				filterData.DiscardReason[DiscardReasonOlderEpisode]++
				continue
			}

			// This scenario can happen when we are filtering for batches, and the subsequent batch contains the previous batch.
			// Example: S01E01-13, followed by S01.
			// This happens because S01E01-13 < S01, so S01 comes afterwards. But S01 contains the previous tag.
			if currentTag.IsMultiEpisode() && currentTag.Contains(latestDetectedTag) {
				out = utils.Filter(out, func(previous parser.ParsedNyaa) bool {
					if currentTag.Contains(previous.ExtractedMetadata.Tag) {
						filterData.DiscardReason[DiscardReasonOlderEpisode]++
						return false
					}

					return true
				})
			}
		}

		latestDetectedTag = currentTag
		out = append(out, nyaaEntry)
	}

	return out, latestDetectedTag
}

func parseResults(entry animelist.Entry, results []nyaa.Item) []parser.ParsedNyaa {
	return utils.Map(results, func(item nyaa.Item) parser.ParsedNyaa {
		return parser.NewParsedNyaa(entry, item)
	})
}

// sortResults will digest the raw data from Nyaa into a parsed metadata struct `ParsedNyaa`.
// it will also sort the response by season and episode.
// it's important it returns a crescent season/episode list, so you don't download a recent episode and
// don't download the oldest ones in case you don't have all episodes since your latestTag.
func sortResults(entry animelist.Entry, results []parser.ParsedNyaa) []parser.ParsedNyaa {
	smallerFunc := func(i, j int) bool {
		first := results[i]
		second := results[j]

		// Sort first by season/episode tag.
		cmp := tagCompare(first.ExtractedMetadata.Tag, second.ExtractedMetadata.Tag)
		if cmp != 0 {
			return cmp < 0
		}

		// Then title similarity.
		titleSimilarityI := utils.Max(utils.Map(entry.Titles, func(curTitle string) float64 {
			return utils.CalculateTextSimilarity(curTitle, first.ExtractedMetadata.Title, ignoreCharset)
		})...)

		titleSimilarityJ := utils.Max(utils.Map(entry.Titles, func(curTitle string) float64 {
			return utils.CalculateTextSimilarity(curTitle, second.ExtractedMetadata.Title, ignoreCharset)
		})...)

		if titleSimilarityI != titleSimilarityJ {
			return titleSimilarityI > titleSimilarityJ
		}

		// Then resolution.
		cmp = second.ExtractedMetadata.VerticalResolution - first.ExtractedMetadata.VerticalResolution
		if cmp != 0 {
			return cmp < 0
		}

		// Then prioritize number of seeds
		return first.NyaaTorrent.Seeders > second.NyaaTorrent.Seeders
	}

	sort.Slice(results, smallerFunc)

	return results
}

// filterRelevantResults is responsible for filtering and ordering the raw Nyaa feed into valid downloadable torrents.
// Preferred sources / qualities are optional (empty = off); see applyPriority.
func filterRelevantResults(
	entry animelist.Entry,
	results []parser.ParsedNyaa,
	latestTag tags.Tag,
	filterData *FilterData,
	cfg Config,
) []parser.ParsedNyaa {
	results = slices.Clone(results)
	// Requires sorted input, since we use tag progression.
	results = sortResults(entry, results)

	if latestTag.IsZero() && entry.AiringStatus == animelist.AiringStatusAired {
		batchResults := utils.Filter(results, func(entry parser.ParsedNyaa) bool {
			return entry.ExtractedMetadata.Tag.IsMultiEpisode()
		})
		if len(batchResults) > 0 {
			filterData.DiscardReason[DiscardReasonNotBatch] += uint(len(batchResults))
			results = batchResults
		}
	} else {
		// Remove batches when there are latest tags, avoid episode download duplication.
		results = utils.Filter(results, func(entry parser.ParsedNyaa) bool {
			return !entry.ExtractedMetadata.Tag.IsMultiEpisode()
		})
	}

	now := time.Now()
	if len(cfg.PreferredSources) > 0 {
		results = applyPriority(results, isPreferredSource(cfg.PreferredSources), cfg.PreferredSourcesDelay, now, filterData,
			DiscardReasonSourceNotPreferred, DiscardReasonAwaitingPreferredSource)
	}
	if len(cfg.PreferredQualities) > 0 {
		results = applyPriority(results, isPreferredQuality(cfg.PreferredQualities), cfg.PreferredQualitiesDelay, now, filterData,
			DiscardReasonQualityNotPreferred, DiscardReasonAwaitingPreferredQuality)
	}

	results, latestDetectedTag := filterEpisodes(results, latestTag, filterData)
	filterData.NewLatestTag = latestDetectedTag

	return results
}

func isPreferredSource(preferred []string) func(parser.ParsedNyaa) bool {
	return func(r parser.ParsedNyaa) bool {
		return slices.ContainsFunc(preferred, func(p string) bool {
			return strings.EqualFold(p, r.ExtractedMetadata.Source)
		})
	}
}

// isPreferredQuality matches the raw torrent title against each preferred quality
// as a case-insensitive substring — the same way rssConfig.qualities filters the
// Nyaa query (e.g. "HEVC" matches "[1080p][HEVC x265 10bit]").
func isPreferredQuality(preferred []string) func(parser.ParsedNyaa) bool {
	return func(r parser.ParsedNyaa) bool {
		title := strings.ToLower(r.NyaaTorrent.Title)
		return slices.ContainsFunc(preferred, func(p string) bool {
			return strings.Contains(title, strings.ToLower(p))
		})
	}
}

// applySourcePriority is kept for the existing tests; new callers use applyPriority.
func applySourcePriority(results []parser.ParsedNyaa, preferred []string, delay time.Duration, now time.Time, filterData *FilterData) []parser.ParsedNyaa {
	return applyPriority(results, isPreferredSource(preferred), delay, now, filterData,
		DiscardReasonSourceNotPreferred, DiscardReasonAwaitingPreferredSource)
}

// applyPriority walks the (tag-ascending) result list one episode at a time:
//
//   - if a preferred release exists for the episode, keep only those;
//   - else if the oldest non-preferred release for it has been up for at least
//     delay, accept the non-preferred releases (the wait is over);
//   - else stop here entirely — drop this episode and every later one this scan.
//
// The last point matters: animeman derives "latest episode" purely from what's
// present in the torrent client (see findLatestTag), so simply not adding a
// deferred episode is enough for it to be reconsidered on the next scan. But a
// LATER episode must not be added while an earlier one is still being held, or
// the earlier one would be permanently skipped. Stopping at the first held
// episode preserves the no-gaps invariant DiscoverEntry relies on.
//
// Multi-episode batches are passed through untouched: the "wait for a preferred
// release" behaviour is about the per-episode release race, and filterEpisodes
// owns batch-vs-episode containment — second-guessing it here would let a batch
// that filterEpisodes was going to discard anyway trigger a hold.
//
// Run once per preference axis (source, then quality). Each pass is idempotent on
// an already-narrowed list, and the "defer this episode and everything after"
// rule composes: whichever axis holds an episode first stops the scan there.
func applyPriority(
	results []parser.ParsedNyaa,
	isPreferred func(parser.ParsedNyaa) bool,
	delay time.Duration,
	now time.Time,
	filterData *FilterData,
	notPreferred, awaiting DiscardReason,
) []parser.ParsedNyaa {
	out := make([]parser.ParsedNyaa, 0, len(results))

	for start, end := 0, 0; start < len(results); start = end {
		// Collect the run of entries sharing this episode tag (input is tag-sorted).
		end = start
		for end < len(results) && tagCompare(results[end].ExtractedMetadata.Tag, results[start].ExtractedMetadata.Tag) == 0 {
			end++
		}
		group := results[start:end]

		if group[0].ExtractedMetadata.Tag.IsMultiEpisode() {
			out = append(out, group...)
			continue
		}

		var preferredHits, other []parser.ParsedNyaa
		for _, r := range group {
			if isPreferred(r) {
				preferredHits = append(preferredHits, r)
			} else {
				other = append(other, r)
			}
		}

		if len(preferredHits) > 0 {
			out = append(out, preferredHits...)
			filterData.DiscardReason[notPreferred] += uint(len(other))
		} else if now.Sub(oldestPubDate(other)) >= delay {
			out = append(out, other...) // grace elapsed — take what we have
		} else {
			// Still waiting on a preferred release for this episode. Defer it and
			// everything after it (see the doc comment).
			filterData.DiscardReason[awaiting] += uint(len(results) - start)
			break
		}
	}

	return out
}

// oldestPubDate returns the earliest parseable Nyaa publish date in the set, or
// the zero time if none parse (which makes callers treat the release as old
// enough — fail open rather than hold an episode forever on a parse quirk).
func oldestPubDate(entries []parser.ParsedNyaa) time.Time {
	var oldest time.Time
	for _, e := range entries {
		t, err := e.NyaaTorrent.PublishedDate()
		if err != nil {
			return time.Time{}
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	return oldest
}

type (
	DiscardReason string

	FilterData struct {
		LatestTag     tags.Tag               `json:"latest_tag,omitzero"`
		NewLatestTag  tags.Tag               `json:"new_latest_tag,omitzero"`
		SearchCount   int                    `json:"search_count,omitempty"`
		NewCount      int                    `json:"new_count,omitempty"`
		DiscardReason map[DiscardReason]uint `json:"discard_reason,omitempty"`
	}
)

const (
	DiscardReasonNotBatch                DiscardReason = "not_batch"
	DiscardReasonNoSeeder                DiscardReason = "no_seeder"
	DiscardReasonOlderEpisode            DiscardReason = "older_episode"
	DiscardReasonPublishedDateMismatch   DiscardReason = "publish_date_mismatch"
	DiscardReasonEpisodeCountMismatch    DiscardReason = "episode_count_mismatch"
	DiscardReasonTitleMismatch           DiscardReason = "title_mismatch"
	DiscardReasonSourceNotPreferred       DiscardReason = "source_not_preferred"
	DiscardReasonAwaitingPreferredSource  DiscardReason = "awaiting_preferred_source"
	DiscardReasonQualityNotPreferred      DiscardReason = "quality_not_preferred"
	DiscardReasonAwaitingPreferredQuality DiscardReason = "awaiting_preferred_quality"
)

func (c *Controller) NyaaSearch(
	ctx context.Context,
	entry animelist.Entry,
	filterData *FilterData,
) ([]nyaa.Item, error) {
	logger := getLogger(ctx)

	titleSanitization := strings.NewReplacer(
		"-", " ",
		"\"", " ",
		"'", " ",
		"(", " ",
		")", " ",
	)

	// Build search query for Nyaa.
	// For title we filter for english and original titles.
	sanitizedTitles := utils.Transform(entry.Titles,
		strings.ToLower,
		parser.StripTitle,
		parser.StripSubtitle,
		titleSanitization.Replace,
	)
	sanitizedTitles = slices.Compact(sanitizedTitles)

	entries, err := c.dep.NYAA.List(ctx, nyaa.ListOptions{
		SearchSuffix:        c.dep.Config.SearchSuffix,
		Titles:              sanitizedTitles,
		VerticalResolutions: c.dep.Config.Qualitites,
		Sources:             c.dep.Config.Sources,
	})
	if err != nil {
		return nil, fmt.Errorf("getting nyaa list: %w", err)
	}

	filterData.SearchCount = len(entries)

	if len(entries) == 0 {
		return nil, nil
	}

	entries = utils.Filter(entries,
		filterMetadata(entry, filterData),
	)

	if len(entries) == 0 {
		logger.
			Debug().
			Msg("no results passed the metadata filter")
	}

	return entries, nil
}

// DiscoverEntry receives an anime list entry and fetches the anime feed, looking for new content.
// It returns the latest discovered tag, whether new episodes were found, and any error.
func (c *Controller) DiscoverEntry(ctx context.Context, entry animelist.Entry) (bool, error) {
	logger := getLogger(ctx)

	filterData := &FilterData{
		SearchCount:   0,
		DiscardReason: make(map[DiscardReason]uint),
	}

	searchResults, err := c.NyaaSearch(ctx, entry, filterData)
	if err != nil {
		return false, fmt.Errorf("searching torrent for anime: %w", err)
	}

	// Remove results without seeders.
	torrentResults := utils.Filter(searchResults,
		func(e nyaa.Item) bool {
			if e.Seeders == 0 {
				filterData.DiscardReason[DiscardReasonNoSeeder]++
				return false
			}

			return true
		},
	)

	if len(torrentResults) == 0 {
		logger.
			Debug().
			Any("filterData", filterData).
			Msg("entry discovery stopped: no valid torrent results found")

		return false, nil
	}

	latestTag, err := c.findLatestTag(ctx, entry)
	if err != nil {
		return false, fmt.Errorf("finding latest anime season episode tag: %w", err)
	}

	filterData.LatestTag = latestTag

	parsedTorrents := parseResults(entry, torrentResults)
	parsedTorrents = filterRelevantResults(entry, parsedTorrents, latestTag, filterData, c.dep.Config)

	// parsedTorrents is already in ascending season/episode order (see sortResults) —
	// that ordering matters here: verifying each add before moving to the next, and
	// stopping at the first one that doesn't verify, guarantees findLatestTag (which
	// only looks at what's actually present in the torrent client, not any memory of
	// what was attempted) can never end up believing a later episode without an earlier
	// one having actually landed. Without this, a later episode succeeding while an
	// earlier one silently failed (accepted by the add call, but never actually fetched
	// by qBittorrent) would permanently hide the earlier gap: the next scan would see
	// the later tag as "latest" and never look for anything below it again.
	//
	// Stopping unconditionally on the first failure would trade that silent-gap bug for a
	// different one, though: a genuinely dead nyaa link (not a transient hiccup) would then
	// fail verification on every scan forever, blocking every episode after it
	// indefinitely — worse than the pre-verification behavior for that specific case. So
	// this only stops for the first maxVerifyFailuresBeforeSkip scans (retrying is exactly
	// what fixes a transient failure); past that it logs at Error and moves on, accepting a
	// visible, logged gap rather than either silence or an indefinite stall.
	var verifiedCount int

	for _, episodeTorrent := range parsedTorrents {
		selectedTitle, seriesTag, addedTags := buildEpisodeTags(entry, episodeTorrent)

		// Already gave up on this exact episode in an earlier scan (see the comment
		// above) — skip straight past it rather than paying for another add call plus
		// verifyTorrentAdded's full poll timeout on something already known to keep
		// failing. Scoped to this specific tag set, not the whole entry, so later
		// episodes are unaffected and still get their own full add+verify+retry budget.
		if c.verifyFailures.pastThreshold(addedTags) {
			logger.
				Debug().
				Stringer("tag", episodeTorrent.ExtractedMetadata.Tag).
				Msg("skipping episode still not verified after repeated attempts in earlier scans")
			continue
		}

		if err := c.addTorrentEntry(ctx, selectedTitle, addedTags, episodeTorrent); err != nil {
			return verifiedCount > 0, fmt.Errorf("adding torrent to client: %w", err)
		}

		if err := c.verifyTorrentAdded(ctx, seriesTag, addedTags); err != nil {
			failureCount, shouldSkip := c.verifyFailures.recordFailure(addedTags)
			if !shouldSkip {
				logger.
					Warn().
					Err(err).
					Stringer("tag", episodeTorrent.ExtractedMetadata.Tag).
					Int("failureCount", failureCount).
					Msg("torrent add could not be verified, stopping here for this entry so it's retried next scan instead of being silently skipped")
				break
			}

			logger.
				Error().
				Err(err).
				Stringer("tag", episodeTorrent.ExtractedMetadata.Tag).
				Int("failureCount", failureCount).
				Msg("torrent add still not verified after repeated attempts, giving up on this episode and moving on — likely a dead release, may need manual intervention")
			continue
		}

		c.verifyFailures.recordSuccess(addedTags)
		verifiedCount++
	}

	// Honest count even on the early-stop path above — logging len(parsedTorrents) here
	// would claim every episode succeeded when the loop may have stopped partway through.
	filterData.NewCount = verifiedCount

	logger.
		Info().
		Any("filterData", filterData).
		Msg("entry discovery finished")

	return verifiedCount > 0, nil
}
