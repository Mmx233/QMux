package e2e

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Mmx233/QMux/client"
	"github.com/Mmx233/QMux/config"
)

const (
	testDataSize100MB     = 100 * 1024 * 1024
	testChunkSize         = 1024 * 1024
	udpPacketHeaderSize   = 8
	udpWarmupEpoch        = 0
	udpMeasurementEpoch   = 1
	udpDrainTimeout       = 500 * time.Millisecond
	maxUDPObservedPackets = 1 << 24
)

type udpDeliveryStats struct {
	SentPackets      uint64
	UniquePackets    uint64
	DuplicatePackets uint64
	ReorderedPackets uint64
	SentBytes        int64
	UniqueBytes      int64
}

func (s *udpDeliveryStats) add(other udpDeliveryStats) {
	s.SentPackets += other.SentPackets
	s.UniquePackets += other.UniquePackets
	s.DuplicatePackets += other.DuplicatePackets
	s.ReorderedPackets += other.ReorderedPackets
	s.SentBytes += other.SentBytes
	s.UniqueBytes += other.UniqueBytes
}

func (s *udpDeliveryStats) lossPercent() float64 {
	if s.SentPackets == 0 {
		return 0
	}
	return float64(s.SentPackets-s.UniquePackets) / float64(s.SentPackets) * 100
}

type udpSequenceMeter struct {
	epoch            uint32
	sentPackets      uint64
	seen             []uint64
	uniquePackets    uint64
	duplicatePackets uint64
	reorderedPackets uint64
	uniqueBytes      int64
	maxSequence      uint32
	hasSequence      bool
}

func newUDPSequenceMeter(epoch uint32, sentPackets uint64) (*udpSequenceMeter, error) {
	if epoch == udpWarmupEpoch {
		return nil, errors.New("UDP measurement epoch must be nonzero")
	}
	if sentPackets > uint64(^uint32(0))+1 {
		return nil, fmt.Errorf("UDP packet count %d exceeds sequence space", sentPackets)
	}
	return &udpSequenceMeter{
		epoch:       epoch,
		sentPackets: sentPackets,
		seen:        make([]uint64, (sentPackets+63)/64),
	}, nil
}

func putUDPPacketHeader(packet []byte, epoch, sequence uint32) {
	binary.BigEndian.PutUint32(packet[:4], epoch)
	binary.BigEndian.PutUint32(packet[4:udpPacketHeaderSize], sequence)
}

func readUDPPacketHeader(packet []byte) (uint32, uint32, error) {
	if len(packet) < udpPacketHeaderSize {
		return 0, 0, fmt.Errorf("UDP packet is %d bytes, need at least %d", len(packet), udpPacketHeaderSize)
	}
	return binary.BigEndian.Uint32(packet[:4]), binary.BigEndian.Uint32(packet[4:udpPacketHeaderSize]), nil
}

func (m *udpSequenceMeter) observePacket(packet []byte, packetSize int) error {
	if len(packet) != packetSize {
		return fmt.Errorf("UDP packet size %d, want %d", len(packet), packetSize)
	}
	epoch, sequence, err := readUDPPacketHeader(packet)
	if err != nil {
		return err
	}
	if epoch != m.epoch {
		return nil
	}
	return m.observeSequence(sequence, packetSize)
}

func (m *udpSequenceMeter) observeSequence(sequence uint32, packetSize int) error {
	if uint64(sequence) >= m.sentPackets {
		return fmt.Errorf("UDP sequence %d outside sent range [0,%d)", sequence, m.sentPackets)
	}
	word := sequence / 64
	mask := uint64(1) << (sequence % 64)
	if m.seen[word]&mask != 0 {
		m.duplicatePackets++
		return nil
	}
	m.seen[word] |= mask
	if m.hasSequence && sequence < m.maxSequence {
		m.reorderedPackets++
	}
	if !m.hasSequence || sequence > m.maxSequence {
		m.maxSequence = sequence
		m.hasSequence = true
	}
	m.uniquePackets++
	m.uniqueBytes += int64(packetSize)
	return nil
}

func (m *udpSequenceMeter) stats(sentBytes int64) udpDeliveryStats {
	return udpDeliveryStats{
		SentPackets:      m.sentPackets,
		UniquePackets:    m.uniquePackets,
		DuplicatePackets: m.duplicatePackets,
		ReorderedPackets: m.reorderedPackets,
		SentBytes:        sentBytes,
		UniqueBytes:      m.uniqueBytes,
	}
}

// ============================================
// TCP Benchmarks - Single Connection
// ============================================

func BenchmarkTCP_SingleConn_100MB(b *testing.B) {
	runTCPThroughputBenchmark(b, 1, testDataSize100MB)
}

// ============================================
// TCP Benchmarks - Dual Connections
// ============================================

func BenchmarkTCP_DualConn_100MB(b *testing.B) {
	runTCPThroughputBenchmark(b, 2, testDataSize100MB)
}

// ============================================
// UDP Benchmarks - Single Connection
// ============================================

func BenchmarkUDP_SingleConn_Throughput(b *testing.B) {
	runUDPThroughputBenchmark(b, 1)
}

// ============================================
// UDP Benchmarks - Dual Connections
// ============================================

func BenchmarkUDP_DualConn_Throughput(b *testing.B) {
	runUDPThroughputBenchmark(b, 2)
}

// ============================================
// Core Benchmark Functions
// ============================================

func runTCPThroughputBenchmark(b *testing.B, connCount int, totalSize int64) {
	certDir := generateTestCertificates(b)

	localListener, trafficPort := setupTCPEchoServer(b, certDir)
	closeOnCleanup(b, localListener)

	b.SetBytes(totalSize * 2 * int64(connCount)) // send + receive per connection
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		runTCPTransfer(b, trafficPort, connCount, totalSize)
	}
}

func runTCPTransfer(b *testing.B, trafficPort int, connCount int, totalSize int64) {
	var wg sync.WaitGroup
	errCh := make(chan error, connCount*2)

	for c := range connCount {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort), 10*time.Second)
		if err != nil {
			b.Fatalf("[conn %d] dial failed: %v", c, err)
		}

		wg.Add(2)

		// Sender
		go func(conn net.Conn, connID int) {
			defer wg.Done()
			data := make([]byte, testChunkSize)
			remaining := totalSize
			for remaining > 0 {
				toSend := testChunkSize
				if int64(toSend) > remaining {
					toSend = int(remaining)
				}
				n, err := conn.Write(data[:toSend])
				if err != nil {
					errCh <- fmt.Errorf("[conn %d] send error: %w", connID, err)
					return
				}
				remaining -= int64(n)
			}
		}(conn, c)

		// Receiver
		go func(conn net.Conn, connID int) {
			defer wg.Done()
			defer func() { _ = conn.Close() }()
			buf := make([]byte, testChunkSize)
			remaining := totalSize
			for remaining > 0 {
				n, err := conn.Read(buf)
				if err != nil {
					errCh <- fmt.Errorf("[conn %d] recv error: %w", connID, err)
					return
				}
				remaining -= int64(n)
			}
		}(conn, c)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		b.Fatal(err)
	}
}

func runUDPThroughputBenchmark(b *testing.B, connCount int) {
	certDir := generateTestCertificates(b)

	localConn, trafficPort := setupUDPEchoServer(b, certDir)
	closeOnCleanup(b, localConn)

	const packetSize = 512
	const packetsPerConn = 5000

	b.ResetTimer()

	var total udpDeliveryStats
	var totalSendDuration time.Duration
	for i := 0; i < b.N; i++ {
		firstEpoch := uint64(i)*uint64(connCount) + 1
		stats, sendDuration := runUDPTransferPipelined(b, trafficPort, connCount, packetSize, packetsPerConn, firstEpoch)
		total.add(stats)
		totalSendDuration += sendDuration
	}
	b.StopTimer()

	if b.N > 0 {
		b.SetBytes(total.UniqueBytes / int64(b.N))
		b.ReportMetric(float64(total.DuplicatePackets)/float64(b.N), "duplicates/op")
		b.ReportMetric(float64(total.ReorderedPackets)/float64(b.N), "reordered/op")
	}
	b.ReportMetric(total.lossPercent(), "loss-%")
	b.ReportMetric(float64(udpDrainTimeout)/float64(time.Millisecond), "drain-cutoff-ms")
	if elapsed := totalSendDuration.Seconds(); elapsed > 0 {
		b.ReportMetric(float64(total.SentBytes)*8/elapsed/1e6, "tx-Mbps")
		b.ReportMetric(float64(total.UniqueBytes)*8/elapsed/1e6, "rx-Mbps")
	}
}

type udpTransferResult struct {
	stats        udpDeliveryStats
	sendDuration time.Duration
	err          error
}

func runUDPTransferPipelined(
	b *testing.B,
	trafficPort, connCount, packetSize, packetsPerConn int,
	firstEpoch uint64,
) (udpDeliveryStats, time.Duration) {
	results := make(chan udpTransferResult, connCount)

	for c := range connCount {
		epoch := firstEpoch + uint64(c)
		if epoch > uint64(^uint32(0)) {
			b.Fatalf("UDP benchmark epoch %d exceeds uint32", epoch)
		}
		conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
		if err != nil {
			b.Fatalf("[conn %d] dial failed: %v", c, err)
		}
		go func(conn net.Conn, connID int, epoch uint32) {
			defer func() { _ = conn.Close() }()
			stats, sendDuration, err := measureFixedUDPEcho(conn, packetSize, packetsPerConn, epoch)
			if err != nil {
				err = fmt.Errorf("[conn %d] %w", connID, err)
			}
			results <- udpTransferResult{stats: stats, sendDuration: sendDuration, err: err}
		}(conn, c, uint32(epoch))
	}

	var total udpDeliveryStats
	var sendDuration time.Duration
	for range connCount {
		result := <-results
		if result.err != nil {
			b.Fatal(result.err)
		}
		total.add(result.stats)
		sendDuration = max(sendDuration, result.sendDuration)
	}
	return total, sendDuration
}

func writeUDPPacket(conn net.Conn, packet []byte, epoch, sequence uint32) (int, error) {
	if len(packet) < udpPacketHeaderSize {
		return 0, fmt.Errorf("UDP packet size %d is smaller than header %d", len(packet), udpPacketHeaderSize)
	}
	putUDPPacketHeader(packet, epoch, sequence)
	n, err := conn.Write(packet)
	if err != nil {
		return n, err
	}
	if n != len(packet) {
		return n, fmt.Errorf("short UDP write: wrote %d of %d bytes", n, len(packet))
	}
	return n, nil
}

func measureFixedUDPEcho(conn net.Conn, packetSize, packets int, epoch uint32) (udpDeliveryStats, time.Duration, error) {
	const batchSize = 100

	meter, err := newUDPSequenceMeter(epoch, uint64(packets))
	if err != nil {
		return udpDeliveryStats{}, 0, err
	}
	receiverDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					receiverDone <- nil
					return
				}
				receiverDone <- fmt.Errorf("read UDP echo: %w", err)
				return
			}
			if err := meter.observePacket(buf[:n], packetSize); err != nil {
				receiverDone <- err
				return
			}
		}
	}()

	packet := make([]byte, packetSize)
	var sentBytes int64
	var sendErr error
	sendStart := time.Now()
	for sequence := range packets {
		n, err := writeUDPPacket(conn, packet, epoch, uint32(sequence))
		if err != nil {
			sendErr = fmt.Errorf("write UDP packet %d: %w", sequence, err)
			break
		}
		sentBytes += int64(n)
		if (sequence+1)%batchSize == 0 && sequence+1 < packets {
			time.Sleep(100 * time.Microsecond)
		}
	}
	sendDuration := time.Since(sendStart)

	deadlineErr := conn.SetReadDeadline(time.Now().Add(udpDrainTimeout))
	if deadlineErr != nil {
		_ = conn.Close()
	}
	receiveErr := <-receiverDone
	if sendErr != nil {
		return meter.stats(sentBytes), sendDuration, sendErr
	}
	if deadlineErr != nil {
		return meter.stats(sentBytes), sendDuration, fmt.Errorf("set UDP drain deadline: %w", deadlineErr)
	}
	if receiveErr != nil {
		return meter.stats(sentBytes), sendDuration, receiveErr
	}
	return meter.stats(sentBytes), sendDuration, nil
}

// ============================================
// Comprehensive Speed Report Test
// ============================================

// ThroughputResult holds the result of a throughput test
type ThroughputResult struct {
	Label            string
	Duration         time.Duration
	BytesSent        int64
	BytesReceived    int64
	PacketsSent      uint64
	PacketsReceived  uint64
	DuplicatePackets uint64
	ReorderedPackets uint64
	SendMbps         float64
	RecvMbps         float64
	LossPercent      float64
	DrainCutoff      time.Duration
}

func (r *ThroughputResult) String() string {
	if r.PacketsSent > 0 {
		return fmt.Sprintf("%s: TX %.2f Mbps, RX %.2f Mbps (sent %d, unique %d, loss %.1f%%, duplicate %d, reorder %d, drain cutoff %s)",
			r.Label, r.SendMbps, r.RecvMbps, r.PacketsSent, r.PacketsReceived, r.LossPercent,
			r.DuplicatePackets, r.ReorderedPackets, r.DrainCutoff)
	}
	return fmt.Sprintf("%s: %.2f Mbps", r.Label, r.RecvMbps)
}

func TestSpeedReport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping speed report in short mode")
	}

	t.Log("=== QMux Speed Report (iperf3-style) ===")
	t.Log("")

	// TCP Tests - Raw baseline (no QMux)
	t.Run("TCP", func(t *testing.T) {
		t.Run("Raw_Discard", func(t *testing.T) {
			result := runRawTCPDiscardTest(t, "Raw TCP")
			t.Log(result.String())
		})

		// QMux TCP
		certDir := generateTestCertificates(t)

		localListener, trafficPort := setupTCPDiscardServerForTest(t, certDir)
		closeOnCleanup(t, localListener)

		t.Run("QMux_Discard", func(t *testing.T) {
			result := runQMuxTCPDiscardTest(t, trafficPort, "QMux TCP")
			t.Log(result.String())
		})
	})

	// UDP Tests
	t.Run("UDP", func(t *testing.T) {
		t.Run("Raw_Discard", func(t *testing.T) {
			result := runRawUDPDiscardTest(t, "Raw UDP")
			t.Log(result.String())
		})

		// QMux UDP
		certDir := generateTestCertificates(t)

		localConn, collector, trafficPort := setupUDPDiscardServerForTest(t, certDir)
		closeOnCleanup(t, localConn)

		t.Run("QMux_Discard", func(t *testing.T) {
			result := runQMuxUDPDiscardTest(t, trafficPort, collector, "QMux UDP")
			t.Log(result.String())
		})
	})
}

func dialBenchmarkConn(t *testing.T, network, address string) net.Conn {
	t.Helper()
	var (
		conn net.Conn
		err  error
	)
	if network == "tcp" {
		conn, err = net.DialTimeout(network, address, 5*time.Second)
	} else {
		conn, err = net.Dial(network, address)
	}
	if err != nil {
		t.Fatalf("dial %s benchmark endpoint: %v", network, err)
	}
	closeOnCleanup(t, conn)

	switch typedConn := conn.(type) {
	case *net.TCPConn:
		_ = typedConn.SetWriteBuffer(4 * 1024 * 1024)
		_ = typedConn.SetNoDelay(false)
	case *net.UDPConn:
		_ = typedConn.SetWriteBuffer(16 * 1024 * 1024)
	}
	return conn
}

func measureWrites(conn net.Conn, payloadSize int, warmupDuration, testDuration time.Duration, afterWarmup func()) (int64, time.Duration) {
	data := make([]byte, payloadSize)
	warmupEnd := time.Now().Add(warmupDuration)
	for time.Now().Before(warmupEnd) {
		_, _ = conn.Write(data)
	}
	if afterWarmup != nil {
		afterWarmup()
	}

	var totalBytes int64
	start := time.Now()
	deadline := start.Add(testDuration)
	for time.Now().Before(deadline) {
		n, err := conn.Write(data)
		if err != nil {
			break
		}
		totalBytes += int64(n)
	}
	return totalBytes, time.Since(start)
}

type udpCollectedPackets struct {
	sequences []uint32
	err       error
}

type udpPacketCollector struct {
	conn       net.PacketConn
	epoch      uint32
	packetSize int
	done       chan udpCollectedPackets
}

func startUDPPacketCollector(conn net.PacketConn, epoch uint32, packetSize int) *udpPacketCollector {
	collector := &udpPacketCollector{
		conn:       conn,
		epoch:      epoch,
		packetSize: packetSize,
		done:       make(chan udpCollectedPackets, 1),
	}
	go func() {
		buf := make([]byte, 65535)
		sequences := make([]uint32, 0, 64*1024)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					collector.done <- udpCollectedPackets{sequences: sequences}
					return
				}
				collector.done <- udpCollectedPackets{sequences: sequences, err: fmt.Errorf("read UDP discard backend: %w", err)}
				return
			}
			if n != packetSize {
				collector.done <- udpCollectedPackets{sequences: sequences, err: fmt.Errorf("UDP packet size %d, want %d", n, packetSize)}
				return
			}
			packetEpoch, sequence, err := readUDPPacketHeader(buf[:n])
			if err != nil {
				collector.done <- udpCollectedPackets{sequences: sequences, err: err}
				return
			}
			if packetEpoch != epoch {
				continue
			}
			// ponytail: bounded test-only capture; raise this ceiling if a faster 5-second run reaches it.
			if len(sequences) == maxUDPObservedPackets {
				collector.done <- udpCollectedPackets{sequences: sequences, err: fmt.Errorf("UDP observation limit %d reached", maxUDPObservedPackets)}
				return
			}
			sequences = append(sequences, sequence)
		}
	}()
	return collector
}

func (c *udpPacketCollector) finish(sentPackets uint64, sentBytes int64) (udpDeliveryStats, error) {
	deadlineErr := c.conn.SetReadDeadline(time.Now().Add(udpDrainTimeout))
	if deadlineErr != nil {
		_ = c.conn.Close()
	}
	collected := <-c.done
	if deadlineErr != nil {
		return udpDeliveryStats{}, fmt.Errorf("set UDP backend drain deadline: %w", deadlineErr)
	}
	if collected.err != nil {
		return udpDeliveryStats{}, collected.err
	}
	meter, err := newUDPSequenceMeter(c.epoch, sentPackets)
	if err != nil {
		return udpDeliveryStats{}, err
	}
	for _, sequence := range collected.sequences {
		if err := meter.observeSequence(sequence, c.packetSize); err != nil {
			return udpDeliveryStats{}, err
		}
	}
	return meter.stats(sentBytes), nil
}

func measureUDPDiscardWrites(conn net.Conn, packetSize int, warmupDuration, testDuration time.Duration) (uint64, int64, time.Duration, error) {
	if packetSize < udpPacketHeaderSize {
		return 0, 0, 0, fmt.Errorf("UDP packet size %d is smaller than header %d", packetSize, udpPacketHeaderSize)
	}
	packet := make([]byte, packetSize)
	warmupEnd := time.Now().Add(warmupDuration)
	for sequence := uint64(0); time.Now().Before(warmupEnd); sequence++ {
		if sequence > uint64(^uint32(0)) {
			return 0, 0, 0, errors.New("UDP warmup exhausted sequence space")
		}
		if _, err := writeUDPPacket(conn, packet, udpWarmupEpoch, uint32(sequence)); err != nil {
			return 0, 0, 0, fmt.Errorf("write UDP warmup packet: %w", err)
		}
	}

	var sentPackets uint64
	var sentBytes int64
	start := time.Now()
	deadline := start.Add(testDuration)
	for time.Now().Before(deadline) {
		if sentPackets == maxUDPObservedPackets {
			return sentPackets, sentBytes, time.Since(start), fmt.Errorf("UDP send limit %d reached", maxUDPObservedPackets)
		}
		n, err := writeUDPPacket(conn, packet, udpMeasurementEpoch, uint32(sentPackets))
		if err != nil {
			return sentPackets, sentBytes, time.Since(start), fmt.Errorf("write UDP measurement packet: %w", err)
		}
		sentPackets++
		sentBytes += int64(n)
	}
	return sentPackets, sentBytes, time.Since(start), nil
}

func newThroughputResult(label string, sentBytes, receivedBytes int64, elapsed time.Duration, reportReceived bool) *ThroughputResult {
	sendMbps := float64(sentBytes) * 8 / elapsed.Seconds() / 1000000
	recvMbps := float64(receivedBytes) * 8 / elapsed.Seconds() / 1000000
	result := &ThroughputResult{
		Label:     label,
		Duration:  elapsed,
		BytesSent: sentBytes,
		SendMbps:  sendMbps,
		RecvMbps:  recvMbps,
	}
	if reportReceived {
		result.BytesReceived = receivedBytes
		if sentBytes > 0 {
			result.LossPercent = float64(sentBytes-receivedBytes) / float64(sentBytes) * 100
		}
	}
	return result
}

func newUDPThroughputResult(label string, stats udpDeliveryStats, elapsed time.Duration) *ThroughputResult {
	return &ThroughputResult{
		Label:            label,
		Duration:         elapsed,
		BytesSent:        stats.SentBytes,
		BytesReceived:    stats.UniqueBytes,
		PacketsSent:      stats.SentPackets,
		PacketsReceived:  stats.UniquePackets,
		DuplicatePackets: stats.DuplicatePackets,
		ReorderedPackets: stats.ReorderedPackets,
		SendMbps:         float64(stats.SentBytes) * 8 / elapsed.Seconds() / 1e6,
		RecvMbps:         float64(stats.UniqueBytes) * 8 / elapsed.Seconds() / 1e6,
		LossPercent:      stats.lossPercent(),
		DrainCutoff:      udpDrainTimeout,
	}
}

func TestUDPDeliveryStats(t *testing.T) {
	const packetSize = 64
	type sample struct {
		epoch    uint32
		sequence uint32
	}
	tests := []struct {
		name          string
		meterEpoch    uint32
		sent          uint64
		samples       []sample
		wantUnique    uint64
		wantLoss      float64
		wantDuplicate uint64
		wantReordered uint64
	}{
		{name: "zero loss", sent: 4, samples: []sample{{1, 0}, {1, 1}, {1, 2}, {1, 3}}, wantUnique: 4},
		{name: "25 percent loss", sent: 4, samples: []sample{{1, 0}, {1, 1}, {1, 3}}, wantUnique: 3, wantLoss: 25},
		{name: "60 percent loss", sent: 5, samples: []sample{{1, 0}, {1, 4}}, wantUnique: 2, wantLoss: 60},
		{name: "100 percent loss", sent: 4, wantLoss: 100},
		{
			name: "duplicate and reorder", sent: 4,
			samples:    []sample{{1, 0}, {1, 2}, {1, 1}, {1, 2}, {1, 3}},
			wantUnique: 4, wantDuplicate: 1, wantReordered: 1,
		},
		{
			name: "late warmup is isolated", sent: 2,
			samples:    []sample{{0, 0}, {1, 0}, {1, 1}, {0, 1}},
			wantUnique: 2,
		},
		{
			name: "stale measurement epoch is isolated", meterEpoch: 2, sent: 2,
			samples:    []sample{{1, 0}, {2, 0}, {2, 1}, {1, 1}},
			wantUnique: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meterEpoch := tt.meterEpoch
			if meterEpoch == 0 {
				meterEpoch = udpMeasurementEpoch
			}
			meter, err := newUDPSequenceMeter(meterEpoch, tt.sent)
			if err != nil {
				t.Fatal(err)
			}
			packet := make([]byte, packetSize)
			for _, got := range tt.samples {
				putUDPPacketHeader(packet, got.epoch, got.sequence)
				if err := meter.observePacket(packet, packetSize); err != nil {
					t.Fatal(err)
				}
			}
			stats := meter.stats(int64(tt.sent) * packetSize)
			if stats.UniquePackets != tt.wantUnique || stats.UniqueBytes != int64(tt.wantUnique)*packetSize {
				t.Fatalf("unique delivery = %d packets/%d bytes, want %d packets/%d bytes",
					stats.UniquePackets, stats.UniqueBytes, tt.wantUnique, int64(tt.wantUnique)*packetSize)
			}
			if got := stats.lossPercent(); got != tt.wantLoss {
				t.Fatalf("loss = %.1f%%, want %.1f%%", got, tt.wantLoss)
			}
			if stats.DuplicatePackets != tt.wantDuplicate || stats.ReorderedPackets != tt.wantReordered {
				t.Fatalf("duplicate/reorder = %d/%d, want %d/%d",
					stats.DuplicatePackets, stats.ReorderedPackets, tt.wantDuplicate, tt.wantReordered)
			}
			result := newUDPThroughputResult("UDP", stats, time.Second)
			if result.DrainCutoff != udpDrainTimeout {
				t.Fatalf("drain cutoff = %s, want %s", result.DrainCutoff, udpDrainTimeout)
			}
			if tt.wantUnique == 0 && (result.BytesReceived != 0 || result.RecvMbps != 0 || result.LossPercent != 100) {
				t.Fatalf("zero delivery result = bytes %d, RX %.2f Mbps, loss %.1f%%",
					result.BytesReceived, result.RecvMbps, result.LossPercent)
			}
		})
	}

	meter, err := newUDPSequenceMeter(udpMeasurementEpoch, 1)
	if err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, packetSize)
	putUDPPacketHeader(packet, udpMeasurementEpoch, 1)
	if err := meter.observePacket(packet, packetSize); err == nil {
		t.Fatal("out-of-range UDP sequence was accepted")
	}
}

// runRawTCPDiscardTest measures raw TCP throughput (no QMux)
func runRawTCPDiscardTest(t *testing.T, label string) *ThroughputResult {
	const bufferSize = 128 * 1024 // 128KB like iperf3

	// Create discard server
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	closeOnCleanup(t, listener)

	// Discard server - just read and discard
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, bufferSize)
				for {
					_, err := c.Read(buf)
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	conn := dialBenchmarkConn(t, "tcp", listener.Addr().String())
	totalBytes, elapsed := measureWrites(conn, bufferSize, 500*time.Millisecond, 5*time.Second, nil)
	return newThroughputResult(label, totalBytes, totalBytes, elapsed, false)
}

// runQMuxTCPDiscardTest measures TCP throughput through QMux
func runQMuxTCPDiscardTest(t *testing.T, trafficPort int, label string) *ThroughputResult {
	const bufferSize = 128 * 1024

	conn := dialBenchmarkConn(t, "tcp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
	totalBytes, elapsed := measureWrites(conn, bufferSize, 500*time.Millisecond, 5*time.Second, nil)
	return newThroughputResult(label, totalBytes, totalBytes, elapsed, false)
}

// runRawUDPDiscardTest measures raw UDP throughput (no QMux)
func runRawUDPDiscardTest(t *testing.T, label string) *ThroughputResult {
	const packetSize = 1400

	// Create discard server
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	closeOnCleanup(t, serverConn)

	if udpConn, ok := serverConn.(*net.UDPConn); ok {
		_ = udpConn.SetReadBuffer(16 * 1024 * 1024)
	}

	collector := startUDPPacketCollector(serverConn, udpMeasurementEpoch, packetSize)
	conn := dialBenchmarkConn(t, "udp", serverConn.LocalAddr().String())
	sentPackets, sentBytes, elapsed, sendErr := measureUDPDiscardWrites(conn, packetSize, 500*time.Millisecond, 5*time.Second)
	stats, collectErr := collector.finish(sentPackets, sentBytes)
	if sendErr != nil {
		t.Fatal(sendErr)
	}
	if collectErr != nil {
		t.Fatal(collectErr)
	}
	return newUDPThroughputResult(label, stats, elapsed)
}

// runQMuxUDPDiscardTest measures UDP throughput through QMux
func runQMuxUDPDiscardTest(t *testing.T, trafficPort int, collector *udpPacketCollector, label string) *ThroughputResult {
	const packetSize = 1400

	conn := dialBenchmarkConn(t, "udp", fmt.Sprintf("127.0.0.1:%d", trafficPort))
	sentPackets, sentBytes, elapsed, sendErr := measureUDPDiscardWrites(conn, packetSize, 500*time.Millisecond, 5*time.Second)
	stats, collectErr := collector.finish(sentPackets, sentBytes)
	if sendErr != nil {
		t.Fatal(sendErr)
	}
	if collectErr != nil {
		t.Fatal(collectErr)
	}
	return newUDPThroughputResult(label, stats, elapsed)
}

func setupQMuxEndpoint(
	t testing.TB,
	certDir, protocol, clientID string,
	localPort int,
	timeout, serverStartupDelay, clientStartupDelay time.Duration,
	fatalOnClientError bool,
) int {
	t.Helper()
	quicPort := getFreePort(t)
	trafficPort := getFreePort(t)
	quicConfig := getOptimizedQuicConfig()

	serverConfig := &config.Server{
		Listeners: []config.QuicListener{{
			QuicAddr:    fmt.Sprintf("127.0.0.1:%d", quicPort),
			TrafficAddr: fmt.Sprintf("127.0.0.1:%d", trafficPort),
			Protocol:    protocol,
			Quic:        quicConfig,
		}},
		Auth: config.ServerAuth{Method: "mtls", CACertFile: filepath.Join(certDir, "ca.crt")},
		TLS: config.ServerTLS{
			ServerCertFile: filepath.Join(certDir, "server.crt"),
			ServerKeyFile:  filepath.Join(certDir, "server.key"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	startTestServer(ctx, serverConfig)
	time.Sleep(serverStartupDelay)

	clientConfig := &config.Client{
		ClientID: clientID,
		Server: config.ClientServer{
			Servers: []config.ServerEndpoint{{Address: fmt.Sprintf("127.0.0.1:%d", quicPort), ServerName: "localhost"}},
		},
		Local: config.LocalService{Host: "127.0.0.1", Port: localPort},
		TLS: config.ClientTLS{
			CACertFile:     filepath.Join(certDir, "ca.crt"),
			ClientCertFile: filepath.Join(certDir, "client.crt"),
			ClientKeyFile:  filepath.Join(certDir, "client.key"),
		},
		Quic: quicConfig,
	}

	c, err := client.New(clientConfig)
	if err != nil {
		if fatalOnClientError {
			t.Fatalf("create %s client: %v", protocol, err)
			return 0
		}
		t.Errorf("create %s client: %v", protocol, err)
		return 0
	}
	startTestClient(ctx, c)
	time.Sleep(clientStartupDelay)
	return trafficPort
}

// setupTCPDiscardServerForTest creates a TCP discard server behind QMux
func setupTCPDiscardServerForTest(t *testing.T, certDir string) (net.Listener, int) {
	const bufferSize = 128 * 1024

	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local server: %v", err)
	}
	localAddr := localListener.Addr().(*net.TCPAddr)

	// Discard server
	go func() {
		for {
			conn, err := localListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, bufferSize)
				for {
					_, err := c.Read(buf)
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return localListener, setupQMuxEndpoint(t, certDir, "tcp", "tcp-discard-client", localAddr.Port,
		10*time.Minute, 300*time.Millisecond, 300*time.Millisecond, true)
}

// setupUDPDiscardServerForTest creates a metered UDP discard server behind QMux.
func setupUDPDiscardServerForTest(t *testing.T, certDir string) (net.PacketConn, *udpPacketCollector, int) {
	const packetSize = 1400

	localConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start local UDP server: %v", err)
	}
	localAddr := localConn.LocalAddr().(*net.UDPAddr)

	// Set large buffers
	if udpConn, ok := localConn.(*net.UDPConn); ok {
		_ = udpConn.SetReadBuffer(16 * 1024 * 1024)
	}

	collector := startUDPPacketCollector(localConn, udpMeasurementEpoch, packetSize)
	trafficPort := setupQMuxEndpoint(t, certDir, "udp", "udp-discard-client", localAddr.Port,
		10*time.Minute, 300*time.Millisecond, 300*time.Millisecond, true)
	return localConn, collector, trafficPort
}

// ============================================
// Setup Functions
// ============================================

func setupTCPEchoServer(b *testing.B, certDir string) (net.Listener, int) {
	localListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start local server: %v", err)
	}
	localAddr := localListener.Addr().(*net.TCPAddr)

	serveTCPEcho(localListener)
	return localListener, setupQMuxEndpoint(b, certDir, "tcp", "tcp-bench-client", localAddr.Port,
		10*time.Minute, 300*time.Millisecond, 300*time.Millisecond, true)
}

func setupUDPEchoServer(b *testing.B, certDir string) (net.PacketConn, int) {
	localConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("failed to start local UDP server: %v", err)
	}
	localAddr := localConn.LocalAddr().(*net.UDPAddr)

	serveUDPEcho(localConn, true)
	return localConn, setupQMuxEndpoint(b, certDir, "udp", "udp-bench-client", localAddr.Port,
		10*time.Minute, 300*time.Millisecond, 300*time.Millisecond, true)
}

func getOptimizedQuicConfig() config.Quic {
	return config.Quic{
		InitialStreamReceiveWindow:     16 * 1024 * 1024,
		MaxStreamReceiveWindow:         64 * 1024 * 1024,
		InitialConnectionReceiveWindow: 32 * 1024 * 1024,
		MaxConnectionReceiveWindow:     128 * 1024 * 1024,
		MaxIncomingStreams:             1000,
		Allow0RTT:                      true,
	}
}
