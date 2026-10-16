package service

import (
	"strings"
	"sync"
)

// SessionBusyGate is the process-local send mutex: it marks a session as
// generating while a turn holds it, so a cancel and a concurrent send see the
// same state. Session rewind, whose exclusive lock used to share this gate,
// went with the execution sandbox.
type SessionBusyGate struct {
	mu     sync.Mutex
	states map[string]*sessionBusyState
}

type sessionBusyState struct {
	sendRefs int
}

// NewSessionBusyGate returns an empty gate. Container provides one shared
// instance so every send on this process sees the others.
func NewSessionBusyGate() *SessionBusyGate {
	return &SessionBusyGate{states: make(map[string]*sessionBusyState)}
}

func (g *SessionBusyGate) stateLocked(sessionID string) *sessionBusyState {
	st := g.states[sessionID]
	if st == nil {
		st = &sessionBusyState{}
		g.states[sessionID] = st
	}
	return st
}

func (g *SessionBusyGate) dropLocked(sessionID string, st *sessionBusyState) {
	if st.sendRefs <= 0 {
		delete(g.states, sessionID)
	}
}

// HoldSend marks sessionID as generating.
func (g *SessionBusyGate) HoldSend(sessionID string) (func(), error) {
	noop := func() {}
	if g == nil {
		return noop, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return noop, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.stateLocked(sessionID)
	st.sendRefs++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			st := g.states[sessionID]
			if st == nil {
				return
			}
			st.sendRefs--
			if st.sendRefs < 0 {
				st.sendRefs = 0
			}
			g.dropLocked(sessionID, st)
		})
	}, nil
}
