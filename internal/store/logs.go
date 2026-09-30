// Package store holds bounded request-log metadata, body captures and live
// subscribers. Persistent stores stream body bytes to private files.
package store

import (
	"log"
	"strings"
	"sync"
	"time"
)

// Attempt records a single upstream attempt within a logged request.
type Attempt struct {
	UpstreamID                string              `json:"upstream_id"`
	UpstreamName              string              `json:"upstream_name"`
	URL                       string              `json:"url"`
	Status                    int                 `json:"status"`
	DurationMS                int64               `json:"duration_ms"`
	Error                     string              `json:"error"`
	RetryReason               string              `json:"retry_reason"`
	ResponseBody              string              `json:"response_body"`
	ResponseBodyTruncated     bool                `json:"response_body_truncated"`
	ResponseHeaders           map[string][]string `json:"response_headers"`
	ResponseBodyBytes         int64               `json:"response_body_bytes"`
	ResponseBodyCapturedBytes int64               `json:"response_body_captured_bytes"`
	ResponseBodyComplete      bool                `json:"response_body_complete"`
	ResponseBodyAvailable     bool                `json:"response_body_available"`
	ResponseBodyError         string              `json:"response_body_error,omitempty"`
	RegexSkipped              bool                `json:"regex_skipped"`
}

// LogEntry is one finished request record, possibly with an incomplete response.
type LogEntry struct {
	ID                        string              `json:"id"`
	Time                      string              `json:"time"`
	Method                    string              `json:"method"`
	Path                      string              `json:"path"`
	Query                     string              `json:"query"`
	Model                     string              `json:"model"`
	ClientIP                  string              `json:"client_ip"`
	Status                    int                 `json:"status"`
	DurationMS                int64               `json:"duration_ms"`
	AttemptCount              int                 `json:"attempt_count"`
	Retried                   bool                `json:"retried"`
	UpstreamID                string              `json:"upstream_id"`
	UpstreamName              string              `json:"upstream_name"`
	RequestHeaders            map[string][]string `json:"request_headers"`
	RequestBody               string              `json:"request_body"`
	RequestBodyTruncated      bool                `json:"request_body_truncated"`
	ResponseHeaders           map[string][]string `json:"response_headers"`
	ResponseBody              string              `json:"response_body"`
	ResponseBodyTruncated     bool                `json:"response_body_truncated"`
	Attempts                  []Attempt           `json:"attempts"`
	Streaming                 bool                `json:"streaming"`
	RegexSkipped              bool                `json:"regex_skipped"`
	Incomplete                bool                `json:"incomplete"`
	Error                     string              `json:"error,omitempty"`
	ResponseBodyPart          string              `json:"response_body_part,omitempty"`
	RequestBodyBytes          int64               `json:"request_body_bytes"`
	RequestBodyCapturedBytes  int64               `json:"request_body_captured_bytes"`
	RequestBodyComplete       bool                `json:"request_body_complete"`
	RequestBodyAvailable      bool                `json:"request_body_available"`
	RequestBodyError          string              `json:"request_body_error,omitempty"`
	ResponseBodyBytes         int64               `json:"response_body_bytes"`
	ResponseBodyCapturedBytes int64               `json:"response_body_captured_bytes"`
	ResponseBodyComplete      bool                `json:"response_body_complete"`
	ResponseBodyAvailable     bool                `json:"response_body_available"`
	ResponseBodyError         string              `json:"response_body_error,omitempty"`
}

// Stats are cumulative counters derived from logged requests.
type Stats struct {
	TotalRequests   int64 `json:"total_requests"`
	RetriedRequests int64 `json:"retried_requests"`
	RetrySuccess    int64 `json:"retry_success"`
	FailedRequests  int64 `json:"failed_requests"`
	UptimeSeconds   int64 `json:"uptime_seconds"`
}

// LogStore is a thread-safe ring buffer of the most recent entries with
// non-blocking live subscribers.
type LogStore struct {
	mu    sync.Mutex
	limit int
	buf   []LogEntry
	start int // index of the oldest entry when the ring is full
	n     int // number of valid entries
	subs  map[int]chan LogEntry
	subID int

	// active reservations survive Finish until Add publishes the completed log.
	// bodies contains immutable raw bytes only in memory mode. Disk mode keeps
	// no complete bodies in RAM. owned names only directories created by us or
	// recognized by a valid, matching metadata.json during startup.
	dir    string
	active map[string]map[string]bool
	bodies map[string]map[string][]byte
	owned  map[string]bool

	startTime    time.Time
	total        int64
	retried      int64
	retrySuccess int64
	failed       int64
}

// NewLogStore creates a memory-only store holding up to limit completed entries.
func NewLogStore(limit int) *LogStore {
	if limit <= 0 {
		limit = 50
	}
	return &LogStore{
		limit:     limit,
		buf:       make([]LogEntry, limit),
		subs:      make(map[int]chan LogEntry),
		active:    make(map[string]map[string]bool),
		bodies:    make(map[string]map[string][]byte),
		owned:     make(map[string]bool),
		startTime: time.Now(),
	}
}

// Add appends an entry, evicting the oldest when full, updates counters and
// broadcasts to subscribers without ever blocking on a slow one.
func (s *LogStore) Add(e LogEntry) {
	e = boundedEntry(e)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		if err := s.saveEntryLocked(e); err != nil {
			log.Printf("store: persist log %q: %v", e.ID, err)
		}
	}
	s.pushLocked(e)
	delete(s.active, e.ID)
	s.total++
	if e.AttemptCount > 1 {
		s.retried++
	}
	if e.Status == 0 || e.Status >= 400 || e.Incomplete {
		s.failed++
	} else if e.Status >= 200 && e.Status <= 399 && e.AttemptCount > 1 {
		s.retrySuccess++
	}
	for _, ch := range s.subs {
		if len(ch) == cap(ch) {
			continue
		}
		select {
		case ch <- cloneEntry(e):
		default: // slow subscriber: drop this entry for it
		}
	}
}

func (s *LogStore) pushLocked(e LogEntry) {
	// IDs identify body files too. Replacing an existing ID must not leave an
	// older duplicate that can later evict the replacement's body files.
	if e.ID != "" {
		for i := 0; i < s.n; i++ {
			if s.buf[(s.start+i)%s.limit].ID != e.ID {
				continue
			}
			for j := i; j < s.n-1; j++ {
				s.buf[(s.start+j)%s.limit] = s.buf[(s.start+j+1)%s.limit]
			}
			s.buf[(s.start+s.n-1)%s.limit] = LogEntry{}
			s.n--
			break
		}
	}
	if s.n < s.limit {
		s.buf[(s.start+s.n)%s.limit] = e
		s.n++
		return
	}
	s.removeBodiesLocked(s.buf[s.start].ID)
	s.buf[s.start] = e
	s.start = (s.start + 1) % s.limit
}

// List returns up to max entries, newest first.
func (s *LogStore) List(max int) []LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked(max)
}

func (s *LogStore) listLocked(max int) []LogEntry {
	n := s.n
	if max > 0 && max < n {
		n = max
	}
	out := make([]LogEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, cloneEntry(s.buf[(s.start+s.n-1-i)%s.limit]))
	}
	return out
}

// Get returns a deep snapshot of one completed log entry.
func (s *LogStore) Get(id string) (LogEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.getLocked(id)
	return cloneEntry(e), ok
}

func (s *LogStore) getLocked(id string) (LogEntry, bool) {
	for i := 0; i < s.n; i++ {
		e := s.buf[(s.start+s.n-1-i)%s.limit]
		if e.ID == id {
			return e, true
		}
	}
	return LogEntry{}, false
}

// Count returns the number of stored entries.
func (s *LogStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// Clear drops completed entries and resets counters without touching active bodies.
func (s *LogStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < s.n; i++ {
		s.removeBodiesLocked(s.buf[(s.start+i)%s.limit].ID)
	}
	s.buf = make([]LogEntry, s.limit)
	s.start, s.n = 0, 0
	s.total, s.retried, s.retrySuccess, s.failed = 0, 0, 0, 0
}

// SetLimit resizes the ring, keeping the newest entries.
func (s *LogStore) SetLimit(limit int) {
	if limit <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit == s.limit {
		return
	}
	old := s.listLocked(s.n)
	if len(old) > limit {
		for _, e := range old[limit:] {
			s.removeBodiesLocked(e.ID)
		}
		old = old[:limit]
	}
	s.limit = limit
	s.buf = make([]LogEntry, limit)
	s.start, s.n = 0, 0
	for i := len(old) - 1; i >= 0; i-- { // oldest first
		s.pushLocked(old[i])
	}
}

// Stats returns a snapshot of cumulative counters.
func (s *LogStore) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		TotalRequests:   s.total,
		RetriedRequests: s.retried,
		RetrySuccess:    s.retrySuccess,
		FailedRequests:  s.failed,
		UptimeSeconds:   int64(time.Since(s.startTime).Seconds()),
	}
}

// Subscribe returns a channel receiving every new entry and a cancel func.
func (s *LogStore) Subscribe() (chan LogEntry, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.subID
	s.subID++
	ch := make(chan LogEntry, 64)
	s.subs[id] = ch
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(ch)
		}
	}
	return ch, cancel
}

func cloneHeaders(src map[string][]string) map[string][]string {
	if src == nil {
		return nil
	}
	dst := make(map[string][]string, len(src))
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
	return dst
}

func cloneEntry(e LogEntry) LogEntry {
	e.RequestHeaders = cloneHeaders(e.RequestHeaders)
	e.ResponseHeaders = cloneHeaders(e.ResponseHeaders)
	if e.Attempts != nil {
		e.Attempts = append([]Attempt{}, e.Attempts...)
		for i := range e.Attempts {
			e.Attempts[i].ResponseHeaders = cloneHeaders(e.Attempts[i].ResponseHeaders)
		}
	}
	return e
}

func boundedEntry(e LogEntry) LogEntry {
	e = cloneEntry(e)
	e.RequestBody = boundedPreview(e.RequestBody, &e.RequestBodyTruncated)
	e.ResponseBody = boundedPreview(e.ResponseBody, &e.ResponseBodyTruncated)
	e.RequestBodyTruncated = e.RequestBodyTruncated || int64(len(e.RequestBody)) < e.RequestBodyBytes || e.RequestBodyCapturedBytes < e.RequestBodyBytes || e.RequestBodyError != ""
	e.ResponseBodyTruncated = e.ResponseBodyTruncated || int64(len(e.ResponseBody)) < e.ResponseBodyBytes || e.ResponseBodyCapturedBytes < e.ResponseBodyBytes || e.ResponseBodyError != ""
	for i := range e.Attempts {
		a := &e.Attempts[i]
		a.ResponseBody = boundedPreview(a.ResponseBody, &a.ResponseBodyTruncated)
		a.ResponseBodyTruncated = a.ResponseBodyTruncated || int64(len(a.ResponseBody)) < a.ResponseBodyBytes || a.ResponseBodyCapturedBytes < a.ResponseBodyBytes || a.ResponseBodyError != ""
	}
	return e
}

func boundedPreview(body string, truncated *bool) string {
	if len(body) > bodyPreviewLimit {
		body = body[:bodyPreviewLimit]
		*truncated = true
	}
	// Even a short substring can otherwise retain its caller's huge backing string.
	return strings.Clone(body)
}
