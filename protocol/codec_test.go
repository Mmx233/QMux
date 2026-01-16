package protocol

import (
	"bytes"
	"testing"

	"github.com/Mmx233/QMux/config"
)

// Payload size constants for benchmarks
const (
	SmallPayloadSize  = 64    // 64 bytes - typical heartbeat
	MediumPayloadSize = 1024  // 1 KB - typical message
	LargePayloadSize  = 65536 // 64 KB - large message
)

// generatePayload creates a byte slice of the specified size
func generatePayload(size int) []byte {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 256)
	}
	return payload
}

// createTestMessage creates a test message struct with a payload of the given size
type testPayload struct {
	Data string `json:"data"`
}

func createTestPayload(size int) testPayload {
	data := make([]byte, size)
	for i := range data {
		data[i] = 'a' + byte(i%26)
	}
	return testPayload{Data: string(data)}
}

// preEncodeMessage encodes a message and returns the bytes for read benchmarks
func preEncodeMessage(msgType byte, payload interface{}) []byte {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, msgType, payload); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// BenchmarkWriteMessage benchmarks message encoding with various payload sizes
func BenchmarkWriteMessage(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"64B", SmallPayloadSize},
		{"1KB", MediumPayloadSize},
		{"64KB", LargePayloadSize},
	}

	for _, s := range sizes {
		b.Run(s.name, func(b *testing.B) {
			payload := createTestPayload(s.size)
			var buf bytes.Buffer
			buf.Grow(s.size + 100) // Pre-allocate buffer

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				buf.Reset()
				if err := WriteMessage(&buf, MsgTypeNewConn, payload); err != nil {
					b.Fatalf("WriteMessage failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkReadMessage benchmarks message decoding with various payload sizes
func BenchmarkReadMessage(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"64B", SmallPayloadSize},
		{"1KB", MediumPayloadSize},
		{"64KB", LargePayloadSize},
	}

	for _, s := range sizes {
		b.Run(s.name, func(b *testing.B) {
			payload := createTestPayload(s.size)
			encoded := preEncodeMessage(MsgTypeNewConn, payload)

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				reader := bytes.NewReader(encoded)
				_, _, err := ReadMessage(reader)
				if err != nil {
					b.Fatalf("ReadMessage failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkDecodeMessage benchmarks JSON unmarshaling of message payloads
func BenchmarkDecodeMessage(b *testing.B) {
	// Create typical message payloads for decoding
	messages := []struct {
		name    string
		msgType byte
		payload interface{}
	}{
		{"Register", MsgTypeRegister, RegisterMsg{
			ClientID:     "client-12345",
			Version:      ProtocolVersion,
			Capabilities: config.DefaultCapabilities,
		}},
		{"NewConn", MsgTypeNewConn, NewConnMsg{
			ConnID:     12345,
			Protocol:   "tcp",
			SourceAddr: "192.168.1.100:54321",
			DestPort:   8080,
			Timestamp:  1234567890,
		}},
		{"Heartbeat", MsgTypeHeartbeat, HeartbeatMsg{
			Timestamp: 1234567890,
		}},
	}

	for _, m := range messages {
		b.Run(m.name, func(b *testing.B) {
			encoded := preEncodeMessage(m.msgType, m.payload)
			// Extract just the JSON payload (skip type byte and length)
			jsonPayload := encoded[5:]

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				var decoded interface{}
				switch m.msgType {
				case MsgTypeRegister:
					decoded = &RegisterMsg{}
				case MsgTypeNewConn:
					decoded = &NewConnMsg{}
				case MsgTypeHeartbeat:
					decoded = &HeartbeatMsg{}
				}
				if err := DecodeMessage(jsonPayload, decoded); err != nil {
					b.Fatalf("DecodeMessage failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkTypedMessages benchmarks each typed message writer function
func BenchmarkTypedMessages(b *testing.B) {
	b.Run("WriteRegister", func(b *testing.B) {
		var buf bytes.Buffer
		buf.Grow(256)

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			buf.Reset()
			if err := WriteRegister(&buf, "client-12345", ProtocolVersion, config.DefaultCapabilities); err != nil {
				b.Fatalf("WriteRegister failed: %v", err)
			}
		}
	})

	b.Run("WriteHeartbeat", func(b *testing.B) {
		var buf bytes.Buffer
		buf.Grow(64)

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			buf.Reset()
			if err := WriteHeartbeat(&buf, 1234567890); err != nil {
				b.Fatalf("WriteHeartbeat failed: %v", err)
			}
		}
	})

	b.Run("WriteNewConn", func(b *testing.B) {
		var buf bytes.Buffer
		buf.Grow(256)

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			buf.Reset()
			if err := WriteNewConn(&buf, 12345, "tcp", "192.168.1.100:54321", 8080, 1234567890); err != nil {
				b.Fatalf("WriteNewConn failed: %v", err)
			}
		}
	})

	b.Run("WriteConnClose", func(b *testing.B) {
		var buf bytes.Buffer
		buf.Grow(128)

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			buf.Reset()
			if err := WriteConnClose(&buf, 12345, "connection closed by peer"); err != nil {
				b.Fatalf("WriteConnClose failed: %v", err)
			}
		}
	})

	b.Run("WriteError", func(b *testing.B) {
		var buf bytes.Buffer
		buf.Grow(128)

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			buf.Reset()
			if err := WriteError(&buf, 500, "internal server error"); err != nil {
				b.Fatalf("WriteError failed: %v", err)
			}
		}
	})
}
