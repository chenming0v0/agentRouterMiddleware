package store

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Retention keeps only the newest limit entries, ordered newest first.
func TestRetentionNewest50Of55(t *testing.T) {
	s := NewLogStore(50)
	for i := 0; i < 55; i++ {
		s.Add(LogEntry{ID: fmt.Sprintf("%d", i), Status: 200})
	}
	if got := s.Count(); got != 50 {
		t.Fatalf("Count=%d want 50", got)
	}
	got := s.List(100)
	if len(got) != 50 {
		t.Fatalf("List(100) len=%d want 50", len(got))
	}
	if got[0].ID != "54" || got[49].ID != "5" {
		t.Errorf("retention window=[%s..%s] want [54..5]", got[0].ID, got[49].ID)
	}
	newest := s.List(10)
	if len(newest) != 10 {
		t.Fatalf("List(10) len=%d want 10", len(newest))
	}
	for i, e := range newest {
		if want := fmt.Sprintf("%d", 54-i); e.ID != want {
			t.Errorf("List(10)[%d].ID=%s want %s", i, e.ID, want)
		}
	}
}

// Shrinking the limit keeps the newest entries; growing it preserves them.
func TestSetLimitKeepsNewest(t *testing.T) {
	s := NewLogStore(50)
	for i := 0; i < 55; i++ {
		s.Add(LogEntry{ID: fmt.Sprintf("%d", i)})
	}
	s.SetLimit(10)
	got := s.List(100)
	if len(got) != 10 {
		t.Fatalf("after SetLimit(10) len=%d want 10", len(got))
	}
	if got[0].ID != "54" || got[9].ID != "45" {
		t.Errorf("after shrink window=[%s..%s] want [54..45]", got[0].ID, got[9].ID)
	}
	s.SetLimit(3)
	got = s.List(100)
	if len(got) != 3 {
		t.Fatalf("after SetLimit(3) len=%d want 3", len(got))
	}
	if got[0].ID != "54" || got[2].ID != "52" {
		t.Errorf("after shrink window=[%s..%s] want [54..52]", got[0].ID, got[2].ID)
	}
}

// List snapshots must not alias the store's mutable maps/slices.
func TestListSnapshotDoesNotShareMutableState(t *testing.T) {
	s := NewLogStore(10)
	s.Add(LogEntry{
		ID:             "x",
		RequestHeaders: map[string][]string{"Authorization": {"secret"}},
		Attempts:       []Attempt{{UpstreamID: "up-1"}},
	})
	got := s.List(1)
	got[0].RequestHeaders["Authorization"][0] = "leaked"
	got[0].RequestHeaders["Injected"] = []string{"x"}
	got[0].Attempts[0].UpstreamID = "leaked"

	again := s.List(1)
	if again[0].RequestHeaders["Authorization"][0] == "leaked" {
		t.Error("List header values alias store memory")
	}
	if _, ok := again[0].RequestHeaders["Injected"]; ok {
		t.Error("List header map aliases store memory")
	}
	if again[0].Attempts[0].UpstreamID == "leaked" {
		t.Error("List attempts slice aliases store memory")
	}
}

// A subscriber that never drains must not block Add.
func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	s := NewLogStore(10)
	_, cancel := s.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			s.Add(LogEntry{ID: fmt.Sprint(i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Add blocked behind a slow subscriber")
	}
}

// Concurrent Add/List/Clear/SetLimit/Subscribe/unsubscribe must stay race-free.
func TestConcurrentMutations(t *testing.T) {
	s := NewLogStore(32)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	worker := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				fn(i)
			}
		}()
	}
	worker(func(i int) { s.Add(LogEntry{ID: fmt.Sprint(i), Status: 200}) })
	worker(func(i int) { _ = s.List(5) })
	worker(func(i int) {
		if i%7 == 0 {
			s.Clear()
		}
	})
	worker(func(i int) { s.SetLimit((i % 16) + 1) })
	worker(func(i int) {
		ch, cancel := s.Subscribe()
		select {
		case <-ch:
		default:
		}
		cancel()
	})

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}
