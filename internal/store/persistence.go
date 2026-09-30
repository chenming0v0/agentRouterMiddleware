package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// NewPersistentLogStore stores complete raw bodies on disk and keeps only
// metadata/previews in the ring. Only directories containing matching, valid
// metadata.json records are recovered or pruned. Abandoned active captures and
// unrelated/corrupt files are left alone, with diagnostics.
func NewPersistentLogStore(limit int, dir string) (*LogStore, error) {
	if dir == "" {
		return nil, errors.New("log directory is required")
	}
	path, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	if err := realDirectory(path); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return nil, err
	}
	s := NewLogStore(limit)
	s.dir = path
	records, err := s.loadRecords()
	if err != nil {
		return nil, err
	}
	// Metadata mtime is the completion/publication time, unlike LogEntry.Time,
	// which is the request start time and can precede a much faster request.
	sort.Slice(records, func(i, j int) bool {
		if records[i].modified.Equal(records[j].modified) {
			if records[i].entry.Time == records[j].entry.Time {
				return records[i].entry.ID < records[j].entry.ID
			}
			return records[i].entry.Time < records[j].entry.Time
		}
		return records[i].modified.Before(records[j].modified)
	})
	keepFrom := max(0, len(records)-s.limit)
	for _, record := range records[:keepFrom] {
		s.removeBodiesLocked(record.entry.ID)
	}
	for _, record := range records[keepFrom:] {
		s.pushLocked(record.entry)
	}
	return s, nil
}

type diskRecord struct {
	entry    LogEntry
	modified time.Time
}

func (s *LogStore) loadRecords() ([]diskRecord, error) {
	dirs, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var records []diskRecord
	for _, dir := range dirs {
		if !dir.IsDir() || !safeSegment(dir.Name()) {
			continue
		}
		record, err := s.loadRecord(dir.Name())
		if err != nil {
			log.Printf("store: leaving unrecognized record %q: %v", dir.Name(), err)
			continue
		}
		s.owned[record.entry.ID] = true
		records = append(records, record)
	}
	return records, nil
}

func (s *LogStore) loadRecord(id string) (diskRecord, error) {
	dir := filepath.Join(s.dir, id)
	path := filepath.Join(dir, "metadata.json")
	info, err := os.Lstat(path)
	if err != nil {
		return diskRecord{}, err
	}
	if !info.Mode().IsRegular() {
		return diskRecord{}, errors.New("metadata is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return diskRecord{}, err
	}
	var e LogEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return diskRecord{}, err
	}
	if e.ID != id || e.ResponseBodyPart != "" && !validBodyPart(e.ResponseBodyPart) {
		return diskRecord{}, errors.New("metadata id or response body part is invalid")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return diskRecord{}, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return diskRecord{}, err
	}
	e = boundedEntry(e)
	s.checkStoredBody(id, "request", &e.RequestBodyCapturedBytes, &e.RequestBodyAvailable,
		&e.RequestBodyComplete, &e.RequestBodyTruncated, &e.RequestBodyError)
	part := e.ResponseBodyPart
	if part == "" {
		part = "response"
	}
	s.checkStoredBody(id, part, &e.ResponseBodyCapturedBytes, &e.ResponseBodyAvailable,
		&e.ResponseBodyComplete, &e.ResponseBodyTruncated, &e.ResponseBodyError)
	for i := range e.Attempts {
		a := &e.Attempts[i]
		s.checkStoredBody(id, fmt.Sprintf("attempt-%d", i+1), &a.ResponseBodyCapturedBytes,
			&a.ResponseBodyAvailable, &a.ResponseBodyComplete, &a.ResponseBodyTruncated, &a.ResponseBodyError)
	}
	return diskRecord{entry: e, modified: info.ModTime()}, nil
}

// checkStoredBody checks only file metadata, never reads a body for listing.
// A missing/short body invalidates capture completeness, not client delivery.
func (s *LogStore) checkStoredBody(id, part string, captured *int64, available, complete, truncated *bool, captureError *string) {
	if !*available && !*complete && *captured == 0 {
		return
	}
	path := s.bodyPath(id, part)
	info, err := os.Lstat(path)
	readable := false
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("body is not a regular file")
	}
	if err == nil {
		err = os.Chmod(path, 0o600)
	}
	if err == nil {
		var f *os.File
		f, err = os.Open(path)
		if err == nil {
			readable = true
			_ = f.Close()
		}
	}
	*available = readable
	if readable && info.Size() != *captured {
		err = fmt.Errorf("stored body size %d does not match captured bytes %d", info.Size(), *captured)
		*captured = info.Size()
	}
	if !readable {
		*captured = 0
	}
	if err != nil {
		*complete, *truncated = false, true
		*captureError = err.Error()
		log.Printf("store: body %q/%q unavailable or damaged: %v", id, part, err)
	}
}

// ensureEntryDirLocked never adopts an arbitrary pre-existing directory. A
// directory is owned only after we create it or load its valid completion marker.
func (s *LogStore) ensureEntryDirLocked(id string) error {
	if !safeSegment(id) {
		return errors.New("invalid log id")
	}
	path := filepath.Join(s.dir, id)
	err := os.Mkdir(path, 0o700)
	if err != nil && (!errors.Is(err, os.ErrExist) || !s.owned[id]) {
		return err
	}
	if err := realDirectory(path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	s.owned[id] = true
	return nil
}

func (s *LogStore) saveEntryLocked(e LogEntry) error {
	if err := s.ensureEntryDirLocked(e.ID); err != nil {
		return err
	}
	dir := filepath.Join(s.dir, e.ID)
	tmp, err := os.CreateTemp(dir, ".metadata-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := json.NewEncoder(tmp).Encode(e); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "metadata.json"))
}

func (s *LogStore) removeBodiesLocked(id string) {
	if s.active[id] != nil {
		return
	}
	delete(s.bodies, id)
	if s.dir == "" || !s.owned[id] || !safeSegment(id) {
		return
	}
	delete(s.owned, id)
	dir := filepath.Join(s.dir, id)
	if err := realDirectory(dir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("store: cannot prune %q: %v", id, err)
		}
		return
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("store: cannot prune %q: %v", id, err)
		return
	}
	for _, file := range files {
		name := file.Name()
		if name != "metadata.json" && !(strings.HasSuffix(name, ".body") && validBodyPart(strings.TrimSuffix(name, ".body"))) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("store: cannot remove %q/%q: %v", id, name, err)
		}
	}
	// Do not recursively delete unrelated files, even inside an owned record.
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("store: leaving nonempty or inaccessible directory %q: %v", id, err)
	}
}
