// Package stats holds monitoring data shared by the client and server.
package stats

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/quic-go/quic-go"
)

// TransportSnapshot combines lifetime traffic with current-connection RTT.
// Loss estimates can decrease after late acknowledgments and are gauges.
type TransportSnapshot struct {
	SentBytes, ReceivedBytes, SentPackets, ReceivedPackets uint64
	LostBytes, LostPackets                                 uint64
	Connections                                            int
	SmoothedRTTSum, SmoothedRTTMax, RTTDeviationMax        time.Duration
}

func (s *TransportSnapshot) addTraffic(q quic.ConnectionStats) {
	s.SentBytes += q.BytesSent
	s.ReceivedBytes += q.BytesReceived
	s.SentPackets += q.PacketsSent
	s.ReceivedPackets += q.PacketsReceived
	s.LostBytes += q.BytesLost
	s.LostPackets += q.PacketsLost
}

// Transport transfers final connection counters into lifetime totals on close.
// Its lock protects membership only; no packet processing acquires it.
type Transport struct {
	mu      sync.Mutex
	live    map[*quic.Conn]struct{}
	retired TransportSnapshot
}

func (t *Transport) Add(conn *quic.Conn) {
	if t == nil || conn == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.live == nil {
		t.live = make(map[*quic.Conn]struct{})
	}
	t.live[conn] = struct{}{}
}

// Remove must run after the owning connection has finished closing.
func (t *Transport) Remove(conn *quic.Conn) {
	if t == nil || conn == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.live[conn]; exists {
		t.retired.addTraffic(conn.ConnectionStats())
		delete(t.live, conn)
	}
}

func (t *Transport) Snapshot() TransportSnapshot {
	t.mu.Lock()
	snapshot := t.retired
	live := make([]*quic.Conn, 0, len(t.live))
	for conn := range t.live {
		live = append(live, conn)
	}
	t.mu.Unlock()
	// Capture membership and retired totals together to avoid double counting
	// a connection that retires while its counters are being read.
	for _, conn := range live {
		q := conn.ConnectionStats()
		snapshot.addTraffic(q)
		if conn.Context().Err() == nil {
			snapshot.Connections++
			snapshot.SmoothedRTTSum += q.SmoothedRTT
			snapshot.SmoothedRTTMax = max(snapshot.SmoothedRTTMax, q.SmoothedRTT)
			snapshot.RTTDeviationMax = max(snapshot.RTTDeviationMax, q.MeanDeviation)
		}
	}
	return snapshot
}

type DurationSnapshot struct {
	Count   uint64
	Sum     float64
	Buckets map[float64]uint64
}

type OperationSnapshot struct {
	Attempts uint64
	Results  map[string]uint64
	Duration DurationSnapshot
}

// Operation records connection/setup events, never packet I/O. Independent
// handlers update counters without a shared application lock. The Prometheus
// SDK owns histogram bucketing and accumulation.
type Operation struct {
	attempts atomic.Uint64
	results  [len(operationResults)]atomic.Uint64
	once     sync.Once
	duration prometheus.Histogram
}

var operationResults = [...]string{"success", "error", "timeout", "canceled", "dial_error", "auth_error", "protocol_error", "capacity", "ack_error"}

func (o *Operation) init() {
	o.once.Do(func() {
		o.duration = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "duration_seconds"})
	})
}

func (o *Operation) Start() time.Time {
	o.attempts.Add(1)
	return time.Now()
}

func (o *Operation) Finish(start time.Time, result string) {
	elapsed := time.Since(start).Seconds()
	o.init()
	index := 1 // Unknown results are the bounded generic error label.
	for i, name := range operationResults {
		if name == result {
			index = i
			break
		}
	}
	o.results[index].Add(1)
	o.duration.Observe(elapsed)
}

func (o *Operation) Snapshot() OperationSnapshot {
	o.init()
	snapshot := OperationSnapshot{Attempts: o.attempts.Load()}
	for i, name := range operationResults {
		if count := o.results[i].Load(); count != 0 {
			if snapshot.Results == nil {
				snapshot.Results = make(map[string]uint64)
			}
			snapshot.Results[name] = count
		}
	}
	var metric dto.Metric
	// The SDK's concrete histogram Write always succeeds.
	_ = o.duration.Write(&metric)
	h := metric.GetHistogram()
	if h.GetSampleCount() == 0 {
		return snapshot
	}
	snapshot.Duration = DurationSnapshot{Count: h.GetSampleCount(), Sum: h.GetSampleSum(), Buckets: make(map[float64]uint64)}
	for _, bucket := range h.GetBucket() {
		snapshot.Duration.Buckets[bucket.GetUpperBound()] = bucket.GetCumulativeCount()
	}
	return snapshot
}

// Result returns bounded labels instead of exposing error text as a label.
func Result(err error, failure string) string {
	if err == nil {
		return "success"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	return failure
}
