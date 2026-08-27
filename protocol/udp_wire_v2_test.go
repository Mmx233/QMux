package protocol

import (
	"bytes"
	"errors"
	"math/rand"
	"strconv"
	"sync/atomic"
	"testing"
)

type udpAssemblerFunc func(uint32, uint16, uint8, uint8, []byte) ([]byte, error)

func (fn udpAssemblerFunc) AddFragment(sessionID uint32, fragID uint16, index, total uint8, payload []byte) ([]byte, error) {
	return fn(sessionID, fragID, index, total, payload)
}

func TestUDPWireV2Constants(t *testing.T) {
	constants := []struct {
		name string
		got  int
		want int
	}{
		{"normal packet type", UDPDatagramTypeNormal, 0x20},
		{"fragment packet type", UDPDatagramTypeFragment, 0x21},
		{"normal header size", UDPHeaderSize, 5},
		{"fragment header size", UDPFragHeaderSize, 9},
		{"normal payload limit", MaxUDPPayload, 1195},
		{"fragment payload limit", MaxFragPayload, 1191},
	}
	for _, constant := range constants {
		if constant.got != constant.want {
			t.Errorf("%s: got %#x, want %#x", constant.name, constant.got, constant.want)
		}
	}
}

func TestUDPWireV2RegressionBinaryPayload(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x80, 0x00, 0x02, 0x03}
	var fragID uint16
	datagrams, err := FragmentUDP(0x01020304, payload, &fragID, true)
	if err != nil {
		t.Fatal(err)
	}

	wantWire := []byte{0x20, 0x01, 0x02, 0x03, 0x04, 0x01, 0x02, 0x80, 0x00, 0x02, 0x03}
	if !bytes.Equal(datagrams[0], wantWire) {
		t.Fatalf("wire mismatch:\n got % x\nwant % x", datagrams[0], wantWire)
	}

	parsed, err := DecodeUDPDatagram(datagrams[0])
	if err != nil {
		t.Fatal(err)
	}
	if parsed.IsFragmented || parsed.SessionID != 0x01020304 || !bytes.Equal(parsed.Payload, payload) {
		t.Fatalf("unexpected parsed datagram: %+v", parsed)
	}
}

func TestUDPWireV2ArbitraryBinaryRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iteration := range 1000 {
		payload := make([]byte, rng.Intn(MaxUDPPayload+1))
		if _, err := rng.Read(payload); err != nil {
			t.Fatal(err)
		}
		var fragID uint16
		datagrams, err := FragmentUDP(uint32(iteration), payload, &fragID, true)
		if err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		parsed, err := DecodeUDPDatagram(datagrams[0])
		if err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		if parsed.SessionID != uint32(iteration) || parsed.IsFragmented || !bytes.Equal(parsed.Payload, payload) {
			t.Fatalf("iteration %d did not round trip", iteration)
		}
	}
}

func TestUDPWireV2BoundaryAndEncoderConsistency(t *testing.T) {
	for _, size := range []int{0, 4, 5, 1195, 1196, 1197, 4096} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i*31 + 7)
			}

			var fragID uint16
			plain, err := FragmentUDP(99, payload, &fragID, true)
			if err != nil {
				t.Fatal(err)
			}
			var atomicFragID atomic.Uint32
			pooled, err := FragmentUDPPooled(99, payload, &atomicFragID, true)
			if err != nil {
				t.Fatal(err)
			}
			defer ReleaseDatagramResults(pooled)

			if len(plain) != len(pooled) {
				t.Fatalf("encoder count mismatch: %d != %d", len(plain), len(pooled))
			}
			for i := range plain {
				if !bytes.Equal(plain[i], pooled[i].Data) {
					t.Fatalf("datagram %d differs between pooled and unpooled encoders", i)
				}
				if len(plain[i]) > MaxDatagramSize {
					t.Fatalf("datagram %d is too large: %d", i, len(plain[i]))
				}
				wantType := byte(UDPDatagramTypeNormal)
				if size > MaxUDPPayload {
					wantType = UDPDatagramTypeFragment
				}
				if plain[i][0] != wantType {
					t.Fatalf("datagram %d type=%#x, want %#x", i, plain[i][0], wantType)
				}
			}
		})
	}

	oversized := make([]byte, MaxUDPPayload+1)
	var fragID uint16
	if _, err := FragmentUDP(1, oversized, &fragID, false); !errors.Is(err, ErrFragmentationDisabled) {
		t.Fatalf("unpooled fragmentation-disabled error=%v", err)
	}
	var atomicFragID atomic.Uint32
	if _, err := FragmentUDPPooled(1, oversized, &atomicFragID, false); !errors.Is(err, ErrFragmentationDisabled) {
		t.Fatalf("pooled fragmentation-disabled error=%v", err)
	}
}

func TestDecodeUDPDatagramRejectsInvalidWire(t *testing.T) {
	fragment := func(index, total byte, payload []byte) []byte {
		dgram := make([]byte, UDPFragHeaderSize+len(payload))
		writeUDPFragmentHeader(dgram, 1, 2, index, total)
		copy(dgram[UDPFragHeaderSize:], payload)
		return dgram
	}

	tests := []struct {
		name string
		wire []byte
		err  error
	}{
		{name: "empty", wire: nil, err: ErrDatagramTooShort},
		{name: "short normal", wire: []byte{0x20, 0, 0, 0}, err: ErrDatagramTooShort},
		{name: "short fragment", wire: []byte{0x21, 0, 0, 0, 1, 0, 1, 0}, err: ErrDatagramTooShort},
		{name: "legacy normal", wire: []byte{0, 0, 0, 1, 0}, err: ErrUnknownDatagramType},
		{name: "unknown v2 type", wire: []byte{0x22, 0, 0, 0, 1}, err: ErrUnknownDatagramType},
		{name: "unknown version", wire: []byte{0x30, 0, 0, 0, 1}, err: ErrUnknownDatagramType},
		{name: "too large", wire: append([]byte{0x20}, make([]byte, MaxDatagramSize)...), err: ErrDatagramTooLarge},
		{name: "fragment total zero", wire: fragment(0, 0, []byte{1}), err: ErrInvalidFragTotal},
		{name: "fragment total one", wire: fragment(0, 1, []byte{1}), err: ErrInvalidFragTotal},
		{name: "fragment index equal total", wire: fragment(2, 2, []byte{1}), err: ErrInvalidFragIndex},
		{name: "empty fragment payload", wire: fragment(0, 2, nil), err: ErrEmptyFragmentPayload},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeUDPDatagram(test.wire); !errors.Is(err, test.err) {
				t.Fatalf("error=%v, want %v", err, test.err)
			}
		})
	}
}

func TestDecodeUDPDatagramTypeWhitelist(t *testing.T) {
	for value := 0; value <= 0xff; value++ {
		wire := []byte{byte(value), 0, 0, 0, 1}
		_, err := DecodeUDPDatagram(wire)

		switch byte(value) {
		case UDPDatagramTypeNormal:
			if err != nil {
				t.Fatalf("normal type rejected: %v", err)
			}
		case UDPDatagramTypeFragment:
			if !errors.Is(err, ErrDatagramTooShort) {
				t.Fatalf("fragment type error=%v, want %v", err, ErrDatagramTooShort)
			}
		default:
			if !errors.Is(err, ErrUnknownDatagramType) {
				t.Fatalf("type %#x error=%v, want %v", value, err, ErrUnknownDatagramType)
			}
		}
	}
}

func FuzzDecodeUDPDatagram(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte{UDPDatagramTypeNormal, 0, 0, 0, 1})
	f.Add([]byte{UDPDatagramTypeFragment, 0, 0, 0, 1, 0, 1, 0, 2, 0xff})
	f.Add([]byte{0, 0, 0, 1, 0x80})

	f.Fuzz(func(t *testing.T, wire []byte) {
		_, _ = DecodeUDPDatagram(wire)
	})
}

func TestDecodeAndAssembleUDPDatagram(t *testing.T) {
	t.Run("normal empty payload is complete", func(t *testing.T) {
		var fragID uint16
		datagrams, err := FragmentUDP(42, nil, &fragID, true)
		if err != nil {
			t.Fatal(err)
		}
		sessionID, payload, complete, err := DecodeAndAssembleUDPDatagram(datagrams[0], nil)
		if err != nil {
			t.Fatal(err)
		}
		if sessionID != 42 || !complete || len(payload) != 0 {
			t.Fatalf("session=%d payload=%v complete=%v", sessionID, payload, complete)
		}
	})

	t.Run("fragment incomplete then complete", func(t *testing.T) {
		want := bytes.Repeat([]byte{0x00, 0xff, 0x80, 0x21}, 700)
		var fragID uint16
		datagrams, err := FragmentUDP(43, want, &fragID, true)
		if err != nil {
			t.Fatal(err)
		}
		assembler := NewFragmentAssembler()
		for i, datagram := range datagrams {
			sessionID, payload, complete, err := DecodeAndAssembleUDPDatagram(datagram, assembler)
			if err != nil {
				t.Fatalf("fragment %d: %v", i, err)
			}
			if sessionID != 43 {
				t.Fatalf("fragment %d session=%d", i, sessionID)
			}
			if i < len(datagrams)-1 && complete {
				t.Fatalf("fragment %d completed early", i)
			}
			if i == len(datagrams)-1 && (!complete || !bytes.Equal(payload, want)) {
				t.Fatalf("final fragment did not produce the original payload")
			}
		}
	})

	t.Run("fragment assembler error", func(t *testing.T) {
		wantErr := errors.New("assembler failed")
		assembler := udpAssemblerFunc(func(uint32, uint16, uint8, uint8, []byte) ([]byte, error) {
			return nil, wantErr
		})
		wire := make([]byte, UDPFragHeaderSize+1)
		writeUDPFragmentHeader(wire, 44, 1, 0, 2)
		wire[UDPFragHeaderSize] = 1
		_, _, complete, err := DecodeAndAssembleUDPDatagram(wire, assembler)
		if !errors.Is(err, wantErr) || complete {
			t.Fatalf("error=%v complete=%v", err, complete)
		}
	})

	t.Run("fragment requires assembler", func(t *testing.T) {
		wire := make([]byte, UDPFragHeaderSize+1)
		writeUDPFragmentHeader(wire, 45, 1, 0, 2)
		wire[UDPFragHeaderSize] = 1
		_, _, complete, err := DecodeAndAssembleUDPDatagram(wire, nil)
		if !errors.Is(err, ErrFragmentAssemblerNil) || complete {
			t.Fatalf("error=%v complete=%v", err, complete)
		}
	})
}

func TestDecodeAndAssembleUDPDatagramSameFragmentIDDifferentSessions(t *testing.T) {
	type closeAssembler interface {
		UDPFragmentAssembler
		Close()
	}
	assemblers := map[string]func() closeAssembler{
		"regular": func() closeAssembler { return NewFragmentAssembler() },
		"sharded": func() closeAssembler { return NewShardedFragmentAssembler(16) },
	}
	firstPayload := bytes.Repeat([]byte("first"), 500)
	secondPayload := bytes.Repeat([]byte("second"), 500)
	var firstCounter, secondCounter uint16
	first, err := FragmentUDP(1, firstPayload, &firstCounter, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FragmentUDP(2, secondPayload, &secondCounter, true)
	if err != nil {
		t.Fatal(err)
	}
	firstHeader, err := DecodeUDPDatagram(first[0])
	if err != nil {
		t.Fatal(err)
	}
	secondHeader, err := DecodeUDPDatagram(second[0])
	if err != nil {
		t.Fatal(err)
	}
	if firstHeader.FragmentID != secondHeader.FragmentID || len(first) != 3 || len(second) != 3 {
		t.Fatalf("independent counters produced fragment IDs %d/%d and counts %d/%d", firstHeader.FragmentID, secondHeader.FragmentID, len(first), len(second))
	}
	sequence := []struct {
		sessionID uint32
		wire      []byte
		want      []byte
	}{
		{sessionID: 1, wire: first[2]},
		{sessionID: 1, wire: first[2]}, // duplicate, out of order
		{sessionID: 2, wire: second[0]},
		{sessionID: 2, wire: second[0]}, // duplicate
		{sessionID: 1, wire: first[0]},
		{sessionID: 2, wire: second[2]},
		{sessionID: 1, wire: first[1], want: firstPayload},
		{sessionID: 2, wire: second[1], want: secondPayload},
	}

	for name, newAssembler := range assemblers {
		t.Run(name, func(t *testing.T) {
			assembler := newAssembler()
			defer assembler.Close()
			for i, fragment := range sequence {
				sessionID, payload, complete, err := DecodeAndAssembleUDPDatagram(fragment.wire, assembler)
				if err != nil {
					t.Fatalf("fragment %d: %v", i, err)
				}
				if sessionID != fragment.sessionID || complete != (fragment.want != nil) || !bytes.Equal(payload, fragment.want) {
					t.Fatalf("fragment %d: session=%d payload bytes=%d complete=%v, want session=%d payload bytes=%d", i, sessionID, len(payload), complete, fragment.sessionID, len(fragment.want))
				}
			}
		})
	}
}
