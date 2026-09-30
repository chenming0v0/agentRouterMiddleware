package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentrouter/internal/config"
	"agentrouter/internal/store"
)

type testEnv struct {
	t    *testing.T
	srv  *Server
	cfg  *config.Store
	logs *store.LogStore
}

func newEnv(t *testing.T, token string) *testEnv {
	t.Helper()
	cs, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	ls := store.NewLogStore(cs.Get().LogLimit)
	return &testEnv{t: t, srv: New(cs, ls, token), cfg: cs, logs: ls}
}

// request builds a request that looks like a loopback admin call by default.
func (e *testEnv) request(method, target string, body any) *http.Request {
	e.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, target, nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal: %v", err)
		}
		r = httptest.NewRequest(method, target, bytes.NewReader(data))
	}
	r.RemoteAddr = "127.0.0.1:5000"
	r.Host = "localhost:18851"
	return r
}

func (e *testEnv) do(r *http.Request) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, r)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestHealth(t *testing.T) {
	e := newEnv(t, "")
	rec := e.do(e.request(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", rec.Code)
	}
	if got := decodeBody[okBody](t, rec).OK; !got {
		t.Fatal("health ok = false")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	// Health is readable from anywhere, even non-loopback.
	r := e.request(http.MethodGet, "/api/health", nil)
	r.RemoteAddr = "203.0.113.7:1234"
	if rec := e.do(r); rec.Code != http.StatusOK {
		t.Fatalf("remote health status = %d, want 200", rec.Code)
	}
	// But the method is enforced.
	if rec := e.do(e.request(http.MethodPost, "/api/health", nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST health status = %d, want 405", rec.Code)
	}
}

func TestLoopbackOnlyAdmin(t *testing.T) {
	e := newEnv(t, "")

	if rec := e.do(e.request(http.MethodGet, "/api/config", nil)); rec.Code != http.StatusOK {
		t.Fatalf("loopback config status = %d, want 200", rec.Code)
	}

	remote := e.request(http.MethodGet, "/api/config", nil)
	remote.RemoteAddr = "203.0.113.7:1234"
	remote.Host = "api.example.com"
	rec := e.do(remote)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote config status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "loopback") {
		t.Fatalf("403 body not actionable: %s", rec.Body.String())
	}

	// Spoofed X-Forwarded-For must not grant loopback access.
	spoof := e.request(http.MethodGet, "/api/config", nil)
	spoof.RemoteAddr = "203.0.113.7:1234"
	spoof.Header.Set("X-Forwarded-For", "127.0.0.1")
	if rec := e.do(spoof); rec.Code != http.StatusForbidden {
		t.Fatalf("spoofed XFF status = %d, want 403", rec.Code)
	}

	// DNS rebinding: loopback peer but a non-loopback Host is rejected.
	rebind := e.request(http.MethodGet, "/api/config", nil)
	rebind.Host = "evil.example.com"
	if rec := e.do(rebind); rec.Code != http.StatusForbidden {
		t.Fatalf("rebinding status = %d, want 403", rec.Code)
	}
}

func TestTokenAuth(t *testing.T) {
	e := newEnv(t, "s3cret")

	if rec := e.do(e.request(http.MethodGet, "/api/config", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", rec.Code)
	} else if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate header")
	}

	wrong := e.request(http.MethodGet, "/api/config", nil)
	wrong.Header.Set("Authorization", "Bearer nope")
	if rec := e.do(wrong); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", rec.Code)
	}

	// A valid token works even from a non-loopback peer.
	ok := e.request(http.MethodGet, "/api/config", nil)
	ok.RemoteAddr = "203.0.113.7:1234"
	ok.Host = "api.example.com"
	ok.Header.Set("Authorization", "Bearer s3cret")
	if rec := e.do(ok); rec.Code != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200", rec.Code)
	}

	// Health stays open.
	if rec := e.do(e.request(http.MethodGet, "/api/health", nil)); rec.Code != http.StatusOK {
		t.Fatalf("health with token configured status = %d, want 200", rec.Code)
	}
}

func TestOriginCheck(t *testing.T) {
	e := newEnv(t, "")

	cross := e.request(http.MethodGet, "/api/config", nil)
	cross.Header.Set("Origin", "http://evil.example.com")
	if rec := e.do(cross); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", rec.Code)
	}

	same := e.request(http.MethodGet, "/api/config", nil)
	same.Header.Set("Origin", "http://localhost:18851")
	if rec := e.do(same); rec.Code != http.StatusOK {
		t.Fatalf("same-origin status = %d, want 200", rec.Code)
	}
}

func TestUnknownAndMethodPaths(t *testing.T) {
	e := newEnv(t, "")
	for _, path := range []string{"/api/confg", "/api", "/api/", "/api/nope"} {
		if rec := e.do(e.request(http.MethodGet, path, nil)); rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404", path, rec.Code)
		}
	}
	if rec := e.do(e.request(http.MethodDelete, "/api/config", nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE config status = %d, want 405", rec.Code)
	}
}

func TestConfigPutInvalidKeepsConfig(t *testing.T) {
	e := newEnv(t, "")
	before := e.cfg.Get()

	bad := e.cfg.Get()
	bad.Retry.MaxAttempts = 3
	bad.Retry.BodyRegexes = []string{"("}

	rec := e.do(e.request(http.MethodPut, "/api/config", bad))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid config PUT status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("invalid config PUT missing error: %s", rec.Body.String())
	}
	after := e.cfg.Get()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("stored config changed after rejected PUT")
	}
}

func TestConfigPutSuccessAppliesLogLimit(t *testing.T) {
	e := newEnv(t, "")
	cfg := e.cfg.Get()
	cfg.LogLimit = 7
	rec := e.do(e.request(http.MethodPut, "/api/config", cfg))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid PUT status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !decodeBody[okBody](t, rec).OK {
		t.Fatal("valid PUT ok = false")
	}
	var got config.Config
	rec = e.do(e.request(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if got.LogLimit != 7 {
		t.Fatalf("log_limit = %d, want 7", got.LogLimit)
	}

	for i := 0; i < 10; i++ {
		e.logs.Add(store.LogEntry{ID: fmt.Sprintf("e%d", i), Status: 200})
	}
	body := decodeBody[logsBody](t, e.do(e.request(http.MethodGet, "/api/logs", nil)))
	if len(body.Logs) != 7 {
		t.Fatalf("stored logs = %d, want 7 after log_limit update", len(body.Logs))
	}
	if body.Total != 7 {
		t.Fatalf("total = %d, want 7 (ring capacity)", body.Total)
	}
	if body.Logs[0].ID != "e9" {
		t.Fatalf("newest entry = %q, want e9", body.Logs[0].ID)
	}
}

func TestConfigPutTrailingGarbage(t *testing.T) {
	e := newEnv(t, "")
	valid, err := json.Marshal(e.cfg.Get())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(append(valid, []byte(" {}")...)))
	r.RemoteAddr = "127.0.0.1:5000"
	r.Host = "localhost:18851"
	if rec := e.do(r); rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing garbage status = %d, want 400", rec.Code)
	}
}

func TestRegexTest(t *testing.T) {
	e := newEnv(t, "")

	rec := e.do(e.request(http.MethodPost, "/api/regex/test", map[string]string{"pattern": "(", "text": "x"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("bad regex status = %d, want 200", rec.Code)
	}
	bad := decodeBody[regexTestResult](t, rec)
	if bad.Matched || bad.Error == "" {
		t.Fatalf("bad regex result = %+v, want unmatched with error", bad)
	}

	rec = e.do(e.request(http.MethodPost, "/api/regex/test", map[string]string{"pattern": "(?i)rate limit", "text": "Rate Limit hit"}))
	good := decodeBody[regexTestResult](t, rec)
	if !good.Matched || good.Error != "" {
		t.Fatalf("good regex result = %+v, want matched", good)
	}
}

func TestUpstreamTest(t *testing.T) {
	e := newEnv(t, "")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer up.Close()

	rec := e.do(e.request(http.MethodPost, "/api/upstreams/test", map[string]string{"base_url": up.URL}))
	res := decodeBody[upstreamTestResult](t, rec)
	if !res.OK || res.Status != http.StatusNotFound {
		t.Fatalf("reachable result = %+v, want ok with status 404", res)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	rec = e.do(e.request(http.MethodPost, "/api/upstreams/test", map[string]string{"base_url": deadURL}))
	res = decodeBody[upstreamTestResult](t, rec)
	if res.OK || res.Error == "" {
		t.Fatalf("unreachable result = %+v, want failure with error", res)
	}
}

func TestLogsStatsAndClear(t *testing.T) {
	e := newEnv(t, "")
	e.logs.Add(store.LogEntry{ID: "a", Status: 200, AttemptCount: 1})
	e.logs.Add(store.LogEntry{ID: "b", Status: 200, AttemptCount: 2})
	e.logs.Add(store.LogEntry{ID: "c", Status: 500, AttemptCount: 1})

	body := decodeBody[logsBody](t, e.do(e.request(http.MethodGet, "/api/logs", nil)))
	if body.Total != 3 || len(body.Logs) != 3 {
		t.Fatalf("logs total=%d len=%d, want 3/3", body.Total, len(body.Logs))
	}
	if body.Logs[0].ID != "c" {
		t.Fatalf("newest entry = %q, want c", body.Logs[0].ID)
	}

	limited := decodeBody[logsBody](t, e.do(e.request(http.MethodGet, "/api/logs?limit=1", nil)))
	if limited.Total != 3 || len(limited.Logs) != 1 {
		t.Fatalf("limited logs total=%d len=%d, want 3/1", limited.Total, len(limited.Logs))
	}
	if rec := e.do(e.request(http.MethodGet, "/api/logs?limit=abc", nil)); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit status = %d, want 400", rec.Code)
	}

	stats := decodeBody[store.Stats](t, e.do(e.request(http.MethodGet, "/api/stats", nil)))
	if stats.TotalRequests != 3 || stats.RetriedRequests != 1 || stats.RetrySuccess != 1 || stats.FailedRequests != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	if rec := e.do(e.request(http.MethodDelete, "/api/logs", nil)); rec.Code != http.StatusOK {
		t.Fatalf("DELETE logs status = %d, want 200", rec.Code)
	}
	body = decodeBody[logsBody](t, e.do(e.request(http.MethodGet, "/api/logs", nil)))
	if body.Total != 0 || len(body.Logs) != 0 {
		t.Fatalf("logs after clear total=%d len=%d, want 0/0", body.Total, len(body.Logs))
	}
}

func TestLogStream(t *testing.T) {
	e := newEnv(t, "")
	e.srv.ping = 30 * time.Millisecond

	ts := httptest.NewServer(e.srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/logs/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content-type = %q", ct)
	}

	lines := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	// The subscription is active once response headers arrive.
	e.logs.Add(store.LogEntry{ID: "evt1", Status: 200})

	var sawLog, sawPing bool
	deadline := time.After(3 * time.Second)
	for !sawLog || !sawPing {
		select {
		case <-deadline:
			t.Fatalf("stream timeout: log=%v ping=%v", sawLog, sawPing)
		case line, open := <-lines:
			if !open {
				t.Fatalf("stream closed: log=%v ping=%v", sawLog, sawPing)
			}
			if strings.Contains(line, "evt1") {
				sawLog = true
			}
			if line == ": ping" {
				sawPing = true
			}
		}
	}
	cancel()
}

func upstreamWithHeaders(id string, headers map[string]string) config.Upstream {
	return config.Upstream{
		ID:        id,
		Name:      "provider-" + id,
		BaseURL:   "https://api.example.com",
		Enabled:   true,
		Weight:    100,
		TimeoutMS: config.DefaultUpstreamTimeoutMS,
		Headers:   headers,
	}
}

func (e *testEnv) putConfig(cfg *config.Config) *httptest.ResponseRecorder {
	return e.do(e.request(http.MethodPut, "/api/config", cfg))
}

func (e *testEnv) getConfig() config.Config {
	e.t.Helper()
	rec := e.do(e.request(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET config status = %d", rec.Code)
	}
	var got config.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		e.t.Fatalf("decode config: %v", err)
	}
	return got
}

func storedUpstream(t *testing.T, cfg *config.Config, id string) config.Upstream {
	t.Helper()
	for _, u := range cfg.Upstreams {
		if u.ID == id {
			return u
		}
	}
	t.Fatalf("upstream %q not found", id)
	return config.Upstream{}
}

// TestConfigMaskAndRoundtrip verifies GET never leaks a configured secret and
// that feeding the masked document back keeps the stored credential.
func TestConfigMaskAndRoundtrip(t *testing.T) {
	e := newEnv(t, "")
	cfg := e.cfg.Get()
	cfg.Upstreams = []config.Upstream{upstreamWithHeaders("u_1", map[string]string{
		"Authorization": "Bearer sk-FAKE-not-a-real-key",
		"X-Custom":      "custom-FAKE-secret",
	})}
	if rec := e.putConfig(cfg); rec.Code != http.StatusOK {
		t.Fatalf("seed PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}

	// GET must mask every configured value but preserve keys and the id.
	masked := e.getConfig()
	got := storedUpstream(t, &masked, "u_1")
	if len(got.Headers) != 2 {
		t.Fatalf("masked headers = %v, want 2 keys", got.Headers)
	}
	for name, value := range got.Headers {
		if value != maskToken {
			t.Fatalf("masked header %q = %q, want %q", name, value, maskToken)
		}
	}
	if masked.Upstreams[0].ID != "u_1" {
		t.Fatalf("masked id = %q, want u_1", masked.Upstreams[0].ID)
	}

	// Masked round-trip: PUT the GET output back and confirm the secret survives.
	if rec := e.putConfig(&masked); rec.Code != http.StatusOK {
		t.Fatalf("roundtrip PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}
	stored := storedUpstream(t, e.cfg.Get(), "u_1")
	if stored.Headers["Authorization"] != "Bearer sk-FAKE-not-a-real-key" {
		t.Fatalf("roundtrip Authorization = %q, want original secret", stored.Headers["Authorization"])
	}
	if stored.Headers["X-Custom"] != "custom-FAKE-secret" {
		t.Fatalf("roundtrip X-Custom = %q, want original secret", stored.Headers["X-Custom"])
	}
	if v := storedUpstream(t, &masked, "u_1").Headers["Authorization"]; v != maskToken {
		t.Fatalf("masked GET leaked value %q", v)
	}
}

// TestConfigHeaderReplaceAndDelete verifies an explicit header map is a full
// replacement: new values replace secrets and omitted keys are removed.
func TestConfigHeaderReplaceAndDelete(t *testing.T) {
	e := newEnv(t, "")
	cfg := e.cfg.Get()
	cfg.Upstreams = []config.Upstream{upstreamWithHeaders("u_1", map[string]string{
		"Authorization": "Bearer sk-FAKE-original",
		"X-Drop":        "remove-me",
	})}
	if rec := e.putConfig(cfg); rec.Code != http.StatusOK {
		t.Fatalf("seed PUT status = %d", rec.Code)
	}

	replacement := e.getConfig()
	replacement.Upstreams[0].Headers = map[string]string{"Authorization": "Bearer sk-plain-new"}
	if rec := e.putConfig(&replacement); rec.Code != http.StatusOK {
		t.Fatalf("replace PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}

	stored := storedUpstream(t, e.cfg.Get(), "u_1").Headers
	if stored["Authorization"] != "Bearer sk-plain-new" {
		t.Fatalf("Authorization = %q, want replacement", stored["Authorization"])
	}
	if _, ok := stored["X-Drop"]; ok {
		t.Fatalf("X-Drop survived explicit replacement: %v", stored)
	}
}

// TestConfigMaskWithoutStoredValueRejected verifies the "***" token is only
// valid as a reference to an existing stored secret.
func TestConfigMaskWithoutStoredValueRejected(t *testing.T) {
	e := newEnv(t, "")
	before := e.cfg.Get()

	cfg := e.cfg.Get()
	cfg.Upstreams = []config.Upstream{upstreamWithHeaders("u_new", map[string]string{"Authorization": maskToken})}
	rec := e.putConfig(cfg)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("dangling mask status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if after := e.cfg.Get(); !reflect.DeepEqual(before, after) {
		t.Fatal("config changed after rejected masked PUT")
	}
}

// TestConfigNullHeadersPreserved verifies an omitted/null headers field keeps
// the stored credential map.
func TestConfigNullHeadersPreserved(t *testing.T) {
	e := newEnv(t, "")
	cfg := e.cfg.Get()
	cfg.Upstreams = []config.Upstream{upstreamWithHeaders("u_1", map[string]string{"Authorization": "Bearer sk-FAKE-keep"})}
	if rec := e.putConfig(cfg); rec.Code != http.StatusOK {
		t.Fatalf("seed PUT status = %d", rec.Code)
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ups := doc["upstreams"].([]any)
	ups[0].(map[string]any)["headers"] = nil
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(raw))
	r.RemoteAddr = "127.0.0.1:5000"
	r.Host = "localhost:18851"
	if rec := e.do(r); rec.Code != http.StatusOK {
		t.Fatalf("null-headers PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if got := storedUpstream(t, e.cfg.Get(), "u_1").Headers["Authorization"]; got != "Bearer sk-FAKE-keep" {
		t.Fatalf("Authorization = %q, want preserved secret", got)
	}
}

// TestConfigHeaderCaseInsensitiveMaskAndClear verifies that a masked value is
// resolved against the stored header by name ignoring case, that an explicit
// map replaces the whole key set, and that {} clears every header.
func TestConfigHeaderCaseInsensitiveMaskAndClear(t *testing.T) {
	e := newEnv(t, "")
	cfg := e.cfg.Get()
	cfg.Upstreams = []config.Upstream{upstreamWithHeaders("u_1", map[string]string{
		"Authorization": "Bearer sk-FAKE-original",
		"X-Keep":        "keep",
	})}
	if rec := e.putConfig(cfg); rec.Code != http.StatusOK {
		t.Fatalf("seed PUT status = %d", rec.Code)
	}

	// A differently-cased masked name refers to the stored secret; the
	// explicitly supplied map still replaces (drops) every absent key.
	masked := e.getConfig()
	masked.Upstreams[0].Headers = map[string]string{"authorization": maskToken}
	if rec := e.putConfig(&masked); rec.Code != http.StatusOK {
		t.Fatalf("case-insensitive mask PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}
	stored := storedUpstream(t, e.cfg.Get(), "u_1").Headers
	if stored["authorization"] != "Bearer sk-FAKE-original" {
		t.Fatalf("case-insensitive mask = %q, want stored secret", stored["authorization"])
	}
	if _, ok := stored["X-Keep"]; ok {
		t.Fatalf("X-Keep survived explicit replacement: %v", stored)
	}

	// An explicit empty object clears all configured headers.
	masked = e.getConfig()
	masked.Upstreams[0].Headers = map[string]string{}
	if rec := e.putConfig(&masked); rec.Code != http.StatusOK {
		t.Fatalf("clear-headers PUT status = %d (body %s)", rec.Code, rec.Body.String())
	}
	if got := storedUpstream(t, e.cfg.Get(), "u_1").Headers; len(got) != 0 {
		t.Fatalf("headers after {} = %v, want empty", got)
	}
}

func seedLogWithBodies(t *testing.T, logs *store.LogStore, id string) (requestBody, attemptBody []byte) {
	t.Helper()
	requestBody = []byte{0x00, 0x01, 'h', 'i', 0xff, '\n'}
	attemptBody = []byte("rate limited by upstream")

	rec := logs.NewBodyRecorder(id, "request", 0)
	if _, err := rec.Write(requestBody); err != nil {
		t.Fatalf("write request body: %v", err)
	}
	if info := rec.Finish(true); !info.Available || !info.Complete {
		t.Fatalf("request capture info = %+v, want available+complete", info)
	}

	rec = logs.NewBodyRecorder(id, "attempt-1", 0)
	if _, err := rec.Write(attemptBody); err != nil {
		t.Fatalf("write attempt body: %v", err)
	}
	if info := rec.Finish(true); !info.Available || !info.Complete {
		t.Fatalf("attempt capture info = %+v, want available+complete", info)
	}

	logs.Add(store.LogEntry{
		ID:               id,
		Status:           http.StatusOK,
		AttemptCount:     2,
		ResponseBodyPart: "attempt-1",
	})
	return requestBody, attemptBody
}

func TestLogMetadataAndBodyRetrieval(t *testing.T) {
	e := newEnv(t, "")
	requestBody, attemptBody := seedLogWithBodies(t, e.logs, "logbody1")

	rec := e.do(e.request(http.MethodGet, "/api/logs/logbody1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata status = %d, want 200", rec.Code)
	}
	entry := decodeBody[store.LogEntry](t, rec)
	if entry.ID != "logbody1" || entry.Status != http.StatusOK || entry.AttemptCount != 2 {
		t.Fatalf("metadata = %+v", entry)
	}

	// Byte-exact raw download, including NUL and 0xff bytes.
	rec = e.do(e.request(http.MethodGet, "/api/logs/logbody1/body/request", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("request body status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), requestBody) {
		t.Fatalf("request body = %v, want %v", rec.Body.Bytes(), requestBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff header")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", cc)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(requestBody)) {
		t.Fatalf("content-length = %q, want %d", cl, len(requestBody))
	}

	// The "response" alias resolves to the recorded final attempt.
	rec = e.do(e.request(http.MethodGet, "/api/logs/logbody1/body/response", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("response alias status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), attemptBody) {
		t.Fatalf("response alias = %q, want %q", rec.Body.Bytes(), attemptBody)
	}

	rec = e.do(e.request(http.MethodGet, "/api/logs/logbody1/body/attempt-1", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != string(attemptBody) {
		t.Fatalf("attempt-1 body = %q status %d", rec.Body.String(), rec.Code)
	}
}

func TestLogBodyErrorsAndAuthorization(t *testing.T) {
	e := newEnv(t, "")
	seedLogWithBodies(t, e.logs, "logbody2")

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/logs/missing", http.StatusNotFound},
		{http.MethodGet, "/api/logs/missing/body/request", http.StatusNotFound},
		{http.MethodGet, "/api/logs/logbody2/body/attempt-2", http.StatusNotFound},
		{http.MethodGet, "/api/logs/logbody2/body/attempt-0", http.StatusBadRequest},
		{http.MethodGet, "/api/logs/logbody2/body/attempt-01", http.StatusBadRequest},
		{http.MethodGet, "/api/logs/logbody2/body/other", http.StatusBadRequest},
		{http.MethodGet, "/api/logs/logbody2/body/../../etc/passwd", http.StatusBadRequest},
		{http.MethodGet, "/api/logs/..%2f..%2fetc", http.StatusBadRequest},
		{http.MethodGet, "/api/logs/", http.StatusBadRequest},
		{http.MethodPost, "/api/logs/logbody2", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/logs/logbody2/body/request", http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		rec := e.do(e.request(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s %s status = %d, want %d (body %s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}

	// Raw body retrieval requires the same authorization as every admin route.
	secured := newEnv(t, "tok")
	seedLogWithBodies(t, secured.logs, "logbody3")
	if rec := secured.do(secured.request(http.MethodGet, "/api/logs/logbody3/body/request", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated body status = %d, want 401", rec.Code)
	}
	authorized := secured.request(http.MethodGet, "/api/logs/logbody3/body/request", nil)
	authorized.Header.Set("Authorization", "Bearer tok")
	if rec := secured.do(authorized); rec.Code != http.StatusOK {
		t.Fatalf("authorized body status = %d, want 200", rec.Code)
	}
}

// TestLogBodyEvicted verifies that once retention drops an entry its raw body is
// gone too and the detail endpoint reports 404 rather than stale bytes.
func TestLogBodyEvicted(t *testing.T) {
	cs, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	ls := store.NewLogStore(1)
	srv := New(cs, ls, "")
	seedLogWithBodies(t, ls, "evict1")
	ls.Add(store.LogEntry{ID: "evict2", Status: http.StatusOK})

	req := httptest.NewRequest(http.MethodGet, "/api/logs/evict1/body/request", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.Host = "localhost:18851"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("evicted body status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	kept := ls.List(10)
	if len(kept) != 1 || kept[0].ID != "evict2" {
		t.Fatalf("retention after eviction = %+v, want only evict2", kept)
	}
}

// TestConfigPutInvalidKeepsMemoryAndDisk rejects an invalid update and proves
// neither the live store nor the persisted file changed.
func TestConfigPutInvalidKeepsMemoryAndDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cs, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv := New(cs, store.NewLogStore(cs.Get().LogLimit), "")

	before := cs.Get()
	diskBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}

	bad := cs.Get()
	bad.Retry.MaxAttempts = 3
	bad.Retry.BodyRegexes = []string{"("}
	data, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(data))
	r.RemoteAddr = "127.0.0.1:5000"
	r.Host = "localhost:18851"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid PUT status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if after := cs.Get(); !reflect.DeepEqual(before, after) {
		t.Fatal("in-memory config changed after rejected PUT")
	}
	diskAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	if !bytes.Equal(diskBefore, diskAfter) {
		t.Fatal("persisted config changed after rejected PUT")
	}
}

// TestAdminResponsesNoStore verifies management JSON is never cached.
func TestAdminResponsesNoStore(t *testing.T) {
	e := newEnv(t, "")
	for _, path := range []string{"/api/config", "/api/logs", "/api/stats"} {
		rec := e.do(e.request(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("GET %s Cache-Control = %q, want no-store", path, cc)
		}
	}
}
