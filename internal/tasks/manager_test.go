package tasks

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestAddListComplete(t *testing.T) {
	m := NewManager(1)
	var ran atomic.Int32
	m.SetExecutor(func(ctx context.Context, task Task) (string, error) {
		ran.Add(1)
		return "done:" + task.Title, nil
	})
	m.SetPersister(&MemoryPersister{})

	task, err := m.Add("hello", "do hello", true)
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "hello" {
		t.Fatalf("title=%s", task.Title)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := m.Get(task.ID)
		if got.Status == StatusCompleted {
			if got.Output != "done:hello" {
				t.Fatalf("output=%q", got.Output)
			}
			if ran.Load() != 1 {
				t.Fatalf("ran=%d", ran.Load())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout waiting for completion")
}

func TestMaxConcurrentQueues(t *testing.T) {
	m := NewManager(1)
	gate := make(chan struct{})
	m.SetExecutor(func(ctx context.Context, task Task) (string, error) {
		<-gate
		return "ok", nil
	})

	a, _ := m.Add("a", "a", true)
	b, _ := m.Add("b", "b", true)

	time.Sleep(50 * time.Millisecond)
	ga, _ := m.Get(a.ID)
	gb, _ := m.Get(b.ID)
	if ga.Status != StatusRunning {
		t.Fatalf("a status=%s", ga.Status)
	}
	if gb.Status != StatusPending {
		t.Fatalf("b status=%s want pending", gb.Status)
	}
	close(gate)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gb, _ = m.Get(b.ID)
		if gb.Status == StatusCompleted {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("b never completed")
}

func TestStopPending(t *testing.T) {
	m := NewManager(1)
	block := make(chan struct{})
	m.SetExecutor(func(ctx context.Context, task Task) (string, error) {
		<-block
		return "", nil
	})
	first, _ := m.Add("busy", "busy", true)
	second, _ := m.Add("wait", "wait", true)
	time.Sleep(30 * time.Millisecond)
	if err := m.Stop(second.ID); err != nil {
		t.Fatal(err)
	}
	gs, _ := m.Get(second.ID)
	if gs.Status != StatusCancelled {
		t.Fatalf("status=%s", gs.Status)
	}
	close(block)
	_ = first
}

func TestFormatBoard(t *testing.T) {
	s := FormatBoard(nil)
	if s == "" {
		t.Fatal("empty")
	}
	s = FormatBoard([]Task{{ID: "abc", Title: "T", Status: StatusPending}})
	if !contains(s, "abc") || !contains(s, "T") {
		t.Fatalf("board=%s", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
