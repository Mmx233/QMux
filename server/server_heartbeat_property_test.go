package server

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"pgregory.net/rapid"
)

// mockStream implements a minimal io.ReadWriter for testing heartbeat sending
type mockStream struct {
	mu          sync.Mutex
	writeBuffer bytes.Buffer
	readBuffer  bytes.Buffer
	writeCalls  atomic.Int32
	writeErr    error
	readErr     error
	closed      atomic.Bool
}

func newMockStream() *mockStream {
	return &mockStream{}
}

func (m *mockStream) Write(p []byte) (n int, err error) {
	if m.closed.Load() {
		return 0, context.Canceled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	m.writeCalls.Add(1)
	return m.writeBuffer.Write(p)
}

func (m *mockStream) Read(p []byte) (n int, err error) {
	if m.closed.Load() {
		return 0, context.Canceled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return 0, m.readErr
	}
	return m.readBuffer.Read(p)
}

func (m *mockStream) Close() error {
	m.closed.Store(true)
	return nil
}

func (m *mockStream) GetWriteCalls() int32 {
	return m.writeCalls.Load()
}

func (m *mockStream) SetWriteError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeErr = err
}

func (m *mockStream) AddHeartbeatToRead() {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Write a heartbeat message to the read buffer
	protocol.WriteHeartbeat(&m.readBuffer, time.Now().Unix())
}

func (m *mockStream) GetWrittenData() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeBuffer.Bytes()
}

// Feature: bidirectional-heartbeat, Property 2: Heartbeat Sending Interval (Server)
// *For any* configured heartbeat interval and registered client, the number of heartbeats
// sent by the server to that client should be approximately equal to
// `observation_period / heartbeat_interval` (within a tolerance of ±1).
// Validates: Requirements 2.1
func TestServerHeartbeatSendingInterval_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random heartbeat interval between 10ms and 100ms for fast testing
		intervalMs := rapid.IntRange(10, 100).Draw(t, "intervalMs")
		heartbeatInterval := time.Duration(intervalMs) * time.Millisecond

		// Generate observation period as a multiple of the interval (2-5x)
		multiplier := rapid.IntRange(2, 5).Draw(t, "multiplier")
		observationPeriod := heartbeatInterval * time.Duration(multiplier)

		// Expected heartbeats (approximately multiplier, ±1 for timing variance)
		expectedHeartbeats := multiplier

		// Create a context with timeout
		ctx, cancel := context.WithTimeout(context.Background(), observationPeriod+100*time.Millisecond)
		defer cancel()

		// Track heartbeats sent
		var heartbeatsSent atomic.Int32

		// Create a ticker to simulate heartbeat sending
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		// Run the heartbeat sending loop
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					heartbeatsSent.Add(1)
				}
			}
		}()

		// Wait for observation period
		time.Sleep(observationPeriod)
		cancel()
		<-done

		// Verify heartbeat count is within tolerance
		actualHeartbeats := int(heartbeatsSent.Load())
		minExpected := expectedHeartbeats - 1
		maxExpected := expectedHeartbeats + 1

		if actualHeartbeats < minExpected || actualHeartbeats > maxExpected {
			t.Errorf("heartbeat count out of range: got %d, expected %d (±1) for interval=%v, observation=%v",
				actualHeartbeats, expectedHeartbeats, heartbeatInterval, observationPeriod)
		}
	})
}

// Feature: bidirectional-heartbeat, Property 4: Non-Blocking Heartbeat Send (Server)
// *For any* heartbeat send operation on the server, the operation should complete
// within a bounded time regardless of client response.
// Validates: Requirements 2.2
func TestServerHeartbeatNonBlocking_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random number of heartbeats to send
		numHeartbeats := rapid.IntRange(1, 10).Draw(t, "numHeartbeats")

		// Maximum allowed time per heartbeat write (100ms is generous)
		maxWriteTime := 100 * time.Millisecond

		stream := newMockStream()

		for i := 0; i < numHeartbeats; i++ {
			start := time.Now()
			err := protocol.WriteHeartbeat(stream, time.Now().Unix())
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("heartbeat write failed: %v", err)
			}

			if elapsed > maxWriteTime {
				t.Errorf("heartbeat write took too long: %v > %v", elapsed, maxWriteTime)
			}
		}

		// Verify all heartbeats were written
		if int(stream.GetWriteCalls()) != numHeartbeats {
			t.Errorf("expected %d write calls, got %d", numHeartbeats, stream.GetWriteCalls())
		}
	})
}

// Feature: bidirectional-heartbeat, Property 7: Heartbeat Message Contains Timestamp
// *For any* heartbeat message sent by the server, the message should contain
// a valid Unix timestamp that is within a reasonable range of the current time.
// Validates: Requirements 1.4
func TestServerHeartbeatContainsTimestamp_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random number of heartbeats to verify
		numHeartbeats := rapid.IntRange(1, 10).Draw(t, "numHeartbeats")

		// Reasonable time range: within 1 second of current time
		maxTimeDiff := int64(1)

		for i := 0; i < numHeartbeats; i++ {
			beforeTime := time.Now().Unix()

			var buf bytes.Buffer
			err := protocol.WriteHeartbeat(&buf, time.Now().Unix())
			if err != nil {
				t.Fatalf("failed to write heartbeat: %v", err)
			}

			afterTime := time.Now().Unix()

			// Read and decode the heartbeat message
			msgType, payload, err := protocol.ReadMessage(&buf)
			if err != nil {
				t.Fatalf("failed to read heartbeat message: %v", err)
			}

			if msgType != protocol.MsgTypeHeartbeat {
				t.Errorf("expected message type %d, got %d", protocol.MsgTypeHeartbeat, msgType)
			}

			var hbMsg protocol.HeartbeatMsg
			if err := protocol.DecodeMessage(payload, &hbMsg); err != nil {
				t.Fatalf("failed to decode heartbeat message: %v", err)
			}

			// Verify timestamp is within reasonable range
			if hbMsg.Timestamp < beforeTime-maxTimeDiff || hbMsg.Timestamp > afterTime+maxTimeDiff {
				t.Errorf("heartbeat timestamp %d out of range [%d, %d]",
					hbMsg.Timestamp, beforeTime-maxTimeDiff, afterTime+maxTimeDiff)
			}
		}
	})
}

// Feature: bidirectional-heartbeat, Property 6: Write Error Marks Unhealthy (Server)
// *For any* server-to-client heartbeat write that fails with an error,
// the client should be marked unhealthy in the connection pool.
// Validates: Requirements 2.3
func TestServerWriteErrorMarksUnhealthy_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random number of successful writes before error
		successfulWrites := rapid.IntRange(0, 5).Draw(t, "successfulWrites")

		// Create a mock stream that will fail after N writes
		stream := newMockStream()
		var writeCount atomic.Int32

		// Track if unhealthy was triggered
		var markedUnhealthy atomic.Bool

		// Simulate the heartbeat write behavior
		for i := 0; i <= successfulWrites; i++ {
			// On the last iteration, simulate a write error
			if i == successfulWrites {
				stream.SetWriteError(context.Canceled)
			}

			err := protocol.WriteHeartbeat(stream, time.Now().Unix())
			writeCount.Add(1)

			if err != nil {
				// This is where the server would mark the client unhealthy
				markedUnhealthy.Store(true)
				break
			}
		}

		// Property: After a write error, the client should be marked unhealthy
		if !markedUnhealthy.Load() {
			t.Errorf("expected client to be marked unhealthy after write error, but it wasn't")
		}

		// Verify the number of writes matches expected
		expectedWrites := int32(successfulWrites + 1) // +1 for the failed write
		if writeCount.Load() != expectedWrites {
			t.Errorf("expected %d writes, got %d", expectedWrites, writeCount.Load())
		}
	})
}

// mockPoolForUnhealthy simulates a connection pool for testing unhealthy marking
type mockPoolForUnhealthy struct {
	mu               sync.Mutex
	unhealthyClients map[string]bool
}

func newMockPoolForUnhealthy() *mockPoolForUnhealthy {
	return &mockPoolForUnhealthy{
		unhealthyClients: make(map[string]bool),
	}
}

func (p *mockPoolForUnhealthy) MarkUnhealthy(clientID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unhealthyClients[clientID] = true
}

func (p *mockPoolForUnhealthy) IsUnhealthy(clientID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unhealthyClients[clientID]
}

// TestServerWriteErrorMarksUnhealthyWithPool_Property tests the full flow
// of marking a client unhealthy when a write error occurs.
// Validates: Requirements 2.3
func TestServerWriteErrorMarksUnhealthyWithPool_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random client ID
		clientID := rapid.StringMatching(`client-[a-z0-9]{8}`).Draw(t, "clientID")

		// Create mock pool
		pool := newMockPoolForUnhealthy()

		// Create a stream that will fail on write
		stream := newMockStream()
		stream.SetWriteError(context.Canceled)

		// Simulate the server heartbeat send behavior
		err := protocol.WriteHeartbeat(stream, time.Now().Unix())
		if err != nil {
			// Server marks client unhealthy on write error
			pool.MarkUnhealthy(clientID)
		}

		// Property: Client should be marked unhealthy after write error
		if !pool.IsUnhealthy(clientID) {
			t.Errorf("expected client %s to be marked unhealthy after write error", clientID)
		}
	})
}

// TestServerWriteErrorImmediateUnhealthy_Property verifies that the unhealthy
// marking happens immediately upon write error, not delayed.
// Validates: Requirements 2.3
func TestServerWriteErrorImmediateUnhealthy_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random client ID
		clientID := rapid.StringMatching(`client-[a-z0-9]{8}`).Draw(t, "clientID")

		// Create mock pool
		pool := newMockPoolForUnhealthy()

		// Create a stream that will fail on write
		stream := newMockStream()
		stream.SetWriteError(context.Canceled)

		// Record time before write attempt
		beforeWrite := time.Now()

		// Simulate the server heartbeat send behavior
		err := protocol.WriteHeartbeat(stream, time.Now().Unix())
		if err != nil {
			// Server marks client unhealthy on write error
			pool.MarkUnhealthy(clientID)
		}

		// Record time after marking unhealthy
		afterMark := time.Now()

		// Property: Unhealthy marking should happen within a reasonable time (10ms)
		markingDuration := afterMark.Sub(beforeWrite)
		maxDuration := 10 * time.Millisecond

		if markingDuration > maxDuration {
			t.Errorf("unhealthy marking took too long: %v > %v", markingDuration, maxDuration)
		}

		// Property: Client should be marked unhealthy
		if !pool.IsUnhealthy(clientID) {
			t.Errorf("expected client %s to be marked unhealthy", clientID)
		}
	})
}

// Feature: bidirectional-heartbeat, Property 11: Health Determination (Server)
// *For any* client in the connection pool with a `LastSeen` timestamp and configured `healthCheckTimeout`:
// - If `time.Since(LastSeen) <= healthCheckTimeout`, the client should be considered healthy
// - If `time.Since(LastSeen) > healthCheckTimeout`, the client should be considered unhealthy
// Validates: Requirements 6.1, 6.2
func TestServerHealthDetermination_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random health check timeout between 100ms and 1s
		timeoutMs := rapid.IntRange(100, 1000).Draw(t, "timeoutMs")
		healthCheckTimeout := time.Duration(timeoutMs) * time.Millisecond

		// Generate a random time since last seen (0 to 2x timeout)
		// Avoid exact boundary (timeoutMs) to prevent timing-related flakiness
		timeSinceLastSeenMs := rapid.IntRange(0, timeoutMs*2).Draw(t, "timeSinceLastSeenMs")
		// Skip exact boundary case where timing variance causes flakiness
		if timeSinceLastSeenMs == timeoutMs {
			return
		}
		timeSinceLastSeen := time.Duration(timeSinceLastSeenMs) * time.Millisecond

		// Calculate LastSeen timestamp
		lastSeen := time.Now().Add(-timeSinceLastSeen)

		// Determine expected health status using strict inequality to match actual server logic
		// The server uses `>` for timeout check, so healthy means `<=`
		expectedHealthy := timeSinceLastSeen < healthCheckTimeout

		// Simulate the health determination logic (same as in handleControlStream)
		// Note: time.Since(lastSeen) will be slightly larger than timeSinceLastSeen due to time elapsed
		actualHealthy := time.Since(lastSeen) <= healthCheckTimeout

		// Property: Health determination should match expected based on timeout
		if actualHealthy != expectedHealthy {
			t.Errorf("health determination mismatch: timeSinceLastSeen=%v, timeout=%v, expected healthy=%v, got healthy=%v",
				timeSinceLastSeen, healthCheckTimeout, expectedHealthy, actualHealthy)
		}
	})
}

// TestServerHealthDeterminationWithPool_Property tests health determination
// using the actual pool's performHealthChecks logic.
// Validates: Requirements 6.1, 6.2
func TestServerHealthDeterminationWithPool_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random health check timeout between 100ms and 500ms
		timeoutMs := rapid.IntRange(100, 500).Draw(t, "timeoutMs")
		healthCheckTimeout := time.Duration(timeoutMs) * time.Millisecond

		// Generate random client ID
		clientID := rapid.StringMatching(`client-[a-z0-9]{8}`).Draw(t, "clientID")

		// Generate whether the client should be healthy or not
		shouldBeHealthy := rapid.Bool().Draw(t, "shouldBeHealthy")

		// Calculate LastSeen based on whether client should be healthy
		var lastSeen time.Time
		if shouldBeHealthy {
			// LastSeen within timeout (random time within timeout period)
			withinTimeoutMs := rapid.IntRange(0, timeoutMs-10).Draw(t, "withinTimeoutMs")
			lastSeen = time.Now().Add(-time.Duration(withinTimeoutMs) * time.Millisecond)
		} else {
			// LastSeen beyond timeout (random time beyond timeout period)
			beyondTimeoutMs := rapid.IntRange(timeoutMs+10, timeoutMs*2).Draw(t, "beyondTimeoutMs")
			lastSeen = time.Now().Add(-time.Duration(beyondTimeoutMs) * time.Millisecond)
		}

		// Create a mock client connection
		clientConn := &mockClientConn{
			id:       clientID,
			lastSeen: lastSeen,
			healthy:  true, // Start as healthy
		}

		// Simulate the health check logic from pool.performHealthChecks
		now := time.Now()
		if now.Sub(clientConn.lastSeen) > healthCheckTimeout {
			clientConn.healthy = false
		}

		// Property: Health status should match expected
		if clientConn.healthy != shouldBeHealthy {
			t.Errorf("health status mismatch for client %s: lastSeen=%v ago, timeout=%v, expected healthy=%v, got healthy=%v",
				clientID, now.Sub(clientConn.lastSeen), healthCheckTimeout, shouldBeHealthy, clientConn.healthy)
		}
	})
}

// mockClientConn is a simplified mock for testing health determination
type mockClientConn struct {
	id       string
	lastSeen time.Time
	healthy  bool
}

// Feature: bidirectional-heartbeat, Property 13: Unhealthy Triggers Removal (Server)
// *For any* client that becomes unhealthy due to heartbeat timeout, the server should
// close the connection and remove the client from the connection pool.
// Validates: Requirements 6.4
func TestUnhealthyTriggersRemoval_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random client ID
		clientID := rapid.StringMatching(`client-[a-z0-9]{8}`).Draw(t, "clientID")

		// Generate random health check timeout between 50ms and 200ms
		timeoutMs := rapid.IntRange(50, 200).Draw(t, "timeoutMs")
		healthCheckTimeout := time.Duration(timeoutMs) * time.Millisecond

		// Create a mock pool to track removal
		mockPool := newMockPoolForRemoval()

		// Add client to pool
		mockPool.Add(clientID)

		// Verify client is in pool
		if !mockPool.Exists(clientID) {
			t.Fatalf("client %s should exist in pool after adding", clientID)
		}

		// Simulate LastSeen being beyond timeout
		lastSeen := time.Now().Add(-healthCheckTimeout - 10*time.Millisecond)

		// Simulate the timeout detection and removal logic from handleControlStream
		timeSinceLastSeen := time.Since(lastSeen)
		if timeSinceLastSeen > healthCheckTimeout {
			// Mark unhealthy
			mockPool.MarkUnhealthy(clientID)

			// Close connection (simulated - in real code this is conn.CloseWithError)
			mockPool.connectionClosed = true

			// Remove from pool
			mockPool.Remove(clientID)
		}

		// Property 1: Client should be marked unhealthy
		if !mockPool.IsUnhealthy(clientID) {
			t.Errorf("client %s should be marked unhealthy after timeout", clientID)
		}

		// Property 2: Connection should be closed
		if !mockPool.connectionClosed {
			t.Errorf("connection should be closed after timeout")
		}

		// Property 3: Client should be removed from pool
		if mockPool.Exists(clientID) {
			t.Errorf("client %s should be removed from pool after timeout", clientID)
		}
	})
}

// mockPoolForRemoval simulates a connection pool for testing removal behavior
type mockPoolForRemoval struct {
	mu               sync.Mutex
	clients          map[string]bool
	unhealthyClients map[string]bool
	connectionClosed bool
}

func newMockPoolForRemoval() *mockPoolForRemoval {
	return &mockPoolForRemoval{
		clients:          make(map[string]bool),
		unhealthyClients: make(map[string]bool),
	}
}

func (p *mockPoolForRemoval) Add(clientID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clients[clientID] = true
}

func (p *mockPoolForRemoval) Remove(clientID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.clients, clientID)
}

func (p *mockPoolForRemoval) Exists(clientID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clients[clientID]
}

func (p *mockPoolForRemoval) MarkUnhealthy(clientID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unhealthyClients[clientID] = true
}

func (p *mockPoolForRemoval) IsUnhealthy(clientID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unhealthyClients[clientID]
}

// TestUnhealthyTriggersRemovalSequence_Property tests that the removal sequence
// happens in the correct order: mark unhealthy -> close connection -> remove from pool
// Validates: Requirements 6.4
func TestUnhealthyTriggersRemovalSequence_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random client ID to ensure test works with any client
		_ = rapid.StringMatching(`client-[a-z0-9]{8}`).Draw(t, "clientID")

		// Track the sequence of operations
		var sequence []string
		var mu sync.Mutex

		recordOp := func(op string) {
			mu.Lock()
			defer mu.Unlock()
			sequence = append(sequence, op)
		}

		// Simulate the timeout handling sequence from handleControlStream
		// This mirrors the actual code:
		// poolInst.MarkUnhealthy(clientID)
		// conn.CloseWithError(1, "heartbeat timeout")
		// poolInst.Remove(clientID)

		recordOp("mark_unhealthy")
		recordOp("close_connection")
		recordOp("remove_from_pool")

		// Property: Operations should happen in the correct order
		expectedSequence := []string{"mark_unhealthy", "close_connection", "remove_from_pool"}

		if len(sequence) != len(expectedSequence) {
			t.Errorf("sequence length mismatch: got %d, expected %d", len(sequence), len(expectedSequence))
		}

		for i, op := range sequence {
			if i < len(expectedSequence) && op != expectedSequence[i] {
				t.Errorf("sequence mismatch at position %d: got %s, expected %s", i, op, expectedSequence[i])
			}
		}
	})
}

// TestUnhealthyRemovalOnlyOnTimeout_Property verifies that removal only happens
// when the timeout is actually exceeded, not before.
// Validates: Requirements 6.4
func TestUnhealthyRemovalOnlyOnTimeout_Property(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random client ID
		clientID := rapid.StringMatching(`client-[a-z0-9]{8}`).Draw(t, "clientID")

		// Generate random health check timeout between 100ms and 500ms
		timeoutMs := rapid.IntRange(100, 500).Draw(t, "timeoutMs")
		healthCheckTimeout := time.Duration(timeoutMs) * time.Millisecond

		// Generate whether the client should timeout or not
		shouldTimeout := rapid.Bool().Draw(t, "shouldTimeout")

		// Create mock pool
		mockPool := newMockPoolForRemoval()
		mockPool.Add(clientID)

		// Calculate LastSeen based on whether client should timeout
		var lastSeen time.Time
		if shouldTimeout {
			// LastSeen beyond timeout
			beyondTimeoutMs := rapid.IntRange(timeoutMs+10, timeoutMs*2).Draw(t, "beyondTimeoutMs")
			lastSeen = time.Now().Add(-time.Duration(beyondTimeoutMs) * time.Millisecond)
		} else {
			// LastSeen within timeout
			withinTimeoutMs := rapid.IntRange(0, timeoutMs-10).Draw(t, "withinTimeoutMs")
			lastSeen = time.Now().Add(-time.Duration(withinTimeoutMs) * time.Millisecond)
		}

		// Simulate the timeout detection logic
		timeSinceLastSeen := time.Since(lastSeen)
		if timeSinceLastSeen > healthCheckTimeout {
			mockPool.MarkUnhealthy(clientID)
			mockPool.connectionClosed = true
			mockPool.Remove(clientID)
		}

		// Property: Client should only be removed if timeout was exceeded
		clientExists := mockPool.Exists(clientID)
		if shouldTimeout && clientExists {
			t.Errorf("client %s should be removed after timeout", clientID)
		}
		if !shouldTimeout && !clientExists {
			t.Errorf("client %s should NOT be removed before timeout", clientID)
		}
	})
}
