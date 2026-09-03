package protocol

import (
	"bytes"
	"sync/atomic"
	"testing"
)

type propertyBufferPool struct {
	size int
	get  func() *[]byte
	put  func(*[]byte)
}

func checkBufferPoolSizes(t testing.TB, pools map[string]propertyBufferPool) {
	t.Helper()
	for name, pool := range pools {
		buffer := pool.get()
		if len(*buffer) != pool.size {
			t.Errorf("%s buffer length = %d, want %d", name, len(*buffer), pool.size)
		}
		if name == "datagram" && cap(*buffer) != pool.size {
			t.Errorf("datagram buffer capacity = %d, want %d", cap(*buffer), pool.size)
		}
		pool.put(buffer)
	}
}

func TestBufferPoolSizes(t *testing.T) {
	for name, pool := range map[string]propertyBufferPool{
		"datagram": {size: MaxDatagramSize, get: GetDatagramBuffer, put: PutDatagramBuffer},
		"read":     {size: ReadBufferSize, get: GetReadBuffer, put: PutReadBuffer},
		"fragment": {size: FragmentBufferSize, get: GetFragmentBuffer, put: PutFragmentBuffer},
	} {
		t.Run(name, func(t *testing.T) {
			buffer := pool.get()
			if buffer == nil || len(*buffer) != pool.size {
				t.Fatalf("buffer length = %d, want %d", len(*buffer), pool.size)
			}
			if name == "datagram" && cap(*buffer) != pool.size {
				t.Fatalf("datagram buffer capacity = %d, want %d", cap(*buffer), pool.size)
			}
			pool.put(buffer)
			reused := pool.get()
			defer pool.put(reused)
			if len(*reused) != pool.size {
				t.Fatalf("reused buffer length = %d, want %d", len(*reused), pool.size)
			}
			if name == "datagram" && cap(*reused) != pool.size {
				t.Fatalf("reused datagram buffer capacity = %d, want %d", cap(*reused), pool.size)
			}
		})
	}
}

func TestDatagramBufferPoolableRequiresExactCapacity(t *testing.T) {
	size := DatagramBufferSize
	exact := make([]byte, size)
	wrongLength := make([]byte, size-1, size)
	largerCapacity := make([]byte, size, size+1)
	tests := []struct {
		name string
		buf  *[]byte
		want bool
	}{
		{name: "nil", buf: nil},
		{name: "wrong length", buf: &wrongLength},
		{name: "larger capacity", buf: &largerCapacity},
		{name: "exact", buf: &exact, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := datagramBufferPoolable(test.buf); got != test.want {
				t.Fatalf("datagramBufferPoolable() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestInitBufferPoolCustomSizes(t *testing.T) {
	t.Cleanup(func() {
		if err := InitBufferPool(DefaultDatagramBufferSize, DefaultReadBufferSize, DefaultFragmentBufferSize); err != nil {
			t.Errorf("restore default buffer pool: %v", err)
		}
	})

	const datagramSize, readSize, fragmentSize = MaxDatagramSize + 512, 2048, 503
	if err := InitBufferPool(datagramSize, readSize, fragmentSize); err != nil {
		t.Fatalf("InitBufferPool: %v", err)
	}

	for name, buffer := range map[string]*[]byte{
		"datagram": GetDatagramBuffer(),
		"read":     GetReadBuffer(),
		"fragment": GetFragmentBuffer(),
	} {
		want := map[string]int{"datagram": datagramSize, "read": readSize, "fragment": fragmentSize}[name]
		if len(*buffer) != want {
			t.Errorf("%s buffer length = %d, want %d", name, len(*buffer), want)
		}
		if name == "datagram" && cap(*buffer) != want {
			t.Errorf("datagram buffer capacity = %d, want %d", cap(*buffer), want)
		}
	}
}

func TestInitBufferPoolDatagramSizeValidation(t *testing.T) {
	tests := []struct {
		name         string
		datagramSize int
		wantErr      bool
	}{
		{name: "minimum", datagramSize: MaxDatagramSize},
		{name: "zero uses default", datagramSize: 0},
		{name: "negative uses default", datagramSize: -17},
		{name: "below minimum", datagramSize: MaxDatagramSize - 1, wantErr: true},
		{name: "small positive", datagramSize: 64, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := InitBufferPool(DefaultDatagramBufferSize, DefaultReadBufferSize, DefaultFragmentBufferSize); err != nil {
				t.Fatalf("initialize default buffer pool: %v", err)
			}
			t.Cleanup(func() {
				if err := InitBufferPool(DefaultDatagramBufferSize, DefaultReadBufferSize, DefaultFragmentBufferSize); err != nil {
					t.Errorf("restore default buffer pool: %v", err)
				}
			})

			poolBefore := udpPool
			datagramBefore, readBefore, fragmentBefore := DatagramBufferSize, ReadBufferSize, FragmentBufferSize
			const readSize, fragmentSize = 23456, 789
			err := InitBufferPool(test.datagramSize, readSize, fragmentSize)
			if test.wantErr {
				if err == nil {
					t.Fatal("InitBufferPool returned nil error")
				}
				if udpPool != poolBefore || DatagramBufferSize != datagramBefore || ReadBufferSize != readBefore || FragmentBufferSize != fragmentBefore {
					t.Fatal("rejected initialization changed buffer pool state")
				}

				checkBufferPoolSizes(t, map[string]propertyBufferPool{
					"datagram": {size: datagramBefore, get: GetDatagramBuffer, put: PutDatagramBuffer},
					"read":     {size: readBefore, get: GetReadBuffer, put: PutReadBuffer},
					"fragment": {size: fragmentBefore, get: GetFragmentBuffer, put: PutFragmentBuffer},
				})

				const sessionID uint32 = 0x12345678
				payload := bytes.Repeat([]byte{0xa5}, MaxUDPPayload)
				var counter atomic.Uint32
				results, fragmentErr := FragmentUDPPooled(sessionID, payload, &counter, true)
				if fragmentErr != nil {
					t.Fatalf("FragmentUDPPooled: %v", fragmentErr)
				}
				defer ReleaseDatagramResults(results)
				if len(results) != 1 {
					t.Fatalf("datagram count = %d, want 1", len(results))
				}
				decoded, decodeErr := DecodeUDPDatagram(results[0].Data)
				if decodeErr != nil {
					t.Fatalf("DecodeUDPDatagram: %v", decodeErr)
				}
				if decoded.SessionID != sessionID || !bytes.Equal(decoded.Payload, payload) || len(results[0].Data) != MaxDatagramSize {
					t.Fatal("rejected initialization changed datagram behavior")
				}
				return
			}

			if err != nil {
				t.Fatalf("InitBufferPool: %v", err)
			}
			checkBufferPoolSizes(t, map[string]propertyBufferPool{
				"datagram": {size: MaxDatagramSize, get: GetDatagramBuffer, put: PutDatagramBuffer},
				"read":     {size: readSize, get: GetReadBuffer, put: PutReadBuffer},
				"fragment": {size: fragmentSize, get: GetFragmentBuffer, put: PutFragmentBuffer},
			})
		})
	}
}
