package protocol

import (
	"bytes"
	"errors"
	"math"
	"math/rand"
	"strconv"
	"sync/atomic"
	"testing"
)

type udpAssemblerFunc func(uint32, uint64, uint8, uint8, []byte) ([]byte, error)

func (fn udpAssemblerFunc) AddFragment(sessionID uint32, fragID uint64, index, total uint8, payload []byte) ([]byte, error) {
	return fn(sessionID, fragID, index, total, payload)
}

func TestUDPWireV2RegressionBinaryPayload(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x80, 0x00, 0x02, 0x03}
	var fragmentSequence uint32
	datagrams, err := FragmentUDP(0x01020304, 1, payload, &fragmentSequence, true)
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

func TestUDPFragmentWireV2WidenedGolden(t *testing.T) {
	if UDPDatagramTypeFragment != 0x22 || UDPFragHeaderSize != 15 || MaxFragPayload != 1185 {
		t.Fatalf("fragment wire constants = type %#x/header %d/payload %d", UDPDatagramTypeFragment, UDPFragHeaderSize, MaxFragPayload)
	}

	payload := make([]byte, MaxUDPPayload+1)
	sequence := uint32(0x55667787)
	datagrams, err := FragmentUDP(0x01020304, 0x11223344, payload, &sequence, true)
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := []byte{
		0x22, 0x01, 0x02, 0x03, 0x04,
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x00, 0x02,
	}
	if len(datagrams) != 2 || !bytes.Equal(datagrams[0][:UDPFragHeaderSize], wantHeader) {
		t.Fatalf("fragment wire header:\n got % x\nwant % x", datagrams[0][:UDPFragHeaderSize], wantHeader)
	}
	parsed, err := DecodeUDPDatagram(append(append([]byte(nil), wantHeader...), 0xaa))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Type != 0x22 || parsed.SessionID != 0x01020304 || parsed.FragmentID != 0x1122334455667788 ||
		parsed.FragmentIndex != 0 || parsed.FragmentTotal != 2 || !bytes.Equal(parsed.Payload, []byte{0xaa}) {
		t.Fatalf("decoded widened fragment = %+v", parsed)
	}
}

func TestUDPWireV2ArbitraryBinaryRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iteration := range 1000 {
		payload := make([]byte, rng.Intn(MaxUDPPayload+1))
		if _, err := rng.Read(payload); err != nil {
			t.Fatal(err)
		}
		var fragmentSequence uint32
		datagrams, err := FragmentUDP(uint32(iteration), 1, payload, &fragmentSequence, true)
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
	for _, size := range []int{0, 4, 5, 1184, 1185, 1195, 1196, 2369, 2370, 2371, 4096} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i*31 + 7)
			}

			plainSequence := uint32(0x55667787)
			plain, err := FragmentUDP(99, 0x11223344, payload, &plainSequence, true)
			if err != nil {
				t.Fatal(err)
			}
			var pooledSequence atomic.Uint32
			pooledSequence.Store(0x55667787)
			pooled, err := FragmentUDPPooled(99, 0x11223344, payload, &pooledSequence, true)
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
			wantSequence := uint32(0x55667787)
			if size > MaxUDPPayload {
				wantSequence++
				if want := (size + MaxFragPayload - 1) / MaxFragPayload; len(plain) != want {
					t.Fatalf("fragment count = %d, want %d", len(plain), want)
				}
			}
			if plainSequence != wantSequence || pooledSequence.Load() != wantSequence {
				t.Fatalf("sequence = %#x/%#x, want %#x", plainSequence, pooledSequence.Load(), wantSequence)
			}
		})
	}

	oversized := make([]byte, MaxUDPPayload+1)
	var fragmentSequence uint32
	if _, err := FragmentUDP(1, 1, oversized, &fragmentSequence, false); !errors.Is(err, ErrFragmentationDisabled) {
		t.Fatalf("unpooled fragmentation-disabled error=%v", err)
	}
	var atomicSequence atomic.Uint32
	if _, err := FragmentUDPPooled(1, 1, oversized, &atomicSequence, false); !errors.Is(err, ErrFragmentationDisabled) {
		t.Fatalf("pooled fragmentation-disabled error=%v", err)
	}
	if fragmentSequence != 0 || atomicSequence.Load() != 0 {
		t.Fatal("fragmentation-disabled packet consumed an identity")
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
		{name: "short fragment", wire: []byte{0x22, 0, 0, 0, 1, 0, 1, 0}, err: ErrDatagramTooShort},
		{name: "legacy normal", wire: []byte{0, 0, 0, 1, 0}, err: ErrUnknownDatagramType},
		{name: "legacy fragment", wire: []byte{0x21, 0, 0, 0, 1, 0, 1, 0, 2}, err: ErrUnknownDatagramType},
		{name: "unknown v2 type", wire: []byte{0x23, 0, 0, 0, 1}, err: ErrUnknownDatagramType},
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
	for length := range UDPFragHeaderSize {
		wire := make([]byte, length)
		if length > 0 {
			wire[0] = UDPDatagramTypeFragment
		}
		if _, err := DecodeUDPDatagram(wire); !errors.Is(err, ErrDatagramTooShort) {
			t.Fatalf("fragment length %d error = %v, want %v", length, err, ErrDatagramTooShort)
		}
	}
}

func TestLegacyFragmentRejectedBeforeAssembly(t *testing.T) {
	wire := []byte{
		0x22, 0, 0, 0, 1,
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0, 2, 0xaa,
	}
	wire[0] = 0x21
	calls := 0
	assembler := udpAssemblerFunc(func(uint32, uint64, uint8, uint8, []byte) ([]byte, error) {
		calls++
		return nil, nil
	})
	if _, _, _, err := DecodeAndAssembleUDPDatagram(wire, assembler); !errors.Is(err, ErrUnknownDatagramType) {
		t.Fatalf("legacy fragment error = %v, want %v", err, ErrUnknownDatagramType)
	}
	if calls != 0 {
		t.Fatalf("legacy fragment reached assembler %d times", calls)
	}

	retaining := NewShardedFragmentAssembler(1, 0, 0)
	defer retaining.Close()
	if _, _, _, err := DecodeAndAssembleUDPDatagram(wire, retaining); !errors.Is(err, ErrUnknownDatagramType) {
		t.Fatalf("legacy fragment retaining assembler error = %v", err)
	}
	if snapshot := retaining.Snapshot(); snapshot.RetainedGroups != 0 || snapshot.RetainedBackingBytes != 0 {
		t.Fatalf("legacy fragment mutated assembler: %+v", snapshot)
	}
}

func TestFragmentSequenceWrapPreservesEpoch(t *testing.T) {
	const epoch = uint32(0x11223344)
	want := []uint64{
		0x11223344ffffffff,
		0x1122334400000000,
		0x1122334400000001,
	}
	payload := make([]byte, MaxUDPPayload+1)

	t.Run("non-pooled", func(t *testing.T) {
		sequence := uint32(math.MaxUint32 - 1)
		for i, wantID := range want {
			datagrams, err := FragmentUDP(1, epoch, payload, &sequence, true)
			if err != nil {
				t.Fatalf("fragment %d: %v", i, err)
			}
			parsed, err := DecodeUDPDatagram(datagrams[0])
			if err != nil || parsed.FragmentID != wantID {
				t.Fatalf("fragment %d identity = %#x, error %v, want %#x", i, parsed.FragmentID, err, wantID)
			}
		}
	})

	t.Run("pooled", func(t *testing.T) {
		var sequence atomic.Uint32
		sequence.Store(math.MaxUint32 - 1)
		for i, wantID := range want {
			datagrams, err := FragmentUDPPooled(1, epoch, payload, &sequence, true)
			if err != nil {
				t.Fatalf("fragment %d: %v", i, err)
			}
			parsed, decodeErr := DecodeUDPDatagram(datagrams[0].Data)
			ReleaseDatagramResults(datagrams)
			if decodeErr != nil || parsed.FragmentID != wantID {
				t.Fatalf("fragment %d identity = %#x, error %v, want %#x", i, parsed.FragmentID, decodeErr, wantID)
			}
		}
	})
}

func TestFragmentIdentitySurvives65537EncoderAllocations(t *testing.T) {
	const (
		sessionID = uint32(7)
		epoch     = uint32(0x10203040)
		lastID    = uint32(65537)
	)
	oldPayload := bytes.Repeat([]byte{0xa1}, MaxUDPPayload+1)
	newPayload := bytes.Repeat([]byte{0xb2}, MaxUDPPayload+1)
	probePayload := make([]byte, MaxUDPPayload+1)
	var sequence atomic.Uint32
	assembler := &FragmentAssembler{fragments: make(map[fragmentKey]*fragmentGroup)}

	oldFragments, err := FragmentUDPPooled(sessionID, epoch, oldPayload, &sequence, true)
	if err != nil {
		t.Fatal(err)
	}
	delayedOld := append([]byte(nil), oldFragments[1].Data...)
	_, _, complete, err := DecodeAndAssembleUDPDatagram(oldFragments[0].Data, assembler)
	ReleaseDatagramResults(oldFragments)
	if err != nil || complete {
		t.Fatalf("retain old identity: complete=%v error=%v", complete, err)
	}

	for id := uint32(2); id <= lastID; id++ {
		payload := probePayload
		if id == lastID {
			payload = newPayload
		}
		fragments, fragmentErr := FragmentUDPPooled(sessionID, epoch, payload, &sequence, true)
		if fragmentErr != nil {
			t.Fatalf("identity %d: %v", id, fragmentErr)
		}
		parsed, decodeErr := DecodeUDPDatagram(fragments[0].Data)
		if decodeErr != nil || parsed.FragmentID != fragmentIdentity(epoch, id) {
			ReleaseDatagramResults(fragments)
			t.Fatalf("identity %d decoded as %#x, error %v", id, parsed.FragmentID, decodeErr)
		}
		if id == lastID {
			var got []byte
			for _, fragment := range fragments {
				_, got, complete, err = DecodeAndAssembleUDPDatagram(fragment.Data, assembler)
				if err != nil {
					break
				}
			}
			if err != nil || !complete || !bytes.Equal(got, newPayload) {
				ReleaseDatagramResults(fragments)
				t.Fatalf("new identity completion: bytes=%d complete=%v error=%v", len(got), complete, err)
			}
		}
		ReleaseDatagramResults(fragments)
	}

	if len(assembler.fragments) != 1 {
		t.Fatalf("retained groups after identity %d = %d, want 1", lastID, len(assembler.fragments))
	}
	_, got, complete, err := DecodeAndAssembleUDPDatagram(delayedOld, assembler)
	if err != nil || !complete || !bytes.Equal(got, oldPayload) {
		t.Fatalf("old identity completion: bytes=%d complete=%v error=%v", len(got), complete, err)
	}
	if len(assembler.fragments) != 0 {
		t.Fatalf("completed identities retained %d groups", len(assembler.fragments))
	}
}

func FuzzDecodeUDPDatagram(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte{UDPDatagramTypeNormal, 0, 0, 0, 1})
	f.Add([]byte{UDPDatagramTypeFragment, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 2, 0, 2, 0xff})
	f.Add([]byte{0x21, 0, 0, 0, 1, 0, 1, 0, 2, 0xff})
	f.Add([]byte{0, 0, 0, 1, 0x80})

	f.Fuzz(func(t *testing.T, wire []byte) {
		_, _ = DecodeUDPDatagram(wire)
	})
}

func TestDecodeAndAssembleUDPDatagram(t *testing.T) {
	t.Run("normal empty payload is complete", func(t *testing.T) {
		var fragmentSequence uint32
		datagrams, err := FragmentUDP(42, 1, nil, &fragmentSequence, true)
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
		var fragmentSequence uint32
		datagrams, err := FragmentUDP(43, 1, want, &fragmentSequence, true)
		if err != nil {
			t.Fatal(err)
		}
		assembler := NewFragmentAssembler(0, 0)
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
		assembler := udpAssemblerFunc(func(uint32, uint64, uint8, uint8, []byte) ([]byte, error) {
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
		"regular": func() closeAssembler { return NewFragmentAssembler(0, 0) },
		"sharded": func() closeAssembler { return NewShardedFragmentAssembler(16, 0, 0) },
	}
	firstPayload := bytes.Repeat([]byte("first"), 500)
	secondPayload := bytes.Repeat([]byte("second"), 500)
	var firstCounter, secondCounter uint32
	first, err := FragmentUDP(1, 1, firstPayload, &firstCounter, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FragmentUDP(2, 1, secondPayload, &secondCounter, true)
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
