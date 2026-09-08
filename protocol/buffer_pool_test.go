package protocol

import (
	"bytes"
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

type copyWriterTo struct {
	io.Reader
	called bool
	n      int64
	err    error
}

func (r *copyWriterTo) WriteTo(io.Writer) (int64, error) {
	r.called = true
	return r.n, r.err
}

type copyReaderFrom struct {
	io.Writer
	called bool
	n      int64
	err    error
}

func (w *copyReaderFrom) ReadFrom(io.Reader) (int64, error) {
	w.called = true
	return w.n, w.err
}

type copyBufferReader struct {
	io.Reader
	bufferSize int
}

func (r *copyBufferReader) Read(p []byte) (int, error) {
	r.bufferSize = len(p)
	return r.Reader.Read(p)
}

func TestCopyBufferedDispatchAndPooledBuffer(t *testing.T) {
	copyPool := NewCopyBufferPool(DefaultCopyBufferSize)
	writerToErr := errors.New("writer-to error")
	src := &copyWriterTo{n: 11, err: writerToErr}
	dst := &copyReaderFrom{n: 22, err: errors.New("reader-from error")}
	n, err := copyPool.CopyBuffered(dst, src, false)
	if n != 11 || !errors.Is(err, writerToErr) || !src.called || dst.called {
		t.Fatalf("WriterTo path = (%d, %v, %t, %t), want (11, %v, true, false)", n, err, src.called, dst.called, writerToErr)
	}

	readerFromErr := errors.New("reader-from error")
	dst = &copyReaderFrom{n: 13, err: readerFromErr}
	n, err = copyPool.CopyBuffered(dst, relayErrorReader{err: errors.New("unexpected read")}, false)
	if n != 13 || !errors.Is(err, readerFromErr) || !dst.called {
		t.Fatalf("ReaderFrom path = (%d, %v, %t), want (13, %v, true)", n, err, dst.called, readerFromErr)
	}

	var copied bytes.Buffer
	pooledSrc := &copyBufferReader{Reader: bytes.NewBufferString("payload")}
	n, err = copyPool.CopyBuffered(struct{ io.Writer }{&copied}, pooledSrc, false)
	if n != 7 || err != nil || copied.String() != "payload" || pooledSrc.bufferSize != DefaultCopyBufferSize {
		t.Fatalf("fallback path = (%d, %v, %q, %d), want (7, nil, %q, %d)", n, err, copied.String(), pooledSrc.bufferSize, "payload", DefaultCopyBufferSize)
	}

	copied.Reset()
	pooledSrc = &copyBufferReader{Reader: bytes.NewBufferString("payload")}
	src = &copyWriterTo{Reader: pooledSrc, n: 11, err: writerToErr}
	dst = &copyReaderFrom{Writer: &copied, n: 13, err: readerFromErr}
	n, err = copyPool.CopyBuffered(dst, src, true)
	if n != 7 || err != nil || copied.String() != "payload" || pooledSrc.bufferSize != DefaultCopyBufferSize || src.called || dst.called {
		t.Fatalf("forced path = (%d, %v, %q, %d, %t, %t), want (7, nil, %q, %d, false, false)", n, err, copied.String(), pooledSrc.bufferSize, src.called, dst.called, "payload", DefaultCopyBufferSize)
	}
}

func TestCopyBufferPoolsKeepIndependentSizes(t *testing.T) {
	pools := []struct {
		pool *CopyBufferPool
		size int
	}{
		{NewCopyBufferPool(4096), 4096},
		{NewCopyBufferPool(8192), 8192},
	}
	for _, test := range pools {
		src := &copyBufferReader{Reader: bytes.NewBufferString("payload")}
		var dst bytes.Buffer
		if _, err := test.pool.CopyBuffered(struct{ io.Writer }{&dst}, src, true); err != nil {
			t.Fatalf("copy with %d-byte pool: %v", test.size, err)
		}
		if src.bufferSize != test.size || dst.String() != "payload" {
			t.Fatalf("copy with %d-byte pool used buffer %d and wrote %q", test.size, src.bufferSize, dst.String())
		}
	}
}

func TestRelayLifecycleWaitsForBothDirections(t *testing.T) {
	copyPool := NewCopyBufferPool(DefaultCopyBufferSize)
	release := make(chan struct{})
	a := &relayTestEndpoint{Reader: relayErrorReader{err: io.EOF}, Writer: io.Discard}
	b := &relayTestEndpoint{Reader: relayBlockingReader{release: release}, Writer: io.Discard}
	halfClosed := make(chan error, 1)

	relay := copyPool.StartRelay(a, b, func(err error) error {
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
	copyPool := NewCopyBufferPool(DefaultCopyBufferSize)
	copyErr := errors.New("a to b failed")
	release := make(chan struct{})
	a := &relayTestEndpoint{Reader: relayErrorReader{err: copyErr}, Writer: io.Discard}
	b := &relayTestEndpoint{Reader: relayBlockingReader{release: release}, Writer: io.Discard}

	relay := copyPool.StartRelay(a, b, func(err error) error {
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
	copyPool := NewCopyBufferPool(DefaultCopyBufferSize)
	copyErr := errors.New("copy failed")
	callbackErr := errors.New("callback failed")
	otherCallbackErr := errors.New("other callback failed")
	a := &relayTestEndpoint{Reader: relayErrorReader{err: copyErr}, Writer: io.Discard}
	b := &relayTestEndpoint{Reader: relayErrorReader{err: io.EOF}, Writer: io.Discard}

	err := copyPool.StartRelay(a, b, func(error) error {
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
	copyPool := NewCopyBufferPool(DefaultCopyBufferSize)
	want := errors.New("copy failed")
	a := &relayTestEndpoint{Reader: relayErrorReader{err: want}, Writer: io.Discard}
	b := &relayTestEndpoint{Reader: relayErrorReader{err: io.EOF}, Writer: io.Discard}
	relay := copyPool.StartRelay(a, b, nil, nil)

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
