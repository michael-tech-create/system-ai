package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeGeminiText(w http.ResponseWriter, text string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"candidates": []map[string]any{
			{
				"content": map[string]any{
					"parts": []map[string]any{
						{"text": text},
					},
				},
			},
		},
	})
}

func TestReviewSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") == "" {
			t.Errorf("missing x-goog-api-key header")
		}
		if strings.Contains(r.URL.RawQuery, "key=") {
			t.Errorf("api key must not appear in query string: %s", r.URL.RawQuery)
		}
		writeGeminiText(w, `{"summary":"ok","issues":[],"suggestions":[]}`)
	}))
	defer server.Close()

	client := NewClient([]string{"test-key"}, WithAPIURL(server.URL), WithHTTPClient(server.Client()))
	review, err := client.Review(context.Background(), `{"files":[]}`)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if review.Summary != "ok" {
		t.Fatalf("summary = %q", review.Summary)
	}
}

func TestReviewRotatesOn429(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		key := r.Header.Get("x-goog-api-key")
		if n == 1 {
			if key != "key-a" {
				t.Errorf("first key = %q", key)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if key != "key-b" {
			t.Errorf("second key = %q", key)
		}
		writeGeminiText(w, `{"summary":"rotated","issues":[],"suggestions":[]}`)
	}))
	defer server.Close()

	client := NewClient([]string{"key-a", "key-b"}, WithAPIURL(server.URL), WithHTTPClient(server.Client()))
	review, err := client.Review(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if review.Summary != "rotated" {
		t.Fatalf("summary = %q", review.Summary)
	}
	if hits.Load() < 2 {
		t.Fatalf("expected rotation, hits=%d", hits.Load())
	}
}

func TestReviewPermanentAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "API key invalid", "status": "UNAUTHENTICATED"},
		})
	}))
	defer server.Close()

	client := NewClient([]string{"bad-key"}, WithAPIURL(server.URL), WithHTTPClient(server.Client()))
	_, err := client.Review(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "permanently failed") && !strings.Contains(err.Error(), "auth") {
		t.Fatalf("error = %v", err)
	}
}

func TestReviewContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	client := NewClient([]string{"k"}, WithAPIURL(server.URL), WithHTTPClient(server.Client()))
	_, err := client.Review(ctx, `{}`)
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestClassifyAPIError(t *testing.T) {
	err := classifyAPIError("Quota exceeded", "RESOURCE_EXHAUSTED", 0)
	if !isRetryableError(err) {
		t.Fatalf("expected retryable: %v", err)
	}
	err = classifyAPIError("Permission denied", "PERMISSION_DENIED", 0)
	if !isFatalKeyError(err) {
		t.Fatalf("expected fatal: %v", err)
	}
}
