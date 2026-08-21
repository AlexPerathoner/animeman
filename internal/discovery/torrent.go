package discovery

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/rs/zerolog/log"
	"github.com/sonalys/animeman/internal/parser"
	"github.com/sonalys/animeman/internal/tags"
	"github.com/sonalys/animeman/internal/utils"
	"github.com/sonalys/animeman/pkg/v1/animelist"
	"github.com/sonalys/animeman/pkg/v1/torrentclient"
)

// qBittorrent's torrents/add returns success as soon as it accepts the request, then
// fetches the .torrent file itself, server-side, asynchronously. If that background
// fetch fails (dead nyaa link, transient network issue), no torrent entry is ever
// created, and the add call's own success response gives no indication of that. These
// bound how long verifyTorrentAdded waits for the torrent to actually materialize
// before giving up on it for this pass. Vars, not consts, so tests can shrink them.
var (
	torrentAddVerifyTimeout  = 10 * time.Second
	torrentAddVerifyInterval = 2 * time.Second
)

// findLatestTag will receive an anime list entry and return all torrents listed from the anime.
func (c *Controller) findLatestTag(ctx context.Context, entry animelist.Entry) (tags.Tag, error) {
	logger := getLogger(ctx)
	torrents := make([]torrentclient.Torrent, 0, 100)

	for _, title := range entry.Titles {
		req := &torrentclient.ListTorrentConfig{
			Tag: utils.Pointer(parser.BuildTitleTag(title)),
		}
		resp, err := c.dep.TorrentClient.List(ctx, req)

		if len(resp) == 0 {
			continue
		}

		logger.
			Trace().
			Str("tag", *req.Tag).
			Msg("identified entry tag on torrent client")

		if err != nil {
			return tags.Tag{}, fmt.Errorf("listing torrents: %w", err)
		}

		torrents = append(torrents, resp...)
	}

	latestTag := getLatestTag(torrents)
	if !latestTag.IsZero() {
		logger.
			Debug().
			Str("latestTag", latestTag.String()).
			Msg("identified latest tag on torrent client")
	}

	return latestTag, nil
}

// TorrentGetDownloadPath returns a torrent path, creating a show folder if configured.
func (c *Controller) TorrentGetDownloadPath(title string) (path string) {
	if c.dep.Config.CreateShowFolder {
		return fmt.Sprintf("%s/%s", c.dep.Config.DownloadPath, title)
	}
	return c.dep.Config.DownloadPath
}

func (c *Controller) buildTorrentName(title string, parsedNyaa parser.ParsedNyaa) string {
	var b strings.Builder

	if parsedNyaa.ExtractedMetadata.Source != "" {
		b.WriteString("[")
		b.WriteString(parsedNyaa.ExtractedMetadata.Source)
		b.WriteString("] ")
	}

	b.WriteString(title)

	tag := parsedNyaa.ExtractedMetadata.Tag

	// Avoid printing S1 on titles, since lots of shows and movies dont require this notation.
	if tag.LastEpisode() > 0 {
		b.WriteString(" ")
		b.WriteString(tag.String())
	}

	if parsedNyaa.ExtractedMetadata.VerticalResolution > 0 {
		b.WriteString(" ")
		fmt.Fprintf(&b, "[%dp]", parsedNyaa.ExtractedMetadata.VerticalResolution)
	}

	return b.String()
}

// selectIdealTitle avoids kanji titles for example, preferring english ones.
func selectIdealTitle(titles []string) string {
	if len(titles) == 0 {
		return ""
	}

	viableCandidates := make([]string, 0, len(titles))

	for _, t := range titles {
		if isASCII(t) {
			viableCandidates = append(viableCandidates, t)
		}
	}

	// Prefer the shortest title for the tags.
	sort.Slice(viableCandidates, func(i, j int) bool {
		return len(viableCandidates[i]) < len(viableCandidates[j])
	})

	if len(viableCandidates) > 0 {
		return viableCandidates[0]
	}

	// Fallback to first element if no ASCII title is found
	return titles[0]
}

func isASCII(s string) bool {
	for _, c := range s {
		if c > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// buildEpisodeTags computes the tags a torrent for this (entry, parsedNyaa) pair would be
// submitted with — pulled out of addTorrentEntry so a caller can identify an episode (to
// consult verifyFailureTracker, for instance) before deciding whether to attempt the add
// at all, without a second, potentially-drifting copy of the real tag-building logic.
// Tags are built from the anime list title, not nyaa's raw parsed title (see the comment
// on addTorrentEntry below), so this — not recomputing independently — is the only correct
// way to get the tags a given add would actually use.
func buildEpisodeTags(animeListEntry animelist.Entry, parsedNyaa parser.ParsedNyaa) (selectedTitle string, tags []string) {
	selectedTitle = selectIdealTitle(animeListEntry.Titles)

	meta := parsedNyaa.ExtractedMetadata.Clone()
	// Use nyaa metadata, but with anime list title.
	// This behavior avoids different sources creating different tags and downloading the same episode twice.
	meta.Title = selectedTitle

	return selectedTitle, meta.BuildTorrentTags()
}

// AddTorrentEntry receives an anime list entry and a downloadable torrent.
// It will configure all necessary metadata and send it to your torrent client.
func (c *Controller) AddTorrentEntry(ctx context.Context, animeListEntry animelist.Entry, parsedNyaa parser.ParsedNyaa) error {
	selectedTitle, tags := buildEpisodeTags(animeListEntry, parsedNyaa)
	return c.addTorrentEntry(ctx, selectedTitle, tags, parsedNyaa)
}

// addTorrentEntry is AddTorrentEntry's implementation, taking the already-built title/tags
// (see buildEpisodeTags) rather than computing them itself, so a caller that already needed
// them beforehand (to consult verifyFailureTracker before deciding whether to add at all)
// isn't forced to either recompute them or discard the first computation.
func (c *Controller) addTorrentEntry(ctx context.Context, selectedTitle string, tags []string, parsedNyaa parser.ParsedNyaa) error {
	req := &torrentclient.AddTorrentConfig{
		Tags:     tags,
		URLs:     []string{parsedNyaa.NyaaTorrent.Link},
		Category: c.dep.Config.Category,
		SavePath: c.TorrentGetDownloadPath(selectedTitle),
	}

	if c.dep.Config.RenameTorrent {
		req.Name = utils.Pointer(c.buildTorrentName(selectedTitle, parsedNyaa))
	}

	if err := c.dep.TorrentClient.AddTorrent(ctx, req); err != nil {
		return fmt.Errorf("adding torrents: %w", err)
	}

	return nil
}

// verifyTorrentAdded confirms a torrent submitted via addTorrentEntry actually exists in
// the torrent client — see the package-level comment on torrentAddVerifyTimeout for why
// the add call succeeding isn't enough on its own. Polls rather than trusting a single
// check immediately after add, since qBittorrent's background fetch takes a moment even
// when it succeeds.
func (c *Controller) verifyTorrentAdded(ctx context.Context, addedTags []string) error {
	// Metadata.Tag can be the zero value when title parsing couldn't extract season/
	// episode info (see parser.Parse's tags.Tag{} default) — its String() is "" in that
	// case, same as any other tag field that ends up empty. An empty string isn't a tag
	// qBittorrent can be reliably checked against (it may be stripped rather than stored
	// as a literal empty tag), so require-matching on it would either always fail this
	// entry's verification or accidentally match on nothing. Filter those out; if that
	// leaves nothing usable there's no way to distinguish this add from any other in the
	// category, so skip verification rather than produce an unreliable result.
	usableTags := make([]string, 0, len(addedTags))
	for _, t := range addedTags {
		if t != "" {
			usableTags = append(usableTags, t)
		}
	}
	if len(usableTags) == 0 {
		return nil
	}

	deadline := time.Now().Add(torrentAddVerifyTimeout)

	for {
		torrents, err := c.dep.TorrentClient.List(ctx, &torrentclient.ListTorrentConfig{
			Category: &c.dep.Config.Category,
			// filter server-side by one usable tag to keep the response small; every
			// usable tag is still checked client-side below since ListTorrentConfig only
			// supports filtering on one tag at a time, and a single tag isn't precise
			// enough on its own to rule out a same-episode-number collision between two
			// different shows sharing the series tag's absence.
			Tag: &usableTags[0],
		})
		if err != nil {
			return fmt.Errorf("listing torrents: %w", err)
		}
		for _, t := range torrents {
			if hasAllTags(t.Tags, usableTags) {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf(
				"torrent tagged %v never appeared in the client after %s — the add request was accepted, "+
					"but the download likely failed to start (dead link, or a network issue reaching it)",
				usableTags, torrentAddVerifyTimeout,
			)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(torrentAddVerifyInterval):
		}
	}
}

func hasAllTags(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TorrentRegenerateTags will scan all torrents from the configured category and update their tags.
// This function exists for when you already have a collection of Anime categorized torrents.
// This function will tag all entries from the configured category for smart episode detection and filtering.
func (c *Controller) TorrentRegenerateTags(ctx context.Context) error {
	torrents, err := c.dep.TorrentClient.List(ctx, &torrentclient.ListTorrentConfig{
		Category: &c.dep.Config.Category,
		Tag:      utils.Pointer(""),
	})
	if err != nil {
		return fmt.Errorf("listing torrents: %w", err)
	}

	for _, torrent := range torrents {
		meta := parser.Parse(torrent.Name, 1)
		tags := meta.BuildTorrentTags()

		log.
			Info().
			Any("metadata", meta).
			Strs("tags", tags).
			Msgf("updating torrent tags")

		if err := c.dep.TorrentClient.AddTorrentTags(ctx, []string{torrent.Hash}, tags); err != nil {
			return fmt.Errorf("updating tags: %w", err)
		}
	}

	return nil
}
