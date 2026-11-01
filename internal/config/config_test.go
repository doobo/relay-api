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
		t.Setenv("H2C_READ_FRAME_KB", "")
		t.Setenv("H2C_STREAM_BUFFER_KB", "")
		t.Setenv("H2C_CONNECTION_BUFFER_KB", "")
		t.Setenv("H2C_DISABLE_CLIENT_PRIORITY", "")
		t.Setenv("LISTEN_REUSEPORT", "")
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
		if cfg.H2CReadFrameKB != 64 || cfg.H2CStreamBufferKB != 1024 || cfg.H2CConnectionBufferKB != 2048 {
			t.Fatalf("h2 buffers = %d/%d/%d, want 64/1024/2048",
				cfg.H2CReadFrameKB, cfg.H2CStreamBufferKB, cfg.H2CConnectionBufferKB)
		}
		if cfg.H2CDisableClientPriority || cfg.ListenReusePort != 1 {
			t.Fatalf("priority/reuseport = %v/%d, want false/1", cfg.H2CDisableClientPriority, cfg.ListenReusePort)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv("TUNNEL_COPY_BUFFER_KB", "128")
		t.Setenv("H2C_MAX_CONCURRENT_STREAMS", "4096")
		t.Setenv("SERVER_IDLE_TIMEOUT_MS", "30000")
		t.Setenv("SEND_TIMEOUT_MS", "5000")
		t.Setenv("H2C_READ_FRAME_KB", "128")
		t.Setenv("H2C_STREAM_BUFFER_KB", "512")
		t.Setenv("H2C_CONNECTION_BUFFER_KB", "3072")
		t.Setenv("H2C_DISABLE_CLIENT_PRIORITY", "true")
		t.Setenv("LISTEN_REUSEPORT", "4")
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
		if cfg.H2CReadFrameKB != 128 || cfg.H2CStreamBufferKB != 512 || cfg.H2CConnectionBufferKB != 3072 {
			t.Fatalf("h2 buffers = %d/%d/%d, want 128/512/3072",
				cfg.H2CReadFrameKB, cfg.H2CStreamBufferKB, cfg.H2CConnectionBufferKB)
		}
		if !cfg.H2CDisableClientPriority || cfg.ListenReusePort != 4 {
			t.Fatalf("priority/reuseport = %v/%d, want true/4", cfg.H2CDisableClientPriority, cfg.ListenReusePort)
		}
	})

	t.Run("invalid values fall back", func(t *testing.T) {
		t.Setenv("TUNNEL_COPY_BUFFER_KB", "0")
		t.Setenv("H2C_MAX_CONCURRENT_STREAMS", "many")
		t.Setenv("SERVER_IDLE_TIMEOUT_MS", "-1")
		t.Setenv("SEND_TIMEOUT_MS", "never")
		t.Setenv("H2C_READ_FRAME_KB", "1")
		t.Setenv("H2C_STREAM_BUFFER_KB", "999999")
		t.Setenv("H2C_CONNECTION_BUFFER_KB", "0")
		t.Setenv("H2C_DISABLE_CLIENT_PRIORITY", "maybe")
		t.Setenv("LISTEN_REUSEPORT", "1000")
		cfg := Load()
		if cfg.TunnelCopyBufferKB != 64 || cfg.H2CMaxConcurrentStreams != 2048 {
			t.Fatalf("got %d/%d, want defaults 64/2048", cfg.TunnelCopyBufferKB, cfg.H2CMaxConcurrentStreams)
		}
		if cfg.ServerIdleTimeoutMs != 75_000 || cfg.SendTimeoutMs != 60_000 {
			t.Fatalf("timeouts = %d/%d, want defaults 75000/60000", cfg.ServerIdleTimeoutMs, cfg.SendTimeoutMs)
		}
		if cfg.H2CReadFrameKB != 16 || cfg.H2CStreamBufferKB != 4095 || cfg.H2CConnectionBufferKB != 2048 {
			t.Fatalf("clamped buffers = %d/%d/%d, want 16/4095/2048",
				cfg.H2CReadFrameKB, cfg.H2CStreamBufferKB, cfg.H2CConnectionBufferKB)
		}
		if cfg.H2CDisableClientPriority || cfg.ListenReusePort != 64 {
			t.Fatalf("priority/reuseport = %v/%d, want false/64", cfg.H2CDisableClientPriority, cfg.ListenReusePort)
		}
	})
}
