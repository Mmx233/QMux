package protocol

import (
	stdjson "encoding/json"
	"testing"

	"github.com/Mmx233/QMux/config"
	jsoniter "github.com/json-iterator/go"
)

// BenchmarkJSONLibraryComparison compares standard library vs json-iterator
func BenchmarkJSONLibraryComparison(b *testing.B) {
	// Test payloads
	registerMsg := RegisterMsg{
		ClientID:     "client-12345",
		Version:      ProtocolVersion,
		Capabilities: config.DefaultCapabilities,
	}
	newConnMsg := NewConnMsg{
		ConnID:     12345,
		Protocol:   "tcp",
		SourceAddr: "192.168.1.100:54321",
		DestPort:   8080,
		Timestamp:  1234567890,
	}
	heartbeatMsg := HeartbeatMsg{
		Timestamp: 1234567890,
	}

	// Pre-encode payloads
	registerJSON, _ := stdjson.Marshal(registerMsg)
	newConnJSON, _ := stdjson.Marshal(newConnMsg)
	heartbeatJSON, _ := stdjson.Marshal(heartbeatMsg)

	// Configure json-iterator for maximum compatibility
	jsoniterStd := jsoniter.ConfigCompatibleWithStandardLibrary

	b.Run("StdLib/Unmarshal/Register", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var msg RegisterMsg
			_ = stdjson.Unmarshal(registerJSON, &msg)
		}
	})

	b.Run("JsonIter/Unmarshal/Register", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var msg RegisterMsg
			_ = jsoniterStd.Unmarshal(registerJSON, &msg)
		}
	})

	b.Run("StdLib/Unmarshal/NewConn", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var msg NewConnMsg
			_ = stdjson.Unmarshal(newConnJSON, &msg)
		}
	})

	b.Run("JsonIter/Unmarshal/NewConn", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var msg NewConnMsg
			_ = jsoniterStd.Unmarshal(newConnJSON, &msg)
		}
	})

	b.Run("StdLib/Unmarshal/Heartbeat", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var msg HeartbeatMsg
			_ = stdjson.Unmarshal(heartbeatJSON, &msg)
		}
	})

	b.Run("JsonIter/Unmarshal/Heartbeat", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var msg HeartbeatMsg
			_ = jsoniterStd.Unmarshal(heartbeatJSON, &msg)
		}
	})

	// Also benchmark Marshal for completeness
	b.Run("StdLib/Marshal/Register", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = stdjson.Marshal(registerMsg)
		}
	})

	b.Run("JsonIter/Marshal/Register", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = jsoniterStd.Marshal(registerMsg)
		}
	})

	b.Run("StdLib/Marshal/NewConn", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = stdjson.Marshal(newConnMsg)
		}
	})

	b.Run("JsonIter/Marshal/NewConn", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = jsoniterStd.Marshal(newConnMsg)
		}
	})
}
