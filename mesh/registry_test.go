package mesh

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryAtomicRoleBudgetsAndRecovery(t *testing.T) {
	registry := NewRegistry(2, 1, 1)
	clientPending, err := registry.BeginInbound(Owner{})
	if err != nil {
		t.Fatal(err)
	}
	peerPending, err := registry.BeginInbound(Owner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.BeginInbound(Owner{}); !errors.Is(err, ErrPendingCapacity) {
		t.Fatalf("pending cap+1 error = %v", err)
	}
	client, err := registry.PrepareClient(clientPending, "instance-a", "group-a")
	if err != nil {
		t.Fatal(err)
	}
	peer, arbitration, err := registry.PreparePeer(peerPending, "peer-a", true)
	if err != nil || arbitration != nil {
		t.Fatalf("prepare peer = (%v, %v, %v)", peer, arbitration, err)
	}
	if snapshot := registry.Snapshot(); snapshot.Pending != 0 || snapshot.ClientTotal != 1 || snapshot.PeerTotal != 1 || snapshot.PendingHighWater != 2 {
		t.Fatalf("atomic role transfer snapshot = %+v", snapshot)
	}

	pending, err := registry.BeginInbound(Owner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.PrepareClient(pending, "instance-b", "group-b"); !errors.Is(err, ErrClientCapacity) {
		t.Fatalf("client cap+1 error = %v", err)
	}
	if registry.Snapshot().Pending != 1 {
		t.Fatal("failed role transfer released pending before caller rollback")
	}
	registry.Release(pending)

	registry.BeginRetire(client)
	if snapshot := registry.Snapshot(); snapshot.ClientRetiring != 1 || snapshot.ClientHighWater != 1 {
		t.Fatalf("retiring/high-water snapshot = %+v", snapshot)
	}
	if pending, err = registry.BeginInbound(Owner{}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.PrepareClient(pending, "instance-b", "group-b"); !errors.Is(err, ErrClientCapacity) {
		t.Fatalf("retiring generation stopped counting toward cap: %v", err)
	}
	registry.Release(pending)
	registry.Release(client)

	if pending, err = registry.BeginInbound(Owner{}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.PrepareClient(pending, "instance-b", "group-b"); err != nil {
		t.Fatalf("capacity did not recover: %v", err)
	}
	registry.Release(pending)
	registry.Release(peer)
}

func TestRegistryConcurrentClientClaimAndCurrentExclusion(t *testing.T) {
	const contenders = 32
	registry := NewRegistry(contenders, contenders, 1)
	pending := make([]*Generation, contenders)
	for i := range pending {
		var err error
		pending[i], err = registry.BeginInbound(Owner{})
		if err != nil {
			t.Fatal(err)
		}
	}
	var prepared atomic.Int32
	var winner atomic.Pointer[Generation]
	var wg sync.WaitGroup
	for _, token := range pending {
		wg.Go(func() {
			generation, err := registry.PrepareClient(token, "instance-a", "group-a")
			if err == nil {
				prepared.Add(1)
				winner.Store(generation)
				return
			}
			if !errors.Is(err, ErrClientPrepared) {
				t.Errorf("losing prepare error = %v", err)
			}
		})
	}
	wg.Wait()
	if prepared.Load() != 1 {
		t.Fatalf("prepared winners = %d, want 1", prepared.Load())
	}
	for _, token := range pending {
		if token != winner.Load() {
			registry.Release(token)
		}
	}
	if committed, _ := registry.CommitReceiver(winner.Load()); !committed {
		t.Fatal("winner commit failed")
	}

	duplicate, err := registry.BeginInbound(Owner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.PrepareClient(duplicate, "instance-a", "group-a"); !errors.Is(err, ErrClientCurrent) {
		t.Fatalf("healthy-current duplicate error = %v", err)
	}
	if !registry.IsCurrent(winner.Load()) {
		t.Fatal("duplicate claim evicted current generation")
	}
	registry.Release(duplicate)

	conflict, err := registry.BeginInbound(Owner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.PrepareClient(conflict, "instance-a", "group-b"); !errors.Is(err, ErrClientGroupConflict) {
		t.Fatalf("cross-group current error = %v", err)
	}
	registry.Release(conflict)

	registry.BeginRetire(winner.Load())
	overlap, err := registry.BeginInbound(Owner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.PrepareClient(overlap, "instance-a", "group-a"); err != nil {
		t.Fatalf("same-group retiring overlap rejected: %v", err)
	}
	registry.Release(overlap)
	registry.Release(winner.Load())
	if snapshot := registry.Snapshot(); snapshot.ClientTotal != 0 || snapshot.ClientBindings != 0 {
		t.Fatalf("client cleanup snapshot = %+v", snapshot)
	}
}

func TestRegistryPeerAuthorityAndDelayedOutboundAck(t *testing.T) {
	registry := NewRegistry(2, 1, 2)
	outbound, err := registry.BeginOutboundPeer("peer-a", Owner{})
	if err != nil {
		t.Fatal(err)
	}
	inboundPending, err := registry.BeginInbound(Owner{})
	if err != nil {
		t.Fatal(err)
	}
	_, arbitration, err := registry.PreparePeer(inboundPending, "peer-a", true)
	if err != nil || arbitration == nil || arbitration.Loser() != outbound {
		t.Fatalf("arbitration = (%v, %v)", arbitration, err)
	}
	if registry.CommitOutboundPeer(outbound) {
		t.Fatal("delayed outbound success ACK displaced preferred inbound")
	}
	registry.BeginRetire(outbound)
	registry.Release(outbound)
	inbound, err := registry.CompleteArbitration(arbitration)
	if err != nil {
		t.Fatalf("complete arbitration: %v", err)
	}
	if committed, _ := registry.CommitReceiver(inbound); !committed {
		t.Fatal("inbound commit failed")
	}
	if !registry.HasPeerCurrent("peer-a") || registry.Snapshot().PeerCurrent != 1 {
		t.Fatalf("peer current snapshot = %+v", registry.Snapshot())
	}
	registry.BeginRetire(inbound)
	registry.Release(inbound)
}

func TestRegistryMaxPeerArbitrationExactLoserAndStaleCallbacks(t *testing.T) {
	registry := NewRegistry(1, 1, 1)
	loser, err := registry.BeginOutboundPeer("peer-a", Owner{})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := registry.BeginInbound(Owner{})
	if err != nil {
		t.Fatal(err)
	}
	_, arbitration, err := registry.PreparePeer(pending, "peer-a", true)
	if err != nil || arbitration == nil {
		t.Fatalf("prepare preferred inbound = (%v, %v)", arbitration, err)
	}
	if !registry.PeerSuppressed("peer-a") {
		t.Fatal("arbitration did not suppress endpoint worker")
	}
	if _, err := registry.BeginOutboundPeer("peer-a", Owner{}); !errors.Is(err, ErrPeerSuppressed) {
		t.Fatalf("suppressed outbound error = %v", err)
	}
	registry.BeginRetire(loser)
	registry.Release(loser)
	if snapshot := registry.Snapshot(); snapshot.PeerTotal != 1 || snapshot.PeerRetiring != 1 || snapshot.PeerHighWater != 1 || snapshot.Arbitrations != 1 {
		t.Fatalf("cleaned loser released capacity before transfer: %+v", snapshot)
	}
	if _, err := registry.BeginOutboundPeer("peer-b", Owner{}); !errors.Is(err, ErrPeerCapacity) {
		t.Fatalf("unrelated peer claimed arbitration capacity: %v", err)
	}
	winner, err := registry.CompleteArbitration(arbitration)
	if err != nil {
		t.Fatal(err)
	}
	if committed, _ := registry.CommitReceiver(winner); !committed {
		t.Fatal("winner commit failed")
	}
	if registry.Release(loser) {
		t.Fatal("stale loser callback released a second token")
	}
	if !registry.IsCurrent(winner) {
		t.Fatal("stale loser callback released winner")
	}
	if snapshot := registry.Snapshot(); snapshot.PeerHighWater != 1 || snapshot.PeerCurrent != 1 || snapshot.Arbitrations != 0 {
		t.Fatalf("max_peers=1 snapshot = %+v", snapshot)
	}
	registry.BeginRetire(winner)
	registry.Release(winner)
}

func TestRegistryArbitrationCancelTimeoutAndStaleTerminal(t *testing.T) {
	registry := NewRegistry(1, 1, 1)
	newArbitration := func() (*Generation, *Arbitration) {
		t.Helper()
		loser, err := registry.BeginOutboundPeer("peer-a", Owner{})
		if err != nil {
			t.Fatal(err)
		}
		pending, err := registry.BeginInbound(Owner{})
		if err != nil {
			t.Fatal(err)
		}
		_, arbitration, err := registry.PreparePeer(pending, "peer-a", true)
		if err != nil || arbitration == nil {
			t.Fatalf("prepare arbitration = (%v, %v)", arbitration, err)
		}
		return loser, arbitration
	}

	firstLoser, canceled := newArbitration()
	registry.BeginRetire(firstLoser)
	registry.Release(firstLoser)
	if !registry.CancelArbitration(canceled, true) {
		t.Fatal("cancel did not terminate exact arbitration")
	}
	if registry.Release(firstLoser) || registry.Snapshot().PeerTotal != 0 {
		t.Fatal("canceled arbitration retained a cleaned loser")
	}

	directLoser, directCancel := newArbitration()
	registry.BeginRetire(directLoser)
	registry.Release(directLoser)
	if !registry.Release(directCancel.Inbound()) {
		t.Fatal("inbound release did not terminate its arbitration")
	}
	if snapshot := registry.Snapshot(); snapshot.PeerTotal != 0 || snapshot.Arbitrations != 0 {
		t.Fatalf("inbound release retained a cleaned loser: %+v", snapshot)
	}

	secondLoser, timedOut := newArbitration()
	if registry.CancelArbitration(canceled, true) {
		t.Fatal("stale terminal callback cleared a newer arbitration")
	}
	if !registry.PeerSuppressed("peer-a") {
		t.Fatal("stale terminal callback removed worker suppression")
	}
	if !registry.CancelArbitration(timedOut, true) {
		t.Fatal("timeout did not terminate exact arbitration")
	}
	registry.Release(secondLoser)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !registry.WaitPeerDialable(ctx, "peer-a") {
		t.Fatal("peer worker did not resume after canceled arbitration cleanup")
	}
	if snapshot := registry.Snapshot(); snapshot.Pending != 0 || snapshot.PeerTotal != 0 || snapshot.Arbitrations != 0 || snapshot.PeerHighWater != 1 {
		t.Fatalf("arbitration terminal cleanup snapshot = %+v", snapshot)
	}
}

func TestRegistryStopClearsArbitrationAndJoinsBothOwners(t *testing.T) {
	registry := NewRegistry(1, 1, 1)
	stopped := make(chan string, 2)
	joined := make(chan struct{})
	owner := func(name string) Owner {
		return Owner{
			Stop: sync.OnceFunc(func() { stopped <- name }),
			Wait: func() { <-joined },
		}
	}
	loser, err := registry.BeginOutboundPeer("peer-a", owner("outbound"))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := registry.BeginInbound(owner("inbound"))
	if err != nil {
		t.Fatal(err)
	}
	_, arbitration, err := registry.PreparePeer(pending, "peer-a", true)
	if err != nil || arbitration == nil {
		t.Fatalf("prepare arbitration = (%v, %v)", arbitration, err)
	}

	stopDone := make(chan struct{})
	go func() {
		registry.Stop()
		close(stopDone)
	}()
	seen := make(map[string]bool, 2)
	for range 2 {
		select {
		case name := <-stopped:
			seen[name] = true
		case <-time.After(time.Second):
			t.Fatal("Stop did not cancel both arbitration owners")
		}
	}
	if !seen["inbound"] || !seen["outbound"] {
		t.Fatalf("stopped arbitration owners = %v", seen)
	}
	if snapshot := registry.Snapshot(); !snapshot.Closed || snapshot.Arbitrations != 0 || snapshot.Pending != 1 || snapshot.PeerTotal != 1 {
		t.Fatalf("arbitration Stop-in-progress snapshot = %+v", snapshot)
	}
	secondStopDone := make(chan struct{})
	go func() {
		registry.Stop()
		close(secondStopDone)
	}()
	select {
	case <-secondStopDone:
		t.Fatal("concurrent Stop returned before arbitration owner joins")
	default:
	}
	close(joined)
	<-stopDone
	<-secondStopDone
	if registry.CancelArbitration(arbitration, true) || registry.Release(loser) || registry.Release(pending) {
		t.Fatal("stale arbitration callbacks changed stopped registry")
	}
	if snapshot := registry.Snapshot(); snapshot.Pending != 0 || snapshot.PeerTotal != 0 || snapshot.Arbitrations != 0 {
		t.Fatalf("stopped arbitration registry leaked state: %+v", snapshot)
	}
}

func TestRegistryStopCancelsThenJoinsOutsideLock(t *testing.T) {
	registry := NewRegistry(2, 2, 2)
	stopped := make(chan struct{})
	joined := make(chan struct{})
	var stopOnce sync.Once
	pending, err := registry.BeginInbound(Owner{
		Stop: func() { stopOnce.Do(func() { close(stopped) }) },
		Wait: func() { <-joined },
	})
	if err != nil {
		t.Fatal(err)
	}
	stopDone := make(chan struct{})
	go func() {
		registry.Stop()
		close(stopDone)
	}()
	<-stopped
	secondStopDone := make(chan struct{})
	go func() {
		registry.Stop()
		close(secondStopDone)
	}()
	// Snapshot takes the registry lock while Stop is waiting for the owner.
	if snapshot := registry.Snapshot(); !snapshot.Closed || snapshot.Pending != 1 {
		t.Fatalf("stop-in-progress snapshot = %+v", snapshot)
	}
	select {
	case <-secondStopDone:
		t.Fatal("concurrent Stop returned before owner join")
	default:
	}
	close(joined)
	<-stopDone
	<-secondStopDone
	if registry.Release(pending) {
		t.Fatal("late pending callback changed stopped registry")
	}
	if snapshot := registry.Snapshot(); snapshot.Pending != 0 || snapshot.ClientTotal != 0 || snapshot.PeerTotal != 0 || snapshot.Arbitrations != 0 {
		t.Fatalf("stopped registry leaked state: %+v", snapshot)
	}
}
