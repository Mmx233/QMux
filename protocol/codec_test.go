package protocol

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/Mmx233/QMux/config"
)

// Payload size constants for benchmarks
const (
	SmallPayloadSize  = 64    // 64 bytes - typical heartbeat
	MediumPayloadSize = 1024  // 1 KB - typical message
	LargePayloadSize  = 65536 // 64 KB - large message
)

func TestBufferSizeConstants(t *testing.T) {
	sizes := []struct {
		name string
		got  int
		want int
	}{
		{"small", SmallBufferSize, 256},
		{"medium", MediumBufferSize, 4096},
		{"large", LargeBufferSize, 65536},
	}
	for _, size := range sizes {
		if size.got != size.want {
			t.Errorf("%s buffer size = %d, want %d", size.name, size.got, size.want)
		}
	}
}

func TestGenericJSONWireContract(t *testing.T) {
	checkWire := func(t *testing.T, wire *bytes.Buffer, want string) {
		t.Helper()
		msgType, got, err := ReadMessage(wire)
		if err != nil {
			t.Fatalf("ReadMessage() error = %v", err)
		}
		if msgType != MsgTypeError {
			t.Fatalf("message type = %#x, want %#x", msgType, MsgTypeError)
		}
		if string(got) != want {
			t.Errorf("JSON = %q, want %q", got, want)
		}
	}

	encodeTests := []struct {
		name    string
		payload any
		want    string
	}{
		{"nil register capabilities", RegisterMsg{}, `{"ClientID":"","Version":"","Capabilities":[]}`},
		{"nil selected capabilities", RegisterAckMsg{}, `{"Success":false,"Message":"","ServerVersion":"","SelectedCapabilities":[]}`},
		{"zero new connection", NewConnMsg{}, `{"ConnID":0,"Protocol":"","SourceAddr":"","DestAddr":"","Timestamp":0}`},
		{"zero connection close", ConnCloseMsg{}, `{"ConnID":0,"Reason":""}`},
		{"nil auth proof", RegisterMsg{Auth: &RegisterAuth{}}, `{"ClientID":"","Version":"","Capabilities":[],"Auth":{"Scheme":"","Proof":""}}`},
	}
	for _, test := range encodeTests {
		t.Run("encode/"+test.name, func(t *testing.T) {
			var wire bytes.Buffer
			if err := WriteMessage(&wire, MsgTypeError, test.payload); err != nil {
				t.Fatalf("WriteMessage() error = %v", err)
			}
			checkWire(t, &wire, test.want)
		})
	}

	t.Run("encode/raw HTML and JS separators", func(t *testing.T) {
		var wire bytes.Buffer
		if err := WriteError(&wire, 0, "<>&\u2028\u2029"); err != nil {
			t.Fatalf("WriteError() error = %v", err)
		}
		checkWire(t, &wire, "{\"Code\":0,\"Message\":\"<>&\u2028\u2029\"}")
	})

	var invalidUTF8Wire bytes.Buffer
	if err := WriteMessage(&invalidUTF8Wire, MsgTypeError, ErrorMsg{Message: string([]byte{0xff})}); err == nil {
		t.Error("WriteMessage() accepted invalid UTF-8")
	}
	if invalidUTF8Wire.Len() != 0 {
		t.Errorf("WriteMessage() wrote %d bytes for invalid UTF-8", invalidUTF8Wire.Len())
	}

	decodeTests := []struct {
		name    string
		payload []byte
		dst     any
		want    any
	}{
		{
			"unknown member ignored",
			[]byte(`{"Timestamp":2,"Extra":true}`),
			&HeartbeatMsg{},
			HeartbeatMsg{Timestamp: 2},
		},
		{
			"wrong case ignored",
			[]byte(`{"timestamp":1}`),
			&HeartbeatMsg{Timestamp: 7},
			HeartbeatMsg{Timestamp: 7},
		},
		{
			"object merge preserves absent and nested fields",
			[]byte(`{"ClientID":"new","Auth":{"Scheme":"new"}}`),
			&RegisterMsg{ClientID: "old", Version: ProtocolVersion, Capabilities: []string{"tcp"}, Auth: &RegisterAuth{Scheme: "old", Proof: []byte("proof")}},
			RegisterMsg{ClientID: "new", Version: ProtocolVersion, Capabilities: []string{"tcp"}, Auth: &RegisterAuth{Scheme: "new", Proof: []byte("proof")}},
		},
		{
			"null zeroes values",
			[]byte(`{"ClientID":null,"Capabilities":null,"Auth":{"Proof":null}}`),
			&RegisterMsg{ClientID: "old", Version: ProtocolVersion, Capabilities: []string{"tcp"}, Auth: &RegisterAuth{Scheme: "token", Proof: []byte("proof")}},
			RegisterMsg{Version: ProtocolVersion, Auth: &RegisterAuth{Scheme: "token"}},
		},
		{
			"null clears pointer",
			[]byte(`{"Auth":null}`),
			&RegisterMsg{Auth: &RegisterAuth{Scheme: "token"}},
			RegisterMsg{},
		},
	}
	for _, test := range decodeTests {
		t.Run("decode/"+test.name, func(t *testing.T) {
			if err := DecodeMessage(test.payload, test.dst); err != nil {
				t.Fatalf("DecodeMessage() error = %v", err)
			}
			got := reflect.ValueOf(test.dst).Elem().Interface()
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("decoded value = %#v, want %#v", got, test.want)
			}
		})
	}

	invalidUTF8 := append([]byte(`{"Message":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	rejectTests := []struct {
		name    string
		payload []byte
		dst     any
	}{
		{"duplicate member", []byte(`{"Timestamp":1,"Timestamp":2}`), &HeartbeatMsg{}},
		{"invalid UTF-8", invalidUTF8, &ErrorMsg{}},
		{"malformed", []byte(`{"Timestamp":`), &HeartbeatMsg{}},
		{"trailing value", []byte(`{"Timestamp":1}{}`), &HeartbeatMsg{}},
		{"type mismatch", []byte(`{"Timestamp":"1"}`), &HeartbeatMsg{}},
		{"int64 overflow", []byte(`{"Timestamp":9223372036854775808}`), &HeartbeatMsg{}},
		{"uint32 overflow", []byte(`{"Code":4294967296}`), &ErrorMsg{}},
	}
	for _, test := range rejectTests {
		t.Run("reject/"+test.name, func(t *testing.T) {
			if err := DecodeMessage(test.payload, test.dst); err == nil {
				t.Error("DecodeMessage() accepted invalid payload")
			}
		})
	}
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
func preEncodeMessage(msgType byte, payload any) []byte {
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
		payload any
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
			DestAddr:   "0.0.0.0:8080",
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
				var decoded any
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
			if err := WriteNewConn(&buf, 12345, "tcp", "192.168.1.100:54321", "0.0.0.0:8080", 1234567890); err != nil {
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
