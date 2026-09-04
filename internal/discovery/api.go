package discovery

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sonalys/animeman/internal/utils"
	"github.com/sonalys/animeman/pkg/v1/animelist"
)

// serveAPI runs the rescan HTTP API until ctx is cancelled. Enabled only when
// Config.APIAddr is set (Config validation guarantees APIToken is set with it).
func (c *Controller) serveAPI(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /rescan", c.handleRescan)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:              c.dep.Config.APIAddr,
		Handler:           c.requireToken(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Info().Msgf("starting rescan API on %s", c.dep.Config.APIAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error().Msgf("rescan API stopped: %s", err)
	}
}

func (c *Controller) requireToken(next http.Handler) http.Handler {
	want := []byte("Bearer " + c.dep.Config.APIToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type rescanRequest struct {
	Title  string   `json:"title"`  // single title, kept for convenience
	Titles []string `json:"titles"` // any-of match — send every title you know for the show
}

type rescanResponse struct {
	Scope string   `json:"scope"`           // "all", "shows", or "none"
	Shows []string `json:"shows,omitempty"` // matched show keys when Scope == "shows"
}

// handleRescan flags shows for an unconditional scan on the next discovery pass
// and wakes the poll loop.
//
//   - body names one or more titles => scan exactly the shows they match; if
//     nothing matches, scan NOTHING and return 404 (never a surprise scan-all)
//   - empty body => explicit "rescan everything"
func (c *Controller) handleRescan(w http.ResponseWriter, r *http.Request) {
	var req rescanRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // body optional — ignore decode errors
	}

	queries := make([]string, 0, len(req.Titles)+1)
	for _, t := range append([]string{req.Title}, req.Titles...) {
		if t = strings.TrimSpace(t); t != "" {
			queries = append(queries, t)
		}
	}

	if len(queries) == 0 {
		c.intervalTracker.MarkForceRescan(nil)
		c.wake()
		log.Info().Msg("rescan requested via API (all shows)")
		writeJSON(w, http.StatusAccepted, rescanResponse{Scope: "all"})
		return
	}

	keys := c.resolveShowKeys(r.Context(), queries)
	if len(keys) == 0 {
		log.Warn().Strs("titles", queries).Msg("rescan: no watched show matched — nothing done")
		writeJSON(w, http.StatusNotFound, rescanResponse{Scope: "none"})
		return
	}

	c.intervalTracker.MarkForceRescan(keys)
	c.wake()
	log.Info().Strs("shows", keys).Msg("rescan requested via API")
	writeJSON(w, http.StatusAccepted, rescanResponse{Scope: "shows", Shows: keys})
}

func (c *Controller) wake() {
	select {
	case c.trigger <- struct{}{}:
	default: // a rescan is already pending
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// resolveShowKeys returns interval-tracker keys of currently-watched shows
// matching any query. An exact (case-insensitive) title equality anywhere wins:
// if any show matches exactly, only exact matches are returned — this keeps a
// request for "Mushoku Tensei III" from also sweeping in seasons I and II, which
// a fuzzy match on such similar titles otherwise would. Only when nothing matches
// exactly does it fall back to substring / >=0.85 similarity. Empty => no match.
func (c *Controller) resolveShowKeys(ctx context.Context, queries []string) []string {
	entries, err := c.dep.AnimeListClient.GetCurrentlyWatching(ctx)
	if err != nil {
		log.Warn().Msgf("rescan: could not fetch anime list for title match: %s", err)
		return nil
	}
	lowered := make([]string, len(queries))
	for i, q := range queries {
		lowered[i] = strings.ToLower(strings.TrimSpace(q))
	}

	var exact, fuzzy []string
	for _, e := range entries {
		switch matchStrength(e, lowered) {
		case matchExact:
			exact = append(exact, getShowKey(e.Titles))
		case matchFuzzy:
			fuzzy = append(fuzzy, getShowKey(e.Titles))
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return fuzzy
}

type matchLevel int

const (
	matchNone matchLevel = iota
	matchFuzzy
	matchExact
)

func matchStrength(e animelist.Entry, lowered []string) matchLevel {
	best := matchNone
	for _, t := range e.Titles {
		tl := strings.ToLower(t)
		for _, q := range lowered {
			if tl == q {
				return matchExact
			}
			if strings.Contains(tl, q) || strings.Contains(q, tl) ||
				utils.CalculateTextSimilarity(tl, q, ignoreCharset) >= 0.85 {
				best = matchFuzzy
			}
		}
	}
	return best
}
