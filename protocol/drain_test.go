package protocol

import (
	"errors"
	"fmt"
	"io"
	"math"
	"testing"

	"github.com/quic-go/quic-go/quicvarint"
)

func TestDecodeDrainRequestStrict(t *testing.T) {
	for _, test := range []struct {
		payload string
		valid   bool
	}{
		{`{}`, true}, {" \n { \t } \r ", true},
		{`{"x":1}`, false}, {`null`, false}, {`[]`, false}, {`1`, false},
		{`{`, false}, {`{} {}`, false},
	} {
		if err := DecodeDrainRequest([]byte(test.payload)); (err == nil) != test.valid {
			t.Errorf("DecodeDrainRequest(%q) error = %v, valid = %v", test.payload, err, test.valid)
		}
	}
}

func TestDecodeDrainCompleteStrict(t *testing.T) {
	maxFence := int64(quicvarint.Max - 2)
	for _, test := range []struct {
		payload string
		want    int64
		valid   bool
	}{
		{`{"AcceptFence":-1}`, -1, true}, {` { "AcceptFence" : 1 } `, 1, true},
		{`{"AcceptFence":` + stringInt(maxFence) + `}`, maxFence, true},
		{`{}`, 0, false}, {`{"Other":1}`, 0, false},
		{`{"AcceptFence":1,"AcceptFence":1}`, 0, false},
		{`{"AcceptFence":null}`, 0, false}, {`{"AcceptFence":"1"}`, 0, false},
		{`{"AcceptFence":1.0}`, 0, false}, {`{"AcceptFence":1e0}`, 0, false},
		{`{"AcceptFence":9223372036854775808}`, 0, false},
		{`{"AcceptFence":` + stringInt(int64(quicvarint.Max+2)) + `}`, 0, false},
		{`{"AcceptFence":` + stringInt(math.MaxInt64) + `}`, 0, false},
		{`null`, 0, false}, {`{"AcceptFence":1`, 0, false},
		{`{"AcceptFence":1} []`, 0, false},
	} {
		got, err := DecodeDrainComplete([]byte(test.payload))
		if (err == nil) != test.valid {
			t.Errorf("DecodeDrainComplete(%q) error = %v, valid = %v", test.payload, err, test.valid)
		} else if err == nil && got.AcceptFence != test.want {
			t.Errorf("DecodeDrainComplete(%q) fence = %d, want %d", test.payload, got.AcceptFence, test.want)
		}
	}
}

func stringInt(value int64) string {
	return fmt.Sprintf("%d", value)
}

type shortDrainWriter struct {
	err error
}

func (w shortDrainWriter) Write(data []byte) (int, error) {
	return len(data) - 1, w.err
}

func TestWriteMessageRequiresFullWrite(t *testing.T) {
	if err := WriteDrainRequest(shortDrainWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("nil-error short write = %v, want io.ErrShortWrite", err)
	}
	want := errors.New("writer failed")
	if err := WriteDrainRequest(shortDrainWriter{err: want}); !errors.Is(err, want) {
		t.Fatalf("concrete write error = %v, want %v", err, want)
	}
}
