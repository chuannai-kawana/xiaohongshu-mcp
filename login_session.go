package main

import (
	"sync"
	"time"
)

// loginSessions tracks a single QR login attempt without exposing QR values,
// verification codes, phone numbers, cookies or account identifiers.
type loginSessions struct {
	mu sync.Mutex
	seq uint64
	cancel func()
	active bool
	stage string
	expiresAt time.Time
}

func (l *loginSessions) start(cancel func(), timeout time.Duration) uint64 {
	l.mu.Lock()
	prev := l.cancel
	l.seq++
	seq := l.seq
	l.cancel = cancel
	l.active = true
	l.stage = "waiting_for_scan"
	l.expiresAt = time.Now().Add(timeout)
	l.mu.Unlock()
	if prev != nil { prev() }
	return seq
}

// setStage ignores updates from a previously cancelled login session.
func (l *loginSessions) setStage(seq uint64, stage string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seq == seq && l.active {
		l.stage = stage
	}
}

func (l *loginSessions) snapshot() (active bool, stage string, expiresAt time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.stage, l.expiresAt
}

func (l *loginSessions) finish(seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seq == seq {
		l.cancel = nil
		l.active = false
	}
}
