package client

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Mmx233/QMux/protocol"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
)

func newDrainControlPair(t *testing.T) (*ServerConnection, *quic.Stream) {
	t.Helper()
	sender, receiver := newHeartbeatFlowControlPair(t)
	stream := openHeartbeatTestStream(t, sender)
	if err := protocol.WriteHeartbeat(stream, time.Now().Unix()); err != nil {
		t.Fatalf("advertise drain control stream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	peerStream, err := receiver.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("accept drain control stream: %v", err)
	}
	var heartbeat protocol.HeartbeatMsg
	if err := protocol.ReadTypedMessage(peerStream, protocol.MsgTypeHeartbeat, &heartbeat); err != nil {
		t.Fatalf("read drain control preface: %v", err)
	}

	sc := NewServerConnection("drain.test:8443", "drain.test", tls.NewLRUClientSessionCache(1), zerolog.Nop())
	sc.capabilities = []string{protocol.CapabilityTCPDrainV1}
	sc.SetHealthConfig(2 * time.Hour)
	sc.UpdateLastReceivedFromServer()
	sc.MarkHealthy()
	sc.controlStream.Store(stream)
	t.Cleanup(func() {
		_ = sc.Close()
		sc.waitControl()
	})
	return sc, peerStream
}

func TestDrainCompletePrequeuedBeforeRequestIsRejected(t *testing.T) {
	sc, peerStream := newDrainControlPair(t)
	if err := protocol.WriteDrainComplete(peerStream, -1); err != nil {
		t.Fatalf("prequeue DrainComplete: %v", err)
	}
	sc.StartHeartbeatLoops(time.Hour)
	awaitLifecycle(t, sc.controlDone, "unsolicited DrainComplete control exit")
	if err := sc.controlResult(); err == nil || !strings.Contains(err.Error(), "unsolicited drain complete") {
		t.Fatalf("control result = %v, want unsolicited DrainComplete", err)
	}
}

func TestControlRejectsUnnegotiatedAndWrongDirectionDrain(t *testing.T) {
	tests := []struct {
		name       string
		capability bool
		write      func(*quic.Stream) error
		want       string
	}{
		{
			name:  "unnegotiated complete",
			write: func(stream *quic.Stream) error { return protocol.WriteDrainComplete(stream, -1) },
			want:  "unnegotiated drain complete",
		},
		{
			name:       "wrong-direction request",
			capability: true,
			write:      func(stream *quic.Stream) error { return protocol.WriteDrainRequest(stream) },
			want:       "wrong-direction drain request",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sc, peerStream := newDrainControlPair(t)
			if !test.capability {
				sc.capabilities = nil
			}
			if err := test.write(peerStream); err != nil {
				t.Fatalf("write drain frame: %v", err)
			}
			sc.StartHeartbeatLoops(time.Hour)
			awaitLifecycle(t, sc.controlDone, "invalid drain direction control exit")
			if err := sc.controlResult(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("control result = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDrainCompleteDuringRequestWriteRequiresFullSuccess(t *testing.T) {
	t.Run("full write commits epoch", func(t *testing.T) {
		sc, peerStream := newDrainControlPair(t)
		if err := sc.controlStream.Load().SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
			t.Fatalf("set stale DrainRequest deadline: %v", err)
		}
		writeStarted := make(chan struct{})
		releaseWrite := make(chan struct{})
		sc.writeDrain = func(stream *quic.Stream) error {
			close(writeStarted)
			<-releaseWrite
			return protocol.WriteDrainRequest(stream)
		}
		sc.StartHeartbeatLoops(time.Hour)
		requestDone := callClientLifecycle(func() error { return sc.requestDrain(context.Background()) })
		awaitLifecycle(t, writeStarted, "DrainRequest write start")
		if err := protocol.WriteDrainComplete(peerStream, -1); err != nil {
			t.Fatalf("write during-request DrainComplete: %v", err)
		}
		awaitRetirementCondition(t, "during-write DrainComplete enqueue", func() bool {
			return sc.controlPending.Load() == 1
		})
		close(releaseWrite)
		var request protocol.DrainRequestMsg
		if err := protocol.ReadTypedMessage(peerStream, protocol.MsgTypeDrainRequest, &request); err != nil {
			t.Fatalf("read committed DrainRequest: %v", err)
		}
		if err := awaitClientLifecycle(t, requestDone, "committed DrainRequest"); err != nil {
			t.Fatalf("requestDrain() error = %v", err)
		}
		if fence := awaitLifecycle(t, sc.drainComplete, "accepted during-write DrainComplete"); fence != -1 {
			t.Fatalf("drain fence = %d, want -1", fence)
		}
		if !sc.controlAlive() {
			t.Fatalf("control stopped after a fully written DrainRequest: %v", sc.controlResult())
		}
	})

	t.Run("write failure does not commit epoch", func(t *testing.T) {
		sc, peerStream := newDrainControlPair(t)
		writeStarted := make(chan struct{})
		releaseWrite := make(chan struct{})
		want := errors.New("drain transport write failed")
		sc.writeDrain = func(*quic.Stream) error {
			close(writeStarted)
			<-releaseWrite
			return want
		}
		sc.StartHeartbeatLoops(time.Hour)
		requestDone := callClientLifecycle(func() error { return sc.requestDrain(context.Background()) })
		awaitLifecycle(t, writeStarted, "failing DrainRequest write start")
		if err := protocol.WriteDrainComplete(peerStream, -1); err != nil {
			t.Fatalf("write during-failure DrainComplete: %v", err)
		}
		awaitRetirementCondition(t, "uncommitted DrainComplete enqueue", func() bool {
			return sc.controlPending.Load() == 1
		})
		close(releaseWrite)
		if err := awaitClientLifecycle(t, requestDone, "failed DrainRequest"); !errors.Is(err, want) {
			t.Fatalf("requestDrain() error = %v, want write failure", err)
		}
		awaitLifecycle(t, sc.controlDone, "failed-write control exit")
		if err := sc.controlResult(); !errors.Is(err, want) {
			t.Fatalf("control result = %v, want write failure", err)
		}
		select {
		case fence := <-sc.drainComplete:
			t.Fatalf("uncommitted DrainComplete accepted fence %d", fence)
		default:
		}
	})
}

func TestControlReaderBlockedInReadIsJoinedOnClose(t *testing.T) {
	sc, _ := newDrainControlPair(t)
	sc.StartHeartbeatLoops(time.Hour)
	if !sc.controlAlive() {
		t.Fatal("control loop did not start")
	}
	if err := sc.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	joined := make(chan struct{})
	go func() {
		sc.waitControl()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("control reader blocked in ReadMessage was not canceled and joined")
	}
	select {
	case <-sc.controlDone:
	default:
		t.Fatal("controlDone remained open after the reader was joined")
	}
}

func TestDrainWriteDeadlineUsesFreshBound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name         string
		interval     time.Duration
		healthExpiry time.Time
		want         time.Time
		fresh        bool
	}{
		{"interval bound", time.Second, now.Add(2 * time.Second), now.Add(time.Second), true},
		{"health bound", 2 * time.Second, now.Add(time.Second), now.Add(time.Second), true},
		{"expired health bound", time.Second, now.Add(-time.Nanosecond), now.Add(-time.Nanosecond), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, fresh := drainWriteDeadline(now, test.interval, test.healthExpiry)
			if !got.Equal(test.want) || fresh != test.fresh {
				t.Fatalf("drainWriteDeadline() = (%v, %t), want (%v, %t)", got, fresh, test.want, test.fresh)
			}
		})
	}
}
