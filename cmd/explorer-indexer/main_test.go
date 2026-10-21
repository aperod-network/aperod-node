package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackfillFromHeightFromEnv(t *testing.T) {
	t.Setenv("BACKFILL_FROM_HEIGHT", "0")
	height, err := backfillFromHeightFromEnv()
	if err != nil {
		t.Fatalf("parse zero height: %v", err)
	}
	if height == nil || *height != 0 {
		t.Fatalf("height = %v, want pointer to 0", height)
	}

	t.Setenv("BACKFILL_FROM_HEIGHT", "1760000")
	height, err = backfillFromHeightFromEnv()
	if err != nil || height == nil || *height != 1760000 {
		t.Fatalf("height = %v, %v; want 1760000, nil", height, err)
	}
}

func TestBackfillFromHeightFromEnvRejectsInvalidValue(t *testing.T) {
	for _, value := range []string{"-1", "1.5", "not-a-height"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("BACKFILL_FROM_HEIGHT", value)
			if _, err := backfillFromHeightFromEnv(); err == nil {
				t.Fatalf("BACKFILL_FROM_HEIGHT=%q unexpectedly accepted", value)
			}
		})
	}
}

func TestIndexerRequestsPerSecondFromEnv(t *testing.T) {
	t.Setenv("INDEXER_REQUESTS_PER_SECOND", "")
	if got, err := indexerRequestsPerSecondFromEnv(); err != nil || got != defaultIndexerRequestsPerSecond {
		t.Fatalf("default request rate = %d, %v; want %d, nil", got, err, defaultIndexerRequestsPerSecond)
	}
	t.Setenv("INDEXER_REQUESTS_PER_SECOND", "37")
	if got, err := indexerRequestsPerSecondFromEnv(); err != nil || got != 37 {
		t.Fatalf("configured request rate = %d, %v; want 37, nil", got, err)
	}
	for _, value := range []string{"0", "51", "-1", "fast"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("INDEXER_REQUESTS_PER_SECOND", value)
			if _, err := indexerRequestsPerSecondFromEnv(); err == nil {
				t.Fatalf("INDEXER_REQUESTS_PER_SECOND=%q unexpectedly accepted", value)
			}
		})
	}
}

func TestRetryAfterDuration(t *testing.T) {
	now := time.Date(2026, time.October, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{header: "3", want: 3 * time.Second, ok: true},
		{header: "0", want: 0, ok: true},
		{header: now.Add(5 * time.Second).Format(http.TimeFormat), want: 5 * time.Second, ok: true},
		{header: "invalid", ok: false},
		{header: "", ok: false},
	} {
		t.Run(tc.header, func(t *testing.T) {
			got, ok := retryAfterDuration(tc.header, now)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("retryAfterDuration(%q) = %s, %t; want %s, %t",
					tc.header, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestNodeClientRetries429AndHonorsRetryAfter(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	client := &nodeClient{base: server.URL, http: server.Client()}
	start := time.Now()
	body, status, err := client.get(server.URL)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("get after 429: %v", err)
	}
	if status != http.StatusOK || string(body) != "ok" {
		t.Fatalf("get = %q, %d; want ok, 200", body, status)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
	if elapsed < minRateLimitRetryDelay {
		t.Fatalf("retried after %s, want at least %s", elapsed, minRateLimitRetryDelay)
	}
}

func TestNodeClientRetries429WithoutRetryAfter(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	client := &nodeClient{base: server.URL, http: server.Client()}
	start := time.Now()
	body, status, err := client.get(server.URL)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("get after headerless 429: %v", err)
	}
	if status != http.StatusOK || string(body) != "ok" {
		t.Fatalf("get = %q, %d; want ok, 200", body, status)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
	if elapsed < time.Second {
		t.Fatalf("retried after %s, want fallback backoff of at least 1s", elapsed)
	}
}

func TestRequestPacerSpacesRequests(t *testing.T) {
	const interval = 20 * time.Millisecond
	pacer := &requestPacer{interval: interval}
	pacer.wait()
	start := time.Now()
	pacer.wait()
	if elapsed := time.Since(start); elapsed < interval*9/10 {
		t.Fatalf("second request started after %s, want at least %s", elapsed, interval*9/10)
	}
}
