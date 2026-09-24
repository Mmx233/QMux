package mesh

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/quic-go/quic-go"
)

var readyForwarding = ForwardingEligibility{SessionReady: true, DeclarationReady: true, VersionEligible: true, L4Healthy: true}

func currentMeshClient(t *testing.T, r *Registry, instance, group string) *Generation {
	t.Helper()
	pending, err := r.BeginInbound(Owner{})
	if err != nil {
		t.Fatal(err)
	}
	g, err := r.PrepareClient(pending, instance, group)
	if err != nil {
		t.Fatal(err)
	}
	if committed, stopped := r.CommitReceiver(g); !committed || stopped {
		t.Fatalf("commit %s = %t, stopped %t", instance, committed, stopped)
	}
	return g
}

func TestMeshSourceConnectionAndRequestAccounting(t *testing.T) {
	source := newTCPSource(config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 2}, 2)
	flow, err := source.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.beginFlow(); !errors.Is(err, ErrSourceFlowCapacity) {
		t.Fatalf("second public socket = %v", err)
	}
	firstRequest, err := flow.beginRequest()
	if err != nil {
		t.Fatal(err)
	}
	secondRequest, err := flow.beginRequest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.beginRequest(); !errors.Is(err, ErrSourceRequestCapacity) {
		t.Fatalf("third HTTP request = %v", err)
	}
	firstSetup, err := flow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	secondSetup, err := flow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.beginSetup(); !errors.Is(err, ErrSourceSetupCapacity) {
		t.Fatalf("third backend setup = %v", err)
	}
	if got := source.snapshot(); got.Flows != 1 || got.Requests != 2 || got.Setups != 2 {
		t.Fatalf("one socket/multiple requests = %+v", got)
	}
	if !firstSetup.complete() || firstSetup.release() || !secondSetup.release() || !firstRequest.release() || !secondRequest.release() || !flow.release() || flow.release() {
		t.Fatal("lease release was not exactly once")
	}
	if got := source.snapshot(); got.Flows != 0 || got.Requests != 0 || got.Setups != 0 || got.FlowHigh != 1 || got.RequestHigh != 2 || got.SetupHigh != 2 {
		t.Fatalf("released source = %+v", got)
	}
	if _, err := flow.beginRequest(); !errors.Is(err, ErrFlowReleased) {
		t.Fatalf("request on closed flow = %v", err)
	}
	source.close()
	if _, err := source.beginFlow(); !errors.Is(err, ErrSourceClosed) {
		t.Fatalf("admission after close = %v", err)
	}
}

func TestMeshSourceAndGenerationCapacityRemainIndependent(t *testing.T) {
	r := NewRegistry(4, 4, 4)
	g := currentMeshClient(t, r, "a", "api")
	if !r.PublishForwarding(g, readyForwarding) {
		t.Fatal("publish current client")
	}
	capacity := config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 1, MaxTCPConnectionsPerGeneration: 1, MaxPendingTCPSetupsPerGeneration: 1}
	sources := []*tcpSource{newTCPSource(capacity, 2), newTCPSource(capacity, 2), newTCPSource(capacity, 0)}
	flows := make([]*flowLease, len(sources))
	setups := make([]*setupLease, len(sources))
	leases := make([]*generationLease, len(sources))
	for i, source := range sources {
		var err error
		flows[i], err = source.beginFlow()
		if err != nil {
			t.Fatal(err)
		}
		setups[i], err = flows[i].beginSetup()
		if err != nil {
			t.Fatal(err)
		}
		leases[i], err = r.beginClientSelection("api", setups[i], "least-connections").next()
		if err != nil {
			t.Fatalf("source %d admission = %v", i, err)
		}
		if got := r.trafficSnapshot(g, source); got.Pending != 1 || got.Inflight != i+1 {
			t.Fatalf("source %d exact generation = %+v", i, got)
		}
	}
	if _, err := flows[0].beginSetup(); !errors.Is(err, ErrSourceSetupCapacity) {
		t.Fatalf("listener 0 setup cap = %v", err)
	}
	if got := sources[0].snapshot(); got.Flows != 1 || got.Setups != 1 {
		t.Fatalf("listener 0 borrowed capacity = %+v", got)
	}
	for i, lease := range leases {
		if !lease.acknowledge() || setups[i].complete() {
			t.Fatalf("ACK transition %d failed", i)
		}
		if got := r.trafficSnapshot(g, sources[i]); got.Pending != 0 || got.Active != 1 {
			t.Fatalf("long response %d lost active generation = %+v", i, got)
		}
		if got := sources[i].snapshot(); got.Setups != 0 || got.Flows != 1 {
			t.Fatalf("ACK transition %d source = %+v", i, got)
		}
	}
	nextSetup, err := flows[0].beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.beginClientSelection("api", nextSetup, "least-connections").next(); !errors.Is(err, ErrGenerationConnectionCapacity) {
		t.Fatalf("same-source generation cap = %v", err)
	}
	nextSetup.release()
	for i := range leases {
		leases[i].release()
		flows[i].release()
	}
	for _, source := range sources {
		if got := source.snapshot(); got.Flows != 0 || got.Setups != 0 || got.Requests != 0 {
			t.Fatalf("source leak = %+v", got)
		}
	}
	if got := r.trafficSnapshot(g, sources[0]); got.Pending != 0 || got.Active != 0 || got.Inflight != 0 {
		t.Fatalf("exact generation leak = %+v", got)
	}
}

func TestMeshGroupSelectionAndAvailabilityEdges(t *testing.T) {
	r := NewRegistry(4, 4, 4)
	a := currentMeshClient(t, r, "a", "api")
	b := currentMeshClient(t, r, "b", "api")
	c := currentMeshClient(t, r, "c", "api") // current but not published ready
	source := newTCPSource(config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 1, MaxTCPConnectionsPerGeneration: 4, MaxPendingTCPSetupsPerGeneration: 4}, 0)
	flow, err := source.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	defer flow.release()
	setup, err := flow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	defer setup.release()
	if _, err := r.beginClientSelection("api", setup, "round-robin").next(); !errors.Is(err, ErrNoMeshCandidate) {
		t.Fatalf("unpublished current selectable: %v", err)
	}
	partial := readyForwarding
	for _, missing := range []string{"session", "declaration", "version", "l4"} {
		partial = readyForwarding
		switch missing {
		case "session":
			partial.SessionReady = false
		case "declaration":
			partial.DeclarationReady = false
		case "version":
			partial.VersionEligible = false
		case "l4":
			partial.L4Healthy = false
		}
		if !r.PublishForwarding(a, partial) || r.GroupAvailability("api") {
			t.Fatalf("missing %s readiness made group available", missing)
		}
	}
	changed := r.GroupChanges()
	if !r.PublishForwarding(a, readyForwarding) || !r.GroupAvailability("api") {
		t.Fatal("first qualified instance missing")
	}
	select {
	case <-changed:
	default:
		t.Fatal("0->1 group edge not notified")
	}
	changed = r.GroupChanges()
	r.PublishForwarding(a, readyForwarding)
	r.PublishForwarding(b, readyForwarding)
	if !r.PublishInstanceL7Health(a, InstanceL7Healthy) || !r.PublishInstanceL7Health(b, InstanceL7Unknown) {
		t.Fatal("publish independent L7 health")
	}
	select {
	case <-changed:
		t.Fatal("idempotent/additional instance produced group edge")
	default:
	}
	for i, want := range []*Generation{a, b, a} {
		lease, err := r.beginClientSelection("api", setup, "round-robin").next()
		if err != nil || lease.Generation() != want {
			t.Fatalf("RR request %d = (%v, %v), want %v", i, lease.Generation(), err, want)
		}
		lease.release()
	}
	held, err := r.beginClientSelection("api", setup, "least-connections").next()
	if err != nil || held.Generation() != a {
		t.Fatalf("least-load first = (%v, %v)", held.Generation(), err)
	}
	second, err := r.beginClientSelection("api", setup, "least-connections").next()
	if err != nil || second.Generation() != b {
		t.Fatalf("least-load second = (%v, %v)", second.Generation(), err)
	}
	held.release()
	second.release()
	r.PublishInstanceL7Health(a, InstanceL7Failed)
	lease, err := r.beginClientSelection("api", setup, "least-connections").next()
	if err != nil || lease.Generation() != b || lease.FailOpen() {
		t.Fatalf("single L7 failure = (%v, %v)", lease, err)
	}
	lease.release()
	r.PublishInstanceL7Health(b, InstanceL7Failed)
	lease, err = r.beginClientSelection("api", setup, "least-connections").next()
	if err != nil || !lease.FailOpen() || !r.GroupAvailability("api") {
		t.Fatalf("all failed failopen = (%v, %v)", lease, err)
	}
	lease.release()
	select {
	case <-changed:
		t.Fatal("L7 failure changed L4/group availability")
	default:
	}
	r.BeginRetire(a)
	if !r.GroupAvailability("api") {
		t.Fatal("retiring one instance withdrew healthy group")
	}
	r.BeginRetire(b)
	if r.GroupAvailability("api") {
		t.Fatal("last qualified instance did not withdraw group")
	}
	select {
	case <-changed:
	default:
		t.Fatal("last-instance group edge not notified")
	}
	r.Release(a)
	r.Release(b)
	r.Release(c)
	if len(r.roundRobin) != 0 || len(r.groupAvailable) != 0 {
		t.Fatalf("retired group keys retained: RR=%d, available=%d", len(r.roundRobin), len(r.groupAvailable))
	}
}

func TestMeshExactStaleReleaseProbeAndStreamLimitRetry(t *testing.T) {
	r := NewRegistry(4, 4, 4)
	a := currentMeshClient(t, r, "a", "api")
	b := currentMeshClient(t, r, "b", "api")
	r.PublishForwarding(a, readyForwarding)
	r.PublishForwarding(b, readyForwarding)
	probeSource := newTCPSource(config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 1, MaxTCPConnectionsPerGeneration: 1, MaxPendingTCPSetupsPerGeneration: 1}, 1)
	probe, err := r.reserveProbe(a, probeSource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.reserveProbe(a, probeSource); !errors.Is(err, ErrGenerationConnectionCapacity) {
		t.Fatalf("probe exact cap = %v", err)
	}
	if got := probeSource.snapshot(); got.Flows != 0 || got.Setups != 0 || got.Requests != 0 {
		t.Fatalf("probe used source flow/setup = %+v", got)
	}
	if probe.acknowledge() || !probe.acknowledgeProbe() {
		t.Fatal("probe ACK did not use its explicit no-source path")
	}
	if got := r.trafficSnapshot(a, probeSource); got.Pending != 0 || got.Active != 1 {
		t.Fatalf("probe ACK generation = %+v", got)
	}
	probe.release()
	recoveredProbe, err := r.reserveProbe(a, probeSource)
	if err != nil {
		t.Fatalf("probe capacity not recovered: %v", err)
	}
	recoveredProbe.release()
	flow, err := probeSource.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	request, err := flow.beginRequest()
	if err != nil {
		t.Fatal(err)
	}
	setup, err := flow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	selection := r.beginClientSelection("api", setup, "round-robin")
	var first *Generation
	opened, err := selection.open(context.Background(), func(g *Generation) (func(), error) {
		if got := probeSource.snapshot(); got.Flows != 1 || got.Setups != 1 || got.Requests != 1 {
			t.Fatalf("source lease changed across candidate retry: %+v", got)
		}
		if first == nil {
			first = g
			return nil, &quic.StreamLimitReachedError{}
		}
		return nil, nil
	})
	if err != nil || opened.Generation() == first {
		t.Fatalf("bounded stream-limit retry = (%v, %v)", opened, err)
	}
	if got := r.trafficSnapshot(first, probeSource); got.Pending != 0 || got.Active != 0 || got.Inflight != 0 {
		t.Fatalf("failed candidate lease retained = %+v", got)
	}
	if !opened.acknowledge() {
		t.Fatal("target ACK failed")
	}
	if got := probeSource.snapshot(); got.Flows != 1 || got.Setups != 0 || got.Requests != 1 {
		t.Fatalf("ACK released active flow/request: %+v", got)
	}
	opened.release()
	request.release()
	flow.release()
	flow, err = probeSource.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	setup, err = flow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.beginClientSelection("api", setup, "round-robin").open(context.Background(), func(*Generation) (func(), error) {
		return nil, &quic.StreamLimitReachedError{}
	})
	setup.release()
	flow.release()
	if !errors.Is(err, ErrLocalTransportBudget) {
		t.Fatalf("all physical stream credits exhausted = %v", err)
	}
	oldLease, err := r.reserveProbe(a, probeSource)
	if err != nil {
		t.Fatal(err)
	}
	r.BeginRetire(a)
	r.Release(a)
	newA := currentMeshClient(t, r, "a", "api")
	r.PublishForwarding(newA, readyForwarding)
	flow, err = probeSource.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	setup, err = flow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	current, err := r.beginClientSelection("api", setup, "least-connections").next()
	if err != nil {
		t.Fatal(err)
	}
	if !oldLease.release() || !current.release() || probe.release() {
		t.Fatal("stale release changed successor accounting")
	}
	setup.release()
	flow.release()
	if got := r.trafficSnapshot(newA, probeSource); got.Pending != 0 || got.Active != 0 {
		t.Fatalf("successor changed by stale release = %+v", got)
	}
}

func TestMeshBusinessACKRequiresExactLiveSetup(t *testing.T) {
	r := NewRegistry(2, 2, 2)
	g := currentMeshClient(t, r, "a", "api")
	r.PublishForwarding(g, readyForwarding)
	source := newTCPSource(config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 2, MaxTCPConnectionsPerGeneration: 2, MaxPendingTCPSetupsPerGeneration: 2}, 0)
	flow, err := source.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	first, err := flow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	other, err := flow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	lease, err := r.beginClientSelection("api", first, "least-connections").next()
	if err != nil {
		t.Fatal(err)
	}
	first.release()
	if lease.acknowledge() {
		t.Fatal("released exact setup produced an ACK transition")
	}
	if got := source.snapshot(); got.Setups != 1 {
		t.Fatalf("ACK consumed another setup token: %+v", got)
	}
	if _, err := r.beginClientSelection("api", first, "least-connections").next(); !errors.Is(err, ErrSourceSetupReleased) {
		t.Fatalf("released setup reserved another generation = %v", err)
	}
	if got := r.trafficSnapshot(g, source); got.Pending != 1 || got.Active != 0 {
		t.Fatalf("failed ACK changed generation state = %+v", got)
	}
	lease.release()
	other.release()
	flow.release()
	if got := source.snapshot(); got.Flows != 0 || got.Setups != 0 {
		t.Fatalf("business setup leak = %+v", got)
	}
}

func TestMeshACKAndReselectionClaimExactSetup(t *testing.T) {
	for _, ackFirst := range []bool{false, true} {
		name := "release_first"
		if ackFirst {
			name = "ack_first"
		}
		t.Run(name, func(t *testing.T) {
			r := NewRegistry(2, 2, 2)
			a := currentMeshClient(t, r, "a", "api")
			b := currentMeshClient(t, r, "b", "api")
			r.PublishForwarding(a, readyForwarding)
			r.PublishForwarding(b, readyForwarding)
			source := newTCPSource(config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 1, MaxTCPConnectionsPerGeneration: 1, MaxPendingTCPSetupsPerGeneration: 1}, 0)
			flow, err := source.beginFlow()
			if err != nil {
				t.Fatal(err)
			}
			defer flow.release()
			setup, err := flow.beginSetup()
			if err != nil {
				t.Fatal(err)
			}
			defer setup.release()
			selection := r.beginClientSelection("api", setup, "round-robin")
			first, err := selection.next()
			if err != nil || first.Generation() != a {
				t.Fatalf("first candidate = (%v, %v)", first, err)
			}
			if !ackFirst {
				if !first.release() {
					t.Fatal("release did not win")
				}
				second, err := selection.next()
				if err != nil || second.Generation() != b {
					t.Fatalf("replacement candidate = (%v, %v)", second, err)
				}
				if first.acknowledge() || !second.acknowledge() {
					t.Fatal("stale ACK consumed replacement setup")
				}
				second.release()
			} else {
				// Hold source settlement so release and reselect run after ACK's Registry transition.
				source.mu.Lock()
				ackDone := make(chan bool, 1)
				go func() { ackDone <- first.acknowledge() }()
				claimed := false
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					if r.mu.TryLock() {
						claimed = first.state != generationLeasePending
						r.mu.Unlock()
					}
					if claimed {
						break
					}
					time.Sleep(time.Millisecond)
				}
				if !claimed {
					source.mu.Unlock()
					t.Fatal("ACK did not reach Registry transition")
				}
				released := first.release()
				replacement, nextErr := selection.next()
				source.mu.Unlock()
				if replacement != nil {
					replacement.release()
				}
				if !released || !errors.Is(nextErr, ErrSourceSetupReleased) {
					t.Fatalf("ACK winner allowed setup reuse: released %t, next %v", released, nextErr)
				}
				select {
				case acknowledged := <-ackDone:
					if !acknowledged {
						t.Fatal("claimed ACK failed after release")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("ACK did not settle source setup")
				}
			}
			if got := source.snapshot(); got.Flows != 1 || got.Setups != 0 {
				t.Fatalf("source after ACK/reselection = %+v", got)
			}
			for _, g := range []*Generation{a, b} {
				if got := r.trafficSnapshot(g, source); got.Pending != 0 || got.Active != 0 || got.Inflight != 0 {
					t.Fatalf("generation %s retained traffic: %+v", g.instanceID, got)
				}
			}
		})
	}
}

func TestMeshStreamLimitPreservesTerminalAndCapacityErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"stop", ErrRegistryStopped},
		{"source_closed", ErrSourceClosed},
		{"setup_released", ErrSourceSetupReleased},
		{"canceled", context.Canceled},
		{"generation_connection_capacity", ErrGenerationConnectionCapacity},
		{"generation_setup_capacity", ErrGenerationSetupCapacity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry(2, 2, 2)
			a := currentMeshClient(t, r, "a", "api")
			b := currentMeshClient(t, r, "b", "api")
			r.PublishForwarding(a, readyForwarding)
			r.PublishForwarding(b, readyForwarding)
			capacity := config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 1, MaxTCPConnectionsPerGeneration: 1, MaxPendingTCPSetupsPerGeneration: 1}
			if tc.name == "generation_setup_capacity" {
				capacity.MaxTCPConnectionsPerGeneration = 2
			}
			source := newTCPSource(capacity, 0)
			flow, err := source.beginFlow()
			if err != nil {
				t.Fatal(err)
			}
			setup, err := flow.beginSetup()
			if err != nil {
				t.Fatal(err)
			}
			selection := r.beginClientSelection("api", setup, "round-robin")
			ctx, cancel := context.WithCancel(context.Background())
			var held *generationLease
			attempts := 0
			_, err = selection.open(ctx, func(g *Generation) (func(), error) {
				attempts++
				if g != a {
					t.Fatalf("unexpected stream attempt on %s", g.instanceID)
				}
				switch tc.name {
				case "stop":
					r.Stop()
				case "source_closed":
					source.close()
				case "setup_released":
					setup.release()
				case "canceled":
					cancel()
				case "generation_connection_capacity", "generation_setup_capacity":
					var reserveErr error
					held, reserveErr = r.reserveProbe(b, source)
					if reserveErr != nil {
						t.Fatal(reserveErr)
					}
				}
				return nil, &quic.StreamLimitReachedError{}
			})
			if held != nil {
				held.release()
			}
			cancel()
			setup.release()
			flow.release()
			if !errors.Is(err, tc.want) || attempts != 1 {
				t.Fatalf("stream limit then %s = (%v, %d attempts), want %v", tc.name, err, attempts, tc.want)
			}
			if got := source.snapshot(); got.Flows != 0 || got.Setups != 0 {
				t.Fatalf("stream limit retained source lease: %+v", got)
			}
		})
	}
}

func TestMeshFixedPeerHopDoesNotCountTerminalInstance(t *testing.T) {
	r := NewRegistry(2, 2, 2)
	peer, err := r.BeginOutboundPeer("edge-b", Owner{})
	if err != nil || !r.CommitOutboundPeer(peer) || !r.PublishForwarding(peer, readyForwarding) {
		t.Fatalf("publish peer = (%v, %v)", peer, err)
	}
	client := currentMeshClient(t, r, "a", "api")
	r.PublishForwarding(client, readyForwarding)
	source := newTCPSource(config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 1, MaxTCPConnectionsPerGeneration: 1, MaxPendingTCPSetupsPerGeneration: 1}, 0)
	flow, err := source.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	setup, err := flow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.reserveNextHop(client, setup); !errors.Is(err, ErrNoMeshCandidate) {
		t.Fatalf("client bypassed terminal selector = %v", err)
	}
	lease, err := r.reserveNextHop(peer, setup)
	if err != nil || !lease.acknowledge() {
		t.Fatalf("peer next-hop ACK = (%v, %v)", lease, err)
	}
	if got := r.trafficSnapshot(peer, source); got.Pending != 0 || got.Active != 1 || got.Inflight != 0 {
		t.Fatalf("peer counted terminal instance load = %+v", got)
	}
	lease.release()
	flow.release()
}

func TestMeshGroupChangesWakeOnEmptyStop(t *testing.T) {
	r := NewRegistry(1, 1, 1)
	changed := r.GroupChanges()
	r.Stop()
	select {
	case <-changed:
	default:
		t.Fatal("Stop did not wake group watcher")
	}
	select {
	case <-r.GroupChanges():
	default:
		t.Fatal("post-Stop group watcher did not remain closed")
	}
}

func TestMeshSelectionClosesLateOpenedStream(t *testing.T) {
	r := NewRegistry(1, 1, 1)
	g := currentMeshClient(t, r, "a", "api")
	r.PublishForwarding(g, readyForwarding)
	source := newTCPSource(config.MeshTCPCapacity{MaxTCPConnections: 1, MaxPendingTCPSetups: 1, MaxTCPConnectionsPerGeneration: 1, MaxPendingTCPSetupsPerGeneration: 1}, 0)
	flow, err := source.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	setup, err := flow.beginSetup()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	closed := false
	lease, err := r.beginClientSelection("api", setup, "least-connections").open(ctx, func(*Generation) (func(), error) {
		cancel()
		return func() { closed = true }, nil
	})
	if lease != nil || !errors.Is(err, context.Canceled) || !closed {
		t.Fatalf("late open = lease %v, err %v, cleaned %t", lease, err, closed)
	}
	if got := r.trafficSnapshot(g, source); got.Pending != 0 || got.Active != 0 || got.Inflight != 0 {
		t.Fatalf("late open retained generation = %+v", got)
	}
	setup.release()
	flow.release()
}

func TestMeshAdmissionLockSeparationAndConcurrentStop(t *testing.T) {
	r := NewRegistry(64, 4, 4)
	g := currentMeshClient(t, r, "a", "api")
	source := newTCPSource(config.MeshTCPCapacity{MaxTCPConnections: 8, MaxPendingTCPSetups: 8, MaxTCPConnectionsPerGeneration: 8, MaxPendingTCPSetupsPerGeneration: 8}, 8)
	source.mu.Lock()
	done := make(chan struct{})
	go func() {
		r.PublishForwarding(g, readyForwarding)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		source.mu.Unlock()
		t.Fatal("Registry publication waited on source lock")
	}
	source.mu.Unlock()
	flow, err := source.beginFlow()
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	done = make(chan struct{})
	go func() {
		setup, err := flow.beginSetup()
		if err == nil {
			setup.release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		r.mu.Unlock()
		t.Fatal("source setup waited on Registry lock")
	}
	r.mu.Unlock()
	flow.release()
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			flow, err := source.beginFlow()
			if err != nil {
				return
			}
			defer flow.release()
			request, err := flow.beginRequest()
			if err != nil {
				return
			}
			defer request.release()
			setup, err := flow.beginSetup()
			if err != nil {
				return
			}
			defer setup.release()
			lease, err := r.beginClientSelection("api", setup, "least-connections").next()
			if err != nil {
				return
			}
			defer lease.release()
			lease.acknowledge()
		})
	}
	source.close()
	r.BeginRetire(g)
	r.Stop()
	wg.Wait()
	if got := source.snapshot(); got.Flows != 0 || got.Setups != 0 || got.Requests != 0 || got.FlowHigh > 8 || got.SetupHigh > 8 || got.RequestHigh > 8 {
		t.Fatalf("source race accounting = %+v", got)
	}
	if got := r.trafficSnapshot(g, source); got.Pending != 0 || got.Active != 0 || got.Inflight != 0 || got.High > 8 || got.SetupHigh > 8 {
		t.Fatalf("generation race accounting = %+v", got)
	}
}

func TestMeshServerOwnsSourceConfig(t *testing.T) {
	files := testMeshMaterial(t)
	conf := testMeshServerConfig("edge-a", reserveMeshUDPAddress(t), config.ClientAuthMethodToken, files, nil, 2)
	conf.Tunnel.Capacity = config.MeshTCPCapacity{MaxTCPConnections: 3}
	limit := 2
	conf.Ingress.Listeners = []config.MeshIngressListener{{Address: "127.0.0.1:8080", Protocol: config.MeshIngressProtocolHTTP, Capacity: config.MeshTCPCapacity{MaxTCPConnections: 1}, MaxInflightRequests: &limit}}
	server, err := NewServer(conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop mesh server: %v", err)
		}
	})
	limit = 99
	conf.Tunnel.Capacity.MaxTCPConnections = 99
	conf.Ingress.Listeners[0].Capacity.MaxTCPConnections = 99
	if got := server.tunnelSource.capacity.MaxTCPConnections; got != 3 {
		t.Fatalf("tunnel source did not retain owned config: %d", got)
	}
	if got := server.ingressSources[0].capacity.MaxTCPConnections; got != 1 || server.ingressSources[0].requestLimit != 2 || *server.config.Ingress.Listeners[0].MaxInflightRequests != 2 {
		t.Fatalf("ingress source was aliased to caller: capacity %d, request %d", got, server.ingressSources[0].requestLimit)
	}
}
