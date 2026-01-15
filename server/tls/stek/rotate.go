package stek

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// RotateManager manages periodic rotation of TLS session ticket encryption keys.
// It maintains multiple keys to allow smooth rotation without breaking existing sessions.
//
// The first key in the slice is used for encrypting new session tickets, while all
// keys can be used for decrypting tickets. This allows clients with tickets encrypted
// with old keys to still resume sessions during the overlap period.
//
// Thread-safety: Keys can be read concurrently via the atomic pointer. Rotation
// happens in a single background goroutine.
type RotateManager struct {
	Keys     *atomic.Pointer[[][32]byte]
	interval time.Duration
	overlap  uint8
	ticker   *time.Ticker
	stopCh   chan struct{}
	logger   zerolog.Logger
}

// NewRotateManager creates a new RotateManager with the specified rotation interval and key overlap.
// The overlap parameter determines how many keys to maintain (current + old keys).
//
// Example:
//
//	manager, err := stek.NewRotateManager(24*time.Hour, 3)
//	if err != nil {
//	    return err
//	}
//	defer manager.Stop()
//	manager.Start(ctx)
func NewRotateManager(interval time.Duration, overlap uint8) (*RotateManager, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("rotation interval must be positive, got %v", interval)
	}
	if overlap < 1 {
		return nil, fmt.Errorf("overlap must be at least 1, got %d", overlap)
	}

	m := &RotateManager{
		Keys:     &atomic.Pointer[[][32]byte]{},
		interval: interval,
		overlap:  overlap,
		logger:   log.With().Str("com", "stek").Logger(),
	}

	// Generate initial key set
	initialKeys := make([][32]byte, overlap)
	for i := range initialKeys {
		key, err := m.generateKey()
		if err != nil {
			return nil, fmt.Errorf("failed to generate initial key %d: %w", i, err)
		}
		initialKeys[i] = key
	}
	m.Keys.Store(&initialKeys)

	m.logger.Info().
		Int("initial_keys", len(initialKeys)).
		Uint8("overlap", overlap).
		Msg("initialized session ticket encryption keys")

	return m, nil
}

// generateKey generates a cryptographically secure 32-byte key for session ticket encryption.
func (m *RotateManager) generateKey() ([32]byte, error) {
	var key [32]byte
	_, err := rand.Read(key[:])
	if err != nil {
		return key, fmt.Errorf("failed to generate session ticket key: %w", err)
	}
	return key, nil
}

// rotate performs a key rotation by generating a new key and maintaining the overlap.
func (m *RotateManager) rotate() error {
	// Generate new key
	newKey, err := m.generateKey()
	if err != nil {
		return err
	}

	// Load current keys
	currentKeys := m.Keys.Load()

	// Calculate new slice size (capped at overlap)
	newSize := len(*currentKeys) + 1
	if newSize > int(m.overlap) {
		newSize = int(m.overlap)
	}

	// Create new slice with new key first
	newKeys := make([][32]byte, newSize)
	newKeys[0] = newKey

	// Copy old keys (up to overlap-1)
	copy(newKeys[1:], *currentKeys)

	// Store atomically
	m.Keys.Store(&newKeys)

	m.logger.Info().
		Int("total_keys", len(newKeys)).
		Int("overlap", int(m.overlap)).
		Msg("rotated session ticket encryption keys")

	return nil
}

// Start begins the periodic key rotation in a background goroutine.
// The rotation will continue until the context is cancelled or Stop is called.
func (m *RotateManager) Start(ctx context.Context) {
	m.ticker = time.NewTicker(m.interval)
	m.stopCh = make(chan struct{})

	m.logger.Info().
		Dur("interval", m.interval).
		Uint8("overlap", m.overlap).
		Msg("starting session ticket key rotation")

	go m.run(ctx)
}

// run is the background goroutine that handles periodic key rotation.
func (m *RotateManager) run(ctx context.Context) {
	for {
		select {
		case <-m.ticker.C:
			if err := m.rotate(); err != nil {
				m.logger.Error().Err(err).Msg("failed to rotate session ticket keys")
				// Continue running despite error
			}
		case <-ctx.Done():
			m.logger.Info().Msg("stopping session ticket key rotation (context cancelled)")
			m.ticker.Stop()
			close(m.stopCh)
			return
		case <-m.stopCh:
			m.logger.Info().Msg("stopping session ticket key rotation")
			m.ticker.Stop()
			return
		}
	}
}

// Stop gracefully stops the key rotation. This method is idempotent and safe to call multiple times.
func (m *RotateManager) Stop() {
	if m.stopCh != nil {
		select {
		case <-m.stopCh:
			// Already stopped
		default:
			close(m.stopCh)
		}
	}
}
