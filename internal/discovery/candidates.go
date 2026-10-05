package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sonalys/animeman/internal/integrations/nyaa"
	"github.com/sonalys/animeman/internal/parser"
	"github.com/sonalys/animeman/internal/utils"
	"github.com/sonalys/animeman/pkg/v1/animelist"
)

// Manual release picking: when the automatic scan keeps missing an episode (release
// group not in rssConfig.sources, quality filter, waiting on a preferred release, dead
// link...), /candidates lists every nyaa release of that one episode — the configured
// source/quality filters are reported per release, not applied — and /grab adds the
// chosen one exactly like a discovery add (same tags, save path, name, verification),
// so later scans treat the episode as held.

type (
	candidatesRequest struct {
		Titles  []string `json:"titles"`
		Episode float64  `json:"episode"`
	}

	grabRequest struct {
		Titles  []string `json:"titles"`
		Episode float64  `json:"episode"`
		ID      string   `json:"id"` // a Candidate.ID from /candidates
	}

	Candidate struct {
		ID         string    `json:"id"`
		Title      string    `json:"title"`
		Source     string    `json:"source"`
		Resolution int       `json:"resolution"`
		Codec      string    `json:"codec"` // hevc | h264 | av1 | "" (unknown)
		Size       string    `json:"size"`
		Seeders    int       `json:"seeders"`
		Leechers   int       `json:"leechers"`
		Downloads  int       `json:"downloads"`
		Published  time.Time `json:"published,omitzero"`
		Trusted    bool      `json:"trusted"`
		Remake     bool      `json:"remake"`
		// MatchesFilters: would pass rssConfig.sources + rssConfig.qualities, i.e. the
		// automatic scan could have picked it.
		MatchesFilters   bool `json:"matches_filters"`
		PreferredSource  bool `json:"preferred_source"`
		PreferredQuality bool `json:"preferred_quality"`

		parsed parser.ParsedNyaa
	}

	candidatesResponse struct {
		Show       string      `json:"show"`
		Episode    float64     `json:"episode"`
		Candidates []Candidate `json:"candidates"`
	}
)

func (c *Controller) handleCandidates(w http.ResponseWriter, r *http.Request) {
	var req candidatesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Episode <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "titles and episode are required"})
		return
	}
	entry, ok := c.resolveEntry(r.Context(), req.Titles)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "no watched show matched"})
		return
	}
	cands, err := c.episodeCandidates(r.Context(), entry, req.Episode)
	if err != nil {
		log.Warn().Err(err).Msg("candidates: nyaa search failed")
		writeJSON(w, http.StatusBadGateway, map[string]string{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, candidatesResponse{
		Show: selectIdealTitle(entry.Titles), Episode: req.Episode, Candidates: cands,
	})
}

func (c *Controller) handleGrab(w http.ResponseWriter, r *http.Request) {
	var req grabRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Episode <= 0 || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "titles, episode and id are required"})
		return
	}
	entry, ok := c.resolveEntry(r.Context(), req.Titles)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "no watched show matched"})
		return
	}

	// Re-search rather than trusting a link from the caller: only a release nyaa
	// actually lists for this show + episode can be added.
	cands, err := c.episodeCandidates(r.Context(), entry, req.Episode)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"detail": err.Error()})
		return
	}
	idx := slices.IndexFunc(cands, func(cd Candidate) bool { return cd.ID == req.ID })
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "that release is no longer listed on nyaa"})
		return
	}
	pick := cands[idx]

	selectedTitle, seriesTag, addedTags := buildEpisodeTags(entry, pick.parsed)
	if err := c.addTorrentEntry(r.Context(), selectedTitle, addedTags, pick.parsed); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"detail": err.Error()})
		return
	}
	if err := c.verifyTorrentAdded(r.Context(), seriesTag, addedTags); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"detail": "qBittorrent accepted it but the download never started — try another release"})
		return
	}
	c.verifyFailures.recordSuccess(addedTags)
	log.Info().Str("title", selectedTitle).Str("release", pick.Title).Msg("manual grab via API")
	writeJSON(w, http.StatusCreated, map[string]string{"detail": "added: " + pick.Title})
}

// resolveEntry is resolveShowKeys returning the entry itself (first exact match, else
// first fuzzy one).
func (c *Controller) resolveEntry(ctx context.Context, titles []string) (animelist.Entry, bool) {
	entries, err := c.dep.AnimeListClient.GetCurrentlyWatching(ctx)
	if err != nil {
		log.Warn().Msgf("could not fetch anime list for title match: %s", err)
		return animelist.Entry{}, false
	}
	lowered := make([]string, 0, len(titles))
	for _, t := range titles {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			lowered = append(lowered, t)
		}
	}
	if len(lowered) == 0 {
		return animelist.Entry{}, false
	}
	var fuzzy *animelist.Entry
	for i, e := range entries {
		switch matchStrength(e, lowered) {
		case matchExact:
			return e, true
		case matchFuzzy:
			if fuzzy == nil {
				fuzzy = &entries[i]
			}
		}
	}
	if fuzzy != nil {
		return *fuzzy, true
	}
	return animelist.Entry{}, false
}

// episodeCandidates searches nyaa for the show twice — title alone, and title plus the
// zero-padded episode number, since a busy show's title-only feed (75 items max) can
// push an older episode out — then keeps the single-episode releases of `episode`.
func (c *Controller) episodeCandidates(ctx context.Context, entry animelist.Entry, episode float64) ([]Candidate, error) {
	titles := slices.Compact(utils.Transform(entry.Titles,
		strings.ToLower, parser.StripTitle, parser.StripSubtitle,
		strings.NewReplacer("-", " ", "\"", " ", "'", " ", "(", " ", ")", " ").Replace,
	))

	seen := map[string]bool{}
	var items []nyaa.Item
	for _, suffix := range []string{fmt.Sprintf("%02d", int(episode)), ""} {
		found, err := c.dep.NYAA.List(ctx, nyaa.ListOptions{
			Titles:       titles,
			SearchSuffix: strings.TrimSpace(c.dep.Config.SearchSuffix + " " + suffix),
		})
		if err != nil {
			return nil, fmt.Errorf("searching nyaa: %w", err)
		}
		for _, it := range found {
			key := it.InfoHash + "|" + it.GUID
			if !seen[key] {
				seen[key] = true
				items = append(items, it)
			}
		}
	}
	return buildCandidates(entry, items, episode, c.dep.Config), nil
}

// buildCandidates is the pure part of episodeCandidates: same title / date / episode
// count sanity filter as a discovery scan, then this episode only, best first.
func buildCandidates(entry animelist.Entry, items []nyaa.Item, episode float64, cfg Config) []Candidate {
	fd := &FilterData{DiscardReason: map[DiscardReason]uint{}}
	items = utils.Filter(items, filterMetadata(entry, fd))

	prefSource := isPreferredSource(cfg.PreferredSources)
	prefQuality := isPreferredQuality(cfg.PreferredQualities)
	inSources := isPreferredSource(cfg.Sources)
	inQualities := isPreferredQuality(cfg.Qualitites)

	out := make([]Candidate, 0, len(items))
	for _, it := range items {
		p := parser.NewParsedNyaa(entry, it)
		tag := p.ExtractedMetadata.Tag
		if tag.IsMultiEpisode() || tag.LastEpisode() != episode {
			continue
		}
		published, _ := it.PublishedDate()
		id := it.InfoHash
		if id == "" {
			id = it.GUID
		}
		out = append(out, Candidate{
			ID:               id,
			Title:            it.Title,
			Source:           p.ExtractedMetadata.Source,
			Resolution:       p.ExtractedMetadata.VerticalResolution,
			Codec:            detectCodec(it.Title),
			Size:             it.Size,
			Seeders:          it.Seeders,
			Leechers:         it.Leechers,
			Downloads:        it.Downloads,
			Published:        published,
			Trusted:          strings.EqualFold(it.Trusted, "yes"),
			Remake:           strings.EqualFold(it.Remake, "yes"),
			MatchesFilters:   (len(cfg.Sources) == 0 || inSources(p)) && (len(cfg.Qualitites) == 0 || inQualities(p)),
			PreferredSource:  len(cfg.PreferredSources) > 0 && prefSource(p),
			PreferredQuality: len(cfg.PreferredQualities) > 0 && prefQuality(p),
			parsed:           p,
		})
	}

	// what the automatic scan would have wanted first, then resolution, then health
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.MatchesFilters != b.MatchesFilters {
			return a.MatchesFilters
		}
		if a.Resolution != b.Resolution {
			return a.Resolution > b.Resolution
		}
		return a.Seeders > b.Seeders
	})
	return out
}

func detectCodec(title string) string {
	t := strings.ToLower(title)
	switch {
	case strings.Contains(t, "hevc"), strings.Contains(t, "x265"), strings.Contains(t, "h265"), strings.Contains(t, "h.265"):
		return "hevc"
	case strings.Contains(t, "av1"):
		return "av1"
	case strings.Contains(t, "x264"), strings.Contains(t, "h264"), strings.Contains(t, "h.264"), strings.Contains(t, "avc"):
		return "h264"
	}
	return ""
}
