package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sonalys/animeman/pkg/v1/animelist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAnimeList struct {
	entries []animelist.Entry
}

func (f fakeAnimeList) GetCurrentlyWatching(context.Context) ([]animelist.Entry, error) {
	return f.entries, nil
}

func watching(title string) animelist.Entry {
	return animelist.NewEntry(
		[]string{title}, animelist.ListStatusWatching, animelist.AiringStatusAiring,
		time.Time{}, time.Time{}, 12, nil,
	)
}

func newAPITestController(entries ...animelist.Entry) *Controller {
	return New(Dependencies{
		AnimeListClient: fakeAnimeList{entries: entries},
		Config:          Config{APIAddr: ":0", APIToken: "secret"},
	})
}

func TestRequireToken(t *testing.T) {
	c := newAPITestController()
	h := c.requireToken(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, tt := range []struct {
		name, auth string
		want       int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"correct token", "Bearer secret", http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/rescan", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, tt.want, rec.Code)
		})
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) rescanResponse {
	t.Helper()
	var resp rescanResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp
}

func post(c *Controller, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, "/rescan", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/rescan", strings.NewReader(body))
	}
	c.handleRescan(rec, r)
	return rec
}

func TestHandleRescanEmptyBodyScansAll(t *testing.T) {
	c := newAPITestController()
	rec := post(c, "")

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "all", decode(t, rec).Scope)

	_, all := c.intervalTracker.consumePending()
	assert.True(t, all)

	select {
	case <-c.trigger:
	default:
		t.Fatal("expected a trigger signal")
	}
}

func TestHandleRescanByTitle(t *testing.T) {
	c := newAPITestController(watching("Frieren"), watching("One Piece"))
	rec := post(c, `{"title":"frieren"}`)

	require.Equal(t, http.StatusAccepted, rec.Code)
	resp := decode(t, rec)
	assert.Equal(t, "shows", resp.Scope)
	assert.Equal(t, []string{"Frieren"}, resp.Shows)

	forced, all := c.intervalTracker.consumePending()
	assert.False(t, all)
	assert.True(t, forced["Frieren"])
}

func TestHandleRescanByTitlesAnyMatch(t *testing.T) {
	// the dashboard sends every title it knows; any one matching is enough
	c := newAPITestController(watching("Sousou no Frieren"))
	rec := post(c, `{"titles":["Frieren: Beyond Journey's End","Sousou no Frieren"]}`)

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, []string{"Sousou no Frieren"}, decode(t, rec).Shows)
}

func TestHandleRescanExactMatchDoesNotSweepInSimilarSeasons(t *testing.T) {
	c := newAPITestController(
		watching("Mushoku Tensei I"),
		watching("Mushoku Tensei II"),
		watching("Mushoku Tensei III"),
	)
	rec := post(c, `{"title":"Mushoku Tensei III"}`)

	require.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, []string{"Mushoku Tensei III"}, decode(t, rec).Shows)
}

func TestHandleRescanUnknownTitleIs404AndScansNothing(t *testing.T) {
	c := newAPITestController(watching("Frieren"))
	rec := post(c, `{"title":"nonexistent show"}`)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "none", decode(t, rec).Scope)

	forced, all := c.intervalTracker.consumePending()
	assert.False(t, all)
	assert.Empty(t, forced)

	select {
	case <-c.trigger:
		t.Fatal("no scan should have been triggered")
	default:
	}
}
