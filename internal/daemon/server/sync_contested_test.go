package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

func contestedFixture() syncdb.ContestedNotespace {
	return syncdb.ContestedNotespace{
		NotespaceID:  "01NS",
		Root:         "/notebooks/default/notespaces/alpha",
		Reason:       "adoption pending: 1 of 2 colliding path(s) hold un-synced local notes that differ (subject match)",
		Detail:       "hash overlap: 1/2 colliding path(s) are already byte-identical",
		Colliding:    2,
		Identical:    1,
		Divergent:    1,
		SubjectMatch: "match",
	}
}

func TestHandleSyncContestedServesTheVerdictWithItsEvidence(t *testing.T) {
	s := New(false)
	s.SetSyncContested(func() []syncdb.ContestedNotespace {
		return []syncdb.ContestedNotespace{contestedFixture()}
	}, nil)

	w := httptest.NewRecorder()
	s.handleSyncContested(w, httptest.NewRequest(http.MethodGet, "/api/sync/contested", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out contestedResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Contested) != 1 {
		t.Fatalf("contested = %+v, want one entry", out.Contested)
	}
	if got := out.Contested[0]; got.NotespaceID != "01NS" || got.Divergent != 1 || got.SubjectMatch != "match" {
		t.Fatalf("entry = %+v, want the evidence carried through", got)
	}
}

// An unconfigured sync stack must say so. An empty list would read as "nothing
// is withheld" on a daemon that is not watching anything at all.
func TestContestedEndpointsRefuseWhenSyncIsNotConfigured(t *testing.T) {
	s := New(false)
	w := httptest.NewRecorder()
	s.handleSyncContested(w, httptest.NewRequest(http.MethodGet, "/api/sync/contested", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleSyncAdoptContested(w, httptest.NewRequest(http.MethodPost, "/api/sync/contested/adopt", bytes.NewBufferString(`{"notespace_id":"01NS"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("adopt status=%d, want 503", w.Code)
	}
}

func TestHandleSyncAdoptContestedNamesWhatItAdopts(t *testing.T) {
	var adoptedID string
	s := New(false)
	s.SetSyncContested(
		func() []syncdb.ContestedNotespace { return nil },
		func(id string) (syncdb.ContestedNotespace, string, error) {
			adoptedID = id
			return contestedFixture(), "/state/sync/adoptions/01NS.toml", nil
		})

	// No id: refused, and nothing is adopted by default.
	w := httptest.NewRecorder()
	s.handleSyncAdoptContested(w, httptest.NewRequest(http.MethodPost, "/api/sync/contested/adopt", bytes.NewBufferString(`{}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for a body naming no notespace", w.Code)
	}
	if adoptedID != "" {
		t.Fatalf("an id-less request adopted %q", adoptedID)
	}

	w = httptest.NewRecorder()
	s.handleSyncAdoptContested(w, httptest.NewRequest(http.MethodPost, "/api/sync/contested/adopt", bytes.NewBufferString(`{"notespace_id":"01NS"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out adoptContestedResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if adoptedID != "01NS" || out.Adopted.NotespaceID != "01NS" || out.Receipt == "" {
		t.Fatalf("adopt = %q -> %+v (receipt %q)", adoptedID, out.Adopted, out.Receipt)
	}
}

// Adopting something that is not contested is the operator's mistake, and a
// script has to be able to tell that from a broken daemon.
func TestHandleSyncAdoptContestedReportsAnUncontestedNotespaceAsConflict(t *testing.T) {
	s := New(false)
	s.SetSyncContested(
		func() []syncdb.ContestedNotespace { return nil },
		func(string) (syncdb.ContestedNotespace, string, error) {
			return syncdb.ContestedNotespace{}, "", errNotContested{}
		})
	w := httptest.NewRecorder()
	s.handleSyncAdoptContested(w, httptest.NewRequest(http.MethodPost, "/api/sync/contested/adopt", bytes.NewBufferString(`{"notespace_id":"01NS"}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409", w.Code)
	}
}

type errNotContested struct{}

func (errNotContested) Error() string { return "notespace 01NS is not contested; nothing to adopt" }

func TestContestedEndpointsRejectTheWrongMethod(t *testing.T) {
	s := New(false)
	s.SetSyncContested(func() []syncdb.ContestedNotespace { return nil }, nil)
	w := httptest.NewRecorder()
	s.handleSyncContested(w, httptest.NewRequest(http.MethodPost, "/api/sync/contested", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d, want 405", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleSyncAdoptContested(w, httptest.NewRequest(http.MethodGet, "/api/sync/contested/adopt", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("adopt status=%d, want 405 — a GET that adopts is a GET something retries", w.Code)
	}
}
