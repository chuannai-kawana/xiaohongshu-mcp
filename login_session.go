package main

import (
	"sync"
	"time"
)

// loginSessions 同时只保留一个扫码会话，二维码仅暂存在内存中。
type loginSessions struct {
	mu        sync.Mutex
	seq       uint64
	cancel    func()
	active    bool
	stage     string
	expiresAt time.Time
	qrImage   string
}

func (l *loginSessions) start(cancel func(), timeout time.Duration, qrImage string) uint64 {
	l.mu.Lock()
	prev := l.cancel
	l.seq++
	seq := l.seq
	l.cancel = cancel
	l.active = true
	l.stage = "waiting_for_scan"
	l.expiresAt = time.Now().Add(timeout)
	l.qrImage = qrImage
	l.mu.Unlock()
	if prev != nil {
		prev()
	}
	return seq
}

// currentQR 复用未过期二维码，避免重复取码中断手机确认流程。
func (l *loginSessions) currentQR() (string, time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	remaining := time.Until(l.expiresAt)
	if !l.active || l.qrImage == "" || remaining <= 0 {
		return "", 0, false
	}
	return l.qrImage, remaining, true
}

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
		l.qrImage = ""
	}
}
