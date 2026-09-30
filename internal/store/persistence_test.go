package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustPersistent(t *testing.T, limit int, dir string) *LogStore {
	t.Helper()
	s, err := NewPersistentLogStore(limit, dir)
	if err != nil {
		t.Fatalf("NewPersistentLogStore: %v", err)
	}
	return s
}

// writeChunked feeds data in several writes so preview/capture boundaries are
// exercised rather than assumed.
func writeChunked(t *testing.T, w io.Writer, data []byte) {
	t.Helper()
	const chunk = 7 << 10
	for len(data) > 0 {
		n := chunk
		if n > len(data) {
			n = len(data)
		}
		nw, err := w.Write(data[:n])
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if nw != n {
			t.Fatalf("short write %d/%d", nw, n)
		}
		data = data[n:]
	}
}

// The mapping helpers mirror how the proxy condenses a BodyInfo onto an entry.
func mapRequestInfo(e *LogEntry, info BodyInfo) {
	e.RequestBody = info.Preview
	e.RequestBodyTruncated = info.Truncated
	e.RequestBodyBytes = info.TotalBytes
	e.RequestBodyCapturedBytes = info.CapturedBytes
	e.RequestBodyComplete = info.Complete
	e.RequestBodyAvailable = info.Available
	e.RequestBodyError = info.Error
}

func mapResponseInfo(e *LogEntry, info BodyInfo) {
	e.ResponseBody = info.Preview
	e.ResponseBodyTruncated = info.Truncated
	e.ResponseBodyBytes = info.TotalBytes
	e.ResponseBodyCapturedBytes = info.CapturedBytes
	e.ResponseBodyComplete = info.Complete
	e.ResponseBodyAvailable = info.Available
	e.ResponseBodyError = info.Error
}

func mapAttemptInfo(a *Attempt, info BodyInfo) {
	a.ResponseBody = info.Preview
	a.ResponseBodyTruncated = info.Truncated
	a.ResponseBodyBytes = info.TotalBytes
	a.ResponseBodyCapturedBytes = info.CapturedBytes
	a.ResponseBodyComplete = info.Complete
	a.ResponseBodyAvailable = info.Available
	a.ResponseBodyError = info.Error
}

func openAll(t *testing.T, s *LogStore, id, part string) []byte {
	t.Helper()
	rc, size, err := s.OpenBody(id, part)
	if err != nil {
		t.Fatalf("OpenBody(%s,%s): %v", id, part, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s/%s: %v", id, part, err)
	}
	if int64(len(data)) != size {
		t.Fatalf("OpenBody(%s,%s) size=%d but read %d bytes", id, part, size, len(data))
	}
	return data
}

func assertBodyEqual(t *testing.T, s *LogStore, id, part string, want []byte) {
	t.Helper()
	got := openAll(t, s, id, part)
	if !bytes.Equal(got, want) {
		t.Errorf("body %s/%s mismatch: got %d bytes sha=%x want %d bytes sha=%x",
			id, part, len(got), sha256.Sum256(got), len(want), sha256.Sum256(want))
	}
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s perm=%o want %o", path, got, want)
	}
}

// ---------------------------------------------------------------------------
// 1. full-capture round trip across a reopen
// ---------------------------------------------------------------------------

func TestPersistentFullCaptureRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := mustPersistent(t, 50, dir)
	id := "req-0001"

	reqData := bytes.Repeat([]byte("request-body-"), 1<<17) // ~1.7 MiB
	rr := s.NewBodyRecorder(id, "request", 0)
	writeChunked(t, rr, reqData)
	reqInfo := rr.Finish(true)
	if reqInfo.Error != "" {
		t.Fatalf("request capture error: %s", reqInfo.Error)
	}
	if reqInfo.TotalBytes != int64(len(reqData)) || reqInfo.CapturedBytes != int64(len(reqData)) {
		t.Errorf("request bytes total=%d captured=%d want %d/%d",
			reqInfo.TotalBytes, reqInfo.CapturedBytes, len(reqData), len(reqData))
	}
	if !reqInfo.Complete || !reqInfo.Available {
		t.Errorf("request complete=%v available=%v want true/true", reqInfo.Complete, reqInfo.Available)
	}
	if !reqInfo.Truncated {
		t.Error("a >32KiB body must have a truncated preview")
	}
	if len(reqInfo.Preview) != bodyPreviewLimit {
		t.Errorf("request preview=%d bytes want preview cap %d", len(reqInfo.Preview), bodyPreviewLimit)
	}

	attempt1 := bytes.Repeat([]byte("failed-attempt-"), 50000)
	r1 := s.NewBodyRecorder(id, "attempt-1", 0)
	writeChunked(t, r1, attempt1)
	i1 := r1.Finish(true)

	attempt2 := bytes.Repeat([]byte("success-attempt-"), 40000)
	r2 := s.NewBodyRecorder(id, "attempt-2", 0)
	writeChunked(t, r2, attempt2)
	i2 := r2.Finish(true)
	if i2.Error != "" || !i2.Complete {
		t.Fatalf("attempt-2 capture error=%q complete=%v", i2.Error, i2.Complete)
	}

	e := LogEntry{
		ID:                id,
		Time:              time.Now().Format(time.RFC3339Nano),
		Method:            "POST",
		Path:              "/v1/chat/completions",
		Status:            200,
		AttemptCount:     2,
		Retried:          true,
		ResponseBodyPart: "attempt-2",
	}
	mapRequestInfo(&e, reqInfo)
	mapResponseInfo(&e, i2)
	var a1, a2 Attempt
	a1.UpstreamID, a1.Status = "u1", 429
	mapAttemptInfo(&a1, i1)
	a2.UpstreamID, a2.Status = "u2", 200
	mapAttemptInfo(&a2, i2)
	e.Attempts = []Attempt{a1, a2}
	s.Add(e)

	got, ok := s.Get(id)
	if !ok {
		t.Fatal("Get after Add: not found")
	}
	if got.RequestBodyBytes != int64(len(reqData)) || !got.RequestBodyComplete || !got.RequestBodyAvailable {
		t.Errorf("stored request bytes=%d complete=%v available=%v", got.RequestBodyBytes, got.RequestBodyComplete, got.RequestBodyAvailable)
	}
	if got.ResponseBodyPart != "attempt-2" {
		t.Errorf("ResponseBodyPart=%q want attempt-2", got.ResponseBodyPart)
	}
	if got.ResponseBodyBytes != int64(len(attempt2)) || !got.ResponseBodyComplete || !got.ResponseBodyAvailable {
		t.Errorf("stored response bytes=%d complete=%v available=%v", got.ResponseBodyBytes, got.ResponseBodyComplete, got.ResponseBodyAvailable)
	}
	if len(got.Attempts) != 2 || got.Attempts[1].ResponseBodyBytes != int64(len(attempt2)) {
		t.Errorf("stored attempts=%d, attempt-2 bytes=%d", len(got.Attempts), got.Attempts[1].ResponseBodyBytes)
	}

	assertBodyEqual(t, s, id, "request", reqData)
	assertBodyEqual(t, s, id, "attempt-1", attempt1)
	assertBodyEqual(t, s, id, "attempt-2", attempt2)
	assertBodyEqual(t, s, id, "response", attempt2) // final alias

	// Reopen the same directory: metadata and full bytes must be recovered.
	s2 := mustPersistent(t, 50, dir)
	got2, ok := s2.Get(id)
	if !ok {
		t.Fatal("reopen: entry not recovered")
	}
	if got2.RequestBodyBytes != int64(len(reqData)) || !got2.RequestBodyComplete || !got2.RequestBodyAvailable {
		t.Errorf("recovered request bytes=%d complete=%v available=%v", got2.RequestBodyBytes, got2.RequestBodyComplete, got2.RequestBodyAvailable)
	}
	if got2.ResponseBodyPart != "attempt-2" || got2.ResponseBodyBytes != int64(len(attempt2)) {
		t.Errorf("recovered response part=%q bytes=%d", got2.ResponseBodyPart, got2.ResponseBodyBytes)
	}
	assertBodyEqual(t, s2, id, "request", reqData)
	assertBodyEqual(t, s2, id, "attempt-1", attempt1)
	assertBodyEqual(t, s2, id, "attempt-2", attempt2)
	assertBodyEqual(t, s2, id, "response", attempt2)
}

// ---------------------------------------------------------------------------
// 2. retention, eviction and active-capture protection
// ---------------------------------------------------------------------------

func TestPersistentRetentionEvictsBodiesAndProtectsActive(t *testing.T) {
	dir := t.TempDir()
	s := mustPersistent(t, 50, dir)

	// Start an active capture before the ring fills; publish it only later.
	keepID := "keep-active"
	kr := s.NewBodyRecorder(keepID, "request", 0)
	keepData := bytes.Repeat([]byte("keep-"), 20000)
	writeChunked(t, kr, keepData)

	for i := 0; i < 51; i++ {
		id := fmt.Sprintf("complete-%03d", i)
		r := s.NewBodyRecorder(id, "request", 0)
		writeChunked(t, r, []byte("body-"+id))
		info := r.Finish(true)
		e := LogEntry{ID: id, Time: time.Now().Format(time.RFC3339Nano), Status: 200}
		mapRequestInfo(&e, info)
		s.Add(e)
	}

	if got := s.Count(); got != 50 {
		t.Fatalf("Count=%d want 50", got)
	}
	if _, ok := s.Get("complete-000"); ok {
		t.Error("oldest completed entry should have been evicted")
	}
	if _, err := os.Stat(filepath.Join(dir, "complete-000")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("evicted entry body dir still present: %v", err)
	}
	if _, ok := s.Get("complete-050"); !ok {
		t.Error("newest completed entry missing")
	}

	kinfo := kr.Finish(true)
	if kinfo.Error != "" || !kinfo.Complete {
		t.Fatalf("active capture error=%q complete=%v", kinfo.Error, kinfo.Complete)
	}
	ke := LogEntry{ID: keepID, Time: time.Now().Format(time.RFC3339Nano), Status: 200}
	mapRequestInfo(&ke, kinfo)
	s.Add(ke)

	if _, ok := s.Get(keepID); !ok {
		t.Fatal("active capture was lost when published")
	}
	assertBodyEqual(t, s, keepID, "request", keepData)
}

func TestClearAndSetLimitPreserveActiveCapture(t *testing.T) {
	dir := t.TempDir()
	s := mustPersistent(t, 10, dir)

	activeID := "active-survivor"
	ar := s.NewBodyRecorder(activeID, "request", 0)
	activeData := []byte("active-body-must-survive-clear-and-resize")
	writeChunked(t, ar, activeData)

	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("done-%02d", i)
		r := s.NewBodyRecorder(id, "request", 0)
		writeChunked(t, r, []byte(id))
		info := r.Finish(true)
		e := LogEntry{ID: id, Time: time.Now().Format(time.RFC3339Nano), Status: 200}
		mapRequestInfo(&e, info)
		s.Add(e)
	}

	s.Clear()
	if got := s.Count(); got != 0 {
		t.Fatalf("Count after Clear=%d want 0", got)
	}
	if _, err := os.Stat(filepath.Join(dir, activeID, "request.body")); err != nil {
		t.Fatalf("active body removed by Clear: %v", err)
	}
	info := ar.Finish(true)
	e := LogEntry{ID: activeID, Time: time.Now().Format(time.RFC3339Nano), Status: 200}
	mapRequestInfo(&e, info)
	s.Add(e)
	assertBodyEqual(t, s, activeID, "request", activeData)

	// SetLimit must likewise leave an active capture alone.
	active2 := "active-resize"
	ar2 := s.NewBodyRecorder(active2, "request", 0)
	active2Data := []byte("survives-setlimit")
	writeChunked(t, ar2, active2Data)
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("more-%02d", i)
		r := s.NewBodyRecorder(id, "request", 0)
		writeChunked(t, r, []byte(id))
		finfo := r.Finish(true)
		me := LogEntry{ID: id, Time: time.Now().Format(time.RFC3339Nano), Status: 200}
		mapRequestInfo(&me, finfo)
		s.Add(me)
	}
	s.SetLimit(2)
	if got := s.Count(); got != 2 {
		t.Fatalf("Count after SetLimit(2)=%d want 2", got)
	}
	if _, err := os.Stat(filepath.Join(dir, active2, "request.body")); err != nil {
		t.Fatalf("active body removed by SetLimit: %v", err)
	}
	info2 := ar2.Finish(true)
	e2 := LogEntry{ID: active2, Time: time.Now().Format(time.RFC3339Nano), Status: 200}
	mapRequestInfo(&e2, info2)
	s.Add(e2)
	assertBodyEqual(t, s, active2, "request", active2Data)
}

// ---------------------------------------------------------------------------
// 3. explicit cap, partial capture, capture failure, invalid input
// ---------------------------------------------------------------------------

func TestExplicitCapAndPartialCapture(t *testing.T) {
	dir := t.TempDir()
	s := mustPersistent(t, 50, dir)
	capBytes := int64(1024)

	data := bytes.Repeat([]byte("X"), 2<<20)
	rr := s.NewBodyRecorder("cap-1", "request", capBytes)
	n, err := rr.Write(data)
	if n != len(data) || err != nil {
		t.Fatalf("Write returned (%d,%v) want (%d,nil)", n, err, len(data))
	}
	info := rr.Finish(true)
	if info.TotalBytes != int64(len(data)) {
		t.Errorf("TotalBytes=%d want full %d", info.TotalBytes, len(data))
	}
	if info.CapturedBytes != capBytes {
		t.Errorf("CapturedBytes=%d want cap %d", info.CapturedBytes, capBytes)
	}
	if info.Complete {
		t.Error("a capped capture must not be complete")
	}
	if !info.Truncated {
		t.Error("a capped capture must be truncated")
	}
	if !info.Available {
		t.Error("a capped capture must remain readable")
	}
	got := openAll(t, s, "cap-1", "request")
	if int64(len(got)) != capBytes || !bytes.Equal(got, data[:capBytes]) {
		t.Errorf("capped body mismatch: got %d bytes want exact %d-byte prefix", len(got), capBytes)
	}

	// Finish(false): all observed bytes captured, but not complete.
	pr := s.NewBodyRecorder("part-1", "request", 0)
	part := []byte("partial-capture-bytes")
	writeChunked(t, pr, part)
	pinfo := pr.Finish(false)
	if pinfo.Complete {
		t.Error("Finish(false) must not claim complete")
	}
	if !pinfo.Available || pinfo.CapturedBytes != int64(len(part)) || pinfo.Error != "" {
		t.Errorf("partial info available=%v captured=%d error=%q", pinfo.Available, pinfo.CapturedBytes, pinfo.Error)
	}
	assertBodyEqual(t, s, "part-1", "request", part)
}

func TestDiskCaptureFailureReturnsSuccessButRecordsError(t *testing.T) {
	dir := t.TempDir()
	s := mustPersistent(t, 50, dir)
	id := "fail-1"

	// Obstruct the per-request body path with a directory so the body file
	// cannot be opened. Ownership is granted so the failing step is the body
	// open (not directory creation), keeping the failure deterministic.
	if err := os.MkdirAll(filepath.Join(dir, id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, id, "request.body"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.owned[id] = true

	rr := s.NewBodyRecorder(id, "request", 0)
	payload := []byte("hello")
	n, err := rr.Write(payload)
	if n != len(payload) || err != nil {
		t.Fatalf("Write returned (%d,%v) want (%d,nil): logging must never fail the caller", n, err, len(payload))
	}
	info := rr.Finish(true)
	if info.Error == "" {
		t.Error("capture failure must be visible in Info.Error")
	}
	if info.Complete {
		t.Error("failed capture must not be complete")
	}
	if info.Available {
		t.Error("failed capture must not be available")
	}
	if info.TotalBytes != int64(len(payload)) {
		t.Errorf("TotalBytes=%d want observed %d", info.TotalBytes, len(payload))
	}
	if info.CapturedBytes != 0 {
		t.Errorf("CapturedBytes=%d want 0", info.CapturedBytes)
	}
}

func TestRecorderRejectsTraversalAndUnknownBodies(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "logs")
	s := mustPersistent(t, 50, root)

	for _, tc := range []struct{ id, part string }{
		{"../escape", "request"},
		{"okid", "../evil"},
		{"okid", "attempt-0"},
		{"", "request"},
	} {
		b := s.NewBodyRecorder(tc.id, tc.part, 0)
		if info := b.Finish(true); info.Error == "" {
			t.Errorf("recorder(%q,%q) accepted invalid input", tc.id, tc.part)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("path traversal created data outside the store: %v", err)
	}

	for _, tc := range []struct{ id, part string }{
		{"../escape", "request"},
		{"unknown", "request"},
		{"okid", "../evil"},
		{"okid", "attempt-0"},
	} {
		if _, _, err := s.OpenBody(tc.id, tc.part); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("OpenBody(%q,%q) err=%v want ErrNotExist", tc.id, tc.part, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. permissions
// ---------------------------------------------------------------------------

func TestPersistentPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	s := mustPersistent(t, 50, root)
	assertPerm(t, root, 0o700)

	id := "perm-1"
	r := s.NewBodyRecorder(id, "request", 0)
	writeChunked(t, r, []byte("perm-body"))
	info := r.Finish(true)
	if info.Error != "" {
		t.Fatalf("capture: %s", info.Error)
	}
	assertPerm(t, filepath.Join(root, id), 0o700)
	assertPerm(t, filepath.Join(root, id, "request.body"), 0o600)

	e := LogEntry{ID: id, Time: time.Now().Format(time.RFC3339Nano), Status: 200}
	mapRequestInfo(&e, info)
	s.Add(e)
	assertPerm(t, filepath.Join(root, id, "metadata.json"), 0o600)
}

// ---------------------------------------------------------------------------
// 5. snapshot isolation and stats semantics (memory store)
// ---------------------------------------------------------------------------

func TestGetAndSubscriberSnapshotsAreIsolated(t *testing.T) {
	s := NewLogStore(10)
	s.Add(LogEntry{
		ID:             "a",
		Status:         200,
		RequestHeaders: map[string][]string{"X-Test": {"1"}},
		Attempts:       []Attempt{{UpstreamID: "u1"}},
	})
	got, ok := s.Get("a")
	if !ok {
		t.Fatal("Get(a) not found")
	}
	got.RequestHeaders["X-Test"][0] = "mutated"
	got.RequestHeaders["Injected"] = []string{"x"}
	got.Attempts[0].UpstreamID = "mutated"

	again, _ := s.Get("a")
	if again.RequestHeaders["X-Test"][0] != "1" {
		t.Error("Get header values alias store memory")
	}
	if _, ok := again.RequestHeaders["Injected"]; ok {
		t.Error("Get header map aliases store memory")
	}
	if again.Attempts[0].UpstreamID != "u1" {
		t.Error("Get attempts slice aliases store memory")
	}

	ch, cancel := s.Subscribe()
	defer cancel()
	s.Add(LogEntry{ID: "b", Status: 200, RequestHeaders: map[string][]string{"X-Test": {"1"}}})
	var ev LogEntry
	select {
	case ev = <-ch:
	case <-time.After(time.Second):
		t.Fatal("no subscriber event for b")
	}
	ev.RequestHeaders["X-Test"][0] = "mutated"
	stored, _ := s.Get("b")
	if stored.RequestHeaders["X-Test"][0] != "1" {
		t.Error("subscriber event aliases store memory")
	}
}

func TestStatsZeroAndIncompleteAreFailures(t *testing.T) {
	s := NewLogStore(10)
	s.Add(LogEntry{ID: "zero", Status: 0})
	s.Add(LogEntry{ID: "incomplete", Status: 200, Incomplete: true})
	s.Add(LogEntry{ID: "ok", Status: 200})
	s.Add(LogEntry{ID: "retried", Status: 200, AttemptCount: 2})

	st := s.Stats()
	if st.TotalRequests != 4 {
		t.Errorf("TotalRequests=%d want 4", st.TotalRequests)
	}
	if st.FailedRequests != 2 {
		t.Errorf("FailedRequests=%d want 2 (status 0 and incomplete)", st.FailedRequests)
	}
	if st.RetriedRequests != 1 {
		t.Errorf("RetriedRequests=%d want 1", st.RetriedRequests)
	}
	if st.RetrySuccess != 1 {
		t.Errorf("RetrySuccess=%d want 1", st.RetrySuccess)
	}
}

// A bounded concurrent persistent workload must stay race-free and never exceed
// the ring limit.
func TestConcurrentPersistentAddAndEviction(t *testing.T) {
	dir := t.TempDir()
	s := mustPersistent(t, 8, dir)

	var wg sync.WaitGroup
	var seq int64
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				id := fmt.Sprintf("c-%d-%d", atomic.AddInt64(&seq, 1), i)
				r := s.NewBodyRecorder(id, "request", 0)
				_, _ = r.Write([]byte(id))
				info := r.Finish(true)
				e := LogEntry{ID: id, Time: time.Now().Format(time.RFC3339Nano), Status: 200}
				mapRequestInfo(&e, info)
				s.Add(e)
				_ = s.List(5)
			}
		}()
	}
	wg.Wait()

	if got := s.Count(); got > 8 {
		t.Errorf("Count=%d exceeds limit 8", got)
	}
}
