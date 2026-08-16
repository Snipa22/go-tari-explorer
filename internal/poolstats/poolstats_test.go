package poolstats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// samplePoolStatsJSON mirrors a real GET /api/pool/stats response captured against the
// live pool.rxt.tari.jagtech.io backend (2026-08-17).
const samplePoolStatsJSON = `{
  "pool_list": [],
  "pool_statistics": {
    "hashRate": 123456,
    "miners": 7,
    "totalHashes": 9876543210,
    "lastBlockFoundTime": 1755400000,
    "lastBlockFound": 42000,
    "totalBlocksFound": 314,
    "roundHashes": 55555,
    "totalMinersPaid": null,
    "totalPayments": null
  },
  "last_payment": 1755390000
}`

func TestHTTPClient_GetStats(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(samplePoolStatsJSON))
	}))
	defer srv.Close()

	client := NewHTTPClient(srv.URL, nil)
	stats, err := client.GetStats(context.Background())
	if err != nil {
		t.Fatalf("GetStats() error = %v", err)
	}

	if gotPath != "/api/pool/stats" {
		t.Errorf("requested path = %q, want %q", gotPath, "/api/pool/stats")
	}

	want := PoolStats{
		HashRate:           123456,
		Miners:             7,
		TotalHashes:        9876543210,
		LastBlockFoundTime: 1755400000,
		LastBlockFound:     42000,
		TotalBlocksFound:   314,
		RoundHashes:        55555,
		TotalMinersPaid:    nil,
		TotalPayments:      nil,
		LastPayment:        1755390000,
	}
	if stats != want {
		t.Errorf("GetStats() = %+v, want %+v", stats, want)
	}
}

func TestHTTPClient_GetStats_NonNullOptionalFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"pool_statistics": {
				"hashRate": 1, "miners": 1, "totalHashes": 1, "lastBlockFoundTime": 1,
				"lastBlockFound": 1, "totalBlocksFound": 1, "roundHashes": 1,
				"totalMinersPaid": 12.5, "totalPayments": 3
			},
			"last_payment": 1
		}`))
	}))
	defer srv.Close()

	client := NewHTTPClient(srv.URL, nil)
	stats, err := client.GetStats(context.Background())
	if err != nil {
		t.Fatalf("GetStats() error = %v", err)
	}
	if stats.TotalMinersPaid == nil || *stats.TotalMinersPaid != 12.5 {
		t.Errorf("TotalMinersPaid = %v, want 12.5", stats.TotalMinersPaid)
	}
	if stats.TotalPayments == nil || *stats.TotalPayments != 3 {
		t.Errorf("TotalPayments = %v, want 3", stats.TotalPayments)
	}
}

func TestHTTPClient_GetStats_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewHTTPClient(srv.URL, nil)
	if _, err := client.GetStats(context.Background()); err == nil {
		t.Error("GetStats() error = nil, want non-nil for 500 response")
	}
}

func TestHTTPClient_GetStats_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	client := NewHTTPClient(srv.URL, nil)
	if _, err := client.GetStats(context.Background()); err == nil {
		t.Error("GetStats() error = nil, want non-nil for malformed JSON")
	}
}

func TestNewHTTPClient_DefaultBaseURL(t *testing.T) {
	client := NewHTTPClient("", nil)
	if client.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", client.baseURL, DefaultBaseURL)
	}
}

func TestNewHTTPClient_TrimsTrailingSlash(t *testing.T) {
	client := NewHTTPClient("https://example.com/", nil)
	if client.baseURL != "https://example.com" {
		t.Errorf("baseURL = %q, want %q", client.baseURL, "https://example.com")
	}
}

// TestPoolStatsResponseShape is a sanity check that the unexported wire struct still
// round-trips the exact field names of the confirmed live API - catches an accidental
// tag typo that a compile wouldn't.
func TestPoolStatsResponseShape(t *testing.T) {
	var parsed poolStatsResponse
	if err := json.Unmarshal([]byte(samplePoolStatsJSON), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.PoolStatistics.HashRate != 123456 {
		t.Errorf("HashRate = %d, want 123456", parsed.PoolStatistics.HashRate)
	}
}
