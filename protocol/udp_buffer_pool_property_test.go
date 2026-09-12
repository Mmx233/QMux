package protocol

import "testing"

type propertyBufferPool struct {
	size int
	get  func() *[]byte
	put  func(*[]byte)
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
