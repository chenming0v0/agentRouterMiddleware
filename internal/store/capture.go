package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const bodyPreviewLimit = 32 << 10

// BodyInfo separates a bounded display preview from the raw captured body.
// Complete describes capture through EOF, not delivery to the client.
type BodyInfo struct {
	Preview       string
	TotalBytes    int64
	CapturedBytes int64
	Truncated     bool // Preview is shorter than observed bytes, or capture failed.
	Complete      bool // EOF and every observed byte captured without an I/O error.
	Available     bool // OpenBody can read the captured bytes, possibly partial/empty.
	Error         string
}

// BodyRecorder observes a body without propagating logging errors to its caller.
// Call Finish before publishing the request with Add. A zero limit captures all
// bytes; a positive limit is an explicit capture cap, independent of the preview.
type BodyRecorder struct {
	mu       sync.Mutex
	store    *LogStore
	id       string
	part     string
	limit    int64
	file     *os.File
	data     []byte // Only used by memory-mode stores.
	preview  []byte
	total    int64
	captured int64
	err      error
	ready    bool
	finished bool
	info     BodyInfo
}

// NewBodyRecorder reserves one request/response/attempt-N part for an active log.
func (s *LogStore) NewBodyRecorder(id, part string, limit int64) *BodyRecorder {
	b := &BodyRecorder{store: s, id: id, part: part, limit: limit}
	if !safeSegment(id) || !validBodyPart(part) || limit < 0 {
		b.err = errors.New("invalid body id, part or capture limit")
		return b
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.getLocked(id); exists {
		b.err = errors.New("cannot capture a completed log")
		return b
	}
	parts := s.active[id]
	if parts == nil {
		parts = make(map[string]bool)
		s.active[id] = parts
	}
	if parts[part] {
		b.err = errors.New("body part already has a recorder")
		return b
	}
	parts[part] = true
	if s.dir == "" {
		b.ready = true
		return b
	}
	if err := s.ensureEntryDirLocked(id); err != nil {
		b.err = err
		return b
	}
	file, err := os.OpenFile(s.bodyPath(id, part), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		b.err = err
		return b
	}
	b.file, b.ready = file, true
	return b
}

// Write always reports success to the proxy, even after a capture I/O failure or
// cap. TotalBytes still counts every observed byte until Finish.
func (b *BodyRecorder) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	length := len(p)
	if b.finished {
		return length, nil
	}
	b.total += int64(length)
	if b.err != nil {
		return length, nil
	}
	if b.limit > 0 && int64(len(p)) > b.limit-b.captured {
		p = p[:int(b.limit-b.captured)]
	}
	if len(p) == 0 {
		return length, nil
	}
	n := len(p)
	if b.file == nil {
		b.data = append(b.data, p...)
	} else {
		n, b.err = b.file.Write(p)
		if b.err == nil && n != len(p) {
			b.err = io.ErrShortWrite
		}
	}
	b.captured += int64(n)
	previewBytes := min(n, bodyPreviewLimit-len(b.preview))
	if previewBytes > 0 && b.preview == nil {
		b.preview = make([]byte, 0, bodyPreviewLimit)
	}
	b.preview = append(b.preview, p[:previewBytes]...)
	return length, nil
}

// Finish closes the body once and returns the same immutable result thereafter.
// A partial transport read must pass eof=false even when all observed bytes were
// successfully captured. Available may be true for a readable partial capture.
func (b *BodyRecorder) Finish(eof bool) BodyInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return b.info
	}
	b.finished = true
	if b.file != nil {
		if err := b.file.Sync(); b.err == nil {
			b.err = err
		}
		if err := b.file.Close(); b.err == nil {
			b.err = err
		}
		b.file = nil
	} else if b.ready {
		b.store.mu.Lock()
		_, completed := b.store.getLocked(b.id)
		if b.store.active[b.id] != nil || completed {
			if b.store.bodies[b.id] == nil {
				b.store.bodies[b.id] = make(map[string][]byte)
			}
			// Ownership moves to the store. Write is now a no-op, so readers can
			// safely retain these immutable bytes through eviction or Clear.
			b.store.bodies[b.id][b.part] = b.data
		}
		b.store.mu.Unlock()
		b.data = nil
	}
	available := false
	if b.ready {
		r, size, err := b.store.OpenBody(b.id, b.part)
		if err == nil {
			available = true
			_ = r.Close()
			if size != b.captured {
				if b.err == nil {
					b.err = fmt.Errorf("body size %d does not match captured bytes %d", size, b.captured)
				}
				b.captured = size
			}
		} else if b.err == nil {
			b.err = err
		}
	}
	b.info = BodyInfo{
		Preview:       string(b.preview),
		TotalBytes:    b.total,
		CapturedBytes: b.captured,
		Truncated:     int64(len(b.preview)) < b.total || b.captured < b.total || b.err != nil,
		Complete:      eof && b.captured == b.total && b.err == nil && available,
		Available:     available,
	}
	b.preview = nil
	if b.err != nil {
		b.info.Error = b.err.Error()
		log.Printf("store: capture %q/%q: %v", b.id, b.part, b.err)
	}
	return b.info
}

// OpenBody returns raw captured bytes. The response alias follows the final
// attempt selected by the proxy. Opening under the store lock makes subsequent
// eviction safe: an open Unix file or immutable memory reader remains readable.
func (s *LogStore) OpenBody(id, part string) (io.ReadCloser, int64, error) {
	if !safeSegment(id) || !validBodyPart(part) {
		return nil, 0, os.ErrNotExist
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, completed := s.getLocked(id)
	if !completed && s.active[id] == nil {
		return nil, 0, os.ErrNotExist
	}
	if part == "response" && e.ResponseBodyPart != "" {
		part = e.ResponseBodyPart
	}
	if !validBodyPart(part) {
		return nil, 0, os.ErrNotExist
	}
	if s.dir == "" {
		data, ok := s.bodies[id][part]
		if !ok {
			return nil, 0, os.ErrNotExist
		}
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
	}
	if !s.owned[id] {
		return nil, 0, os.ErrNotExist
	}
	if err := realDirectory(filepath.Join(s.dir, id)); err != nil {
		return nil, 0, bodyOpenError(err)
	}
	path := s.bodyPath(id, part)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, bodyOpenError(err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, bodyOpenError(err)
	}
	info, err = f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

func bodyOpenError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return os.ErrNotExist
	}
	return err
}

func (s *LogStore) bodyPath(id, part string) string {
	return filepath.Join(s.dir, id, part+".body")
}

func safeSegment(s string) bool {
	if s == "" || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}

func validBodyPart(part string) bool {
	if part == "request" || part == "response" {
		return true
	}
	if !safeSegment(part) || !strings.HasPrefix(part, "attempt-") {
		return false
	}
	number := strings.TrimPrefix(part, "attempt-")
	if number == "" || number[0] < '1' || number[0] > '9' {
		return false
	}
	for i := 1; i < len(number); i++ {
		if number[i] < '0' || number[i] > '9' {
			return false
		}
	}
	return true
}

func realDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a real directory", path)
	}
	return nil
}
