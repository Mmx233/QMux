package server

import (
	"testing"
	"time"
)

func TestPendingRegistrationSaturation(t *testing.T) {
	slots := make(chan struct{}, 2)
	first, ok := acquirePendingRegistration(slots)
	if !ok {
		t.Fatal("first acquire failed")
	}
	second, ok := acquirePendingRegistration(slots)
	if !ok {
		t.Fatal("second acquire failed")
	}
	if permit, ok := acquirePendingRegistration(slots); ok || permit != nil {
		t.Fatal("acquire succeeded after pending capacity was exhausted")
	}
	first.Release()
	if permit, ok := acquirePendingRegistration(slots); !ok || permit == nil {
		t.Fatal("acquire failed after a pending registration released its permit")
	} else {
		permit.Release()
	}
	second.Release()
}

func TestRegisteredConnectionDoesNotHoldPendingPermit(t *testing.T) {
	slots := make(chan struct{}, 1)
	permit, ok := acquirePendingRegistration(slots)
	if !ok {
		t.Fatal("initial acquire failed")
	}
	registered := make(chan struct{})
	connectionDone := make(chan struct{})
	goroutineDone := make(chan struct{})
	go func() {
		defer close(goroutineDone)
		defer permit.Release() // Mirrors failure/exit cleanup in handleConnection.
		permit.Release()       // Registration committed; heartbeat lifetime begins.
		close(registered)
		<-connectionDone
	}()

	select {
	case <-registered:
	case <-time.After(time.Second):
		t.Fatal("registration phase did not complete")
	}
	next, ok := acquirePendingRegistration(slots)
	if !ok {
		t.Fatal("long-lived registered connection still occupied pending capacity")
	}
	next.Release()
	close(connectionDone)
	select {
	case <-goroutineDone:
	case <-time.After(time.Second):
		t.Fatal("long-lived connection did not exit")
	}
}
