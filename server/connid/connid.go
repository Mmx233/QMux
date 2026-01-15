package connid

import "sync/atomic"

var counter atomic.Uint64

// Generate generates a unique connection ID
func Generate() uint64 {
	return counter.Add(1)
}
