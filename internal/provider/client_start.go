package provider

import (
	"context"
	"errors"
	"time"

	bridge "github.com/tctien342/pig-cursor-sdk/internal/bridge"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

type clientInitialization struct {
	done   chan struct{}
	cancel context.CancelFunc
	client *cursor.Client
	err    error
}

// Startup belongs to the engine, not its first waiter. A cancelled turn must
// neither poison later attempts nor cancel another session's initialization.
func (e *Engine) startClient(ctx context.Context, apiKey string) (*cursor.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return nil, context.Canceled
	}
	if e.client != nil {
		client := e.client
		e.mu.Unlock()
		return client, nil
	}
	if e.startErr != "" {
		message := e.startErr
		e.mu.Unlock()
		return nil, errors.New(message)
	}
	start := e.starting
	if start == nil {
		owned, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start = &clientInitialization{done: make(chan struct{}), cancel: cancel}
		e.starting = start
		reap := !e.reaped
		e.reaped = true
		go e.initializeClient(owned, start, apiKey, reap)
	}
	e.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-start.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e.mu.Lock()
		stopped := e.stopped
		e.mu.Unlock()
		if stopped {
			return nil, context.Canceled
		}
		return start.client, start.err
	}
}

func (e *Engine) initializeClient(ctx context.Context, start *clientInitialization, apiKey string, reap bool) {
	defer start.cancel()
	bin := e.cfg.BridgeBin
	if bin == "" {
		bin = cursor.FindBinary()
	}
	if reap {
		if n := bridge.ReapStale(DefaultStateDir(e.cfg.ConfigHome), e.cfg.Logger); n > 0 {
			e.cfg.Logger.Info("reaped bridges left by dead sessions", "count", n)
		}
	}
	factory := e.newClient
	if factory == nil {
		factory = cursor.New
	}
	began := time.Now()
	var client *cursor.Client
	err := ctx.Err()
	if err == nil {
		client, err = factory(ctx, cursor.Options{BridgeBin: bin, Workspace: e.cfg.Workspace, APIKey: apiKey, Logger: e.cfg.Logger})
	}
	if err == nil && client == nil {
		err = errors.New("cursor: bridge factory returned no client")
	}
	e.mu.Lock()
	discard := e.stopped || ctx.Err() != nil || err != nil
	if e.stopped {
		err = context.Canceled
	} else if ctx.Err() != nil {
		err = ctx.Err()
	}
	if !discard {
		e.client = client
		start.client = client
	} else if err != nil && !cursor.IsCancellation(err) && !errors.Is(err, context.DeadlineExceeded) {
		e.startErr = err.Error()
	}
	start.err = err
	e.starting = nil
	e.mu.Unlock()
	// A factory that loses the shutdown race may still return a live handle.
	// Dispose outside e.mu, exactly once. Stop initiates (does not await) this path.
	if discard && client != nil {
		e.disposeClient(client)
	}
	if !discard {
		e.cfg.Logger.Info("cursor bridge started", "ms", time.Since(began).Milliseconds(), "pid", client.PID())
	}
	close(start.done)
}
func (e *Engine) disposeClient(client *cursor.Client) {
	if client == nil {
		return
	}
	if e.closeClient != nil {
		e.closeClient(client)
	} else {
		client.Close()
	}
}
