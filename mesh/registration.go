package mesh

import (
	"context"
	"fmt"
	"slices"
	"time"

	sharedtoken "github.com/Mmx233/QMux/auth/token"
	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/protocol"
	"github.com/Mmx233/QMux/server/auth"
	"github.com/quic-go/quic-go"
)

func outboundRegistration(
	ctx context.Context,
	conn *quic.Conn,
	registration protocol.MeshRegister,
	authConfig config.ClientAuth,
	declaration []byte,
	ledger *declarationLedger,
	paths *pathBudget,
	limits config.MeshServerLimits,
	snapshot *peerSnapshot,
) (*quic.Stream, *stagedState, error) {
	if err := waitForMeshHandshake(ctx, conn); err != nil {
		return nil, nil, err
	}
	authConfig.ApplyDefaults()
	expectedScheme := ""
	if authConfig.Method == config.ClientAuthMethodToken {
		expectedScheme = sharedtoken.MeshScheme
		proof, err := sharedtoken.ComputeMesh(
			[]byte(authConfig.Token),
			sharedtoken.MeshTranscript{
				Version:        registration.Version,
				Capabilities:   registration.Capabilities,
				Role:           registration.Role,
				TargetServerID: registration.TargetServerID,
				PeerServerID:   registration.PeerServerID,
				InstanceID:     registration.InstanceID,
				GroupID:        registration.GroupID,
			},
			conn.ConnectionState().TLS,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("compute mesh token proof: %w", err)
		}
		registration.Auth = &protocol.RegisterAuth{Scheme: sharedtoken.MeshScheme, Proof: proof}
	}

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, nil, meshRegistrationError(ctx, "open mesh control stream", err)
	}
	stream.SetPriority(0, true)
	committed := false
	defer func() {
		if !committed {
			stream.CancelRead(meshStreamError)
			stream.CancelWrite(meshStreamError)
			_ = stream.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err := stream.SetDeadline(deadline); err != nil {
			return nil, nil, meshRegistrationError(ctx, "set mesh registration deadline", err)
		}
	}
	cancelUnblocked := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		defer close(cancelUnblocked)
		_ = stream.SetDeadline(time.Now())
	})
	stopCalled := false
	stopAndWait := func() bool {
		stopCalled = true
		if stopCancellation() {
			return true
		}
		<-cancelUnblocked
		return false
	}
	defer func() {
		if !stopCalled {
			_ = stopAndWait()
		}
	}()

	if err := protocol.WriteMeshRegister(stream, registration); err != nil {
		return nil, nil, meshRegistrationError(ctx, "write mesh registration", err)
	}
	var state *stagedState
	if registration.Role == protocol.MeshRoleClient {
		if err := sendClientInitial(stream, declaration); err != nil {
			return nil, nil, meshRegistrationError(ctx, "write mesh declaration", err)
		}
	} else {
		written := make(chan error, 1)
		go func() { written <- sendPeerSnapshot(ctx, stream, snapshot) }()
		var err error
		state, err = receiveInitial(ctx, stream, ledger, paths, limits, protocol.MeshRolePeer, "")
		if err != nil {
			stream.CancelWrite(meshStreamError)
			<-written
			return nil, nil, meshRegistrationError(ctx, "read mesh peer initial state", err)
		}
		if err := <-written; err != nil {
			state.close()
			return nil, nil, meshRegistrationError(ctx, "write mesh peer initial state", err)
		}
		if err := protocol.WriteMeshControl(stream, protocol.MeshReady{State: protocol.MeshStateStaged}); err != nil {
			state.close()
			return nil, nil, meshRegistrationError(ctx, "write mesh peer Ready", err)
		}
	}
	ack, err := protocol.ReadMeshRegisterAck(stream)
	if err != nil {
		state.close()
		return nil, nil, meshRegistrationError(ctx, "read mesh registration acknowledgment", err)
	}
	if err := protocol.ValidateMeshRegisterAck(
		ack,
		registration.TargetServerID,
		registration.Role,
		expectedScheme,
		registration.Capabilities,
	); err != nil {
		state.close()
		return nil, nil, err
	}
	if ack.State != protocol.MeshStateStaged {
		state.close()
		return nil, nil, fmt.Errorf("unexpected mesh initial state result %q", ack.State)
	}
	if !stopAndWait() || ctx.Err() != nil {
		state.close()
		return nil, nil, fmt.Errorf("mesh registration canceled: %w", context.Cause(ctx))
	}
	if err := stream.SetDeadline(time.Time{}); err != nil {
		state.close()
		return nil, nil, fmt.Errorf("clear mesh registration deadline: %w", err)
	}
	committed = true
	return stream, state, nil
}

func authenticateInbound(
	authenticator auth.Auth,
	conn *quic.Conn,
	registration protocol.MeshRegister,
) error {
	authRegistration := auth.MeshRegistration{
		Version:        registration.Version,
		Capabilities:   registration.Capabilities,
		Role:           registration.Role,
		TargetServerID: registration.TargetServerID,
		PeerServerID:   registration.PeerServerID,
		InstanceID:     registration.InstanceID,
		GroupID:        registration.GroupID,
	}
	if registration.Auth != nil {
		authRegistration.Scheme = registration.Auth.Scheme
		authRegistration.Proof = slices.Clone(registration.Auth.Proof)
	}
	return authenticator.VerifyMesh(conn.ConnectionState().TLS, authRegistration)
}

func writeMeshAck(
	stream *quic.Stream,
	success bool,
	message, serverID, role, authScheme string,
) error {
	ack := protocol.MeshRegisterAck{Success: success, Message: message}
	if success {
		ack.State = protocol.MeshStateStaged
		ack.ServerID = serverID
		ack.Role = role
		ack.SelectedVersion = protocol.MeshProtocolVersion
		ack.SelectedCapabilities = protocol.MeshCapabilities()
		ack.SelectedAuthScheme = authScheme
	}
	return protocol.WriteMeshRegisterAck(stream, ack)
}

func waitForMeshHandshake(ctx context.Context, conn *quic.Conn) error {
	select {
	case <-conn.HandshakeComplete():
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for mesh TLS handshake: %w", context.Cause(ctx))
	case <-conn.Context().Done():
		return fmt.Errorf("wait for mesh TLS handshake: connection closed: %w", context.Cause(conn.Context()))
	}
}

func meshRegistrationError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w: %w", operation, context.Cause(ctx), err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
