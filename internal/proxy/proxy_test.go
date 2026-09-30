package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentrouter/internal/config"
	"agentrouter/internal/store"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// baseConfig is a valid configuration with an enabled test upstream and zero
// backoff. MaxAttempts > 0 keeps the retry block explicit (no normalization).
func baseConfig(upstreamURL string) *config.Config {
	return &config.Config{
		Listen:              config.DefaultListen,
		LogLimit:            config.DefaultLogLimit,
		MaxRequestBodyBytes: config.DefaultMaxRequestBodyBytes,
		MaxBufferBytes:      config.DefaultMaxBufferBytes,
		MaxLogBodyBytes:     config.DefaultMaxLogBodyBytes,
		BodySniffTimeoutMS:  config.DefaultBodySniffTimeoutMS,
		ErrorBodyTimeoutMS:  config.DefaultErrorBodyTimeoutMS,
		Upstreams: []config.Upstream{{
			ID:        "u_test",
			Name:      "test",
			BaseURL:   upstreamURL,
			Enabled:   true,
			Weight:    100,
			TimeoutMS: config.DefaultUpstreamTimeoutMS,
		}},
		Retry: config.Retry{MaxAttempts: 3},
	}
}

func mustStore(t *testing.T, c *config.Config) *config.Store {
	t.Helper()
	st, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := st.Set(c); err != nil {
		t.Fatalf("Store.Set: %v (config=%+v)", err, c)
	}
	return st
}

func newProxy(t *testing.T, st *config.Store) (*store.LogStore, *Proxy) {
	t.Helper()
	logs := store.NewLogStore(200)
	return logs, New(st, logs)
}

func newProxyServer(t *testing.T, st *config.Store) (*store.LogStore, *httptest.Server) {
	t.Helper()
	logs, p := newProxy(t, st)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return logs, srv
}

func waitLogEntry(t *testing.T, logs *store.LogStore) store.LogEntry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if entries := logs.List(1); len(entries) == 1 {
			return entries[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for a log entry")
	return store.LogEntry{}
}

// ---------------------------------------------------------------------------
// routing: globs, strict matching, weights
// ---------------------------------------------------------------------------

func TestGlobSemantics(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		input   string
		want    bool
	}{
		{"star_crosses_slash", "/v1/*", "/v1/chat/completions", true},
		{"question_matches_one", "/v1/a?c", "/v1/abc", true},
		{"question_rejects_zero", "/v1/a?c", "/v1/ac", false},
		{"prefix_mismatch", "/v1/*", "/v2/chat", false},
		{"literal_dot_quoted", "/v1/a.b", "/v1/axb", false},
		{"empty_pattern_nonempty_input", "", "/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := globMatch(tc.pattern, tc.input); got != tc.want {
				t.Errorf("globMatch(%q,%q)=%v want %v", tc.pattern, tc.input, got, tc.want)
			}
		})
	}
}

func TestMatchAnyEmptyListMatchesEverything(t *testing.T) {
	if !matchAny(nil, "/anything") {
		t.Error("empty glob list should match (explicit default route)")
	}
	if matchAny([]string{"/v1/*"}, "/v2/x") {
		t.Error("non-matching glob list should not match")
	}
}

func TestPickUpstreamRequiresPathAndModelMatch(t *testing.T) {
	cfg := &config.Config{Upstreams: []config.Upstream{
		{ID: "u1", Name: "n1", BaseURL: "http://a", Enabled: true, Weight: 100,
			PathGlobs: []string{"/v1/*"}, ModelGlobs: []string{"gpt-4o*"}},
		{ID: "u2", Name: "n2", BaseURL: "http://b", Enabled: true, Weight: 100,
			PathGlobs: []string{"/other/*"}, ModelGlobs: []string{"*"}},
	}}
	if up, status, _ := pickUpstream(cfg, "/v1/chat/completions", "gpt-4o-mini"); up == nil || up.ID != "u1" || status != 0 {
		t.Errorf("pickUpstream = (%v,%d), want u1/0 (path AND model)", up, status)
	}
	if up, status, _ := pickUpstream(cfg, "/v1/chat/completions", "claude"); up != nil || status != http.StatusNotFound {
		t.Errorf("pickUpstream = (%v,%d), want nil/404 when no rule matches", up, status)
	}
}

func TestPickUpstreamDisabledNoMatchZeroWeight(t *testing.T) {
	allOff := &config.Config{Upstreams: []config.Upstream{
		{ID: "off", Name: "off", BaseURL: "http://a", Enabled: false, Weight: 100},
	}}
	if up, status, _ := pickUpstream(allOff, "/v1/x", "m"); up != nil || status != http.StatusBadGateway {
		t.Errorf("all-disabled = (%v,%d), want nil/502", up, status)
	}

	zero := &config.Config{Upstreams: []config.Upstream{
		{ID: "z", Name: "z", BaseURL: "http://a", Enabled: true, Weight: 0},
	}}
	if up, status, _ := pickUpstream(zero, "/v1/x", "m"); up != nil || status != http.StatusServiceUnavailable {
		t.Errorf("zero-weight match = (%v,%d), want nil/503", up, status)
	}

	mixed := &config.Config{Upstreams: []config.Upstream{
		{ID: "off", Name: "off", BaseURL: "http://a", Enabled: false, Weight: 1000},
		{ID: "on", Name: "on", BaseURL: "http://b", Enabled: true, Weight: 1},
	}}
	if up, _, _ := pickUpstream(mixed, "/v1/x", "m"); up == nil || up.ID != "on" {
		t.Errorf("disabled upstream was selected: %v", up)
	}
}

func TestWeightedPickNeverSelectsZeroWeight(t *testing.T) {
	candidates := []*config.Upstream{
		{ID: "heavy", Weight: 100},
		{ID: "zero", Weight: 0},
	}
	for i := 0; i < 300; i++ {
		if got := weightedPick(candidates); got.ID != "heavy" {
			t.Fatalf("weightedPick selected %q; zero-weight candidates must be excluded", got.ID)
		}
	}
}

func TestTargetURLJoinsEscapedPathAndMergesQuery(t *testing.T) {
	up := &config.Upstream{BaseURL: "http://up.example/base?token=1"}
	r := httptest.NewRequest(http.MethodGet, "http://proxy/v1/a%2Fb?x=1&y=2", nil)
	want := "http://up.example/base/v1/a%2Fb?token=1&x=1&y=2"
	if got := targetURL(up, r); got != want {
		t.Errorf("targetURL=%q want %q", got, want)
	}

	empty := &config.Upstream{BaseURL: "http://up.example/base/"}
	r2 := httptest.NewRequest(http.MethodGet, "http://proxy", nil)
	if got := targetURL(empty, r2); got != "http://up.example/base/" {
		t.Errorf("targetURL(empty path)=%q want base + /", got)
	}
}

func TestNoMatchAndZeroWeightHTTPStatus(t *testing.T) {
	t.Run("no_match_404", func(t *testing.T) {
		cfg := baseConfig("http://upstream.invalid")
		cfg.Upstreams[0].PathGlobs = []string{"/v1/*"}
		_, srv := newProxyServer(t, mustStore(t, cfg))
		resp, err := http.Get(srv.URL + "/other/path")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status=%d want 404 when no rule matches", resp.StatusCode)
		}
	})
	t.Run("zero_weight_503", func(t *testing.T) {
		cfg := baseConfig("http://upstream.invalid")
		cfg.Upstreams[0].Weight = 0
		_, srv := newProxyServer(t, mustStore(t, cfg))
		resp, err := http.Get(srv.URL + "/v1/chat/completions")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status=%d want 503 when only matching weights are 0", resp.StatusCode)
		}
	})
}

func TestNoEnabledUpstreamReturns502(t *testing.T) {
	cfg := baseConfig("http://upstream.invalid")
	cfg.Upstreams = nil
	logs, srv := newProxyServer(t, mustStore(t, cfg))

	resp, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status=%d want 502", resp.StatusCode)
	}
	if !strings.Contains(string(body), "no enabled upstream") {
		t.Errorf("body=%q want no-enabled-upstream error", body)
	}
	entry := waitLogEntry(t, logs)
	if entry.Status != http.StatusBadGateway || entry.AttemptCount != 0 {
		t.Errorf("log status=%d attempts=%d want 502/0", entry.Status, entry.AttemptCount)
	}
}

// ---------------------------------------------------------------------------
// retry: status OR body regex, transparency, exhaustion
// ---------------------------------------------------------------------------

type capturedRequest struct {
	method string
	path   string
	query  string
	auth   string
	body   string
}

func TestRetryOnStatusThenSuccessIsTransparent(t *testing.T) {
	for _, code := range []int{400, 404, 429} {
		t.Run("status_"+strconv.Itoa(code), func(t *testing.T) {
			var mu sync.Mutex
			var calls []capturedRequest
			var n int32

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				calls = append(calls, capturedRequest{
					method: r.Method,
					path:   r.URL.Path,
					query:  r.URL.RawQuery,
					auth:   r.Header.Get("Authorization"),
					body:   string(body),
				})
				mu.Unlock()

				if atomic.AddInt32(&n, 1) < 3 {
					w.WriteHeader(code)
					_, _ = io.WriteString(w, "upstream failed")
					return
				}
				w.Header().Set("X-Final", "yes")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer upstream.Close()

			cfg := baseConfig(upstream.URL)
			cfg.Retry = config.Retry{MaxAttempts: 3, StatusCodes: []int{400, 404, 429}}
			logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

			payload := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
			req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/chat/completions?stream=true", strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer topsecret")
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			gotBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("client status=%d want 200", resp.StatusCode)
			}
			if string(gotBody) != `{"ok":true}` {
				t.Errorf("client body=%q want final success body", gotBody)
			}
			if h := resp.Header.Get("X-Final"); h != "yes" {
				t.Errorf("client header X-Final=%q want yes", h)
			}
			if got := atomic.LoadInt32(&n); got != 3 {
				t.Errorf("upstream calls=%d want exactly 3 (<= MaxAttempts)", got)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(calls) != 3 {
				t.Fatalf("captured %d attempts, want 3", len(calls))
			}
			want := calls[0]
			if want.method != http.MethodPost || want.path != "/v1/chat/completions" ||
				want.query != "stream=true" || want.auth != "Bearer topsecret" || want.body != payload {
				t.Errorf("first attempt was not as expected: %+v", want)
			}
			for i, c := range calls {
				if c != want {
					t.Errorf("attempt %d differed from attempt 1:\n got %+v\nwant %+v", i+1, c, want)
				}
			}

			entry := waitLogEntry(t, logs)
			if entry.Status != http.StatusOK || entry.AttemptCount != 3 || !entry.Retried {
				t.Errorf("log entry status=%d attempts=%d retried=%v, want 200/3/true", entry.Status, entry.AttemptCount, entry.Retried)
			}
			if len(entry.Attempts) != 3 {
				t.Errorf("logged attempts=%d want 3", len(entry.Attempts))
			}
			if entry.Model != "gpt-4o" {
				t.Errorf("logged model=%q want gpt-4o", entry.Model)
			}
		})
	}
}

func TestRetryExhaustionReturnsLastResponse(t *testing.T) {
	var n int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&n, 1)
		w.Header().Set("X-Attempt", strconv.Itoa(int(c)))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, fmt.Sprintf("attempt-%d", c))
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.Retry = config.Retry{MaxAttempts: 3, StatusCodes: []int{429}}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	resp, err := http.Get(proxySrv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("client status=%d want 429 (not synthetic 200)", resp.StatusCode)
	}
	if string(body) != "attempt-3" {
		t.Errorf("client body=%q want last upstream body attempt-3", body)
	}
	if h := resp.Header.Get("X-Attempt"); h != "3" {
		t.Errorf("client header X-Attempt=%q want last attempt header 3", h)
	}

	entry := waitLogEntry(t, logs)
	if entry.Status != http.StatusTooManyRequests {
		t.Errorf("logged status=%d want 429", entry.Status)
	}
	if len(entry.Attempts) != 3 {
		t.Fatalf("logged attempts=%d want 3", len(entry.Attempts))
	}
	for i := 0; i < 3; i++ {
		if entry.Attempts[i].Status != http.StatusTooManyRequests {
			t.Errorf("attempt %d status=%d want 429", i+1, entry.Attempts[i].Status)
		}
	}
	if entry.Attempts[0].RetryReason == "" || entry.Attempts[1].RetryReason == "" {
		t.Error("retried attempts must record a reason")
	}
	if entry.Attempts[2].RetryReason == "" {
		t.Error("final retryable attempt must still record the matched-but-exhausted reason")
	}
}

func TestBodyRegexRetryAndPassThrough(t *testing.T) {
	t.Run("matching_body_retries_then_succeeds", func(t *testing.T) {
		var n int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&n, 1) == 1 {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"error":"Rate Limit exceeded"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
		}))
		defer upstream.Close()

		cfg := baseConfig(upstream.URL)
		cfg.Retry = config.Retry{MaxAttempts: 3, BodyRegexes: []string{"(?i)rate limit"}}
		logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

		resp, err := http.Get(proxySrv.URL + "/v1/chat/completions")
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
			t.Errorf("status=%d body=%q want 200 {\"ok\":true}", resp.StatusCode, body)
		}
		if got := atomic.LoadInt32(&n); got != 2 {
			t.Errorf("upstream calls=%d want 2", got)
		}
		entry := waitLogEntry(t, logs)
		if entry.AttemptCount != 2 || entry.Attempts[0].RetryReason == "" {
			t.Errorf("logged attempts=%d reason=%q want 2/non-empty", entry.AttemptCount, entry.Attempts[0].RetryReason)
		}
	})

	t.Run("non_matching_body_passes_through_once", func(t *testing.T) {
		var n int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&n, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"error":"bad request"}`)
		}))
		defer upstream.Close()

		cfg := baseConfig(upstream.URL)
		cfg.Retry = config.Retry{MaxAttempts: 3, BodyRegexes: []string{"rate limit"}}
		logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

		resp, err := http.Get(proxySrv.URL + "/v1/chat/completions")
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK || string(body) != `{"error":"bad request"}` {
			t.Errorf("status=%d body=%q want pass-through 200", resp.StatusCode, body)
		}
		if got := atomic.LoadInt32(&n); got != 1 {
			t.Errorf("upstream calls=%d want 1", got)
		}
		if entry := waitLogEntry(t, logs); entry.AttemptCount != 1 || entry.Retried {
			t.Errorf("logged attempts=%d retried=%v want 1/false", entry.AttemptCount, entry.Retried)
		}
	})
}

func TestRetryOrSemantics(t *testing.T) {
	t.Run("status_only", func(t *testing.T) {
		var n int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&n, 1) == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, "plain failure")
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
		}))
		defer upstream.Close()

		cfg := baseConfig(upstream.URL)
		cfg.Retry = config.Retry{MaxAttempts: 3, StatusCodes: []int{429}}
		_, proxySrv := newProxyServer(t, mustStore(t, cfg))

		resp, err := http.Get(proxySrv.URL + "/v1/chat/completions")
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&n) != 2 {
			t.Errorf("status=%d calls=%d want 200/2", resp.StatusCode, n)
		}
	})

	t.Run("body_only", func(t *testing.T) {
		var n int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&n, 1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, "please retry, rate limit reached")
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
		}))
		defer upstream.Close()

		cfg := baseConfig(upstream.URL)
		cfg.Retry = config.Retry{MaxAttempts: 3, StatusCodes: []int{400}, BodyRegexes: []string{"rate limit"}}
		_, proxySrv := newProxyServer(t, mustStore(t, cfg))

		resp, err := http.Get(proxySrv.URL + "/v1/chat/completions")
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&n) != 2 {
			t.Errorf("status=%d calls=%d want 200/2", resp.StatusCode, n)
		}
	})
}

// Status retry must not be disabled by a large body, and the final retryable
// attempt must return the real upstream status, never a synthetic 200.
func TestStatusRetryLargeBodyAndFinalStatus(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1<<20)
	var n int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.MaxBufferBytes = 1024
	cfg.Retry = config.Retry{MaxAttempts: 3, StatusCodes: []int{429}}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(proxySrv.URL + "/v1/large")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status=%d want 429", resp.StatusCode)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("final body mismatch: got %d bytes want %d", len(got), len(payload))
	}
	if calls := atomic.LoadInt32(&n); calls != 3 {
		t.Errorf("upstream calls=%d want 3 (large body must not disable status retry)", calls)
	}
	entry := waitLogEntry(t, logs)
	if entry.AttemptCount != 3 {
		t.Errorf("attempts=%d want 3", entry.AttemptCount)
	}
	if !entry.Attempts[0].ResponseBodyComplete {
		t.Errorf("attempt 1 body should have drained to EOF: %+v", entry.Attempts[0])
	}
}

// A never-ending retryable body must be bounded by ErrorBodyTimeoutMS instead
// of hanging the request.
func TestStatusRetryNeverEndingBodyIsBounded(t *testing.T) {
	var n int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		if c < 3 {
			_, _ = io.WriteString(w, "still loading")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "final-429")
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.ErrorBodyTimeoutMS = 100
	cfg.Retry = config.Retry{MaxAttempts: 3, StatusCodes: []int{429}}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(proxySrv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests || string(body) != "final-429" {
		t.Errorf("status=%d body=%q want 429/final-429", resp.StatusCode, body)
	}
	if calls := atomic.LoadInt32(&n); calls != 3 {
		t.Errorf("upstream calls=%d want 3", calls)
	}
	entry := waitLogEntry(t, logs)
	if len(entry.Attempts) != 3 {
		t.Fatalf("attempts=%d want 3", len(entry.Attempts))
	}
	if !strings.Contains(entry.Attempts[0].Error, "drain") {
		t.Errorf("timed-out drain should record an error, got %q", entry.Attempts[0].Error)
	}
}

func TestRequestBodyLimitRejectsBeforeForwarding(t *testing.T) {
	var n int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.MaxRequestBodyBytes = 10
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	resp, err := http.Post(proxySrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(strings.Repeat("x", 20)))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d want 413", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&n); got != 0 {
		t.Errorf("upstream calls=%d want 0 (rejected before forwarding)", got)
	}
	entry := waitLogEntry(t, logs)
	if entry.Status != http.StatusRequestEntityTooLarge {
		t.Errorf("log status=%d want 413", entry.Status)
	}
	// The available request prefix is captured even though the read failed.
	if entry.RequestBody != strings.Repeat("x", 11) {
		t.Errorf("captured request prefix=%q want 11 x's", entry.RequestBody)
	}
	if entry.RequestBodyComplete {
		t.Error("oversized request body must not be marked complete")
	}
	if !entry.RequestBodyAvailable {
		t.Error("available request prefix should be flagged available")
	}
}

// ---------------------------------------------------------------------------
// cancellation
// ---------------------------------------------------------------------------

func TestSleepCtxHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	if sleepCtx(ctx, 5*time.Second) {
		t.Fatal("sleepCtx returned true for an already-canceled context")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("sleepCtx took %v; want prompt return", elapsed)
	}
}

func TestCancelDuringBackoffStopsAttemptsAndLogs499(t *testing.T) {
	var n int32
	firstHit := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			select {
			case firstHit <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "retry later")
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.Retry = config.Retry{MaxAttempts: 3, BaseDelayMS: 5000, MaxDelayMS: 5000, StatusCodes: []int{429}}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxySrv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	clientDone := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		clientDone <- err
	}()

	select {
	case <-firstHit:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the first attempt")
	}
	cancel()

	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Error("client did not observe cancellation promptly")
	}

	entry := waitLogEntry(t, logs)
	if entry.Status != 499 {
		t.Errorf("canceled request status=%d want 499 (not success)", entry.Status)
	}
	if entry.AttemptCount != 1 {
		t.Errorf("attempts=%d want 1", entry.AttemptCount)
	}

	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("upstream calls=%d want 1: cancellation must stop further attempts", got)
	}
}

// Cancellation while draining a failed body must also stop promptly with 499.
func TestCancelDuringErrorBodyDrain(t *testing.T) {
	var n int32
	firstHit := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			select {
			case firstHit <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "streaming forever")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.ErrorBodyTimeoutMS = 5000
	cfg.Retry = config.Retry{MaxAttempts: 3, StatusCodes: []int{429}}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxySrv.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		_ = err
	}()

	select {
	case <-firstHit:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the first attempt")
	}
	cancel()

	entry := waitLogEntry(t, logs)
	if entry.Status != 499 {
		t.Errorf("status=%d want 499", entry.Status)
	}
	if entry.AttemptCount != 1 {
		t.Errorf("attempts=%d want 1", entry.AttemptCount)
	}
}

// ---------------------------------------------------------------------------
// SSE and streaming commit boundary
// ---------------------------------------------------------------------------

func TestSSEFirstEventDeliveredBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseUpstream()

	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		// A retry-regex frame must never cause a second upstream request once
		// bytes have been committed to the client.
		_, _ = io.WriteString(w, "data: rate limit\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.Retry = config.Retry{MaxAttempts: 2, BodyRegexes: []string{"rate limit"}}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	type firstEvent struct {
		text string
		err  error
	}
	firstCh := make(chan firstEvent, 1)
	fullCh := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Get(proxySrv.URL + "/v1/chat/completions")
		if err != nil {
			firstCh <- firstEvent{err: err}
			return
		}
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		var seen strings.Builder
		for {
			line, err := br.ReadString('\n')
			seen.WriteString(line)
			if strings.Contains(seen.String(), "data: first") {
				firstCh <- firstEvent{text: "data: first"}
				rest, _ := io.ReadAll(br)
				seen.Write(rest)
				fullCh <- seen.String()
				return
			}
			if err != nil {
				firstCh <- firstEvent{err: err}
				return
			}
		}
	}()

	select {
	case got := <-firstCh:
		if got.err != nil {
			t.Errorf("client failed to read the first SSE event: %v", got.err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Errorf("first SSE event was not delivered while the upstream held the stream open")
	}

	releaseUpstream()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Errorf("client read did not finish after the upstream was released")
	}

	select {
	case got := <-fullCh:
		want := "data: first\n\ndata: rate limit\n\n"
		if got != want {
			t.Errorf("full SSE body=%q want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Error("full SSE body was not captured")
	}

	if n := atomic.LoadInt32(&upstreamCalls); n != 1 {
		t.Errorf("upstream calls=%d want 1: a committed SSE frame must not be retried", n)
	}
	entry := waitLogEntry(t, logs)
	if !entry.Streaming || !entry.RegexSkipped {
		t.Errorf("SSE log streaming=%v regex_skipped=%v want true/true", entry.Streaming, entry.RegexSkipped)
	}
}

// A non-SSE body that does not reach EOF within the sniff window is committed
// and streamed; its prefix must never trigger a body-regex retry.
func TestNonSSESlowSniffCommitsWithoutRegex(t *testing.T) {
	proceed := make(chan struct{})
	firstSent := make(chan struct{}, 1)
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"partial":"rate limit"`)
		w.(http.Flusher).Flush()
		select {
		case firstSent <- struct{}{}:
		default:
		}
		<-proceed
		_, _ = io.WriteString(w, `,"rest":true}`)
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.BodySniffTimeoutMS = 100
	cfg.MaxBufferBytes = 1 << 20
	cfg.Retry = config.Retry{MaxAttempts: 3, BodyRegexes: []string{"rate limit"}}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	type result struct {
		body string
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get(proxySrv.URL + "/v1/chat/completions")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		resCh <- result{body: string(b), err: err}
	}()

	select {
	case <-firstSent:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not send the first chunk")
	}
	time.Sleep(250 * time.Millisecond) // let the 100ms sniff window elapse
	close(proceed)

	res := <-resCh
	if res.err != nil {
		t.Fatalf("client read: %v", res.err)
	}
	want := `{"partial":"rate limit","rest":true}`
	if res.body != want {
		t.Errorf("client body=%q want %q", res.body, want)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream calls=%d want 1: incomplete body must not be regex-retried", got)
	}
	entry := waitLogEntry(t, logs)
	if !entry.Streaming || !entry.RegexSkipped {
		t.Errorf("streaming=%v regex_skipped=%v want true/true", entry.Streaming, entry.RegexSkipped)
	}
}

// A compressed response is forwarded byte-for-byte and never treated as regex
// material, even when the raw bytes happen to match.
func TestCompressedResponseSkipsRegex(t *testing.T) {
	raw := []byte("\x1f\x8b\x08\x00rate limit not really gzip\x00\xff")
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.Retry = config.Retry{MaxAttempts: 3, BodyRegexes: []string{"rate limit"}}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	// Pin identity so the Go client does not transparently gunzip (and thus
	// hide) the raw bytes under test.
	req, err := http.NewRequest(http.MethodGet, proxySrv.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, raw) {
		t.Errorf("compressed bytes were altered: got %x want %x", got, raw)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("Content-Encoding=%q want gzip preserved", resp.Header.Get("Content-Encoding"))
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream calls=%d want 1: compressed body must not be regex-matched", got)
	}
	if entry := waitLogEntry(t, logs); !entry.RegexSkipped {
		t.Error("compressed response should be marked regex_skipped")
	}
}

// ---------------------------------------------------------------------------
// large responses and honest capture/truncation
// ---------------------------------------------------------------------------

func TestLargeResponsePassthroughPreservesBytes(t *testing.T) {
	payload := make([]byte, 5000)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.MaxBufferBytes = 512
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	resp, err := http.Get(proxySrv.URL + "/v1/large")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read client body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("client bytes mismatch: got %d bytes want %d", len(got), len(payload))
	}

	entry := waitLogEntry(t, logs)
	if entry.Status != http.StatusOK {
		t.Errorf("logged status=%d want 200", entry.Status)
	}
	if !entry.Streaming {
		t.Error("capped response should be logged as streaming")
	}
	if entry.ResponseBodyBytes != int64(len(payload)) || !entry.ResponseBodyComplete {
		t.Errorf("log body bytes=%d complete=%v want %d/true",
			entry.ResponseBodyBytes, entry.ResponseBodyComplete, len(payload))
	}
}

// When MaxLogBodyBytes caps capture, the flags must say so honestly and never
// present a partial prefix as a complete body.
func TestLargeResponseLogTruncatedHonestly(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 4096)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.MaxLogBodyBytes = 100
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	resp, err := http.Get(proxySrv.URL + "/v1/large")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	entry := waitLogEntry(t, logs)
	if !entry.ResponseBodyTruncated {
		t.Error("capped capture must be marked truncated")
	}
	if entry.ResponseBodyCapturedBytes != 100 {
		t.Errorf("captured=%d want 100 (explicit cap)", entry.ResponseBodyCapturedBytes)
	}
	if entry.ResponseBodyBytes != int64(len(payload)) {
		t.Errorf("observed bytes=%d want %d", entry.ResponseBodyBytes, len(payload))
	}
	if entry.ResponseBodyComplete {
		t.Error("a capped capture must not be marked complete")
	}
	retrieved, size, err := logs.OpenBody(entry.ID, "response")
	if err != nil {
		t.Fatalf("OpenBody: %v", err)
	}
	defer retrieved.Close()
	raw, _ := io.ReadAll(retrieved)
	if int64(len(raw)) != size || len(raw) != 100 {
		t.Errorf("OpenBody returned %d bytes (size=%d) want 100", len(raw), size)
	}
}

// Full-capture mode (MaxLogBodyBytes 0) must persist every byte, retrievable
// verbatim through OpenBody.
func TestFullBodyCaptureRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("Payload-0123456789-"), 150000) // ~3MiB
	sum := sha256.Sum256(payload)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.MaxLogBodyBytes = 0
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	resp, err := http.Get(proxySrv.URL + "/v1/full")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("client body mismatch: got %d bytes want %d", len(got), len(payload))
	}

	entry := waitLogEntry(t, logs)
	if entry.ResponseBodyCapturedBytes != int64(len(payload)) || !entry.ResponseBodyComplete {
		t.Errorf("captured=%d complete=%v want %d/true",
			entry.ResponseBodyCapturedBytes, entry.ResponseBodyComplete, len(payload))
	}
	rc, size, err := logs.OpenBody(entry.ID, "response")
	if err != nil {
		t.Fatalf("OpenBody: %v", err)
	}
	defer rc.Close()
	full, _ := io.ReadAll(rc)
	if size != int64(len(payload)) || sha256.Sum256(full) != sum {
		t.Errorf("OpenBody round-trip mismatch: size=%d want=%d", size, len(payload))
	}
}

// ---------------------------------------------------------------------------
// transport errors: sentinel abort after commit
// ---------------------------------------------------------------------------

func TestStreamingIOErrorIsNotLoggedAsCleanSuccess(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 40)
	cfg := baseConfig("http://upstream.invalid")
	cfg.MaxBufferBytes = 8
	logs := store.NewLogStore(10)
	p := New(mustStore(t, cfg), logs)
	p.client = &http.Client{Transport: &failAfterTransport{data: data, failAfter: len(data)}}
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	resp, err := http.Get(proxySrv.URL + "/v1/stream")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	_, readErr := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// An aborted stream must not look like a clean, complete body to the client.
	if readErr == nil {
		t.Fatal("client read succeeded; a post-commit abort must surface an abnormal EOF/reset")
	}

	entry := waitLogEntry(t, logs)
	if !entry.Incomplete {
		t.Errorf("post-commit read error must mark the log incomplete: %+v", entry)
	}
	if entry.Error == "" {
		t.Error("post-commit read error must be recorded in Error")
	}
	attemptErr := ""
	if len(entry.Attempts) > 0 {
		attemptErr = entry.Attempts[0].Error
	}
	if attemptErr == "" {
		t.Error("post-commit read error must be recorded on the attempt")
	}
}

// Direct-handler test: a downstream write failure after commit must log
// Incomplete and abort the HTTP stream with http.ErrAbortHandler.
func TestDownstreamWriteErrorAbortsWithSentinel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: chunk\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	logs, p := newProxy(t, mustStore(t, cfg))

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	aw := &abortWriter{}
	recovered := callAndRecover(func() { p.ServeHTTP(aw, req) })
	err, ok := recovered.(error)
	if !ok || !errors.Is(err, http.ErrAbortHandler) {
		t.Fatalf("panic=%v want http.ErrAbortHandler", recovered)
	}

	entry := waitLogEntry(t, logs)
	if !entry.Incomplete || entry.Error == "" {
		t.Errorf("write failure log incomplete=%v error=%q want true/non-empty", entry.Incomplete, entry.Error)
	}
}

func callAndRecover(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

// failAfterTransport simulates a streaming response whose body errors after a
// prefix has already been delivered.
type failAfterTransport struct {
	data      []byte
	failAfter int
}

func (t *failAfterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        http.Header{"Content-Type": []string{"text/plain"}},
		Body:          io.NopCloser(&failAfterReader{data: t.data, failAfter: t.failAfter}),
		ContentLength: -1,
		Request:       req,
	}, nil
}

type failAfterReader struct {
	data      []byte
	failAfter int
	off       int
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.off >= r.failAfter {
		return 0, io.ErrUnexpectedEOF
	}
	end := r.failAfter
	if end > len(r.data) {
		end = len(r.data)
	}
	n := copy(p, r.data[r.off:end])
	r.off += n
	return n, nil
}

// abortWriter fails every body write after the headers.
type abortWriter struct {
	header http.Header
	status int
}

func (w *abortWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *abortWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *abortWriter) Write([]byte) (int, error) {
	return 0, errors.New("client write failed")
}

// ---------------------------------------------------------------------------
// redirects, loop detection, framing
// ---------------------------------------------------------------------------

func TestUpstreamRedirectIsReturnedNotFollowed(t *testing.T) {
	var finalHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/redir" {
			w.Header().Set("Location", "/v1/final")
			w.WriteHeader(http.StatusFound)
			return
		}
		atomic.AddInt32(&finalHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "final")
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	_, proxySrv := newProxyServer(t, mustStore(t, cfg))

	// The test client must not follow either, so the proxy's own behavior is
	// what is observed.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(proxySrv.URL + "/v1/redir")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("proxy status=%d want 302: upstream redirects must be returned", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&finalHits); got != 0 {
		t.Errorf("redirect target hit %d times; proxy followed the upstream redirect", got)
	}
}

func TestLoopMarkerRejectedWith508(t *testing.T) {
	cfg := baseConfig("http://upstream.invalid")
	logs, p := newProxy(t, mustStore(t, cfg))
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(hopHeader, p.hopID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusLoopDetected {
		t.Errorf("status=%d want 508 for our own hop marker", resp.StatusCode)
	}
	if entry := waitLogEntry(t, logs); entry.Status != http.StatusLoopDetected {
		t.Errorf("log status=%d want 508", entry.Status)
	}
}

func TestForwardedRequestCarriesFreshHopMarker(t *testing.T) {
	const clientMarker = "client-supplied-marker"
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(hopHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	// A configured alias must not be able to replace the enforced marker.
	cfg.Upstreams[0].Headers = map[string]string{hopHeader: "alias-marker"}
	logs, p := newProxy(t, mustStore(t, cfg))
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(hopHeader, clientMarker)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var forwarded string
	select {
	case forwarded = <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive the request")
	}
	// The inbound chain is preserved and our marker appended; the configured
	// alias must never erase it.
	want := clientMarker + "," + p.hopID
	if forwarded != want {
		t.Errorf("forwarded hop chain=%q want %q", forwarded, want)
	}
	if strings.Contains(forwarded, "alias-marker") {
		t.Errorf("configured alias erased/supplanted the hop chain: %q", forwarded)
	}
	_ = waitLogEntry(t, logs)
}

func TestHopByHopHeadersStrippedBothSides(t *testing.T) {
	type captured struct {
		custom, connection string
	}
	got := make(chan captured, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- captured{custom: r.Header.Get("X-Custom-Hop"), connection: r.Header.Get("Connection")}
		w.Header().Set("Connection", "X-Resp-Hop")
		w.Header().Set("X-Resp-Hop", "nope")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	_, proxySrv := newProxyServer(t, mustStore(t, cfg))

	req, err := http.NewRequest(http.MethodGet, proxySrv.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "X-Custom-Hop")
	req.Header.Set("X-Custom-Hop", "should-not-forward")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case c := <-got:
		if c.custom != "" || c.connection != "" {
			t.Errorf("hop-by-hop headers leaked upstream: %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive the request")
	}
	if v := resp.Header.Get("X-Resp-Hop"); v != "" {
		t.Errorf("hop-by-hop response header leaked to client: %q", v)
	}
}

func TestHeadResponseFraming(t *testing.T) {
	cases := []struct {
		name              string
		method            string
		status            int
		header            http.Header
		wantContentLength int64
	}{
		{
			name:              "head preserves representation length",
			method:            http.MethodHead,
			status:            http.StatusOK,
			header:            http.Header{"Content-Length": []string{"123"}},
			wantContentLength: 123,
		},
		{
			name:              "204 has no body",
			method:            http.MethodGet,
			status:            http.StatusNoContent,
			wantContentLength: 0,
		},
		{
			name:              "304 has no body",
			method:            http.MethodGet,
			status:            http.StatusNotModified,
			wantContentLength: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header()[k] = v
				}
				w.WriteHeader(tc.status)
			}))
			defer upstream.Close()

			cfg := baseConfig(upstream.URL)
			_, proxySrv := newProxyServer(t, mustStore(t, cfg))

			req, err := http.NewRequest(tc.method, proxySrv.URL+"/v1/chat/completions", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Errorf("status=%d want %d", resp.StatusCode, tc.status)
			}
			if len(body) != 0 {
				t.Errorf("returned %d body bytes want 0", len(body))
			}
			if resp.ContentLength != tc.wantContentLength {
				t.Errorf("Content-Length=%d want %d", resp.ContentLength, tc.wantContentLength)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// redaction and header logging
// ---------------------------------------------------------------------------

func TestLogRedactsSecretsWhileForwardingThem(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen["Authorization"] = r.Header.Get("Authorization")
		seen["Cookie"] = r.Header.Get("Cookie")
		seen["X-Api-Key"] = r.Header.Get("X-Api-Key")
		seen["X-Configured-Secret"] = r.Header.Get("X-Configured-Secret")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	cfg.Upstreams[0].Headers = map[string]string{"X-Configured-Secret": "configured-value"}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("X-Api-Key", "sk-live-123")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	if seen["Authorization"] != "Bearer secret-token" || seen["Cookie"] != "session=abc" ||
		seen["X-Api-Key"] != "sk-live-123" || seen["X-Configured-Secret"] != "configured-value" {
		t.Errorf("upstream did not receive original/configured headers: %+v", seen)
	}
	mu.Unlock()

	entry := waitLogEntry(t, logs)
	h := http.Header(entry.RequestHeaders)
	for _, name := range []string{"Authorization", "Cookie", "X-Api-Key"} {
		if got := h.Get(name); got != "***" {
			t.Errorf("logged %s=%q want ***", name, got)
		}
	}
	if entry.AttemptCount < 1 || entry.DurationMS < 0 {
		t.Errorf("attempts=%d duration=%d want >=1/>=0", entry.AttemptCount, entry.DurationMS)
	}
}

func TestAttemptResponseHeadersLoggedRedacted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=upstream")
		w.Header().Set("X-Trace", "abc")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	cfg := baseConfig(upstream.URL)
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	resp, err := http.Get(proxySrv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	entry := waitLogEntry(t, logs)
	if len(entry.Attempts) != 1 {
		t.Fatalf("attempts=%d want 1", len(entry.Attempts))
	}
	ah := http.Header(entry.Attempts[0].ResponseHeaders)
	if ah.Get("Set-Cookie") != "***" {
		t.Errorf("attempt Set-Cookie=%q want ***", ah.Get("Set-Cookie"))
	}
	if ah.Get("X-Trace") != "abc" {
		t.Errorf("attempt X-Trace=%q want abc", ah.Get("X-Trace"))
	}
}

// ---------------------------------------------------------------------------
// bounded sniff/drain deadlines (regression: an elapsed deadline must not
// degrade into the 0-timeout "wait forever" streaming sentinel)
// ---------------------------------------------------------------------------

// blockingBody blocks every Read until Close, simulating a producer that sent a
// prefix and then went silent.
type blockingBody struct {
	release chan struct{}
	once    sync.Once
}

func newBlockingBody() *blockingBody {
	return &blockingBody{release: make(chan struct{})}
}

func (b *blockingBody) Read(p []byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

func (b *blockingBody) Close() error {
	b.once.Do(func() { close(b.release) })
	return nil
}

// An already-expired deadline must stop the sniff immediately, before waiting on
// the producer or the request context.
func TestExpiredSniffDeadlineReturnsImmediately(t *testing.T) {
	pump := newBodyPump(newBlockingBody(), nil)
	defer pump.stop()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct {
		sniffed  []byte
		complete bool
		err      error
	}, 1)
	start := time.Now()
	go func() {
		s, c, e := sniffBounded(pump, ctx, time.Now().Add(-time.Second), 1<<20)
		done <- struct {
			sniffed  []byte
			complete bool
			err      error
		}{s, c, e}
	}()

	select {
	case r := <-done:
		if len(r.sniffed) != 0 || r.complete || r.err != nil {
			t.Fatalf("expired sniff = %+v, want empty/incomplete/no-error", r)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("expired sniff waited %v; want prompt return", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("expired sniff blocked on the producer instead of returning")
	}
}

// An already-expired drain deadline must report a drain timeout immediately.
func TestExpiredDrainDeadlineReturnsImmediately(t *testing.T) {
	pump := newBodyPump(newBlockingBody(), nil)
	defer pump.stop()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := drainBoundedUntil(pump, ctx, time.Now().Add(-time.Second))
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errBodyDrainTimeout) {
			t.Fatalf("expired drain err = %v, want errBodyDrainTimeout", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("expired drain waited %v; want prompt return", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("expired drain blocked on the producer instead of returning")
	}
}

// ---------------------------------------------------------------------------
// downstream write failures must never be logged as a clean success
// ---------------------------------------------------------------------------

// TestDownstreamWriteFailureLoggedIncomplete drives both the buffered and the
// streamed (oversize prefix) commit paths into a failing writer. Neither may
// publish a clean success, and both must abort with the sentinel.
func TestDownstreamWriteFailureLoggedIncomplete(t *testing.T) {
	cases := []struct {
		name      string
		maxBuffer int64
		body      []byte
		// buffered capture is complete at the upstream even though delivery failed
		wantUpstreamComplete bool
	}{
		{
			name:                 "buffered small JSON",
			maxBuffer:            config.DefaultMaxBufferBytes,
			body:                 []byte(`{"ok":true}`),
			wantUpstreamComplete: true,
		},
		{
			name:                 "oversize prefix then EOF",
			maxBuffer:            8,
			body:                 bytes.Repeat([]byte("p"), 64),
			wantUpstreamComplete: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(tc.body)
			}))
			defer upstream.Close()

			cfg := baseConfig(upstream.URL)
			cfg.MaxBufferBytes = tc.maxBuffer
			logs, p := newProxy(t, mustStore(t, cfg))

			req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
			aw := &abortWriter{}
			recovered := callAndRecover(func() { p.ServeHTTP(aw, req) })
			err, ok := recovered.(error)
			if !ok || !errors.Is(err, http.ErrAbortHandler) {
				t.Fatalf("panic=%v want http.ErrAbortHandler", recovered)
			}

			entry := waitLogEntry(t, logs)
			if !entry.Incomplete || entry.Error == "" {
				t.Fatalf("write failure log incomplete=%v error=%q want true/non-empty", entry.Incomplete, entry.Error)
			}
			if len(entry.Attempts) != 1 || entry.Attempts[0].Error == "" {
				t.Fatalf("attempt error not recorded: %+v", entry.Attempts)
			}
			if got := entry.Attempts[0].ResponseBodyComplete; got != tc.wantUpstreamComplete {
				t.Errorf("upstream capture complete=%v want %v (delivery and capture are independent)", got, tc.wantUpstreamComplete)
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Errorf("upstream calls=%d want 1: a committed response must not be retried", got)
			}
		})
	}
}

// A generated (non-upstream) error whose write fails must also be marked
// incomplete rather than logged as delivered.
func TestGeneratedErrorWriteFailureLoggedIncomplete(t *testing.T) {
	logs, p := newProxy(t, mustStore(t, baseConfig("http://upstream.invalid")))

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req.Header.Set(hopHeader, p.hopID) // force the generated 508 path
	aw := &abortWriter{}
	recovered := callAndRecover(func() { p.ServeHTTP(aw, req) })
	err, ok := recovered.(error)
	if !ok || !errors.Is(err, http.ErrAbortHandler) {
		t.Fatalf("panic=%v want http.ErrAbortHandler", recovered)
	}

	entry := waitLogEntry(t, logs)
	if entry.Status != http.StatusLoopDetected {
		t.Errorf("status=%d want 508", entry.Status)
	}
	if !entry.Incomplete || entry.Error == "" {
		t.Errorf("generated error write failure incomplete=%v error=%q want true/non-empty", entry.Incomplete, entry.Error)
	}
}

// ---------------------------------------------------------------------------
// cancellation during a non-SSE sniff must retain the in-flight attempt
// ---------------------------------------------------------------------------

// gatedBody yields one prefix chunk and then blocks until Close.
type gatedBody struct {
	prefix    []byte
	sent      bool
	release   chan struct{}
	prefixHit chan struct{}
	closed    atomic.Bool
	closeOnce sync.Once
}

func newGatedBody(prefix []byte, prefixHit chan struct{}) *gatedBody {
	return &gatedBody{prefix: prefix, release: make(chan struct{}), prefixHit: prefixHit}
}

func (b *gatedBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		n := copy(p, b.prefix)
		select {
		case b.prefixHit <- struct{}{}:
		default:
		}
		return n, nil
	}
	<-b.release
	return 0, io.EOF
}

func (b *gatedBody) Close() error {
	b.closed.Store(true)
	b.closeOnce.Do(func() { close(b.release) })
	return nil
}

// staticTransport returns one canned response without any network hop.
type staticTransport struct {
	status int
	header http.Header
	body   io.ReadCloser
}

func (t *staticTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    t.status,
		Status:        fmt.Sprintf("%d %s", t.status, http.StatusText(t.status)),
		Header:        t.header.Clone(),
		Body:          t.body,
		ContentLength: -1,
		Request:       req,
	}, nil
}

// TestCancelDuringSniffRecordsAttemptAndPartialBody cancels while the sniff loop
// is waiting for more data. The attempt must still be recorded with its upstream
// status/headers and the partial body must remain downloadable.
func TestCancelDuringSniffRecordsAttemptAndPartialBody(t *testing.T) {
	prefix := []byte(`{"partial":"kept"`)
	prefixHit := make(chan struct{}, 1)
	body := newGatedBody(prefix, prefixHit)
	const upstreamStatus = http.StatusOK
	tr := &staticTransport{
		status: upstreamStatus,
		header: http.Header{"Content-Type": []string{"application/json"}, "X-Upstream": []string{"gated"}},
		body:   body,
	}

	cfg := baseConfig("http://upstream.invalid")
	cfg.BodySniffTimeoutMS = 5000 // long: only cancellation ends the sniff
	logs := store.NewLogStore(10)
	p := New(mustStore(t, cfg), logs)
	p.client = &http.Client{Transport: tr}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-prefixHit:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream prefix was never read")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after cancellation")
	}

	entry := waitLogEntry(t, logs)
	if entry.Status != 499 {
		t.Errorf("status=%d want 499", entry.Status)
	}
	if entry.AttemptCount != 1 || len(entry.Attempts) != 1 {
		t.Fatalf("attempt_count=%d attempts=%d, want 1/1: a canceled sniff must not drop the attempt",
			entry.AttemptCount, len(entry.Attempts))
	}
	att := entry.Attempts[0]
	if att.Status != upstreamStatus {
		t.Errorf("attempt status=%d want %d", att.Status, upstreamStatus)
	}
	if http.Header(att.ResponseHeaders).Get("X-Upstream") != "gated" {
		t.Errorf("attempt lost upstream headers: %v", att.ResponseHeaders)
	}
	if !att.ResponseBodyAvailable || att.ResponseBodyComplete {
		t.Errorf("partial capture available=%v complete=%v want true/false",
			att.ResponseBodyAvailable, att.ResponseBodyComplete)
	}
	if att.ResponseBodyCapturedBytes != int64(len(prefix)) {
		t.Errorf("captured=%d want %d", att.ResponseBodyCapturedBytes, len(prefix))
	}
	if !entry.Incomplete || entry.Error == "" {
		t.Errorf("canceled sniff incomplete=%v error=%q want true/non-empty", entry.Incomplete, entry.Error)
	}
	if !body.closed.Load() {
		t.Error("upstream body was not closed; the reader may still be running")
	}

	rc, _, err := logs.OpenBody(entry.ID, "attempt-1")
	if err != nil {
		t.Fatalf("OpenBody(attempt-1): %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, prefix) {
		t.Errorf("retained partial body=%q want %q", got, prefix)
	}
}

// ---------------------------------------------------------------------------
// A/B hop chain: two real proxy instances must detect the cycle on first return
// ---------------------------------------------------------------------------

// TestMutualProxyLoopTerminatesWith508 points two real proxies at each other and
// requires the origin to answer 508 when its own marker returns in the chain. A
// test-local guard bounds a broken build so it cannot run away.
func TestMutualProxyLoopTerminatesWith508(t *testing.T) {
	var proxyA, proxyB *Proxy
	var aHits, bHits int32
	guard := func(counter *int32, next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(counter, 1) > 8 {
				w.Header().Set("X-Loop-Guard", "tripped")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	srvA := httptest.NewServer(guard(&aHits, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyA.ServeHTTP(w, r)
	})))
	defer srvA.Close()
	srvB := httptest.NewServer(guard(&bHits, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyB.ServeHTTP(w, r)
	})))
	defer srvB.Close()

	cfgA := baseConfig(srvB.URL)
	cfgA.Upstreams[0].TimeoutMS = 2000
	cfgB := baseConfig(srvA.URL)
	cfgB.Upstreams[0].TimeoutMS = 2000
	proxyA = New(mustStore(t, cfgA), store.NewLogStore(10))
	proxyB = New(mustStore(t, cfgB), store.NewLogStore(10))

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srvA.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mutual loop did not terminate with a response: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Loop-Guard"); got != "" {
		t.Fatalf("loop guard tripped (status %d): the hop chain was not detected", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusLoopDetected {
		t.Fatalf("status=%d want 508 on first return to the origin", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&aHits); n > 2 {
		t.Fatalf("origin served %d requests; the cycle was not detected on first return", n)
	}
}

// ---------------------------------------------------------------------------
// dedicated /{id}/... provider routing
// ---------------------------------------------------------------------------

type recordedCall struct {
	method    string
	path      string
	escaped   string
	query     string
	body      string
	auth      string
	apiKey    string
	googleKey string
}

// recordingUpstream records every request and answers using the supplied
// per-call-number status/body function.
type recordingUpstream struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (u *recordingUpstream) handler(reply func(n int) (int, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		u.mu.Lock()
		n := len(u.calls) + 1
		u.calls = append(u.calls, recordedCall{
			method:    req.Method,
			path:      req.URL.Path,
			escaped:   req.URL.EscapedPath(),
			query:     req.URL.RawQuery,
			body:      string(body),
			auth:      req.Header.Get("Authorization"),
			apiKey:    req.Header.Get("X-API-Key"),
			googleKey: req.Header.Get("X-Goog-Api-Key"),
		})
		u.mu.Unlock()
		code, text := reply(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, text)
	}
}

func (u *recordingUpstream) snapshot() []recordedCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recordedCall(nil), u.calls...)
}

func (u *recordingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func alwaysOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"ok":true}`)
}

// dedicatedConfig builds two providers: A is explicitly selectable even though
// it has weight 0, mismatching legacy globs and a conflicting stored
// Authorization override; B is the healthy legacy default.
func dedicatedConfig(aURL, bURL string) *config.Config {
	cfg := baseConfig(aURL)
	cfg.Upstreams = []config.Upstream{
		{
			ID: "providerA", Name: "A", BaseURL: aURL, Enabled: true, Weight: 0,
			TimeoutMS: config.DefaultUpstreamTimeoutMS,
			PathGlobs: []string{"/never/*"}, ModelGlobs: []string{"no-such-model"},
			Headers: map[string]string{"Authorization": "Bearer STORED-OVERRIDE"},
		},
		{
			ID: "providerB", Name: "B", BaseURL: bURL, Enabled: true, Weight: 100,
			TimeoutMS: config.DefaultUpstreamTimeoutMS,
		},
	}
	cfg.Retry = config.Retry{MaxAttempts: 3, StatusCodes: []int{http.StatusTooManyRequests}}
	return cfg
}

// TestDedicatedRoutePinsProvider is the core contract: /{id}/... selects exactly
// that upstream, strips only the identifier, passes the caller's own key
// through untouched (ignoring stored overrides), and retries only that provider.
func TestDedicatedRoutePinsProvider(t *testing.T) {
	const requestBody = `{"model":"m","messages":[]}`
	a := &recordingUpstream{}
	b := &recordingUpstream{}
	upA := httptest.NewServer(a.handler(func(n int) (int, string) {
		if n == 1 {
			return http.StatusTooManyRequests, `{"error":"retry"}`
		}
		return http.StatusOK, `{"ok":"A"}`
	}))
	defer upA.Close()
	upB := httptest.NewServer(b.handler(func(int) (int, string) { return http.StatusOK, `{"ok":"B"}` }))
	defer upB.Close()

	cfg := dedicatedConfig(upA.URL, upB.URL)
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/providerA/v1/chat/completions?trace=xyz", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer vendor-A")
	req.Header.Set("X-API-Key", "vendor-A-api-key")
	req.Header.Set("X-Goog-Api-Key", "vendor-A-google-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != `{"ok":"A"}` {
		t.Fatalf("status=%d body=%q want 200/{\"ok\":\"A\"}", resp.StatusCode, got)
	}

	if n := b.count(); n != 0 {
		t.Fatalf("provider B received %d calls; a dedicated route must never reach another provider", n)
	}
	calls := a.snapshot()
	if len(calls) != 2 {
		t.Fatalf("provider A calls=%d want 2 (429 then success)", len(calls))
	}
	for i, c := range calls {
		if c.method != http.MethodPost {
			t.Errorf("call %d method=%q want POST", i+1, c.method)
		}
		if c.path != "/v1/chat/completions" {
			t.Errorf("call %d path=%q want /v1/chat/completions (identifier stripped)", i+1, c.path)
		}
		if c.query != "trace=xyz" {
			t.Errorf("call %d query=%q want trace=xyz", i+1, c.query)
		}
		if c.body != requestBody {
			t.Errorf("call %d body=%q want %q", i+1, c.body, requestBody)
		}
		if c.auth != "Bearer vendor-A" {
			t.Errorf("call %d Authorization=%q want caller key (stored override must be ignored)", i+1, c.auth)
		}
		if c.apiKey != "vendor-A-api-key" {
			t.Errorf("call %d X-API-Key=%q want caller key", i+1, c.apiKey)
		}
		if c.googleKey != "vendor-A-google-key" {
			t.Errorf("call %d X-Goog-Api-Key=%q want caller key", i+1, c.googleKey)
		}
	}

	entry := waitLogEntry(t, logs)
	if entry.AttemptCount != 2 || len(entry.Attempts) != 2 {
		t.Fatalf("attempts=%d/%d want 2/2", entry.AttemptCount, len(entry.Attempts))
	}
	for i, att := range entry.Attempts {
		if att.UpstreamID != "providerA" {
			t.Errorf("attempt %d upstream=%q want providerA (pinned)", i+1, att.UpstreamID)
		}
		if !strings.Contains(att.URL, "/v1/chat/completions") {
			t.Errorf("attempt %d logged URL=%q want stripped path", i+1, att.URL)
		}
	}
	if entry.Path != "/providerA/v1/chat/completions" {
		t.Errorf("logged client path=%q want the original incoming path", entry.Path)
	}
}

// TestDedicatedRouteNoStoredKeyAndPerCallerKey proves a provider needs no
// stored credential and every caller's key is forwarded as-is.
func TestDedicatedRouteNoStoredKeyAndPerCallerKey(t *testing.T) {
	a := &recordingUpstream{}
	upA := httptest.NewServer(a.handler(func(int) (int, string) { return http.StatusOK, `{"ok":true}` }))
	defer upA.Close()

	cfg := baseConfig(upA.URL)
	cfg.Upstreams = []config.Upstream{{
		ID: "openai", Name: "OpenAI", BaseURL: upA.URL, Enabled: true, Weight: 1,
		TimeoutMS: config.DefaultUpstreamTimeoutMS,
	}}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	for i, key := range []string{"Bearer caller-one", "Bearer caller-two"} {
		req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/openai/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d status=%d want 200", i+1, resp.StatusCode)
		}
	}

	calls := a.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls=%d want 2", len(calls))
	}
	if calls[0].auth != "Bearer caller-one" || calls[1].auth != "Bearer caller-two" {
		t.Fatalf("forwarded keys=%q,%q want each caller's own key", calls[0].auth, calls[1].auth)
	}
	if calls[0].auth == calls[1].auth {
		t.Fatal("second caller's key was replaced or cached")
	}
	_ = logs
}

// TestDedicatedRouteExhaustionStaysSameProvider drains all attempts and must
// still target only the selected provider (whose last response is returned).
func TestDedicatedRouteExhaustionStaysSameProvider(t *testing.T) {
	a := &recordingUpstream{}
	b := &recordingUpstream{}
	upA := httptest.NewServer(a.handler(func(int) (int, string) { return http.StatusTooManyRequests, `{"error":"busy"}` }))
	defer upA.Close()
	upB := httptest.NewServer(b.handler(func(int) (int, string) { return http.StatusOK, `{"ok":"B"}` }))
	defer upB.Close()

	cfg := dedicatedConfig(upA.URL, upB.URL)
	cfg.Retry = config.Retry{MaxAttempts: 2, StatusCodes: []int{http.StatusTooManyRequests}}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	resp, err := http.Get(proxySrv.URL + "/providerA/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d want the selected provider's last 429", resp.StatusCode)
	}
	if a.count() != 2 {
		t.Errorf("provider A calls=%d want 2", a.count())
	}
	if b.count() != 0 {
		t.Errorf("provider B calls=%d want 0 on exhaustion", b.count())
	}
	entry := waitLogEntry(t, logs)
	for _, att := range entry.Attempts {
		if att.UpstreamID != "providerA" {
			t.Errorf("attempt upstream=%q want providerA", att.UpstreamID)
		}
	}
	if len(entry.Attempts) == 0 || !strings.Contains(entry.Attempts[0].RetryReason, "429") {
		t.Errorf("retry reason not recorded: %+v", entry.Attempts)
	}
}

// TestUnknownAndDisabledProviderPrefixes proves a dedicated-shaped URL with an
// unknown ID 404s and a disabled known provider 503s, neither touching any
// upstream, while other legacy paths keep working.
func TestUnknownAndDisabledProviderPrefixes(t *testing.T) {
	a := &recordingUpstream{}
	b := &recordingUpstream{}
	upA := httptest.NewServer(a.handler(func(int) (int, string) { return http.StatusOK, `{"ok":"A"}` }))
	defer upA.Close()
	upB := httptest.NewServer(b.handler(func(int) (int, string) { return http.StatusOK, `{"ok":"B"}` }))
	defer upB.Close()

	cfg := baseConfig(upA.URL)
	cfg.Upstreams = []config.Upstream{
		{ID: "alpha", Name: "Alpha", BaseURL: upA.URL, Enabled: true, Weight: 100, TimeoutMS: config.DefaultUpstreamTimeoutMS},
		{ID: "beta", Name: "Beta", BaseURL: upB.URL, Enabled: false, Weight: 100, TimeoutMS: config.DefaultUpstreamTimeoutMS},
	}
	_, proxySrv := newProxyServer(t, mustStore(t, cfg))

	cases := []struct {
		path string
		want int
	}{
		{"/ghost/v1/chat/completions", http.StatusNotFound},
		{"/ghost/v1", http.StatusNotFound},
		{"/ghost/v1beta/models", http.StatusNotFound},
		{"/ghost/v2/chat", http.StatusNotFound},
		{"/beta/v1/chat/completions", http.StatusServiceUnavailable},
		{"/beta", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		resp, err := http.Get(proxySrv.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s status=%d want %d", tc.path, resp.StatusCode, tc.want)
		}
	}
	if a.count() != 0 || b.count() != 0 {
		t.Fatalf("unknown/disabled prefixes reached a provider: A=%d B=%d", a.count(), b.count())
	}

	// A non-dedicated legacy path keeps its existing behavior and reaches the
	// enabled provider.
	resp, err := http.Get(proxySrv.URL + "/ghost/other")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || a.count() != 1 {
		t.Fatalf("legacy /ghost/other status=%d A=%d want 200/1", resp.StatusCode, a.count())
	}
}

// TestDedicatedRouteEscapedPathAndQuery verifies the escaped remainder and
// query survive the identifier strip byte-for-byte.
func TestDedicatedRouteEscapedPathAndQuery(t *testing.T) {
	a := &recordingUpstream{}
	upA := httptest.NewServer(a.handler(func(int) (int, string) { return http.StatusOK, `{"ok":true}` }))
	defer upA.Close()

	cfg := baseConfig(upA.URL)
	cfg.Upstreams = []config.Upstream{{
		ID: "openai", Name: "OpenAI", BaseURL: upA.URL, Enabled: true, Weight: 1,
		TimeoutMS: config.DefaultUpstreamTimeoutMS,
	}}
	_, proxySrv := newProxyServer(t, mustStore(t, cfg))

	resp, err := http.Get(proxySrv.URL + "/openai/v1/a%2Fb/c?x=1&y=two")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	calls := a.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls=%d want 1", len(calls))
	}
	if calls[0].escaped != "/v1/a%2Fb/c" {
		t.Errorf("upstream escaped path=%q want /v1/a%%2Fb/c", calls[0].escaped)
	}
	if calls[0].query != "x=1&y=two" {
		t.Errorf("upstream query=%q want x=1&y=two", calls[0].query)
	}
}

// TestDedicatedRouteLogRedactsQueryCredential verifies a query credential is
// forwarded unchanged but never stored in the log.
func TestDedicatedRouteLogRedactsQueryCredential(t *testing.T) {
	a := &recordingUpstream{}
	upA := httptest.NewServer(a.handler(func(int) (int, string) { return http.StatusOK, `{"ok":true}` }))
	defer upA.Close()

	cfg := baseConfig(upA.URL)
	cfg.Upstreams = []config.Upstream{{
		ID: "openai", Name: "OpenAI", BaseURL: upA.URL, Enabled: true, Weight: 1,
		TimeoutMS: config.DefaultUpstreamTimeoutMS,
	}}
	logs, proxySrv := newProxyServer(t, mustStore(t, cfg))

	const secret = "sk-query-secret-value"
	resp, err := http.Get(proxySrv.URL + "/openai/v1/models?api_key=" + secret + "&x=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	calls := a.snapshot()
	if len(calls) != 1 || calls[0].query != "api_key="+secret+"&x=1" {
		t.Fatalf("upstream query=%q want the credential forwarded unchanged", calls[0].query)
	}
	entry := waitLogEntry(t, logs)
	if strings.Contains(entry.Query, secret) {
		t.Errorf("logged query leaked the credential: %q", entry.Query)
	}
	if !strings.Contains(entry.Query, "x=1") {
		t.Errorf("logged query lost non-credential params: %q", entry.Query)
	}
	for _, att := range entry.Attempts {
		if strings.Contains(att.URL, secret) {
			t.Errorf("logged attempt URL leaked the credential: %q", att.URL)
		}
	}
}

// TestResolveRouteShapes pins the exact classifier used by the dispatcher.
func TestResolveRouteShapes(t *testing.T) {
	cfg := &config.Config{Upstreams: []config.Upstream{
		{ID: "openai", Name: "OpenAI", BaseURL: "http://a", Enabled: true, Weight: 1, TimeoutMS: 1,
			PathGlobs: []string{"/legacy/*"}},
		{ID: "off", Name: "Off", BaseURL: "http://b", Enabled: false, Weight: 1, TimeoutMS: 1,
			PathGlobs: []string{"/legacy/*"}},
	}}
	cases := []struct {
		path      string
		wantID    string
		wantStat  int
		dedicated bool
	}{
		{"/openai/v1/chat/completions", "openai", 0, true},
		{"/openai", "openai", 0, true},
		{"/openai/", "openai", 0, true},
		{"/openai/v1beta/models", "openai", 0, true},
		{"/off/v1/chat", "", http.StatusServiceUnavailable, false},
		{"/ghost/v1/chat", "", http.StatusNotFound, false},
		{"/ghost/v2", "", http.StatusNotFound, false},
		// Legacy fallback: not dedicated, and the legacy globs do not match.
		{"/ghost/other", "", http.StatusNotFound, false},
		{"/v1/chat", "", http.StatusNotFound, false},
		{"/v1beta/models", "", http.StatusNotFound, false},
		{"/v2/chat", "", http.StatusNotFound, false},
		{"/api/chat", "", http.StatusNotFound, false},
		// An encoded slash never smuggles the identifier into selection.
		{"/openai%2Fv1/x", "", http.StatusNotFound, false},
		// Legacy paths with matching globs keep working.
		{"/legacy/x", "openai", 0, false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://proxy"+tc.path, nil)
		d := resolveRoute(cfg, r, "")
		gotID := ""
		if d.up != nil {
			gotID = d.up.ID
		}
		if gotID != tc.wantID || d.status != tc.wantStat || d.dedicated != tc.dedicated {
			t.Errorf("resolveRoute(%q) = (id=%q,status=%d,dedicated=%v), want (%q,%d,%v)",
				tc.path, gotID, d.status, d.dedicated, tc.wantID, tc.wantStat, tc.dedicated)
		}
	}
}

// TestResolveRouteLegacyAndEncoded pins the two route-resolution edge cases:
// genuine legacy API roots must keep legacy routing, a percent-encoded
// unreserved identifier still names its provider, and an unknown provider-shaped
// URL (plain or with an encoded version segment) is refused without a fallback.
// A decoded slash or backslash in the identifier is never a provider match.
func TestResolveRouteLegacyAndEncoded(t *testing.T) {
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		ID: "providerA", Name: "A", BaseURL: "http://a", Enabled: true, Weight: 1,
		TimeoutMS: config.DefaultUpstreamTimeoutMS,
	}}

	cases := []struct {
		name          string
		target        string
		wantUp        string
		wantStatus    int
		wantDedicated bool
		wantOutbound  string
	}{
		// Genuine legacy API roots stay on legacy routing.
		{"legacy api root", "/api/v1/foo", "providerA", 0, false, ""},
		{"legacy v1 root", "/v1/foo", "providerA", 0, false, ""},
		{"legacy v1beta root", "/v1beta/models", "providerA", 0, false, ""},
		{"legacy v2 root", "/v2/chat", "providerA", 0, false, ""},
		// Known provider: identifier stripped, remaining escaped path exact.
		{"plain dedicated", "/providerA/v1/a%2Fb?x=%2F", "providerA", 0, true, "/v1/a%2Fb"},
		{"encoded unreserved id", "/%70roviderA/v1/chat?x=1", "providerA", 0, true, "/v1/chat"},
		// Unknown provider-shaped URLs are refused, including encoded versions.
		{"unknown plain version", "/missing/v1/foo", "", http.StatusNotFound, false, ""},
		{"unknown encoded version", "/missing/v%31/foo", "", http.StatusNotFound, false, ""},
		{"unknown encoded v1beta", "/missing/v1beta%2Fmodels", "", http.StatusNotFound, false, ""},
		// A decoded slash/backslash identifier must never match or fall back.
		{"encoded slash id", "/providerA%2Fv1/x", "", http.StatusNotFound, false, ""},
		{"encoded backslash id", "/providerA%5Cv1/x", "", http.StatusNotFound, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://proxy"+tc.target, nil)
			gotID := ""
			d := resolveRoute(cfg, r, "")
			if d.up != nil {
				gotID = d.up.ID
				if d.dedicated && tc.wantOutbound != "" {
					if got := outboundPath(r, d.dedicated); got != tc.wantOutbound {
						t.Errorf("outboundPath(%q) = %q, want %q", tc.target, got, tc.wantOutbound)
					}
				}
			}
			if gotID != tc.wantUp || d.status != tc.wantStatus || d.dedicated != tc.wantDedicated {
				t.Errorf("resolveRoute(%q) = (id=%q,status=%d,dedicated=%v), want (%q,%d,%v)",
					tc.target, gotID, d.status, d.dedicated, tc.wantUp, tc.wantStatus, tc.wantDedicated)
			}
		})
	}
}

// TestEncodedProviderRouteForwardsExactly proves over real HTTP that a
// percent-encoded known identifier selects its provider, strips only that
// segment, preserves the remaining escaped path/query and the caller key, and
// never falls back to another provider. Unknown encoded providers are refused
// without contacting any upstream.
func TestEncodedProviderRouteForwardsExactly(t *testing.T) {
	a := &recordingUpstream{}
	b := &recordingUpstream{}
	upA := httptest.NewServer(a.handler(func(int) (int, string) { return http.StatusOK, `{"ok":"A"}` }))
	defer upA.Close()
	upB := httptest.NewServer(b.handler(func(int) (int, string) { return http.StatusOK, `{"ok":"B"}` }))
	defer upB.Close()

	cfg := dedicatedConfig(upA.URL, upB.URL)
	_, proxySrv := newProxyServer(t, mustStore(t, cfg))

	req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/%70roviderA/v1/a%2Fb?x=%2F", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer caller-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != `{"ok":"A"}` {
		t.Fatalf("status=%d body=%q want 200/{\"ok\":\"A\"}", resp.StatusCode, got)
	}
	if n := b.count(); n != 0 {
		t.Errorf("provider B received %d calls; encoded identifier must pin provider A only", n)
	}
	calls := a.snapshot()
	if len(calls) != 1 {
		t.Fatalf("provider A calls=%d want 1", len(calls))
	}
	if calls[0].escaped != "/v1/a%2Fb" {
		t.Errorf("forwarded escaped path=%q want /v1/a%%2Fb (only identifier stripped)", calls[0].escaped)
	}
	if calls[0].query != "x=%2F" {
		t.Errorf("forwarded query=%q want x=%%2F preserved byte-exact", calls[0].query)
	}
	if calls[0].auth != "Bearer caller-key" {
		t.Errorf("forwarded Authorization=%q want caller key", calls[0].auth)
	}

	// Unknown provider-shaped URLs, plain and encoded, are 404 with no network.
	for _, path := range []string{"/missing/v1/foo", "/missing/v%31/foo", "/providerA%2Fv1/x", "/providerA%5Cv1/x"} {
		resp, err := http.Get(proxySrv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status=%d want 404", path, resp.StatusCode)
		}
	}
	if a.count() != 1 || b.count() != 0 {
		t.Errorf("refused URLs reached an upstream: A=%d B=%d want 1/0", a.count(), b.count())
	}
}

// TestLegacyRoutingPinsProviderAcrossRetries proves legacy requests also never
// switch provider mid-retry.
func TestLegacyRoutingPinsProviderAcrossRetries(t *testing.T) {
	a := &recordingUpstream{}
	b := &recordingUpstream{}
	upA := httptest.NewServer(a.handler(func(int) (int, string) { return http.StatusTooManyRequests, `{"error":"busy"}` }))
	defer upA.Close()
	upB := httptest.NewServer(b.handler(func(int) (int, string) { return http.StatusTooManyRequests, `{"error":"busy"}` }))
	defer upB.Close()

	cfg := baseConfig(upA.URL)
	cfg.Upstreams = []config.Upstream{
		{ID: "a", Name: "A", BaseURL: upA.URL, Enabled: true, Weight: 100, TimeoutMS: config.DefaultUpstreamTimeoutMS},
		{ID: "b", Name: "B", BaseURL: upB.URL, Enabled: true, Weight: 100, TimeoutMS: config.DefaultUpstreamTimeoutMS},
	}
	cfg.Retry = config.Retry{MaxAttempts: 4, StatusCodes: []int{http.StatusTooManyRequests}}
	_, proxySrv := newProxyServer(t, mustStore(t, cfg))

	resp, err := http.Get(proxySrv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", resp.StatusCode)
	}
	total := a.count() + b.count()
	if total != 4 {
		t.Fatalf("total upstream calls=%d want 4 (all attempts)", total)
	}
	if a.count() != 0 && b.count() != 0 {
		t.Fatalf("legacy retries switched provider: A=%d B=%d", a.count(), b.count())
	}
}
