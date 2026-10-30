package config

import "testing"

// TestLoadTunnelTuningKnobs locks the deployment knobs added for the relay
// paths: the tunnel copy chunk, the per-connection HTTP/2 stream cap, and the
// server-level idle/send timeouts.
func TestLoadTunnelTuningKnobs(t *testing.T) {
	t.Setenv("DATABASE_PATH", t.TempDir()+"/gateway.db")

	t.Run("defaults", func(t *testing.T) {
		t.Setenv("TUNNEL_COPY_BUFFER_KB", "")
		t.Setenv("H2C_MAX_CONCURRENT_STREAMS", "")
		t.Setenv("SERVER_IDLE_TIMEOUT_MS", "")
		t.Setenv("SEND_TIMEOUT_MS", "")
		cfg := Load()
		if cfg.TunnelCopyBufferKB != 64 {
			t.Fatalf("TunnelCopyBufferKB = %d, want 64", cfg.TunnelCopyBufferKB)
		}
		if cfg.H2CMaxConcurrentStreams != 2048 {
			t.Fatalf("H2CMaxConcurrentStreams = %d, want 2048", cfg.H2CMaxConcurrentStreams)
		}
		if cfg.ServerIdleTimeoutMs != 75_000 {
			t.Fatalf("ServerIdleTimeoutMs = %d, want 75000", cfg.ServerIdleTimeoutMs)
		}
		if cfg.SendTimeoutMs != 60_000 {
			t.Fatalf("SendTimeoutMs = %d, want 60000", cfg.SendTimeoutMs)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("TUNNEL_COPY_BUFFER_KB", "128")
		t.Setenv("H2C_MAX_CONCURRENT_STREAMS", "4096")
		t.Setenv("SERVER_IDLE_TIMEOUT_MS", "30000")
		t.Setenv("SEND_TIMEOUT_MS", "5000")
		cfg := Load()
		if cfg.TunnelCopyBufferKB != 128 {
			t.Fatalf("TunnelCopyBufferKB = %d, want 128", cfg.TunnelCopyBufferKB)
		}
		if cfg.H2CMaxConcurrentStreams != 4096 {
			t.Fatalf("H2CMaxConcurrentStreams = %d, want 4096", cfg.H2CMaxConcurrentStreams)
		}
		if cfg.ServerIdleTimeoutMs != 30_000 || cfg.SendTimeoutMs != 5_000 {
			t.Fatalf("timeouts = %d/%d, want 30000/5000", cfg.ServerIdleTimeoutMs, cfg.SendTimeoutMs)
		}
	})

	t.Run("invalid values fall back", func(t *testing.T) {
		t.Setenv("TUNNEL_COPY_BUFFER_KB", "0")
		t.Setenv("H2C_MAX_CONCURRENT_STREAMS", "many")
		t.Setenv("SERVER_IDLE_TIMEOUT_MS", "-1")
		t.Setenv("SEND_TIMEOUT_MS", "never")
		cfg := Load()
		if cfg.TunnelCopyBufferKB != 64 || cfg.H2CMaxConcurrentStreams != 2048 {
			t.Fatalf("got %d/%d, want defaults 64/2048", cfg.TunnelCopyBufferKB, cfg.H2CMaxConcurrentStreams)
		}
		if cfg.ServerIdleTimeoutMs != 75_000 || cfg.SendTimeoutMs != 60_000 {
			t.Fatalf("timeouts = %d/%d, want defaults 75000/60000", cfg.ServerIdleTimeoutMs, cfg.SendTimeoutMs)
		}
	})
}
