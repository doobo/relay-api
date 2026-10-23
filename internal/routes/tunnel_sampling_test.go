package routes

import "testing"

// TestTunnelUsageSampling covers the /free usage sampling rule: successes are
// recorded one in N, errors are always recorded.
func TestTunnelUsageSampling(t *testing.T) {
	handler := NewTunnelRoutes(nil)
	handler.SetUsageSample(3)

	recorded := 0
	for i := 0; i < 6; i++ {
		if handler.shouldRecord(200, nil) {
			recorded++
		}
	}
	if recorded != 2 {
		t.Fatalf("sampled %d of 6 successes, want 2", recorded)
	}

	if !handler.shouldRecord(502, nil) {
		t.Fatal("5xx response was sampled out; errors must always be recorded")
	}
	errMsg := "boom"
	if !handler.shouldRecord(200, &errMsg) {
		t.Fatal("response with an error message was sampled out")
	}

	handler.SetUsageSample(1)
	for i := 0; i < 3; i++ {
		if !handler.shouldRecord(200, nil) {
			t.Fatal("sample rate 1 must record every request")
		}
	}
}
