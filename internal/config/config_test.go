package config

import "testing"

// TestLoadTunnelTuningKnobs locks the two deployment knobs added for the relay
// paths: the tunnel copy chunk and the per-connection HTTP/2 stream cap.
func TestLoadTunnelTuningKnobs(t *testing.T) {
	t.Setenv("DATABASE_PATH", t.TempDir()+"/gateway.db")

	t.Run("defaults", func(t *testing.T) {
		t.Setenv("TUNNEL_COPY_BUFFER_KB", "")
		t.Setenv("H2C_MAX_CONCURRENT_STREAMS", "")
		cfg := Load()
		if cfg.TunnelCopyBufferKB != 64 {
			t.Fatalf("TunnelCopyBufferKB = %d, want 64", cfg.TunnelCopyBufferKB)
		}
		if cfg.H2CMaxConcurrentStreams != 2048 {
			t.Fatalf("H2CMaxConcurrentStreams = %d, want 2048", cfg.H2CMaxConcurrentStreams)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("TUNNEL_COPY_BUFFER_KB", "128")
		t.Setenv("H2C_MAX_CONCURRENT_STREAMS", "4096")
		cfg := Load()
		if cfg.TunnelCopyBufferKB != 128 {
			t.Fatalf("TunnelCopyBufferKB = %d, want 128", cfg.TunnelCopyBufferKB)
		}
		if cfg.H2CMaxConcurrentStreams != 4096 {
			t.Fatalf("H2CMaxConcurrentStreams = %d, want 4096", cfg.H2CMaxConcurrentStreams)
		}
	})

	t.Run("invalid values fall back", func(t *testing.T) {
		t.Setenv("TUNNEL_COPY_BUFFER_KB", "0")
		t.Setenv("H2C_MAX_CONCURRENT_STREAMS", "many")
		cfg := Load()
		if cfg.TunnelCopyBufferKB != 64 || cfg.H2CMaxConcurrentStreams != 2048 {
			t.Fatalf("got %d/%d, want defaults 64/2048", cfg.TunnelCopyBufferKB, cfg.H2CMaxConcurrentStreams)
		}
	})
}
