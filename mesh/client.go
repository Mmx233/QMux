package mesh

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mmx233/QMux/config"
	"github.com/Mmx233/QMux/internal/outbound"
	"github.com/Mmx233/QMux/internal/tlsreload"
	"github.com/Mmx233/QMux/protocol"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var (
	ErrMeshClientAlreadyStarted = errors.New("mesh client is already started")
	ErrMeshClientStopped        = errors.New("mesh client is stopped")
)

type clientEndpointState struct {
	endpoint  config.MeshServerEndpoint
	lifecycle outbound.Endpoint[*Session]
}

type Client struct {
	config      config.MeshClient
	declaration []byte
	logger      zerolog.Logger
	tlsState    atomic.Pointer[outboundTLSState]
	reloader    *tlsreload.Reloader

	attemptTimeout time.Duration
	stableGrace    time.Duration
	reconnectDelay func(int) time.Duration
	sessions       chan *Session

	publishMu sync.Mutex
	closed    bool
	endpoints []clientEndpointState

	lifecycleMu  sync.Mutex
	started      bool
	stopping     bool
	cancel       context.CancelCauseFunc
	ready        chan struct{}
	done         chan struct{}
	readyOnce    sync.Once
	doneOnce     sync.Once
	sessionsOnce sync.Once
	workerWG     sync.WaitGroup
	workers      atomic.Int64
}

type ClientEndpointSnapshot struct {
	ServerID          string
	Address           string
	Current           bool
	ReconnectStage    int
	ReconnectAttempts uint64
}

type ClientSnapshot struct {
	Closed          bool
	EndpointWorkers int64
	Endpoints       []ClientEndpointSnapshot
}

func NewClient(conf *config.MeshClient) (*Client, error) {
	if conf == nil {
		return nil, errors.New("mesh client config is nil")
	}
	owned := config.CloneMeshClientConfig(conf)
	if err := config.FinalizeMeshClientConfig(&owned); err != nil {
		return nil, fmt.Errorf("invalid mesh client config: %w", err)
	}
	logger := log.With().Str("com", "mesh-client").Str("instance_id", owned.InstanceID).Logger()
	client := &Client{
		config:         owned,
		declaration:    owned.Group.CanonicalBytes(),
		logger:         logger,
		attemptTimeout: outbound.AttemptTimeout,
		stableGrace:    defaultStableGrace(),
		reconnectDelay: func(stage int) time.Duration { return outbound.ReconnectDelay(stage, rand.Int64N) },
		sessions:       make(chan *Session, max(1, len(owned.Tunnel.Servers))),
		ready:          make(chan struct{}),
		done:           make(chan struct{}),
	}
	for _, endpoint := range owned.Tunnel.Servers {
		client.endpoints = append(client.endpoints, clientEndpointState{endpoint: endpoint})
	}
	var err error
	client.reloader, err = newOutboundTLSReloader(
		"mesh-client-outbound",
		owned.Tunnel.TLS,
		owned.Tunnel.Auth,
		&client.tlsState,
		logger,
	)
	if err != nil {
		return nil, fmt.Errorf("load mesh client TLS material: %w", err)
	}
	return client, nil
}

func (c *Client) Sessions() <-chan *Session { return c.sessions }
func (c *Client) Ready() <-chan struct{}    { return c.ready }
func (c *Client) Done() <-chan struct{}     { return c.done }

func (c *Client) Snapshot() ClientSnapshot {
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	snapshot := ClientSnapshot{
		Closed:          c.closed,
		EndpointWorkers: c.workers.Load(),
		Endpoints:       make([]ClientEndpointSnapshot, len(c.endpoints)),
	}
	for i := range c.endpoints {
		endpoint := &c.endpoints[i]
		snapshot.Endpoints[i] = ClientEndpointSnapshot{
			ServerID:          endpoint.endpoint.ServerID,
			Address:           endpoint.endpoint.Address,
			Current:           !endpoint.lifecycle.Empty(),
			ReconnectStage:    endpoint.lifecycle.RetryStage(),
			ReconnectAttempts: endpoint.lifecycle.ReconnectAttempts(),
		}
	}
	return snapshot
}

func (c *Client) Start(ctx context.Context) (runErr error) {
	c.lifecycleMu.Lock()
	if c.stopping {
		c.lifecycleMu.Unlock()
		return ErrMeshClientStopped
	}
	if c.started {
		c.lifecycleMu.Unlock()
		return ErrMeshClientAlreadyStarted
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	c.started = true
	c.cancel = cancel
	c.lifecycleMu.Unlock()

	defer func() {
		c.closePublishing()
		c.reloader.Stop()
		_ = c.reloader.Wait()
		c.readyOnce.Do(func() { close(c.ready) })
		c.sessionsOnce.Do(func() { close(c.sessions) })
		c.doneOnce.Do(func() { close(c.done) })
	}()

	if err := c.reloader.PrepareStart(runCtx, c.config.Tunnel.TLS.AutoReload); err != nil {
		return fmt.Errorf("prepare mesh client TLS: %w", err)
	}
	fatal := make(chan error, 1)
	if c.config.Tunnel.TLS.AutoReload {
		go reportTLSFailure("mesh client TLS", c.reloader, fatal)
	}
	for i := range c.endpoints {
		endpoint := &c.endpoints[i]
		c.workerWG.Go(func() { c.endpointWorker(runCtx, endpoint) })
	}
	c.readyOnce.Do(func() { close(c.ready) })

	select {
	case <-runCtx.Done():
		runErr = context.Cause(runCtx)
	case runErr = <-fatal:
		cancel(runErr)
	}
	c.closePublishing()
	c.workerWG.Wait()
	if errors.Is(runErr, ErrMeshClientStopped) {
		return nil
	}
	return runErr
}

func (c *Client) Stop() error {
	c.lifecycleMu.Lock()
	if c.stopping {
		done := c.done
		c.lifecycleMu.Unlock()
		<-done
		return nil
	}
	c.stopping = true
	started := c.started
	cancel := c.cancel
	c.lifecycleMu.Unlock()
	if !started {
		c.closePublishing()
		c.reloader.Stop()
		_ = c.reloader.Wait()
		c.readyOnce.Do(func() { close(c.ready) })
		c.sessionsOnce.Do(func() { close(c.sessions) })
		c.doneOnce.Do(func() { close(c.done) })
		return nil
	}
	if cancel != nil {
		cancel(ErrMeshClientStopped)
	}
	<-c.done
	return nil
}

func (c *Client) endpointWorker(ctx context.Context, endpoint *clientEndpointState) {
	c.workers.Add(1)
	defer c.workers.Add(-1)
	retry := false
	for {
		if retry {
			c.publishMu.Lock()
			stage := endpoint.lifecycle.RetryStage()
			c.publishMu.Unlock()
			delay := c.reconnectDelay(stage)
			if !outbound.WaitReconnect(ctx, ctx, delay) {
				return
			}
			c.publishMu.Lock()
			endpoint.lifecycle.AdvanceRetry(stage, outbound.MaxReconnectStage)
			c.publishMu.Unlock()
		}
		_, err := c.runEndpointAttempt(ctx, endpoint)
		if err != nil && ctx.Err() == nil {
			c.logger.Debug().Str("server_id", endpoint.endpoint.ServerID).Err(err).Msg("mesh client connection ended")
		}
		if ctx.Err() != nil {
			return
		}
		retry = true
	}
}

func (c *Client) runEndpointAttempt(
	ctx context.Context,
	endpoint *clientEndpointState,
) (stable bool, resultErr error) {
	attemptCtx, cancel := outbound.AttemptContext(ctx, ctx, c.attemptTimeout)
	owner := outbound.NewOwner(cancel)
	var session *Session
	defer func() {
		c.publishMu.Lock()
		exactCurrent := session != nil && endpoint.lifecycle.RetireForReconnect(session, session.ReconnectStable())
		stable = exactCurrent && session.ReconnectStable()
		c.publishMu.Unlock()
		owner.Finish()
	}()
	state := c.tlsState.Load()
	if state == nil {
		return false, errors.New("mesh client TLS state is unavailable")
	}
	configured := endpoint.endpoint
	conn, err := outbound.Dial(
		attemptCtx,
		configured.Address,
		configured.ServerName,
		state.baseTLSConfig,
		state.sessionCaches.GetOrCreate(configured.Address),
		c.config.Tunnel.Quic.GetConfig(),
	)
	if err != nil {
		return false, err
	}
	installConnectionOwner(owner, conn)
	registration := protocol.MeshRegister{
		Version:        protocol.MeshProtocolVersion,
		Capabilities:   protocol.MeshCapabilities(),
		Role:           protocol.MeshRoleClient,
		TargetServerID: configured.ServerID,
		InstanceID:     c.config.InstanceID,
		GroupID:        c.config.Group.GroupID,
	}
	stream, _, err := outboundRegistration(attemptCtx, conn, registration, c.config.Tunnel.Auth, c.declaration, nil, nil, config.MeshServerLimits{}, nil)
	if err != nil {
		return false, err
	}
	owner.Cancel()
	session = newSession(
		ctx,
		RoleClient,
		DirectionOutbound,
		configured.ServerID,
		c.config.InstanceID,
		c.config.Group.GroupID,
		"",
		conn,
		stream,
		c.stableGrace,
	)
	installSessionOwner(owner, session)
	c.publishMu.Lock()
	if c.closed || ctx.Err() != nil || !endpoint.lifecycle.Publish(session) {
		c.publishMu.Unlock()
		return false, errors.New("mesh client publication gate is closed")
	}
	c.publishMu.Unlock()

	if !outbound.DeliverThenStart(ctx, conn.Context(), c.sessions, session, func() {
		resultErr = session.run(c.config.Tunnel.HeartbeatInterval, c.config.Tunnel.HealthTimeout)
	}) {
		return false, context.Cause(ctx)
	}
	return false, resultErr
}

func (c *Client) closePublishing() {
	c.lifecycleMu.Lock()
	cancel := c.cancel
	c.lifecycleMu.Unlock()
	c.publishMu.Lock()
	c.closed = true
	if cancel != nil {
		cancel(ErrMeshClientStopped)
	}
	var sessions []*Session
	for i := range c.endpoints {
		if session := c.endpoints[i].lifecycle.Take(); session != nil {
			sessions = append(sessions, session)
		}
	}
	c.publishMu.Unlock()
	for _, session := range sessions {
		session.Close()
	}
}
