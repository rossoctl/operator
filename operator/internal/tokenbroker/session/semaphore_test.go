package session

import (
	"context"
	"testing"
	"time"
)

func TestSemaphore_AcquireRelease(t *testing.T) {
	sem := NewSemaphore(1)

	ctx := context.Background()

	// Acquire semaphore
	err := sem.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	// Release semaphore
	sem.Release()

	// Should be able to acquire again
	err = sem.Acquire(ctx)
	if err != nil {
		t.Fatalf("Second acquire failed: %v", err)
	}

	sem.Release()
}

func TestSemaphore_BlocksWhenFull(t *testing.T) {
	sem := NewSemaphore(1)

	ctx := context.Background()

	// Acquire semaphore
	err := sem.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	// Try to acquire again (should block)
	acquired := make(chan bool, 1)
	go func() {
		ctx2, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := sem.Acquire(ctx2)
		acquired <- (err == nil)
	}()

	// Wait a bit to ensure goroutine is blocked
	time.Sleep(50 * time.Millisecond)

	// Release semaphore
	sem.Release()

	// Second acquire should now succeed
	select {
	case success := <-acquired:
		if !success {
			t.Error("Second acquire should have succeeded after release")
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("Second acquire timed out")
	}

	sem.Release() // Clean up
}

func TestSemaphore_ContextCancellation(t *testing.T) {
	sem := NewSemaphore(1)

	// Acquire semaphore
	ctx := context.Background()
	err := sem.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	// Try to acquire with cancelled context
	ctx2, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err = sem.Acquire(ctx2)
	if err == nil {
		t.Error("Acquire should have failed with cancelled context")
	}

	sem.Release()
}

func TestSemaphore_TryAcquire(t *testing.T) {
	sem := NewSemaphore(1)

	// TryAcquire should succeed
	if !sem.TryAcquire() {
		t.Error("TryAcquire should have succeeded")
	}

	// TryAcquire should fail (semaphore full)
	if sem.TryAcquire() {
		t.Error("TryAcquire should have failed (semaphore full)")
	}

	// Release and try again
	sem.Release()

	if !sem.TryAcquire() {
		t.Error("TryAcquire should have succeeded after release")
	}

	sem.Release()
}

func TestSemaphore_MultipleReleases(t *testing.T) {
	sem := NewSemaphore(1)

	ctx := context.Background()

	// Acquire once
	err := sem.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	// Release multiple times (should not panic)
	sem.Release()
	sem.Release() // Extra release should be ignored
	sem.Release() // Extra release should be ignored

	// Should still be able to acquire
	err = sem.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire after multiple releases failed: %v", err)
	}

	sem.Release()
}

func TestSemaphore_ConcurrentAcquire(t *testing.T) {
	sem := NewSemaphore(1)

	const numGoroutines = 10
	acquired := make(chan int, numGoroutines)

	// Launch multiple goroutines trying to acquire
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			ctx := context.Background()
			if err := sem.Acquire(ctx); err == nil {
				acquired <- id
				time.Sleep(10 * time.Millisecond) // Hold briefly
				sem.Release()
			}
		}(i)
	}

	// Collect results
	seen := make(map[int]bool)
	for i := 0; i < numGoroutines; i++ {
		select {
		case id := <-acquired:
			if seen[id] {
				t.Errorf("Goroutine %d acquired semaphore twice", id)
			}
			seen[id] = true
		case <-time.After(2 * time.Second):
			t.Fatal("Timeout waiting for goroutines")
		}
	}

	if len(seen) != numGoroutines {
		t.Errorf("Expected %d goroutines to acquire, got %d", numGoroutines, len(seen))
	}
}
