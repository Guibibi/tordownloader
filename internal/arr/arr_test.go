package arr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeArr is a minimal Sonarr/Radarr queue API: GET /api/v3/queue lists
// records, DELETE /api/v3/queue/{id} records the call and its params.
type fakeArr struct {
	t       *testing.T
	apiKey  string
	records []queueRecord
	deletes []deleteCall
}

type deleteCall struct {
	id     string
	params url.Values
}

func (f *fakeArr) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/queue", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != f.apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(queuePage{TotalRecords: len(f.records), Records: f.records})
	})
	mux.HandleFunc("DELETE /api/v3/queue/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != f.apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.deletes = append(f.deletes, deleteCall{id: r.PathValue("id"), params: r.URL.Query()})
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func TestNotifyFailedBlocklistsMatchingRecords(t *testing.T) {
	fake := &fakeArr{t: t, apiKey: "k", records: []queueRecord{
		{ID: 7, DownloadID: "AABBCCDDEEFF00112233445566778899AABBCCDD", Title: "Show S01E01"},
		{ID: 8, DownloadID: "AABBCCDDEEFF00112233445566778899AABBCCDD", Title: "Show S01E02"},
		{ID: 9, DownloadID: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", Title: "Other"},
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	n := New([]Instance{{Name: "sonarr", URL: srv.URL + "/", APIKey: "k"}}, nil)
	// Lowercase hash must match the *arr's uppercase downloadId.
	handled, err := n.NotifyFailed(context.Background(), "aabbccddeeff00112233445566778899aabbccdd", "Show")
	if err != nil {
		t.Fatalf("NotifyFailed: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if len(fake.deletes) != 2 {
		t.Fatalf("expected 2 queue deletes (both episodes of the grab), got %d", len(fake.deletes))
	}
	for _, d := range fake.deletes {
		if d.id != "7" && d.id != "8" {
			t.Errorf("deleted unexpected queue id %s", d.id)
		}
		for param, want := range map[string]string{
			"removeFromClient": "true",
			"blocklist":        "true",
			"skipRedownload":   "false",
		} {
			if got := d.params.Get(param); got != want {
				t.Errorf("delete %s: param %s = %q, want %q", d.id, param, got, want)
			}
		}
	}
}

func TestNotifyFailedNotFound(t *testing.T) {
	fake := &fakeArr{t: t, apiKey: "k"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	n := New([]Instance{{Name: "sonarr", URL: srv.URL, APIKey: "k"}}, nil)
	handled, err := n.NotifyFailed(context.Background(), "aabbccddeeff00112233445566778899aabbccdd", "Show")
	if err != nil {
		t.Fatalf("NotifyFailed: %v", err)
	}
	if handled {
		t.Fatal("expected handled=false for a hash no instance tracks")
	}
	if len(fake.deletes) != 0 {
		t.Fatalf("expected no deletes, got %d", len(fake.deletes))
	}
}

func TestNotifyFailedUnreachableInstanceIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := New([]Instance{{Name: "sonarr", URL: srv.URL, APIKey: "k"}}, nil)
	handled, err := n.NotifyFailed(context.Background(), "aabbccddeeff00112233445566778899aabbccdd", "Show")
	if err == nil {
		t.Fatal("expected an error when the instance cannot be checked")
	}
	if handled {
		t.Fatal("expected handled=false on error")
	}
}

func TestNotifyFailedSecondInstanceHasIt(t *testing.T) {
	empty := &fakeArr{t: t, apiKey: "k1"}
	srvEmpty := httptest.NewServer(empty.handler())
	defer srvEmpty.Close()

	owner := &fakeArr{t: t, apiKey: "k2", records: []queueRecord{
		{ID: 3, DownloadID: "AABBCCDDEEFF00112233445566778899AABBCCDD", Title: "Movie"},
	}}
	srvOwner := httptest.NewServer(owner.handler())
	defer srvOwner.Close()

	n := New([]Instance{
		{Name: "sonarr", URL: srvEmpty.URL, APIKey: "k1"},
		{Name: "radarr", URL: srvOwner.URL, APIKey: "k2"},
	}, nil)
	handled, err := n.NotifyFailed(context.Background(), "aabbccddeeff00112233445566778899aabbccdd", "Movie")
	if err != nil {
		t.Fatalf("NotifyFailed: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true via the second instance")
	}
	if len(owner.deletes) != 1 || owner.deletes[0].id != "3" {
		t.Fatalf("expected one delete of queue id 3, got %+v", owner.deletes)
	}
}

func TestSystemStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(statusResponse{AppName: "Sonarr", Version: "4.0.10"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	n := New(nil, nil)
	st, err := n.systemStatus(context.Background(), Instance{Name: "sonarr", URL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("systemStatus: %v", err)
	}
	if st.AppName != "Sonarr" || st.Version != "4.0.10" {
		t.Fatalf("status = %+v", st)
	}

	_, err = n.systemStatus(context.Background(), Instance{Name: "sonarr", URL: srv.URL, APIKey: "wrong"})
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("expected rejected-API-key error, got %v", err)
	}
}

func TestVerifyConnectionsSurvivesDownInstance(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(statusResponse{AppName: "Radarr", Version: "5.11"})
	}))
	defer up.Close()

	n := New([]Instance{
		{Name: "sonarr", URL: "http://127.0.0.1:1", APIKey: "k"}, // nothing listening
		{Name: "radarr", URL: up.URL, APIKey: "k"},
	}, nil)
	// Must log-and-continue past the unreachable instance without panicking.
	n.VerifyConnections(context.Background())
}

func TestNotifyFailedDelete404IsSuccess(t *testing.T) {
	records := []queueRecord{{ID: 5, DownloadID: "AABBCCDDEEFF00112233445566778899AABBCCDD"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/queue", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(queuePage{TotalRecords: 1, Records: records})
	})
	mux.HandleFunc("DELETE /api/v3/queue/{id}", func(w http.ResponseWriter, r *http.Request) {
		// Already removed on the *arr side (e.g. a concurrent manual removal).
		http.Error(w, fmt.Sprintf("queue item %s not found", r.PathValue("id")), http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	n := New([]Instance{{Name: "sonarr", URL: srv.URL, APIKey: "k"}}, nil)
	handled, err := n.NotifyFailed(context.Background(), "aabbccddeeff00112233445566778899aabbccdd", "Show")
	if err != nil {
		t.Fatalf("NotifyFailed: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true when the queue item was already gone")
	}
}
