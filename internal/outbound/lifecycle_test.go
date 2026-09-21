package outbound

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEndpointExactCurrentAndReconnectIntent(t *testing.T) {
	type generation struct{ id int }
	old := &generation{id: 1}
	fresh := &generation{id: 2}
	stale := &generation{id: 3}
	var endpoint Endpoint[*generation]

	if !endpoint.Publish(old) || endpoint.Publish(fresh) {
		t.Fatal("publish did not preserve a single current generation")
	}
	if endpoint.ClaimReconnect(stale) || endpoint.ClaimReconnect(nil) {
		t.Fatal("stale or initial-failure callback claimed a reconnect intent")
	}
	if !endpoint.ClaimReconnect(old) || endpoint.ClaimReconnect(old) {
		t.Fatal("exact generation did not own exactly one reconnect intent")
	}
	endpoint.SetRetryStage(MaxReconnectStage)
	if !endpoint.RetireForReconnect(old, true) || !endpoint.Empty() || endpoint.RetryStage() != 0 {
		t.Fatal("stable exact generation did not retire and reset its retry stage")
	}
	endpoint.ReleaseReconnect()
	if !endpoint.Publish(fresh) || endpoint.RetireForReconnect(stale, true) || !endpoint.Is(fresh) {
		t.Fatal("stale retirement changed the successor generation")
	}
}

func TestOwnerStopBeforeResourceTransferClosesAndJoinsNewOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	owner := NewOwner(cancel)
	owner.Stop()
	if ctx.Err() == nil {
		t.Fatal("owner stop did not cancel its attempt")
	}

	var stopped atomic.Int32
	var joined atomic.Int32
	owner.SetResource(func() { stopped.Add(1) }, func() { joined.Add(1) })
	owner.Finish()
	owner.Wait()
	if stopped.Load() != 1 || joined.Load() != 1 {
		t.Fatalf("resource ownership = stop %d/join %d, want 1/1", stopped.Load(), joined.Load())
	}
}

func TestOwnerConcurrentStopWaitsForResourceClose(t *testing.T) {
	owner := NewOwner(nil)
	closing := make(chan struct{})
	release := make(chan struct{})
	owner.SetResource(func() {
		close(closing)
		<-release
	}, nil)

	var stops sync.WaitGroup
	stops.Go(func() {
		owner.Stop()
	})
	<-closing

	secondDone := make(chan struct{})
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		owner.Stop()
		close(secondDone)
	}()
	<-secondStarted
	select {
	case <-secondDone:
		t.Fatal("concurrent Stop returned before resource close completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	stops.Wait()
	<-secondDone
}

func TestDeliverThenStartOrdersControlAfterDelivery(t *testing.T) {
	destination := make(chan int, 1)
	started := false
	if !DeliverThenStart(context.Background(), context.Background(), destination, 7, func() {
		if len(destination) != 1 {
			t.Fatal("control started before delivery")
		}
		started = true
	}) {
		t.Fatal("delivery failed")
	}
	if !started || <-destination != 7 {
		t.Fatal("delivery did not start control with the published value visible")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	started = false
	if DeliverThenStart(canceled, canceled, make(chan int), 8, func() { started = true }) || started {
		t.Fatal("canceled delivery started control")
	}
}
