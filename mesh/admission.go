package mesh

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/Mmx233/QMux/config"
)

var (
	ErrSourceClosed          = errors.New("mesh TCP source is closed")
	ErrSourceFlowCapacity    = errors.New("mesh TCP source connection capacity reached")
	ErrSourceSetupCapacity   = errors.New("mesh TCP source setup capacity reached")
	ErrSourceSetupReleased   = errors.New("mesh TCP source setup has been released")
	ErrSourceRequestCapacity = errors.New("mesh HTTP request capacity reached")
	ErrNotHTTPSource         = errors.New("mesh TCP source has no HTTP request capacity")
	ErrFlowReleased          = errors.New("mesh TCP source flow has been released")
)

// tcpSource owns one ingress listener's or the local tunnel's independent limits.
type tcpSource struct {
	mu           sync.Mutex
	capacity     config.MeshTCPCapacity
	requestLimit int
	closed       atomic.Bool
	flows        int
	setups       int
	requests     int
	flowHigh     int
	setupHigh    int
	requestHigh  int
}

type sourceSnapshot struct {
	Closed                           bool
	Flows, Setups, Requests          int
	FlowHigh, SetupHigh, RequestHigh int
}

func newTCPSource(capacity config.MeshTCPCapacity, requestLimit int) *tcpSource {
	return &tcpSource{capacity: capacity, requestLimit: requestLimit}
}

func (s *tcpSource) snapshot() sourceSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sourceSnapshot{s.closed.Load(), s.flows, s.setups, s.requests, s.flowHigh, s.setupHigh, s.requestHigh}
}

func (s *tcpSource) close() {
	s.mu.Lock()
	s.closed.Store(true)
	s.mu.Unlock()
}

type flowLease struct {
	source   *tcpSource
	released bool // guarded by source.mu
}

func (s *tcpSource) beginFlow() (*flowLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, ErrSourceClosed
	}
	if s.flows >= s.capacity.MaxTCPConnections {
		return nil, ErrSourceFlowCapacity
	}
	s.flows++
	s.flowHigh = max(s.flowHigh, s.flows)
	return &flowLease{source: s}, nil
}

func (f *flowLease) release() bool {
	if f == nil || f.source == nil {
		return false
	}
	s := f.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.released {
		return false
	}
	f.released = true
	s.flows--
	return true
}

type setupLease struct {
	source   *tcpSource
	released atomic.Bool
}

func (f *flowLease) beginSetup() (*setupLease, error) {
	if f == nil || f.source == nil {
		return nil, ErrFlowReleased
	}
	s := f.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, ErrSourceClosed
	}
	if f.released {
		return nil, ErrFlowReleased
	}
	return s.beginSetupLocked()
}

func (s *tcpSource) beginSetupLocked() (*setupLease, error) {
	if s.setups >= s.capacity.MaxPendingTCPSetups {
		return nil, ErrSourceSetupCapacity
	}
	s.setups++
	s.setupHigh = max(s.setupHigh, s.setups)
	return &setupLease{source: s}, nil
}

func (l *setupLease) complete() bool { return l.release() }

func (l *setupLease) claim() bool {
	return l != nil && l.source != nil && l.released.CompareAndSwap(false, true)
}

func (l *setupLease) live() bool {
	if l == nil || l.source == nil {
		return false
	}
	return !l.released.Load()
}

func (l *setupLease) release() bool {
	if l == nil || l.source == nil {
		return false
	}
	s := l.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.released.Swap(true) {
		return false
	}
	s.setups--
	return true
}

func (l *setupLease) settle() {
	s := l.source
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setups--
}

type requestLease struct {
	source   *tcpSource
	released bool // guarded by source.mu
}

func (f *flowLease) beginRequest() (*requestLease, error) {
	if f == nil || f.source == nil {
		return nil, ErrFlowReleased
	}
	s := f.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, ErrSourceClosed
	}
	if f.released {
		return nil, ErrFlowReleased
	}
	if s.requestLimit == 0 {
		return nil, ErrNotHTTPSource
	}
	if s.requests >= s.requestLimit {
		return nil, ErrSourceRequestCapacity
	}
	s.requests++
	s.requestHigh = max(s.requestHigh, s.requests)
	return &requestLease{source: s}, nil
}

func (l *requestLease) release() bool {
	if l == nil || l.source == nil {
		return false
	}
	s := l.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.released {
		return false
	}
	l.released = true
	s.requests--
	return true
}
