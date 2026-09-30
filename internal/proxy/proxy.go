// Package proxy implements the retrying reverse proxy that sits in front of
// the configured LLM upstreams.
package proxy

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	rand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentrouter/internal/config"
	"agentrouter/internal/store"
)

var (
	errBodyTooLarge     = errors.New("request body too large")
	errBodyDrainTimeout = errors.New("error body drain timeout")
)

const (
	// hopHeader carries this instance's loop marker. Every forwarded request
	// gets a fresh random value; seeing it come back means a routing cycle.
	hopHeader = "X-AgentRouter-Hop"
	// bodyChunkSize bounds the single response reader's buffer and therefore
	// the largest slice handed to the client at once.
	bodyChunkSize = 32 << 10
)

// hopHeaders are stripped from both inbound and outbound messages (RFC 7230).
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

var redactNames = map[string]bool{
	"authorization": true,
	"x-api-key":     true,
	"api-key":       true,
	// Google's provider credential header.
	"x-goog-api-key": true,
	"cookie":         true,
	"set-cookie":     true,
}

// querySecretNames are query parameters that commonly carry a provider
// credential. They are redacted in stored logs only; the upstream request keeps
// the caller's original values.
var querySecretNames = map[string]bool{
	"key":          true,
	"api_key":      true,
	"api-key":      true,
	"x-api-key":    true,
	"access_token": true,
	"apikey":       true,
}

var regexCache sync.Map // pattern string -> globResult (shared shape)

// Proxy is an http.Handler that forwards requests to a weighted upstream and
// transparently retries responses that look like transient upstream failures.
type Proxy struct {
	cfg    *config.Store
	logs   *store.LogStore
	client *http.Client
	hopID  string
}

// New builds a Proxy reading its live configuration from cfg.
func New(cfg *config.Store, logs *store.LogStore) *Proxy {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &Proxy{
		cfg:   cfg,
		logs:  logs,
		hopID: newHopID(),
		client: &http.Client{
			Transport: transport,
			// Upstream redirects are part of the response, never a reason to
			// issue a second upstream request behind the client's back.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// ServeHTTP implements http.Handler. It buffers the request body, then drives
// the retry loop. Nothing is committed to the client until the irreversible
// commit boundary is crossed, after which no further attempt is made.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	cfg := p.cfg.Get()

	if loopDetected(hopChain(r.Header), p.hopID) {
		entry := newEntry(r, cfg, start)
		p.finishGateway(w, entry, cfg, http.StatusLoopDetected, "routing loop detected", start)
		return
	}

	entry := newEntry(r, cfg, start)
	limit := int64(cfg.MaxLogBodyBytes)
	reqRec := p.logs.NewBodyRecorder(entry.ID, "request", limit)
	body, err := readRequestBody(r, cfg.MaxRequestBodyBytes, reqRec)
	_ = r.Body.Close()
	applyRequestInfo(entry, reqRec.Finish(err == nil))

	if err != nil {
		status := http.StatusBadRequest
		message := err.Error()
		if errors.Is(err, errBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
			message = "request body too large"
		}
		p.finishGateway(w, entry, cfg, status, message, start)
		return
	}

	entry.Model = extractModel(body)
	p.forward(w, r, cfg, entry, body, start)
}

func newEntry(r *http.Request, cfg *config.Config, start time.Time) *store.LogEntry {
	return &store.LogEntry{
		ID:             newID(),
		Time:           start.Format(time.RFC3339Nano),
		Method:         r.Method,
		Path:           r.URL.Path,
		Query:          redactQuery(r.URL.RawQuery),
		ClientIP:       clientIP(r),
		RequestHeaders: redactHeaders(r.Header, cfg),
		Attempts:       []store.Attempt{},
	}
}

type outcome int

const (
	outcomeCommitted outcome = iota
	outcomeRetry
	outcomeCanceled
)

// forward runs the attempt loop. It always records exactly one log entry. The
// upstream is resolved once and pinned: a request never switches provider
// between retries, whether it was selected by a dedicated /{id}/... URL or by
// legacy glob+model routing.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, cfg *config.Config, entry *store.LogEntry, body []byte, start time.Time) {
	rt := &cfg.Retry
	limit := int64(cfg.MaxLogBodyBytes)

	decision := resolveRoute(cfg, r, entry.Model)
	if decision.up == nil {
		entry.AttemptCount = 0
		p.finishGateway(w, entry, cfg, decision.status, decision.message, start)
		return
	}
	up := decision.up

	for attempt := 1; attempt <= rt.MaxAttempts; attempt++ {
		if r.Context().Err() != nil {
			p.finishCanceled(entry, start)
			return
		}
		entry.AttemptCount = attempt
		attStart := time.Now()
		att := store.Attempt{UpstreamID: up.ID, UpstreamName: up.Name, URL: redactURLQuery(targetURLPath(up, outboundPath(r, decision.dedicated), r.URL.RawQuery))}
		rec := p.logs.NewBodyRecorder(entry.ID, fmt.Sprintf("attempt-%d", attempt), limit)

		resp, cancel, err := p.doAttempt(r, up, body, decision.dedicated)
		if err != nil {
			att.Error = err.Error()
			att.DurationMS = ms(time.Since(attStart))
			applyAttemptInfo(&att, rec.Finish(false))
			entry.Attempts = append(entry.Attempts, att)
			cancel()

			if r.Context().Err() != nil {
				p.finishCanceled(entry, start)
				return
			}
			if rt.RetryOnNetworkError && attempt < rt.MaxAttempts {
				entry.Attempts[len(entry.Attempts)-1].RetryReason = "network error: " + err.Error()
				if !sleepCtx(r.Context(), backoffDelay(rt, attempt)) {
					p.finishCanceled(entry, start)
					return
				}
				continue
			}
			if errors.Is(err, context.DeadlineExceeded) {
				p.finishGateway(w, entry, cfg, http.StatusGatewayTimeout, "upstream attempt timed out", start)
			} else {
				p.finishGateway(w, entry, cfg, http.StatusBadGateway, err.Error(), start)
			}
			return
		}

		switch p.handleResponse(w, r, cfg, entry, up, resp, cancel, rec, &att, attempt, attStart, start) {
		case outcomeRetry:
			if !sleepCtx(r.Context(), backoffDelay(rt, attempt)) {
				p.finishCanceled(entry, start)
				return
			}
		case outcomeCanceled:
			p.finishCanceled(entry, start)
			return
		default:
			return
		}
	}
}

// handleResponse decides whether the received response can still be retried or
// must be committed. It owns the attempt's reader goroutine and recorder.
func (p *Proxy) handleResponse(
	w http.ResponseWriter, r *http.Request, cfg *config.Config,
	entry *store.LogEntry, up *config.Upstream, resp *http.Response,
	cancel context.CancelFunc, rec *store.BodyRecorder, att *store.Attempt,
	attempt int, attStart, start time.Time,
) outcome {
	rt := &cfg.Retry
	att.Status = resp.StatusCode
	att.ResponseHeaders = redactHeaders(resp.Header, cfg)

	pump := newBodyPump(resp.Body, rec)
	defer func() {
		pump.stop()
		cancel()
	}()

	ct := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	isSSE := strings.HasPrefix(ct, "text/event-stream")
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	compressed := enc != "" && enc != "identity"

	// Retryable status with attempts remaining: drain the failed body under a
	// hard time bound, then retry. The body size never disables the retry.
	statusReason := retryStatusReason(rt, resp.StatusCode)
	if statusReason != "" && attempt < rt.MaxAttempts {
		complete, drainErr := drainBounded(pump, r.Context(), cfg.ErrorBodyTimeoutMS)
		pump.stop()
		info := rec.Finish(complete && drainErr == nil)
		applyAttemptInfo(att, info)
		att.RetryReason = statusReason
		if drainErr != nil {
			att.Error = drainErr.Error()
		}
		att.DurationMS = ms(time.Since(attStart))
		entry.Attempts = append(entry.Attempts, *att)
		if r.Context().Err() != nil {
			return outcomeCanceled
		}
		return outcomeRetry
	}
	// A retryable status on the final attempt is delivered as-is, but the
	// matched reason is still recorded as exhausted.
	if statusReason != "" {
		att.RetryReason = statusReason + " (exhausted)"
	}

	// SSE is committed the moment headers arrive: the first frame must reach the
	// client while the upstream still holds the stream open, and SSE frames are
	// never body-regex material.
	if isSSE {
		p.commitStream(w, r, cfg, entry, up, resp, pump, rec, att, attempt, attStart, start, nil, true, true)
		return outcomeCommitted
	}

	sniffWindow := durationMS(cfg.BodySniffTimeoutMS, config.DefaultBodySniffTimeoutMS)
	deadline := time.Now().Add(sniffWindow)
	maxBuf := cfg.MaxBufferBytes
	if maxBuf <= 0 {
		maxBuf = config.DefaultMaxBufferBytes
	}

	sniffed, complete, readErr := sniffBounded(pump, r.Context(), deadline, maxBuf)

	if r.Context().Err() != nil {
		p.cancelAttempt(entry, att, pump, rec, attStart, r.Context().Err())
		return outcomeCanceled
	}

	// Pre-commit read failure: no usable upstream response.
	if readErr != nil {
		applyAttemptInfo(att, rec.Finish(false))
		att.Error = readErr.Error()
		att.DurationMS = ms(time.Since(attStart))
		entry.Attempts = append(entry.Attempts, *att)
		if rt.RetryOnNetworkError && attempt < rt.MaxAttempts {
			entry.Attempts[len(entry.Attempts)-1].RetryReason = "read error: " + readErr.Error()
			return outcomeRetry
		}
		p.finishGateway(w, entry, cfg, http.StatusBadGateway, readErr.Error(), start)
		return outcomeCommitted
	}

	// Only a body fully read through EOF within the window is eligible for
	// regex retry; a compressed body is never decoded for matching.
	reason := ""
	if complete && !compressed {
		reason = retryReason(rt, resp.StatusCode, sniffed)
	}
	if reason != "" && attempt < rt.MaxAttempts {
		applyAttemptInfo(att, rec.Finish(complete))
		att.RetryReason = reason
		att.DurationMS = ms(time.Since(attStart))
		entry.Attempts = append(entry.Attempts, *att)
		return outcomeRetry
	}
	if reason != "" {
		att.RetryReason = reason + " (exhausted)"
	}

	regexSkipped := compressed || !complete
	if complete {
		p.commitBuffered(w, cfg, entry, up, resp, rec, att, attempt, attStart, start, sniffed, regexSkipped)
		return outcomeCommitted
	}
	// Cap or timeout: commit what we have and stream the remainder, unchanged.
	p.commitStream(w, r, cfg, entry, up, resp, pump, rec, att, attempt, attStart, start, sniffed, regexSkipped, true)
	return outcomeCommitted
}

// commitBuffered writes a complete response and records it. The final response
// aliases the attempt's capture; no second copy is made.
func (p *Proxy) commitBuffered(
	w http.ResponseWriter, cfg *config.Config, entry *store.LogEntry,
	up *config.Upstream, resp *http.Response, rec *store.BodyRecorder,
	att *store.Attempt, attempt int, attStart, start time.Time, body []byte, regexSkipped bool,
) {
	info := rec.Finish(true)
	applyAttemptInfo(att, info)
	att.RegexSkipped = regexSkipped
	att.DurationMS = ms(time.Since(attStart))
	entry.Attempts = append(entry.Attempts, *att)

	entry.UpstreamID, entry.UpstreamName = up.ID, up.Name
	entry.Status = resp.StatusCode
	entry.ResponseHeaders = redactHeaders(resp.Header, cfg)
	entry.ResponseBodyPart = fmt.Sprintf("attempt-%d", attempt)
	entry.RegexSkipped = regexSkipped
	applyResponseInfo(entry, info)
	entry.Retried = entry.AttemptCount > 1
	entry.DurationMS = ms(time.Since(start))

	writeResponseHeaders(w, resp.Header)
	if err := writeBufferedBody(w, resp, body); err != nil {
		entry.Attempts[len(entry.Attempts)-1].Error = err.Error()
		p.abortDelivery(entry, err)
		return
	}
	p.logs.Add(*entry)
}

// commitStream flushes the prefix and then every subsequent chunk as it
// arrives. A post-commit read or write failure marks the log incomplete, then
// aborts the HTTP stream so a clean chunk terminator never lies to the client.
func (p *Proxy) commitStream(
	w http.ResponseWriter, r *http.Request, cfg *config.Config,
	entry *store.LogEntry, up *config.Upstream, resp *http.Response,
	pump *bodyPump, rec *store.BodyRecorder, att *store.Attempt,
	attempt int, attStart, start time.Time, prefix []byte, regexSkipped, streaming bool,
) {
	entry.Streaming = streaming
	entry.RegexSkipped = regexSkipped
	att.RegexSkipped = regexSkipped
	entry.UpstreamID, entry.UpstreamName = up.ID, up.Name
	entry.Status = resp.StatusCode
	entry.ResponseHeaders = redactHeaders(resp.Header, cfg)
	entry.ResponseBodyPart = fmt.Sprintf("attempt-%d", attempt)

	writeResponseHeaders(w, resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Observe every write: a failed or short header flush, prefix write or
	// chunk must surface as an incomplete delivery rather than a clean success.
	var readErr, writeErr error
	if err := flushErr(w); err != nil {
		writeErr = err
	}
	if writeErr == nil && len(prefix) > 0 {
		if err := writeFull(w, prefix); err != nil {
			writeErr = err
		} else if err := flushErr(w); err != nil {
			writeErr = err
		}
	}
	for writeErr == nil {
		data, err, _ := pumpNext(pump, r.Context(), 0)
		if err != nil {
			readErr = err
			break
		}
		if len(data) == 0 {
			continue
		}
		if err := writeFull(w, data); err != nil {
			writeErr = err
			break
		}
		if err := flushErr(w); err != nil {
			writeErr = err
			break
		}
	}

	cleanEOF := readErr == nil || errors.Is(readErr, io.EOF)
	if !cleanEOF || writeErr != nil {
		entry.Incomplete = true
		message := ""
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			message = readErr.Error()
		}
		if writeErr != nil {
			message = writeErr.Error()
		}
		entry.Error = message
		if att.Error == "" {
			att.Error = message
		}
	}

	info := rec.Finish(cleanEOF && writeErr == nil)
	applyAttemptInfo(att, info)
	att.DurationMS = ms(time.Since(attStart))
	entry.Attempts = append(entry.Attempts, *att)
	applyResponseInfo(entry, info)
	entry.Retried = entry.AttemptCount > 1
	entry.DurationMS = ms(time.Since(start))
	p.logs.Add(*entry)

	if entry.Incomplete {
		panic(http.ErrAbortHandler)
	}
}

// finishGateway records and serves an error generated by the proxy itself
// (never by an upstream), capturing the exact bytes written to the client.
func (p *Proxy) finishGateway(w http.ResponseWriter, entry *store.LogEntry, cfg *config.Config, status int, message string, start time.Time) {
	body := gatewayBody(message)
	rec := p.logs.NewBodyRecorder(entry.ID, "response", int64(cfg.MaxLogBodyBytes))
	_, _ = rec.Write(body)
	entry.ResponseBodyPart = "response"
	applyResponseInfo(entry, rec.Finish(true))
	entry.Status = status
	entry.Error = message
	entry.Retried = entry.AttemptCount > 1
	entry.DurationMS = ms(time.Since(start))

	if err := writeErrorResponse(w, status, body); err != nil {
		entry.Incomplete = true
		entry.Error = err.Error()
		p.logs.Add(*entry)
		panic(http.ErrAbortHandler)
	}
	p.logs.Add(*entry)
}

// finishCanceled logs a request whose client disconnected mid-retry; there is
// nobody left to answer.
func (p *Proxy) finishCanceled(entry *store.LogEntry, start time.Time) {
	entry.Status = 499
	entry.Retried = entry.AttemptCount > 1
	entry.DurationMS = ms(time.Since(start))
	p.logs.Add(*entry)
}

// doAttempt issues one upstream request. The returned cancel function must be
// invoked once the response body has been consumed. On a dedicated route the
// request is a pure credential pass-through: configured header overrides are
// ignored, so the caller's own provider key reaches exactly the selected
// upstream. Legacy routes keep applying configured overrides.
func (p *Proxy) doAttempt(r *http.Request, up *config.Upstream, body []byte, dedicated bool) (*http.Response, context.CancelFunc, error) {
	ctx := r.Context()
	cancel := context.CancelFunc(func() {})
	if up.TimeoutMS > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(up.TimeoutMS)*time.Millisecond)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, targetURLPath(up, outboundPath(r, dedicated), r.URL.RawQuery), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	req.Header = outboundHeaders(r.Header, up.Headers, p.hopID, !dedicated)
	// Ask for an identity encoding: retry body regexes must run against plain
	// text, never against gzip/br compressed bytes.
	if base, perr := url.Parse(up.BaseURL); perr == nil && base.Host != "" {
		req.Host = base.Host
	}
	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	return resp, cancel, nil
}

// ---------------------------------------------------------------------------
// response body reader
// ---------------------------------------------------------------------------

// bodyEvent is either a data chunk (err == nil) or a terminal error. The
// channel preserves ordering, so a final chunk is never overtaken by its error.
type bodyEvent struct {
	data []byte
	err  error
}

// bodyPump is the single goroutine reading one response body. It writes every
// observed byte to the recorder and forwards bounded chunks to the consumer.
type bodyPump struct {
	evc  chan bodyEvent
	quit chan struct{}
	done chan struct{}
	body io.ReadCloser
	once sync.Once
}

func newBodyPump(body io.ReadCloser, rec *store.BodyRecorder) *bodyPump {
	p := &bodyPump{
		evc:  make(chan bodyEvent, 1),
		quit: make(chan struct{}),
		done: make(chan struct{}),
		body: body,
	}
	go p.run(rec)
	return p
}

func (p *bodyPump) run(rec *store.BodyRecorder) {
	defer close(p.done)
	defer close(p.evc)
	buf := make([]byte, bodyChunkSize)
	for {
		n, err := p.body.Read(buf)
		if n > 0 {
			if rec != nil {
				_, _ = rec.Write(buf[:n])
			}
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case p.evc <- bodyEvent{data: data}:
			case <-p.quit:
				return
			}
		}
		if err != nil {
			select {
			case p.evc <- bodyEvent{err: err}:
			case <-p.quit:
			}
			return
		}
	}
}

// stop requests termination and waits for the reader goroutine. It is safe to
// call more than once.
func (p *bodyPump) stop() {
	p.once.Do(func() {
		close(p.quit)
		_ = p.body.Close()
	})
	<-p.done
}

// pumpNext waits for one event or the timeout/context. timedOut is true when
// the timer fired first; the caller keeps ownership of the pump.
func pumpNext(p *bodyPump, ctx context.Context, timeout time.Duration) (data []byte, err error, timedOut bool) {
	var tc <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		tc = timer.C
	}
	select {
	case ev, ok := <-p.evc:
		if !ok {
			return nil, io.EOF, false
		}
		if ev.err != nil {
			return nil, ev.err, false
		}
		return ev.data, nil, false
	case <-tc:
		return nil, nil, true
	case <-ctx.Done():
		return nil, ctx.Err(), false
	}
}

// sniffBounded reads the response prefix under a hard deadline. It never waits
// past the deadline: an already-expired or elapsed deadline stops the sniff
// even while the producer keeps delivering data. A deadline or cap stop is not
// an error; the caller commits what it already has.
func sniffBounded(pump *bodyPump, ctx context.Context, deadline time.Time, maxBuf int64) (sniffed []byte, complete bool, readErr error) {
	for int64(len(sniffed)) <= maxBuf {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return sniffed, false, nil
		}
		data, err, timedOut := pumpNext(pump, ctx, remaining)
		if timedOut {
			return sniffed, false, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return sniffed, true, nil
			}
			return sniffed, false, err
		}
		sniffed = append(sniffed, data...)
	}
	return sniffed, false, nil
}

// drainBounded consumes a failed response body up to the timeout (or the
// attempt/client deadline), reporting whether EOF was reached.
func drainBounded(pump *bodyPump, ctx context.Context, timeoutMS int) (bool, error) {
	return drainBoundedUntil(pump, ctx, time.Now().Add(durationMS(timeoutMS, config.DefaultErrorBodyTimeoutMS)))
}

// drainBoundedUntil is drainBounded with an explicit deadline. The remaining
// time is checked before every wait so an elapsed deadline is a prompt timeout
// rather than an unbounded wait for the producer or context.
func drainBoundedUntil(pump *bodyPump, ctx context.Context, deadline time.Time) (complete bool, err error) {
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, errBodyDrainTimeout
		}
		_, rerr, timedOut := pumpNext(pump, ctx, remaining)
		if timedOut {
			return false, errBodyDrainTimeout
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return true, nil
			}
			return false, rerr
		}
	}
}

// ---------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------

// outboundPath is the escaped path forwarded upstream: the identifier segment
// is stripped from a dedicated /{id}/... route, otherwise the path is verbatim.
func outboundPath(r *http.Request, dedicated bool) string {
	escaped := r.URL.EscapedPath()
	if !dedicated {
		return escaped
	}
	return stripFirstEscapedSegment(escaped)
}

// routeDecision is the single, pinned upstream choice for one client request.
// dedicated is true when the request addressed an upstream by /{id}/..., in
// which case configured header overrides are ignored (pure credential
// pass-through) and the identifier segment is stripped from the outbound path.
type routeDecision struct {
	up        *config.Upstream
	status    int
	message   string
	dedicated bool
}

// resolveRoute selects the upstream for r exactly once. A leading segment that
// matches a configured upstream ID is a dedicated provider URL: only that
// upstream is ever used, even when it is disabled (503) or would not match the
// legacy globs/weight. The identifier is unescaped exactly once, so an
// unreserved percent-encoded ID (e.g. %70roviderA) still names its provider; a
// decoded slash or backslash is refused, never treated as a match or allowed to
// smuggle an extra segment. Genuine legacy API roots (api, v1, v1beta, v2) are
// excluded from provider classification. Unknown /{id}/v1... shapes are refused
// with 404 rather than silently falling back, so a dedicated URL never leaks to
// another provider; other paths keep the legacy glob+model routing.
func resolveRoute(cfg *config.Config, r *http.Request, model string) routeDecision {
	seg, rest, ok := firstEscapedSegment(r.URL.EscapedPath())
	if ok && seg != "" {
		id, err := url.PathUnescape(seg)
		if err != nil || strings.ContainsAny(id, `/\`) {
			return routeDecision{status: http.StatusNotFound, message: "unknown provider"}
		}
		if up := lookupUpstream(cfg, id); up != nil {
			if !up.Enabled {
				return routeDecision{status: http.StatusServiceUnavailable, message: "provider is disabled"}
			}
			return routeDecision{up: up, dedicated: true}
		}
		if !isLegacyAPIRoot(id) && looksLikeDedicatedURL(rest) {
			return routeDecision{status: http.StatusNotFound, message: "unknown provider"}
		}
	}
	up, status, message := pickUpstream(cfg, r.URL.Path, model)
	return routeDecision{up: up, status: status, message: message}
}

// isLegacyAPIRoot reports whether a decoded first path segment is a genuine
// reserved API root the router serves through legacy routing. Such a path must
// never be mistaken for an unknown provider-shaped dedicated URL.
func isLegacyAPIRoot(seg string) bool {
	switch seg {
	case "api", "v1", "v1beta", "v2":
		return true
	}
	return false
}

// lookupUpstream finds an upstream by its exact configured ID.
func lookupUpstream(cfg *config.Config, id string) *config.Upstream {
	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].ID == id {
			return &cfg.Upstreams[i]
		}
	}
	return nil
}

// firstEscapedSegment splits the first path segment from an escaped request
// path without decoding it, so an encoded slash or backslash can never smuggle
// an extra segment into identifier matching.
func firstEscapedSegment(escaped string) (seg, rest string, ok bool) {
	if len(escaped) == 0 || escaped[0] != '/' {
		return "", escaped, false
	}
	body := escaped[1:]
	if body == "" {
		return "", "", false
	}
	if i := strings.IndexByte(body, '/'); i >= 0 {
		return body[:i], body[i:], true
	}
	return body, "", true
}

// stripFirstEscapedSegment removes the /{id} prefix from an escaped path. The
// bare and trailing-slash forms both target the upstream root.
func stripFirstEscapedSegment(escaped string) string {
	rest := strings.TrimPrefix(escaped, "/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i:]
	}
	return "/"
}

// looksLikeDedicatedURL reports whether rest names a well-known API version,
// the unmistakable shape of a /{id}/v1... dedicated URL. Such a path with an
// unknown ID is refused instead of falling back to a legacy route.
func looksLikeDedicatedURL(rest string) bool {
	// Decode only for classification; outbound forwarding retains the original
	// escaped path, including encoded separators in the remaining segments.
	rest, err := url.PathUnescape(rest)
	if err != nil || len(rest) == 0 || rest[0] != '/' {
		return false
	}
	seg := strings.TrimLeft(rest, "/")
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	switch seg {
	case "v1", "v1beta", "v2":
		return true
	}
	return false
}

// pickUpstream applies strict path AND model matching. It never falls back to
// an arbitrary upstream: callers get a status describing why no route matched.
func pickUpstream(cfg *config.Config, reqPath, model string) (*config.Upstream, int, string) {
	enabled := 0
	var candidates []*config.Upstream
	for i := range cfg.Upstreams {
		u := &cfg.Upstreams[i]
		if !u.Enabled {
			continue
		}
		enabled++
		if matchAny(u.PathGlobs, reqPath) && matchAny(u.ModelGlobs, model) {
			candidates = append(candidates, u)
		}
	}
	if len(candidates) == 0 {
		if enabled == 0 {
			return nil, http.StatusBadGateway, "no enabled upstream"
		}
		return nil, http.StatusNotFound, "no matching upstream"
	}
	positive := make([]*config.Upstream, 0, len(candidates))
	for _, u := range candidates {
		if u.Weight > 0 {
			positive = append(positive, u)
		}
	}
	if len(positive) == 0 {
		return nil, http.StatusServiceUnavailable, "no upstream with positive weight"
	}
	return weightedPick(positive), 0, ""
}

// matchAny treats an empty glob list as a match (an explicit default route).
func matchAny(globs []string, s string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if globMatch(g, s) {
			return true
		}
	}
	return false
}

// weightedPick chooses by positive weight only; zero-weight candidates are
// never selected. The sum is widened to int64 to avoid overflow.
func weightedPick(candidates []*config.Upstream) *config.Upstream {
	var total int64
	for _, u := range candidates {
		if u.Weight > 0 {
			total += int64(u.Weight)
		}
	}
	if total <= 0 {
		return candidates[0]
	}
	n := rand.Int64N(total)
	for _, u := range candidates {
		if u.Weight <= 0 {
			continue
		}
		n -= int64(u.Weight)
		if n < 0 {
			return u
		}
	}
	return candidates[len(candidates)-1]
}

// retryStatusReason reports a matching status code, or "".
func retryStatusReason(rt *config.Retry, status int) string {
	for _, code := range rt.StatusCodes {
		if code == status {
			return fmt.Sprintf("status %d", status)
		}
	}
	return ""
}

// retryReason returns a non-empty human-readable reason when the response is
// considered retryable by status or (already fully read) body, else "".
func retryReason(rt *config.Retry, status int, body []byte) string {
	if reason := retryStatusReason(rt, status); reason != "" {
		return reason
	}
	for _, pattern := range rt.BodyRegexes {
		re := compiledRegexp(pattern)
		if re != nil && re.Match(body) {
			return fmt.Sprintf("body matched /%s/", pattern)
		}
	}
	return ""
}

func compiledRegexp(pattern string) *regexp.Regexp {
	if v, ok := regexCache.Load(pattern); ok {
		return v.(globResult).re
	}
	re, err := regexp.Compile(pattern)
	var g globResult
	if err != nil {
		g = globResult{err: err}
	} else {
		g = globResult{re: re}
	}
	regexCache.Store(pattern, g)
	return g.re
}

// ---------------------------------------------------------------------------
// timing
// ---------------------------------------------------------------------------

// backoffDelay is min(base * 2^(attempt-1), max), optionally jittered by a
// random factor in [0.75, 1.25].
func backoffDelay(rt *config.Retry, attempt int) time.Duration {
	shift := attempt - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 20 {
		shift = 20
	}
	d := time.Duration(rt.BaseDelayMS) * time.Millisecond * time.Duration(1<<shift)
	maxDelay := time.Duration(rt.MaxDelayMS) * time.Millisecond
	if d > maxDelay || d < 0 {
		d = maxDelay
	}
	if rt.Jitter {
		d = time.Duration(float64(d) * (0.75 + rand.Float64()*0.5))
	}
	return d
}

// sleepCtx waits for d or the context, reporting false if the context ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func durationMS(v, def int) time.Duration {
	if v <= 0 {
		v = def
	}
	return time.Duration(v) * time.Millisecond
}

// ---------------------------------------------------------------------------
// request/response helpers
// ---------------------------------------------------------------------------

// readRequestBody reads the whole body, rejecting anything over max bytes and
// teeing every observed byte into the recorder (nil body is safe).
func readRequestBody(r *http.Request, max int64, rec *store.BodyRecorder) ([]byte, error) {
	if max <= 0 {
		max = config.DefaultMaxRequestBodyBytes
	}
	if r.Body == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	limited := io.LimitReader(r.Body, max+1)
	tmp := make([]byte, bodyChunkSize)
	for {
		n, err := limited.Read(tmp)
		if n > 0 {
			if rec != nil {
				_, _ = rec.Write(tmp[:n])
			}
			buf.Write(tmp[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return buf.Bytes(), err
		}
	}
	if int64(buf.Len()) > max {
		return buf.Bytes(), errBodyTooLarge
	}
	return buf.Bytes(), nil
}

func applyRequestInfo(entry *store.LogEntry, info store.BodyInfo) {
	entry.RequestBody = info.Preview
	entry.RequestBodyTruncated = info.Truncated
	entry.RequestBodyBytes = info.TotalBytes
	entry.RequestBodyCapturedBytes = info.CapturedBytes
	entry.RequestBodyComplete = info.Complete
	entry.RequestBodyAvailable = info.Available
	entry.RequestBodyError = info.Error
}

func applyResponseInfo(entry *store.LogEntry, info store.BodyInfo) {
	entry.ResponseBody = info.Preview
	entry.ResponseBodyTruncated = info.Truncated
	entry.ResponseBodyBytes = info.TotalBytes
	entry.ResponseBodyCapturedBytes = info.CapturedBytes
	entry.ResponseBodyComplete = info.Complete
	entry.ResponseBodyAvailable = info.Available
	entry.ResponseBodyError = info.Error
}

func applyAttemptInfo(att *store.Attempt, info store.BodyInfo) {
	att.ResponseBody = info.Preview
	att.ResponseBodyTruncated = info.Truncated
	att.ResponseBodyBytes = info.TotalBytes
	att.ResponseBodyCapturedBytes = info.CapturedBytes
	att.ResponseBodyComplete = info.Complete
	att.ResponseBodyAvailable = info.Available
	att.ResponseBodyError = info.Error
}

// maxHopChain bounds the preserved loop-marker chain so a pathological route
// cannot grow the header without limit; exceeding it is itself a loop.
const maxHopChain = 16

// hopChain splits the inbound loop-marker header into its ordered chain,
// accepting both repeated headers and comma-joined values.
func hopChain(h http.Header) []string {
	var chain []string
	for _, value := range h.Values(hopHeader) {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				chain = append(chain, part)
			}
		}
	}
	return chain
}

// loopDetected reports whether chain already contains our own marker (a cycle
// back to this instance) or has grown beyond any plausible route.
func loopDetected(chain []string, self string) bool {
	if len(chain) > maxHopChain {
		return true
	}
	for _, marker := range chain {
		if marker == self {
			return true
		}
	}
	return false
}

// appendHop appends our marker to the preserved chain, keeping it bounded.
func appendHop(chain []string, self string) string {
	chain = append(chain, self)
	if len(chain) > maxHopChain {
		chain = chain[len(chain)-maxHopChain:]
	}
	return strings.Join(chain, ",")
}

// outboundHeaders clones client headers, optionally applies the upstream
// overrides, then strips hop-by-hop headers so a configured override cannot
// smuggle them. The loop-marker chain is appended to (never replaced) and set
// last, so both a configured alias and an inbound chain can never erase this
// instance's mark: a cycle A->B->A is detected when the chain comes back to A.
// On dedicated routes applyOverrides is false: the caller's own provider
// credentials pass through untouched.
func outboundHeaders(src http.Header, extra map[string]string, hopID string, applyOverrides bool) http.Header {
	chain := appendHop(hopChain(src), hopID)
	dst := src.Clone()
	if applyOverrides {
		for k, v := range extra {
			dst.Set(k, v)
		}
	}
	stripHopByHop(dst)
	dst.Set("Accept-Encoding", "identity")
	dst.Set(hopHeader, chain)
	return dst
}

func stripHopByHop(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

func writeResponseHeaders(w http.ResponseWriter, h http.Header) {
	dst := w.Header()
	for k, values := range h {
		dst[k] = append([]string(nil), values...)
	}
	stripHopByHop(dst)
}

// writeFull writes all of p, converting a short write into io.ErrShortWrite so
// a truncated response is never mistaken for a complete one.
func writeFull(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// flushErr flushes buffered response bytes, surfacing a write error where the
// ResponseWriter supports flushing. An unsupported writer is not an error.
func flushErr(w http.ResponseWriter) error {
	err := http.NewResponseController(w).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// abortDelivery publishes a log whose response decision was already committed
// to the wire but whose delivery failed, then aborts the HTTP stream so the
// client never mistakes a truncation for a clean terminator. Upstream capture
// completeness is deliberately independent: a fully captured body can still be
// recorded as an incomplete delivery.
func (p *Proxy) abortDelivery(entry *store.LogEntry, err error) {
	entry.Incomplete = true
	entry.Error = err.Error()
	p.logs.Add(*entry)
	panic(http.ErrAbortHandler)
}

// cancelAttempt stops the attempt's body reader, then freezes and records its
// capture. The reader goroutine has exited before Finish, so no late write can
// race the frozen result. Any partial bytes stay available for download.
func (p *Proxy) cancelAttempt(entry *store.LogEntry, att *store.Attempt, pump *bodyPump, rec *store.BodyRecorder, attStart time.Time, cause error) {
	pump.stop()
	info := rec.Finish(false)
	applyAttemptInfo(att, info)
	att.Error = cause.Error()
	att.DurationMS = ms(time.Since(attStart))
	entry.Attempts = append(entry.Attempts, *att)
	entry.Incomplete = true
	entry.Error = cause.Error()
}

// writeBufferedBody writes a complete body with correct framing and reports any
// write failure. HEAD and bodyless statuses never gain a forced
// Content-Length: 0, so an upstream representation length survives.
func writeBufferedBody(w http.ResponseWriter, resp *http.Response, body []byte) error {
	if bodylessResponse(resp) {
		w.WriteHeader(resp.StatusCode)
		return nil
	}
	if resp.Header.Get("Content-Length") == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(resp.StatusCode)
	if len(body) == 0 {
		return nil
	}
	return writeFull(w, body)
}

// writeErrorResponse writes a pre-encoded JSON error body, reporting any write
// failure so a generated error is never logged as delivered.
func writeErrorResponse(w http.ResponseWriter, status int, body []byte) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return writeFull(w, body)
}

func bodylessResponse(resp *http.Response) bool {
	if resp.Request != nil && resp.Request.Method == http.MethodHead {
		return true
	}
	switch resp.StatusCode {
	case http.StatusContinue, http.StatusNoContent, http.StatusNotModified:
		return true
	}
	return false
}

// targetURL joins the upstream base URL with the original escaped path and
// query, preserving a base path and the base query. No double-escaping occurs
// because the joined raw path is unescaped exactly once for url.URL.
func targetURL(up *config.Upstream, r *http.Request) string {
	return targetURLPath(up, r.URL.EscapedPath(), r.URL.RawQuery)
}

// targetURLPath is targetURL with an explicit escaped path (used by dedicated
// routes, whose identifier segment has already been stripped) and raw query.
func targetURLPath(up *config.Upstream, reqPath, rawQuery string) string {
	base, err := url.Parse(up.BaseURL)
	if err != nil || base.Host == "" {
		target := strings.TrimRight(up.BaseURL, "/")
		if reqPath == "" {
			reqPath = "/"
		}
		target += reqPath
		if rawQuery != "" {
			target += "?" + rawQuery
		}
		return target
	}
	basePath := strings.TrimRight(base.EscapedPath(), "/")
	if reqPath == "" {
		reqPath = "/"
	}
	rawPath := basePath + reqPath
	u := *base
	if decoded, derr := url.PathUnescape(rawPath); derr == nil {
		u.Path = decoded
		u.RawPath = rawPath
	} else {
		u.Path = rawPath
		u.RawPath = ""
	}
	u.RawQuery = mergeQuery(base.RawQuery, rawQuery)
	u.ForceQuery = false
	return u.String()
}

func mergeQuery(base, req string) string {
	switch {
	case base == "":
		return req
	case req == "":
		return base
	default:
		return base + "&" + req
	}
}

func extractModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.Model
}

// redactHeaders masks common credentials and every header name configured on
// any upstream. The forwarded request is never touched.
func redactHeaders(h http.Header, cfg *config.Config) map[string][]string {
	configured := map[string]bool{}
	if cfg != nil {
		for i := range cfg.Upstreams {
			for name := range cfg.Upstreams[i].Headers {
				configured[strings.ToLower(name)] = true
			}
		}
	}
	out := make(map[string][]string, len(h))
	for k, v := range h {
		lk := strings.ToLower(k)
		if redactNames[lk] || configured[lk] {
			out[k] = []string{"***"}
			continue
		}
		out[k] = append([]string(nil), v...)
	}
	return out
}

// redactQuery masks known credential parameters in a raw query for storage,
// preserving parameter order and every non-credential value.
func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		name := part
		if j := strings.IndexByte(name, '='); j >= 0 {
			name = name[:j]
		}
		if querySecretNames[strings.ToLower(name)] {
			parts[i] = name + "=***"
		}
	}
	return strings.Join(parts, "&")
}

// redactURLQuery applies redactQuery to the query portion of a logged URL.
func redactURLQuery(rawURL string) string {
	i := strings.IndexByte(rawURL, '?')
	if i < 0 {
		return rawURL
	}
	return rawURL[:i+1] + redactQuery(rawURL[i+1:])
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func newID() string {
	b := make([]byte, 8)
	if _, err := crand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}

func newHopID() string {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// gatewayBody is the exact byte sequence writeJSON emits for an error message.
func gatewayBody(message string) []byte {
	b, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		return []byte("{}\n")
	}
	return append(b, '\n')
}

func ms(d time.Duration) int64 {
	return d.Milliseconds()
}
