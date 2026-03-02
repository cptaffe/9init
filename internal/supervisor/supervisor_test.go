package supervisor

import "testing"

// TestRetryable is a regression test for the bug where "restart <name>" was
// a no-op when the service was in StateStopped.
//
// onRetry previously gated exclusively on StateCrashed, so issuing a restart
// command to a service that had exited cleanly (or been explicitly stopped)
// left it in StateStopped forever.
func TestRetryable(t *testing.T) {
	tests := []struct {
		state State
		want  bool
	}{
		// Should start.
		{StateStopped, true},  // regression: was false before the fix
		{StateCrashed, true},

		// Should not start without explicit intervention.
		{StateFailed, false},   // needs "start" to clear crash budget
		{StateReady, false},    // already running
		{StateStarting, false}, // already starting
		{StateWatching, false}, // externally managed; 9init never starts it
	}
	for _, tt := range tests {
		if got := retryable(tt.state); got != tt.want {
			t.Errorf("retryable(%v) = %v, want %v", tt.state, got, tt.want)
		}
	}
}
