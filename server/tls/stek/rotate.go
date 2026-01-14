package stek

import "sync/atomic"

type RotateManager struct {
	Keys *atomic.Pointer[[][32]byte]
}
