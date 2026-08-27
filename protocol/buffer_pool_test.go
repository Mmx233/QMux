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

func TestRelayLifecycleWaitsForBothDirections(t *testing.T) {
	release := make(chan struct{})
	a := &relayTestEndpoint{Reader: relayErrorReader{err: io.EOF}, Writer: io.Discard}
	b := &relayTestEndpoint{Reader: relayBlockingReader{release: release}, Writer: io.Discard}
	halfClosed := make(chan error, 1)

	relay := StartRelay(a, b, func(err error) error {
		halfClosed <- err
		return nil
	}, nil)
	select {
	case err := <-halfClosed:
		if err != nil {
			t.Fatalf("a to b copy error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a to b completion callback was not called")
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
		if err != nil {
			t.Fatalf("Wait() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after both copies exited")
	}
}

func TestRelayLifecycleErrorCallbackReleasesSibling(t *testing.T) {
	copyErr := errors.New("a to b failed")
	release := make(chan struct{})
	a := &relayTestEndpoint{Reader: relayErrorReader{err: copyErr}, Writer: io.Discard}
	b := &relayTestEndpoint{Reader: relayBlockingReader{release: release}, Writer: io.Discard}

	relay := StartRelay(a, b, func(err error) error {
		if errors.Is(err, copyErr) {
			close(release)
		}
		return nil
	}, nil)
	done := make(chan error, 1)
	go func() {
		done <- relay.Wait()
	}()

	select {
	case err := <-done:
		if !errors.Is(err, copyErr) {
			t.Fatalf("Wait() error = %v, want %v", err, copyErr)
		}
	case <-time.After(time.Second):
		t.Fatal("completion callback did not release the sibling copy")
	}
}

func TestRelayLifecycleJoinsCopyAndCallbackErrors(t *testing.T) {
	copyErr := errors.New("copy failed")
	callbackErr := errors.New("callback failed")
	otherCallbackErr := errors.New("other callback failed")
	a := &relayTestEndpoint{Reader: relayErrorReader{err: copyErr}, Writer: io.Discard}
	b := &relayTestEndpoint{Reader: relayErrorReader{err: io.EOF}, Writer: io.Discard}

	err := StartRelay(a, b, func(error) error {
		return callbackErr
	}, func(error) error {
		return otherCallbackErr
	}).Wait()
	for _, want := range []error{copyErr, callbackErr, otherCallbackErr} {
		if !errors.Is(err, want) {
			t.Fatalf("Wait() error = %v, want it to contain %v", err, want)
		}
	}
}

func TestRelayLifecycleWaitIsRepeatableAndConcurrent(t *testing.T) {
	want := errors.New("copy failed")
	a := &relayTestEndpoint{Reader: relayErrorReader{err: want}, Writer: io.Discard}
	b := &relayTestEndpoint{Reader: relayErrorReader{err: io.EOF}, Writer: io.Discard}
	relay := StartRelay(a, b, nil, nil)

	const waiters = 8
	results := make(chan error, waiters)
	for range waiters {
		go func() {
			results <- relay.Wait()
		}()
	}
	for range waiters {
		if err := <-results; !errors.Is(err, want) {
			t.Fatalf("Wait() error = %v, want %v", err, want)
		}
	}
	if err := relay.Wait(); !errors.Is(err, want) {
		t.Fatalf("repeated Wait() error = %v, want %v", err, want)
	}
}
