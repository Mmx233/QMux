package protocol

import (
	"errors"
	"io"
	"testing"
	"time"
)

type relayTestEndpoint struct {
	io.Reader
	io.Writer
}

type relayErrorReader struct {
	err error
}

func (r relayErrorReader) Read([]byte) (int, error) {
	return 0, r.err
}

type relayBlockingReader struct {
	release <-chan struct{}
}

func (r relayBlockingReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

func TestRelayLifecycleWaitFirstAndWait(t *testing.T) {
	firstErr := errors.New("first direction finished")
	release := make(chan struct{})
	a := &relayTestEndpoint{Reader: relayErrorReader{err: firstErr}, Writer: io.Discard}
	b := &relayTestEndpoint{Reader: relayBlockingReader{release: release}, Writer: io.Discard}

	relay := StartRelay(a, b)
	if err := relay.WaitFirst(); !errors.Is(err, firstErr) {
		t.Fatalf("WaitFirst() error = %v, want %v", err, firstErr)
	}
	if err := relay.WaitFirst(); !errors.Is(err, firstErr) {
		t.Fatalf("second WaitFirst() error = %v, want %v", err, firstErr)
	}

	done := make(chan error, 1)
	go func() {
		done <- relay.Wait()
	}()

	select {
	case err := <-done:
		t.Fatalf("Wait returned %v while the second copy was still blocked", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, firstErr) {
			t.Fatalf("Wait() error = %v, want %v", err, firstErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after both copies exited")
	}
}

func TestRelayPreservesFirstCompletionSemantics(t *testing.T) {
	firstErr := errors.New("first direction finished")
	release := make(chan struct{})
	a := &relayTestEndpoint{Reader: relayErrorReader{err: firstErr}, Writer: io.Discard}
	b := &relayTestEndpoint{Reader: relayBlockingReader{release: release}, Writer: io.Discard}

	result := make(chan error, 1)
	go func() {
		result <- Relay(a, b)
	}()

	select {
	case err := <-result:
		if !errors.Is(err, firstErr) {
			t.Fatalf("Relay() error = %v, want %v", err, firstErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Relay waited for both copy directions")
	}

	close(release)
}
