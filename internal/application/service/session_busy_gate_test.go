package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The gate is the process-local send mutex: a hold is reentrant, and releasing
// every hold drops the session state. Its former rewind half (TryLockRewind /
// RewindHeld) went with the execution sandbox.
func TestSessionBusyGateNestedSendHolds(t *testing.T) {
	gate := NewSessionBusyGate()
	first, err := gate.HoldSend("sess-1")
	require.NoError(t, err)
	second, err := gate.HoldSend("sess-1")
	require.NoError(t, err)

	first()
	second()

	// A fresh hold after every release still works.
	third, err := gate.HoldSend("sess-1")
	require.NoError(t, err)
	third()
}

func TestSessionBusyGateEmptySessionIsNoop(t *testing.T) {
	gate := NewSessionBusyGate()
	release, err := gate.HoldSend("  ")
	require.NoError(t, err)
	release()
}
