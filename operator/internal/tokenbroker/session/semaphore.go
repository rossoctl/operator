// Package session provides session management for the Token Broker.
package session

import (
	"context"
	"fmt"
)

// SimpleSemaphore implements a simple semaphore with capacity 1.
type SimpleSemaphore struct {
	ch chan struct{}
}

// NewSemaphore creates a new semaphore with the specified capacity.
func NewSemaphore(capacity int) *SimpleSemaphore {
	return &SimpleSemaphore{
		ch: make(chan struct{}, capacity),
	}
}

// Acquire acquires the semaphore, blocking until available or context is cancelled.
func (s *SimpleSemaphore) Acquire(ctx context.Context) error {
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("semaphore acquisition cancelled: %w", ctx.Err())
	}
}

// Release releases the semaphore.
func (s *SimpleSemaphore) Release() {
	select {
	case <-s.ch:
	default:
		// Semaphore was not acquired, ignore
	}
}

// TryAcquire attempts to acquire the semaphore without blocking.
// Returns true if acquired, false otherwise.
func (s *SimpleSemaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}
