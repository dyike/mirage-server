package rfb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"
)

// HandshakeError preserves the transport error for errors.Is/As while naming
// the failed protocol stage. No authentication data is added to the error.
type HandshakeError struct {
	Stage                 string
	Elapsed, StageElapsed time.Duration
	Err                   error
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("rfb: %s after %s (stage %s): %v", e.Stage, e.Elapsed.Round(time.Millisecond), e.StageElapsed.Round(time.Millisecond), e.Err)
}
func (e *HandshakeError) Unwrap() error { return e.Err }

func connectionErrorClass(err error) string {
	var auth *AuthError
	var network net.Error
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &auth):
		return "authentication_rejected"
	case errors.As(err, &network):
		if network.Timeout() {
			return "timeout"
		}
		return "network"
	default:
		return "protocol_or_internal"
	}
}

// Only a fixed stage, timing and error classification reach this log.
// Never format err itself: a server's authentication reason can contain names.
func (c *Client) connectionLog(stage, status string, elapsed, stageElapsed time.Duration, err error) {
	if c.cfg.Diagnostics || c.cfg.ConnectionDiagnostics {
		log.Printf("MIRAGE_CONNECTION stage=%s status=%s elapsed_ms=%d stage_ms=%d error=%s", stage, status, elapsed.Milliseconds(), stageElapsed.Milliseconds(), connectionErrorClass(err))
	}
}

func (c *Client) recoveryLog(attempt int, age time.Duration, err error) {
	if c.cfg.Diagnostics || c.cfg.ConnectionDiagnostics {
		log.Printf("MIRAGE_CONNECTION stage=picture-recovery attempt=%d frame_idle_ms=%d error=%s", attempt, age.Milliseconds(), connectionErrorClass(err))
	}
}

type handshakeProgress struct {
	c                     *Client
	stage                 string
	started, stageStarted time.Time
}

func newHandshakeProgress(c *Client, stage string) *handshakeProgress {
	now := time.Now()
	p := &handshakeProgress{c: c, stage: stage, started: now, stageStarted: now}
	c.connectionLog(stage, "begin", 0, 0, nil)
	return p
}

func (p *handshakeProgress) next(stage string) {
	now := time.Now()
	p.c.connectionLog(p.stage, "ok", now.Sub(p.started), now.Sub(p.stageStarted), nil)
	p.stage, p.stageStarted = stage, now
	p.c.connectionLog(stage, "begin", now.Sub(p.started), 0, nil)
}

func (p *handshakeProgress) finish(err error) error {
	now := time.Now()
	elapsed, stageElapsed := now.Sub(p.started), now.Sub(p.stageStarted)
	status := "ok"
	if err != nil {
		status = "failed"
	}
	p.c.connectionLog(p.stage, status, elapsed, stageElapsed, err)
	// Keep authentication/protocol error types unchanged for callers that
	// distinguish rejected credentials from transport failures.
	var network net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &network) {
		return &HandshakeError{Stage: p.stage, Elapsed: elapsed, StageElapsed: stageElapsed, Err: err}
	}
	return err
}
