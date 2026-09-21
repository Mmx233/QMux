package outbound

import (
	"context"
	"sync"
)

// Current stores one exact published generation. Its owner supplies the lock.
type Current[T comparable] struct {
	value T
}

func (c *Current[T]) Load() T { return c.value }

func (c *Current[T]) Empty() bool {
	var zero T
	return c.value == zero
}

func (c *Current[T]) Is(expected T) bool {
	var zero T
	return expected != zero && c.value == expected
}

func (c *Current[T]) Replace(next T) (previous T, replaced bool) {
	previous = c.value
	var zero T
	c.value = next
	return previous, previous != zero
}

func (c *Current[T]) Publish(next T) bool {
	var zero T
	if next == zero || c.value != zero {
		return false
	}
	c.value = next
	return true
}

func (c *Current[T]) Retire(expected T) bool {
	if !c.Is(expected) {
		return false
	}
	var zero T
	c.value = zero
	return true
}

func (c *Current[T]) Take() T {
	current := c.value
	var zero T
	c.value = zero
	return current
}

// Endpoint adds persistent retry state and a single reconnect intent to Current.
// Like Current, it is deliberately lock-free and must be protected by its owner.
type Endpoint[T comparable] struct {
	Current[T]
	Retry
	reconnecting bool
}

func (e *Endpoint[T]) ClaimReconnect(expected T) bool {
	if e.reconnecting {
		return false
	}
	var zero T
	if expected == zero {
		if !e.Empty() {
			return false
		}
	} else if !e.Is(expected) {
		return false
	}
	e.reconnecting = true
	return true
}

func (e *Endpoint[T]) ReleaseReconnect() { e.reconnecting = false }

func (e *Endpoint[T]) Reconnecting() bool { return e.reconnecting }

func (e *Endpoint[T]) RetireForReconnect(expected T, stable bool) bool {
	if !e.Retire(expected) {
		return false
	}
	if stable {
		e.ResetRetry()
	}
	return true
}

type Retry struct {
	stage    int
	attempts uint64
}

func (r *Retry) RetryStage() int { return r.stage }

func (r *Retry) SetRetryStage(stage int) { r.stage = stage }

func (r *Retry) AdvanceRetry(stage, maximum int) {
	r.stage = min(stage+1, maximum)
	r.attempts++
}

func (r *Retry) ResetRetry() { r.stage = 0 }

func (r *Retry) ReconnectAttempts() uint64 { return r.attempts }

// Owner provides one stop, control-join, and completion boundary for a
// generation. SetResource may transfer ownership from a provisional transport
// to a role-specific control owner.
type Owner struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	stop    func()
	join    func()
	stopped bool
	done    chan struct{}
	finish  sync.Once
}

func NewOwner(cancel context.CancelFunc) *Owner {
	return &Owner{cancel: cancel, done: make(chan struct{})}
}

func (o *Owner) SetResource(stop, join func()) {
	o.mu.Lock()
	o.stop = stop
	o.join = join
	if o.stopped && stop != nil {
		stop()
	}
	o.mu.Unlock()
}

func (o *Owner) Cancel() {
	o.mu.Lock()
	cancel := o.cancel
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (o *Owner) Stop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stopped {
		return
	}
	o.stopped = true
	if o.cancel != nil {
		o.cancel()
	}
	if o.stop != nil {
		o.stop()
	}
}

func (o *Owner) Join() {
	o.mu.Lock()
	join := o.join
	o.mu.Unlock()
	if join != nil {
		join()
	}
}

func (o *Owner) Finish() {
	o.finish.Do(func() {
		o.Stop()
		o.Join()
		close(o.done)
	})
}

func (o *Owner) Wait() { <-o.done }

// DeliverThenStart makes delivery the ownership commit point. Control starts
// after the value is visible, including when cancellation races just after send.
func DeliverThenStart[T any](
	parent, owner context.Context,
	destination chan<- T,
	value T,
	start func(),
) bool {
	select {
	case destination <- value:
	case <-parent.Done():
		return false
	case <-owner.Done():
		return false
	}
	if start != nil {
		start()
	}
	return true
}
