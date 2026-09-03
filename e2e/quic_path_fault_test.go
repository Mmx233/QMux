package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/protocol"
)

const (
	pathHeartbeatInterval = 250 * time.Millisecond
	pathHealthTimeout     = 10 * time.Second
	pathBlackholeWindow   = 2 * pathHeartbeatInterval
	pathRecoveryBound     = 3 * time.Second
	pathRebindBound       = 6 * time.Second
	pathStableWindow      = 2 * pathHeartbeatInterval
	pathTestTimeout       = 75 * time.Second
	pathProxyWriteTimeout = pathHeartbeatInterval
)

const (
	dropClientToServer uint32 = 1 << iota
	dropServerToClient
)

type pathProxyBinding struct {
	conn      *net.UDPConn
	done      chan struct{}
	retired   atomic.Bool
	forwarded atomic.Uint64
}

func (b *pathProxyBinding) sourcePort() int {
	return b.conn.LocalAddr().(*net.UDPAddr).Port
}

type pathFaultProxySnapshot struct {
	ActiveBindings      int
	ActiveSourcePort    int
	Bindings            int
	RetiredBindings     int
	FrontReaders        int64
	BindingReaders      int64
	ClientToServerDrops uint64
	ServerToClientDrops uint64
	SizeDrops           uint64
}

type pathFaultProxy struct {
	front      *net.UDPConn
	serverAddr *net.UDPAddr
	started    chan struct{}
	terminal   chan error

	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	active    *pathProxyBinding
	bindings  map[*pathProxyBinding]struct{}
	usedPorts map[int]struct{}
	stopped   bool
	wg        sync.WaitGroup

	clientMu   sync.RWMutex
	clientAddr *net.UDPAddr
	frontWrite sync.Mutex

	faults              atomic.Uint32
	maxServerPacket     atomic.Int64
	clientToServerDrops atomic.Uint64
	serverToClientDrops atomic.Uint64
	sizeDrops           atomic.Uint64
	frontReaders        atomic.Int64
	bindingReaders      atomic.Int64

	firstErrOnce sync.Once
	firstErrMu   sync.Mutex
	firstErr     error
}

type pathFaultProxyRun struct {
	*pathFaultProxy
	run *faultRun
}

type pathProxyRebind struct {
	old     *pathProxyBinding
	current *pathProxyBinding
	oldPort int
}

func newPathFaultProxy(serverAddress string) (*pathFaultProxy, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", serverAddress)
	if err != nil {
		return nil, fmt.Errorf("resolve proxy server address: %w", err)
	}
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("listen proxy front: %w", err)
	}
	upstream, err := net.DialUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, serverAddr)
	if err != nil {
		_ = front.Close()
		return nil, fmt.Errorf("dial proxy upstream: %w", err)
	}
	binding := &pathProxyBinding{conn: upstream, done: make(chan struct{})}
	port := binding.sourcePort()
	return &pathFaultProxy{
		front:      front,
		serverAddr: serverAddr,
		started:    make(chan struct{}),
		terminal:   make(chan error, 1),
		active:     binding,
		bindings:   map[*pathProxyBinding]struct{}{binding: {}},
		usedPorts: map[int]struct{}{
			port: {},
		},
	}, nil
}

func startPathFaultProxy(
	t *testing.T,
	parent context.Context,
	serverAddress string,
	timeline *faultTimeline,
) *pathFaultProxyRun {
	t.Helper()
	proxy, err := newPathFaultProxy(serverAddress)
	if err != nil {
		t.Fatalf("create path fault proxy: %v", err)
	}
	run := startFaultRun(parent, "QUIC path fault proxy", timeline, proxy.start)
	select {
	case <-proxy.started:
		return &pathFaultProxyRun{pathFaultProxy: proxy, run: run}
	case <-run.exited:
		t.Fatalf("path fault proxy exited during startup: %v", run.exitErr)
	case <-parent.Done():
		t.Fatalf("context ended during path fault proxy startup: %v", context.Cause(parent))
	}
	return nil
}

func (p *pathFaultProxy) start(parent context.Context) error {
	p.mu.Lock()
	p.ctx, p.cancel = context.WithCancel(parent)
	p.startBindingReaderLocked(p.active)
	p.wg.Add(1)
	p.frontReaders.Add(1)
	go p.readFront()
	close(p.started)
	p.mu.Unlock()

	select {
	case <-p.ctx.Done():
	case <-p.terminal:
	}

	p.mu.Lock()
	p.stopped = true
	p.cancel()
	_ = p.front.Close()
	for binding := range p.bindings {
		binding.retired.Store(true)
		_ = binding.conn.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()

	p.firstErrMu.Lock()
	err := p.firstErr
	p.firstErrMu.Unlock()
	if err != nil {
		return err
	}
	return context.Cause(p.ctx)
}

func (p *pathFaultProxy) fail(err error) {
	p.firstErrOnce.Do(func() {
		p.firstErrMu.Lock()
		p.firstErr = err
		p.firstErrMu.Unlock()
		p.cancel()
		select {
		case p.terminal <- err:
		default:
		}
	})
}

func (p *pathFaultProxy) expectedClose(err error, binding *pathProxyBinding) bool {
	return errors.Is(err, net.ErrClosed) || p.ctx.Err() != nil || binding != nil && binding.retired.Load()
}

func (p *pathFaultProxy) rememberClient(addr *net.UDPAddr) bool {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	if p.clientAddr == nil {
		copied := *addr
		copied.IP = append(net.IP(nil), addr.IP...)
		p.clientAddr = &copied
		return true
	}
	return p.clientAddr.String() == addr.String()
}

func (p *pathFaultProxy) client() *net.UDPAddr {
	p.clientMu.RLock()
	defer p.clientMu.RUnlock()
	if p.clientAddr == nil {
		return nil
	}
	copied := *p.clientAddr
	copied.IP = append(net.IP(nil), p.clientAddr.IP...)
	return &copied
}

func (p *pathFaultProxy) readFront() {
	defer p.wg.Done()
	defer p.frontReaders.Add(-1)
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := p.front.ReadFromUDP(buf)
		if err != nil {
			if !p.expectedClose(err, nil) {
				p.fail(fmt.Errorf("read proxy front: %w", err))
			}
			return
		}
		if !p.rememberClient(addr) {
			p.fail(fmt.Errorf("proxy received a second client address %s", addr))
			return
		}
		if p.faults.Load()&dropClientToServer != 0 {
			p.clientToServerDrops.Add(1)
			continue
		}

		p.mu.Lock()
		if p.stopped || p.active == nil {
			p.mu.Unlock()
			return
		}
		binding := p.active
		err = binding.conn.SetWriteDeadline(time.Now().Add(pathProxyWriteTimeout))
		if err == nil {
			_, err = binding.conn.Write(buf[:n])
		}
		p.mu.Unlock()
		if err != nil {
			if !p.expectedClose(err, binding) {
				p.fail(fmt.Errorf("write proxy upstream: %w", err))
			}
			return
		}
	}
}

func (p *pathFaultProxy) startBindingReaderLocked(binding *pathProxyBinding) {
	p.wg.Add(1)
	p.bindingReaders.Add(1)
	go p.readBinding(binding)
}

func (p *pathFaultProxy) readBinding(binding *pathProxyBinding) {
	defer p.wg.Done()
	defer p.bindingReaders.Add(-1)
	defer close(binding.done)
	buf := make([]byte, 64*1024)
	for {
		n, err := binding.conn.Read(buf)
		if err != nil {
			if !p.expectedClose(err, binding) {
				p.fail(fmt.Errorf("read proxy upstream: %w", err))
			}
			return
		}
		if binding.retired.Load() {
			continue
		}
		if p.faults.Load()&dropServerToClient != 0 {
			p.serverToClientDrops.Add(1)
			continue
		}
		if limit := p.maxServerPacket.Load(); limit > 0 && int64(n) > limit {
			p.sizeDrops.Add(1)
			continue
		}
		clientAddr := p.client()
		if clientAddr == nil {
			p.fail(errors.New("proxy received an upstream packet before learning the client address"))
			return
		}
		p.frontWrite.Lock()
		err = p.front.SetWriteDeadline(time.Now().Add(pathProxyWriteTimeout))
		if err == nil {
			_, err = p.front.WriteToUDP(buf[:n], clientAddr)
		}
		p.frontWrite.Unlock()
		if err != nil {
			if !p.expectedClose(err, binding) {
				p.fail(fmt.Errorf("write proxy front: %w", err))
			}
			return
		}
		binding.forwarded.Add(1)
	}
}

func (p *pathFaultProxy) dialUniqueBindingLocked() (*pathProxyBinding, error) {
	for range 16 {
		conn, err := net.DialUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, p.serverAddr)
		if err != nil {
			return nil, fmt.Errorf("dial rebound proxy upstream: %w", err)
		}
		binding := &pathProxyBinding{conn: conn, done: make(chan struct{})}
		port := binding.sourcePort()
		if _, used := p.usedPorts[port]; !used {
			p.usedPorts[port] = struct{}{}
			return binding, nil
		}
		_ = conn.Close()
	}
	return nil, errors.New("could not allocate a distinct proxy source port")
}

func (p *pathFaultProxy) rebind() (*pathProxyRebind, error) {
	p.mu.Lock()
	if p.stopped || p.ctx == nil || p.ctx.Err() != nil {
		p.mu.Unlock()
		return nil, errors.New("path fault proxy is stopped")
	}
	if len(p.bindings) != 1 {
		p.mu.Unlock()
		return nil, errors.New("path fault proxy already has a retired binding")
	}
	newBinding, err := p.dialUniqueBindingLocked()
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	p.bindings[newBinding] = struct{}{}
	p.startBindingReaderLocked(newBinding)
	oldBinding := p.active
	oldPort := oldBinding.sourcePort()
	p.active = newBinding
	oldBinding.retired.Store(true)
	p.mu.Unlock()
	return &pathProxyRebind{old: oldBinding, current: newBinding, oldPort: oldPort}, nil
}

func (p *pathFaultProxy) finishRebind(rebind *pathProxyRebind, timeout time.Duration) error {
	p.mu.Lock()
	_, owned := p.bindings[rebind.old]
	if !owned {
		p.mu.Unlock()
		return errors.New("retired proxy binding is not owned")
	}
	closeErr := rebind.old.conn.Close()
	p.mu.Unlock()
	if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		return fmt.Errorf("close retired proxy binding: %w", closeErr)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-rebind.old.done:
		p.mu.Lock()
		delete(p.bindings, rebind.old)
		p.mu.Unlock()
		return nil
	case <-p.ctx.Done():
		return context.Cause(p.ctx)
	case <-timer.C:
		err := fmt.Errorf("timed out after %s joining retired proxy binding", timeout)
		p.fail(err)
		return err
	}
}

func (p *pathFaultProxy) setBlackhole(mask uint32) {
	p.faults.Store(mask)
}

func (p *pathFaultProxy) setServerPacketLimit(limit int64) {
	p.maxServerPacket.Store(limit)
}

func (p *pathFaultProxy) clearFaults() {
	p.faults.Store(0)
	p.maxServerPacket.Store(0)
}

func (p *pathFaultProxy) frontPort() int {
	return p.front.LocalAddr().(*net.UDPAddr).Port
}

func (p *pathFaultProxy) snapshot() pathFaultProxySnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot := pathFaultProxySnapshot{
		Bindings:            len(p.bindings),
		FrontReaders:        p.frontReaders.Load(),
		BindingReaders:      p.bindingReaders.Load(),
		ClientToServerDrops: p.clientToServerDrops.Load(),
		ServerToClientDrops: p.serverToClientDrops.Load(),
		SizeDrops:           p.sizeDrops.Load(),
	}
	for binding := range p.bindings {
		if binding.retired.Load() {
			snapshot.RetiredBindings++
		}
	}
	if p.active != nil && !p.active.retired.Load() {
		snapshot.ActiveBindings = 1
		snapshot.ActiveSourcePort = p.active.sourcePort()
	}
	return snapshot
}

type pathStateExpectation struct {
	clientDSendItemsHighWater   int64
	clientDSendBackingHighWater int64
}

type serverDSendObservation struct {
	initialized      bool
	regressed        bool
	itemsHighWater   int64
	backingHighWater int64
}

func (o *serverDSendObservation) matches(items, backing, itemsHighWater, backingHighWater int64) bool {
	bufferSize := int64(protocol.DatagramBufferSize)
	if items < 0 || backing < 0 || itemsHighWater < items || backingHighWater < backing ||
		backing != items*bufferSize ||
		backingHighWater != itemsHighWater*bufferSize {
		return false
	}
	if o.initialized && (itemsHighWater < o.itemsHighWater || backingHighWater < o.backingHighWater) {
		o.regressed = true
	}
	if o.regressed {
		return false
	}
	o.initialized = true
	o.itemsHighWater = itemsHighWater
	o.backingHighWater = backingHighWater
	return items == 0 && backing == 0
}

func TestServerDSendObservationLatchesHighWaterRegression(t *testing.T) {
	bufferSize := int64(protocol.DatagramBufferSize)
	var observation serverDSendObservation
	if !observation.matches(0, 0, 1, bufferSize) {
		t.Fatal("initial quiescent observation did not match")
	}
	if observation.matches(1, bufferSize, 2, 2*bufferSize) {
		t.Fatal("nonzero current ownership matched stable state")
	}
	if observation.matches(0, 0, 1, bufferSize) {
		t.Fatal("regressed quiescent observation matched")
	}
	if !observation.regressed || observation.itemsHighWater != 2 || observation.backingHighWater != 2*bufferSize {
		t.Fatalf("latched observation = %+v, want retained item/backing high-water 2/%d", observation, 2*bufferSize)
	}
}

func pathStateMatches(
	serverRun *faultServerRun,
	clientInstance *client.Client,
	proxy *pathFaultProxy,
	expect pathStateExpectation,
	serverDSend *serverDSendObservation,
) bool {
	serverSnapshot := serverRun.Snapshot()
	clientSnapshot := clientInstance.Snapshot()
	proxySnapshot := proxy.snapshot()
	if !serverSnapshot.Ready || len(serverSnapshot.Routes) != 1 || len(clientSnapshot.Endpoints) != 1 {
		return false
	}
	route := serverSnapshot.Routes[0]
	endpoint := clientSnapshot.Endpoints[0]
	pool := route.PoolCapacity
	udp := route.UDPAdmission
	serverDSendMatches := serverDSend.matches(
		udp.DSendItems,
		udp.DSendBackingBytes,
		udp.DSendItemsHighWater,
		udp.DSendBackingBytesHighWater,
	)
	return route.Listening && route.Ready && route.TCPEligibleClients == 1 && route.UDPEligibleClients == 1 &&
		endpoint.Handshaking == 0 && endpoint.Pending == 0 && endpoint.Registered == 1 && endpoint.Retiring == 0 &&
		endpoint.GenerationHighWater == 1 && endpoint.AccountingFaults == 0 &&
		clientSnapshot.UDPSessions.Current == 1 && clientSnapshot.UDPSessions.Permits == 1 &&
		clientSnapshot.UDPSessions.HighWater == 1 && clientSnapshot.UDPSessions.CapacityDrops == 0 &&
		clientSnapshot.UDPSessions.AccountingFaults == 0 && clientSnapshot.LiveAssemblers == 1 &&
		clientSnapshot.DSend.Workers == 1 && clientSnapshot.DSend.OwnedItems == 0 &&
		clientSnapshot.DSend.OwnedBacking == 0 && clientSnapshot.DSend.OwnedItemsHighWater == expect.clientDSendItemsHighWater &&
		clientSnapshot.DSend.OwnedBackingHighWater == expect.clientDSendBackingHighWater &&
		clientSnapshot.DSend.SendErrors == 0 && clientSnapshot.DSend.FragmentDrops == 0 &&
		clientSnapshot.Fragments.RetainedGroups == 0 && clientSnapshot.Fragments.RetainedBackingBytes == 0 &&
		clientSnapshot.Fragments.GroupCapacityDrops == 0 && clientSnapshot.Fragments.ByteCapacityDrops == 0 &&
		pool.ServerPending == 0 && pool.Reservations == 0 && pool.Registered == 1 && pool.ServerRetiring == 0 &&
		pool.TCPPending == 0 && pool.TCPActive == 1 && pool.UDPSessions == 1 && pool.AccountingFaults == 0 &&
		pool.PendingRegistrations.Current == 0 && pool.PendingRegistrations.CapacityDrops == 0 &&
		pool.ClientGenerations.Current == 1 && pool.ClientGenerations.HighWater == 1 && pool.ClientGenerations.CapacityDrops == 0 &&
		pool.TCPConnectionsPerGeneration.Current == 1 && pool.TCPConnectionsPerGeneration.CapacityDrops == 0 &&
		pool.PendingTCPSetupsPerGeneration.Current == 0 && pool.PendingTCPSetupsPerGeneration.CapacityDrops == 0 &&
		pool.UDPSessionsPerGeneration.Current == 1 && pool.UDPSessionsPerGeneration.HighWater == 1 &&
		pool.UDPSessionsPerGeneration.CapacityDrops == 0 && route.Handshake.Current == 0 && route.Handshake.AccountingFaults == 0 &&
		udp.SessionsCurrent == 1 && udp.SessionPermits == 1 && udp.SessionHighWater == 1 &&
		udp.ListenerCapacityDrops == 0 && udp.GenerationCapacityDrops == 0 && udp.AccountingFaults == 0 &&
		serverDSendMatches && udp.DSendWorkers == 1 &&
		udp.DSendErrors == 0 && udp.QueueFullDrops == 0 && udp.NoEligibleDrops == 0 && udp.FragmentDrops == 0 &&
		udp.DecodeDrops == 0 && udp.UnknownSessionDrops == 0 && udp.PublicWriteDrops == 0 &&
		udp.Fragment.RetainedGroups == 0 && udp.Fragment.RetainedBackingBytes == 0 &&
		udp.Fragment.GroupCapacityDrops == 0 && udp.Fragment.ByteCapacityDrops == 0 &&
		proxySnapshot.ActiveBindings == 1 && proxySnapshot.Bindings == 1 && proxySnapshot.RetiredBindings == 0 &&
		proxySnapshot.FrontReaders == 1 && proxySnapshot.BindingReaders == 1
}

func waitForStablePathState(
	ctx context.Context,
	serverRun *faultServerRun,
	clientInstance *client.Client,
	proxyRun *pathFaultProxyRun,
	expect pathStateExpectation,
	serverDSend *serverDSendObservation,
	runs ...*faultRun,
) error {
	var stableSince time.Time
	return waitForFault(ctx, pathRecoveryBound, func() string {
		return fmt.Sprintf("stable QUIC path state; server=%+v client=%+v proxy=%+v",
			serverRun.Snapshot(), clientInstance.Snapshot(), proxyRun.snapshot())
	}, func(time.Duration) bool {
		if !pathStateMatches(serverRun, clientInstance, proxyRun.pathFaultProxy, expect, serverDSend) {
			stableSince = time.Time{}
			return false
		}
		if stableSince.IsZero() {
			stableSince = time.Now()
		}
		return time.Since(stableSince) >= pathStableWindow
	}, runs...)
}

func probeHeldTCP(conn net.Conn, sequence uint64, timeout time.Duration) error {
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set held TCP deadline: %w", err)
	}
	payload := sequencePayload(sequence)
	if n, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write held TCP: %w", err)
	} else if n != len(payload) {
		return io.ErrShortWrite
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echo); err != nil {
		return fmt.Errorf("read held TCP: %w", err)
	}
	if !bytes.Equal(echo, payload) {
		return fmt.Errorf("held TCP echo mismatch: got %x, want %x", echo, payload)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear held TCP deadline: %w", err)
	}
	return nil
}

func probeExactUDP(conn *net.UDPConn, payload []byte, timeout time.Duration) error {
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set exact UDP deadline: %w", err)
	}
	if n, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write exact UDP: %w", err)
	} else if n != len(payload) {
		return io.ErrShortWrite
	}
	echo := make([]byte, len(payload))
	n, err := conn.Read(echo)
	if err != nil {
		return fmt.Errorf("read exact UDP: %w", err)
	}
	if n != len(payload) || !bytes.Equal(echo[:n], payload) {
		return fmt.Errorf("exact UDP echo mismatch: got %x, want %x", echo[:n], payload)
	}
	return nil
}

func waitFaultWindow(ctx context.Context, started time.Time) error {
	remaining := pathBlackholeWindow - time.Since(started)
	if remaining <= 0 {
		return nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func TestQUICPathFaultMatrix_MTLS(t *testing.T) {
	if testing.Short() {
		t.Skip("STAB-005 fault matrix")
	}
	testCtx, cancelTest := context.WithTimeout(context.Background(), pathTestTimeout)
	defer cancelTest()
	timeline := newFaultTimeline(t, "QUIC path fault matrix")
	certDir := generateTestCertificates(t)
	backendPort := startRestartEchoBackend(t)
	quicPort, trafficPort, releasePorts := reserveRestartServerPorts(t)
	if err := releasePorts(); err != nil {
		t.Fatalf("release path fault server ports: %v", err)
	}

	serverRun := startFaultServer(t, testCtx, "path fault server", newMTLSServerConfig(
		certDir, "both", quicPort, trafficPort, pathHeartbeatInterval, pathHealthTimeout,
	), timeline)
	defer func() {
		if err := serverRun.run.stopAndJoin(3 * time.Second); err != nil {
			t.Errorf("stop path fault server: %v", err)
		}
	}()
	proxyRun := startPathFaultProxy(t, testCtx, fmt.Sprintf("127.0.0.1:%d", quicPort), timeline)
	defer func() {
		proxyRun.clearFaults()
		if err := proxyRun.run.stopAndJoin(3 * time.Second); err != nil {
			t.Errorf("stop path fault proxy: %v", err)
		}
	}()
	clientInstance := newTestClient(t, newMTLSClientConfig(
		certDir, "path-fault-client", backendPort, pathHeartbeatInterval, pathHealthTimeout, proxyRun.frontPort(),
	))
	clientRun := startFaultClient(testCtx, "path fault client", clientInstance, timeline)
	defer func() {
		if err := clientRun.stopAndJoin(3 * time.Second); err != nil {
			t.Errorf("stop path fault client: %v", err)
		}
	}()

	trafficAddress := fmt.Sprintf("127.0.0.1:%d", trafficPort)
	if err := waitForFault(testCtx, 10*time.Second, func() string {
		return fmt.Sprintf("one TCP/UDP eligible route; snapshot=%+v", serverRun.Snapshot())
	}, func(time.Duration) bool {
		snapshot := serverRun.Snapshot()
		return len(snapshot.Routes) == 1 && snapshot.Routes[0].Ready &&
			snapshot.Routes[0].TCPEligibleClients == 1 && snapshot.Routes[0].UDPEligibleClients == 1
	}, serverRun.run, proxyRun.run, clientRun); err != nil {
		t.Fatalf("path fault baseline did not become ready: %v", err)
	}

	udpProbe, err := newSequencedUDPProbe(trafficAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udpProbe.Close() }()
	if err := probeSequencedUDPEventually(testCtx, timeline, udpProbe, pathRecoveryBound, pathProxyWriteTimeout); err != nil {
		t.Fatalf("initial UDP echo: %v", err)
	}
	heldTCP := openVerifiedTCPConnection(t, trafficPort)
	defer func() { _ = heldTCP.Close() }()

	smallState := pathStateExpectation{
		clientDSendItemsHighWater:   1,
		clientDSendBackingHighWater: int64(protocol.DatagramBufferSize),
	}
	var serverDSend serverDSendObservation
	allRuns := []*faultRun{serverRun.run, proxyRun.run, clientRun}
	if err := waitForStablePathState(testCtx, serverRun, clientInstance, proxyRun, smallState, &serverDSend, allRuns...); err != nil {
		t.Fatalf("path fault warm state: %v", err)
	}
	timeline.add("baseline ready: one generation, held TCP, persistent UDP, source port %d", proxyRun.snapshot().ActiveSourcePort)

	blackholes := []struct {
		name string
		mask uint32
	}{
		{name: "client-to-server", mask: dropClientToServer},
		{name: "server-to-client", mask: dropServerToClient},
		{name: "bidirectional", mask: dropClientToServer | dropServerToClient},
	}
	var tcpSequence uint64
	for _, fault := range blackholes {
		tcpSequence++
		if err := probeHeldTCP(heldTCP, tcpSequence, pathBlackholeWindow); err != nil {
			t.Fatalf("%s warm held TCP probe: %v", fault.name, err)
		}
		if _, err := udpProbe.probeNext(pathBlackholeWindow); err != nil {
			t.Fatalf("%s warm UDP probe: %v", fault.name, err)
		}
		before := proxyRun.snapshot()
		started := time.Now()
		proxyRun.setBlackhole(fault.mask)
		_, probeErr := udpProbe.probeNext(pathProxyWriteTimeout)
		if probeErr == nil {
			t.Fatalf("%s blackhole unexpectedly delivered UDP", fault.name)
		}
		if err := waitFaultWindow(testCtx, started); err != nil {
			t.Fatalf("hold %s blackhole: %v", fault.name, err)
		}
		after := proxyRun.snapshot()
		if fault.mask&dropClientToServer != 0 && after.ClientToServerDrops <= before.ClientToServerDrops {
			t.Fatalf("%s blackhole recorded no client-to-server drop: before=%+v after=%+v", fault.name, before, after)
		}
		if fault.mask&dropServerToClient != 0 && after.ServerToClientDrops <= before.ServerToClientDrops {
			t.Fatalf("%s blackhole recorded no server-to-client drop: before=%+v after=%+v", fault.name, before, after)
		}
		proxyRun.clearFaults()
		if err := probeSequencedUDPEventually(testCtx, timeline, udpProbe, pathRecoveryBound, pathProxyWriteTimeout); err != nil {
			t.Fatalf("%s UDP recovery: %v", fault.name, err)
		}
		tcpSequence++
		if err := probeHeldTCP(heldTCP, tcpSequence, pathRecoveryBound); err != nil {
			t.Fatalf("%s held TCP recovery: %v", fault.name, err)
		}
		if err := waitForStablePathState(testCtx, serverRun, clientInstance, proxyRun, smallState, &serverDSend, allRuns...); err != nil {
			t.Fatalf("%s stable recovery: %v", fault.name, err)
		}
		timeline.add("%s blackhole recovered on the original generation", fault.name)
	}

	seenSourcePorts := map[int]struct{}{proxyRun.snapshot().ActiveSourcePort: {}}
	for cycle := 1; cycle <= 3; cycle++ {
		started := time.Now()
		rebind, err := proxyRun.rebind()
		if err != nil {
			t.Fatalf("NAT rebind %d: %v", cycle, err)
		}
		binding, oldPort := rebind.current, rebind.oldPort
		newPort := binding.sourcePort()
		if _, seen := seenSourcePorts[newPort]; seen || newPort == oldPort {
			t.Fatalf("NAT rebind %d reused source port: old=%d new=%d seen=%v", cycle, oldPort, newPort, seenSourcePorts)
		}
		seenSourcePorts[newPort] = struct{}{}
		forwardedBefore := binding.forwarded.Load()
		if err := probeSequencedUDPEventually(testCtx, timeline, udpProbe, pathRebindBound, pathProxyWriteTimeout); err != nil {
			t.Fatalf("NAT rebind %d UDP recovery: %v", cycle, err)
		}
		if binding.forwarded.Load() <= forwardedBefore {
			t.Fatalf("NAT rebind %d received no response on source port %d", cycle, newPort)
		}
		if err := proxyRun.finishRebind(rebind, pathProxyWriteTimeout); err != nil {
			t.Fatalf("NAT rebind %d retire old source port %d: %v", cycle, oldPort, err)
		}
		tcpSequence++
		if err := probeHeldTCP(heldTCP, tcpSequence, pathRecoveryBound); err != nil {
			t.Fatalf("NAT rebind %d held TCP recovery: %v", cycle, err)
		}
		if err := waitForStablePathState(testCtx, serverRun, clientInstance, proxyRun, smallState, &serverDSend, allRuns...); err != nil {
			t.Fatalf("NAT rebind %d stable state: %v", cycle, err)
		}
		timeline.add("NAT rebind %d recovered %d -> %d in %s", cycle, oldPort, newPort, time.Since(started))
	}

	proxyRun.setServerPacketLimit(600)
	if _, err := udpProbe.probeNext(pathBlackholeWindow); err != nil {
		t.Fatalf("small UDP under packet-size fault: %v", err)
	}
	tcpSequence++
	if err := probeHeldTCP(heldTCP, tcpSequence, pathBlackholeWindow); err != nil {
		t.Fatalf("small held TCP under packet-size fault: %v", err)
	}
	largePayload := bytes.Repeat([]byte{0xa5}, protocol.MaxFragPayload+64)
	sizeDropsBefore := proxyRun.snapshot().SizeDrops
	if err := probeExactUDP(udpProbe.conn, largePayload, pathBlackholeWindow); err == nil {
		t.Fatal("large UDP unexpectedly crossed the packet-size fault")
	}
	if err := waitForFault(testCtx, pathRecoveryBound, func() string {
		return fmt.Sprintf("one retained client fragment; client=%+v proxy=%+v", clientInstance.Snapshot(), proxyRun.snapshot())
	}, func(time.Duration) bool {
		snapshot := clientInstance.Snapshot()
		return snapshot.Fragments.RetainedGroups == 1 &&
			snapshot.Fragments.RetainedBackingBytes == int64(protocol.FragmentBufferSize) &&
			proxyRun.snapshot().SizeDrops > sizeDropsBefore
	}, allRuns...); err != nil {
		t.Fatalf("size-selective loss evidence: %v", err)
	}
	timeline.add("size-selective loss retained one client fragment")
	proxyRun.clearFaults()
	if err := probeExactUDP(udpProbe.conn, largePayload, pathRecoveryBound); err != nil {
		t.Fatalf("large UDP recovery: %v", err)
	}

	largeState := pathStateExpectation{
		clientDSendItemsHighWater:   2,
		clientDSendBackingHighWater: 2 * int64(protocol.DatagramBufferSize),
	}
	fragmentCleanupBound := 2*protocol.FragmentTimeout + pathHeartbeatInterval
	if err := waitForFault(testCtx, fragmentCleanupBound, func() string {
		return fmt.Sprintf("fragment cleanup; client=%+v", clientInstance.Snapshot())
	}, func(time.Duration) bool {
		snapshot := clientInstance.Snapshot()
		return snapshot.Fragments.RetainedGroups == 0 && snapshot.Fragments.RetainedBackingBytes == 0
	}, allRuns...); err != nil {
		t.Fatalf("incomplete fragment cleanup: %v", err)
	}
	if err := waitForStablePathState(testCtx, serverRun, clientInstance, proxyRun, largeState, &serverDSend, allRuns...); err != nil {
		t.Fatalf("final stable path state: %v", err)
	}
	timeline.add("size-selective fault recovered; fragment state and owned buffers returned")
}
