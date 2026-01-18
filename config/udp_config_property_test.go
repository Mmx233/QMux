package config

import (
	"testing"

	"pgregory.net/rapid"
)

// Feature: udp-performance-optimization, Property 8: Config Defaults and Overrides
// **Validates: Requirements 6.2, 6.3, 6.4**

// TestProperty_UDPConfig_FragmentAssemblerShards_Defaults verifies that
// GetFragmentAssemblerShards returns 16 when FragmentAssemblerShards is zero or negative.
func TestProperty_UDPConfig_FragmentAssemblerShards_Defaults(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate zero or negative values
		shardValue := rapid.IntRange(-1000, 0).Draw(t, "shard_value")

		cfg := &UDPConfig{
			FragmentAssemblerShards: shardValue,
		}

		result := cfg.GetFragmentAssemblerShards()
		if result != 16 {
			t.Fatalf("expected GetFragmentAssemblerShards() to return 16 for value %d, got %d",
				shardValue, result)
		}
	})
}

// TestProperty_UDPConfig_FragmentAssemblerShards_CustomValues verifies that
// GetFragmentAssemblerShards returns the configured value when it's positive.
func TestProperty_UDPConfig_FragmentAssemblerShards_CustomValues(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate positive values
		shardValue := rapid.IntRange(1, 1000).Draw(t, "shard_value")

		cfg := &UDPConfig{
			FragmentAssemblerShards: shardValue,
		}

		result := cfg.GetFragmentAssemblerShards()
		if result != shardValue {
			t.Fatalf("expected GetFragmentAssemblerShards() to return %d, got %d",
				shardValue, result)
		}
	})
}

// TestProperty_UDPConfig_EnableBufferPooling_NilDefault verifies that
// IsBufferPoolingEnabled returns true when EnableBufferPooling is nil.
func TestProperty_UDPConfig_EnableBufferPooling_NilDefault(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random values for other fields to ensure nil EnableBufferPooling
		// is independent of other config values
		shardValue := rapid.IntRange(-100, 100).Draw(t, "shard_value")
		readBufferSize := rapid.IntRange(-100, 100000).Draw(t, "read_buffer_size")

		cfg := &UDPConfig{
			EnableBufferPooling:     nil, // Explicitly nil
			FragmentAssemblerShards: shardValue,
			ReadBufferSize:          readBufferSize,
		}

		result := cfg.IsBufferPoolingEnabled()
		if result != true {
			t.Fatalf("expected IsBufferPoolingEnabled() to return true when nil, got %v", result)
		}
	})
}

// TestProperty_UDPConfig_EnableBufferPooling_ExplicitValues verifies that
// IsBufferPoolingEnabled returns the configured value when explicitly set.
func TestProperty_UDPConfig_EnableBufferPooling_ExplicitValues(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate boolean value
		enablePooling := rapid.Bool().Draw(t, "enable_pooling")

		cfg := &UDPConfig{
			EnableBufferPooling: &enablePooling,
		}

		result := cfg.IsBufferPoolingEnabled()
		if result != enablePooling {
			t.Fatalf("expected IsBufferPoolingEnabled() to return %v, got %v",
				enablePooling, result)
		}
	})
}

// TestProperty_UDPConfig_ReadBufferSize_Defaults verifies that
// GetReadBufferSize returns 65535 when ReadBufferSize is zero or negative.
func TestProperty_UDPConfig_ReadBufferSize_Defaults(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate zero or negative values
		bufferSize := rapid.IntRange(-100000, 0).Draw(t, "buffer_size")

		cfg := &UDPConfig{
			ReadBufferSize: bufferSize,
		}

		result := cfg.GetReadBufferSize()
		if result != 65535 {
			t.Fatalf("expected GetReadBufferSize() to return 65535 for value %d, got %d",
				bufferSize, result)
		}
	})
}

// TestProperty_UDPConfig_ReadBufferSize_CustomValues verifies that
// GetReadBufferSize returns the configured value when it's positive.
func TestProperty_UDPConfig_ReadBufferSize_CustomValues(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate positive values
		bufferSize := rapid.IntRange(1, 1000000).Draw(t, "buffer_size")

		cfg := &UDPConfig{
			ReadBufferSize: bufferSize,
		}

		result := cfg.GetReadBufferSize()
		if result != bufferSize {
			t.Fatalf("expected GetReadBufferSize() to return %d, got %d",
				bufferSize, result)
		}
	})
}

// TestProperty_UDPConfig_AllDefaults verifies that a zero-value UDPConfig
// returns all expected defaults.
func TestProperty_UDPConfig_AllDefaults(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Zero-value config should return all defaults
		cfg := &UDPConfig{}

		shards := cfg.GetFragmentAssemblerShards()
		if shards != 16 {
			t.Fatalf("expected default shards to be 16, got %d", shards)
		}

		pooling := cfg.IsBufferPoolingEnabled()
		if pooling != true {
			t.Fatalf("expected default buffer pooling to be true, got %v", pooling)
		}

		bufferSize := cfg.GetReadBufferSize()
		if bufferSize != 65535 {
			t.Fatalf("expected default read buffer size to be 65535, got %d", bufferSize)
		}
	})
}

// TestProperty_UDPConfig_AllCustomValues verifies that when all fields are set
// to valid positive values, the getter methods return those values.
func TestProperty_UDPConfig_AllCustomValues(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate valid positive values for all fields
		shardValue := rapid.IntRange(1, 256).Draw(t, "shard_value")
		enablePooling := rapid.Bool().Draw(t, "enable_pooling")
		bufferSize := rapid.IntRange(1, 1000000).Draw(t, "buffer_size")

		cfg := &UDPConfig{
			FragmentAssemblerShards: shardValue,
			EnableBufferPooling:     &enablePooling,
			ReadBufferSize:          bufferSize,
		}

		// Verify all getters return configured values
		if result := cfg.GetFragmentAssemblerShards(); result != shardValue {
			t.Fatalf("expected GetFragmentAssemblerShards() to return %d, got %d",
				shardValue, result)
		}

		if result := cfg.IsBufferPoolingEnabled(); result != enablePooling {
			t.Fatalf("expected IsBufferPoolingEnabled() to return %v, got %v",
				enablePooling, result)
		}

		if result := cfg.GetReadBufferSize(); result != bufferSize {
			t.Fatalf("expected GetReadBufferSize() to return %d, got %d",
				bufferSize, result)
		}
	})
}
