package manifest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// roundTripperFunc lets a test stub http.Client transport without a network.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

const sampleBody = `{"plugins":{"core-app":["1.2.0","1.1.0"],"core-datasource":["0.4.1"],"yfinance-app":[]},"preview":{"core-app":["0.1.1"],"core-datasource":["0.1.6","0.1.4","0.1.5","0.1.3"],"yfinance-app":["0.1.1"]}}`

func newTestClient(rt roundTripperFunc, ttl time.Duration) *Client {
	return New("https://example.test/plugins.json",
		&http.Client{Transport: rt}, ttl,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestParse_PluginIDsAndVersions(t *testing.T) {
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(sampleBody), nil
	}, time.Minute)

	ids, err := c.PluginIDs(context.Background())
	if err != nil {
		t.Fatalf("PluginIDs: %v", err)
	}
	// All keys, including the empty-list plugin, sorted.
	if want := []string{"core-app", "core-datasource", "yfinance-app"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("PluginIDs = %v, want %v", ids, want)
	}

	vs, err := c.ValidatedVersions(context.Background(), "core-app")
	if err != nil {
		t.Fatalf("ValidatedVersions: %v", err)
	}
	if want := []string{"1.2.0", "1.1.0"}; !reflect.DeepEqual(vs, want) {
		t.Fatalf("ValidatedVersions(core-app) = %v, want %v", vs, want)
	}

	if vs, _ := c.ValidatedVersions(context.Background(), "yfinance-app"); len(vs) != 0 {
		t.Fatalf("ValidatedVersions(yfinance-app) = %v, want empty", vs)
	}
}

func TestParse_PreviewIDsAndVersions(t *testing.T) {
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(sampleBody), nil
	}, time.Minute)

	ids, err := c.PreviewPluginIDs(context.Background())
	if err != nil {
		t.Fatalf("PreviewPluginIDs: %v", err)
	}
	if want := []string{"core-app", "core-datasource", "yfinance-app"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("PreviewPluginIDs = %v, want %v", ids, want)
	}

	// Versions come back semver-desc regardless of file order.
	vs, err := c.PreviewVersions(context.Background(), "core-datasource")
	if err != nil {
		t.Fatalf("PreviewVersions: %v", err)
	}
	if want := []string{"0.1.6", "0.1.5", "0.1.4", "0.1.3"}; !reflect.DeepEqual(vs, want) {
		t.Fatalf("PreviewVersions(core-datasource) = %v, want %v", vs, want)
	}
}

func TestParse_MissingPreviewSectionIsEmpty(t *testing.T) {
	// A manifest with no `preview` key must yield empty preview sets, not an error.
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(`{"plugins":{"core-app":["1.0.0"]}}`), nil
	}, time.Minute)

	ids, err := c.PreviewPluginIDs(context.Background())
	if err != nil {
		t.Fatalf("PreviewPluginIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("PreviewPluginIDs = %v, want empty", ids)
	}
	vs, err := c.PreviewVersions(context.Background(), "core-app")
	if err != nil {
		t.Fatalf("PreviewVersions: %v", err)
	}
	if len(vs) != 0 {
		t.Fatalf("PreviewVersions(core-app) = %v, want empty", vs)
	}
}

func TestValidatedVersions_SortsDescendingRegardlessOfInputOrder(t *testing.T) {
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(`{"plugins":{"p":["1.1.0","1.10.0","1.2.0"]}}`), nil
	}, time.Minute)
	vs, _ := c.ValidatedVersions(context.Background(), "p")
	if want := []string{"1.10.0", "1.2.0", "1.1.0"}; !reflect.DeepEqual(vs, want) {
		t.Fatalf("ValidatedVersions = %v, want %v", vs, want)
	}
}

func TestCache_NoDoubleFetchWithinTTL(t *testing.T) {
	var calls int32
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(sampleBody), nil
	}, time.Minute)

	for i := 0; i < 5; i++ {
		if _, err := c.PluginIDs(context.Background()); err != nil {
			t.Fatalf("PluginIDs: %v", err)
		}
		if _, err := c.ValidatedVersions(context.Background(), "core-app"); err != nil {
			t.Fatalf("ValidatedVersions: %v", err)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("fetch called %d times, want 1 within TTL", n)
	}
}

func TestCache_RefetchAfterTTL(t *testing.T) {
	var calls int32
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(sampleBody), nil
	}, time.Minute)

	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }

	if _, err := c.PluginIDs(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute) // expire the cache
	if _, err := c.PluginIDs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("fetch called %d times, want 2 after TTL expiry", n)
	}
}

func TestServeStaleOnError(t *testing.T) {
	var fail atomic.Bool
	var calls int32
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		if fail.Load() {
			return nil, fmt.Errorf("network down")
		}
		return jsonResp(sampleBody), nil
	}, time.Minute)

	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }

	// Warm the cache.
	if _, err := c.PluginIDs(context.Background()); err != nil {
		t.Fatalf("warm fetch: %v", err)
	}
	// Expire it and make the next fetch fail.
	now = now.Add(2 * time.Minute)
	fail.Store(true)

	ids, err := c.PluginIDs(context.Background())
	if err != nil {
		t.Fatalf("expected stale serve, got err: %v", err)
	}
	if want := []string{"core-app", "core-datasource", "yfinance-app"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("stale PluginIDs = %v, want %v", ids, want)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("fetch called %d times, want 2 (warm + failed refresh)", n)
	}
}

func TestColdMissReturnsError(t *testing.T) {
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("network down")
	}, time.Minute)
	if _, err := c.PluginIDs(context.Background()); err == nil {
		t.Fatal("expected error on cold miss, got nil")
	}
}

func TestNon200IsError(t *testing.T) {
	c := newTestClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("not found")),
			Header:     make(http.Header),
		}, nil
	}, time.Minute)
	if _, err := c.PluginIDs(context.Background()); err == nil {
		t.Fatal("expected error on non-200, got nil")
	}
}
