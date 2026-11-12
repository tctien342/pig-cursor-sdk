package provider

import (
	"context"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func (s *Session) activeAgent() *cursor.Agent { return s.agentSlot.Load() }
func (s *Session) setAgent(a *cursor.Agent)   { s.agentSlot.Store(a) }

// A queued request captures its abort generation before waiting for the previous
// turn. An abort invalidates already-queued work, but a later explicit user turn
// can install a new cancellation context and proceed.
func (s *Session) generation() uint64 {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.abortGeneration
}
func (s *Session) beginTurn(parent context.Context, generation uint64) (context.Context, func(), bool) {
	s.gate.Lock()
	defer s.gate.Unlock()
	if generation != s.abortGeneration || parent.Err() != nil {
		return nil, func() {}, false
	}
	ctx, cancel := context.WithCancel(parent)
	s.turnContext = ctx
	s.turnCancel = cancel
	s.turnGeneration = generation
	return ctx, func() {
		cancel()
		s.gate.Lock()
		s.turnCancel = nil
		s.turnContext = nil
		s.gate.Unlock()
	}, true
}
func (s *Session) context(fallback sdk.Context) context.Context {
	s.gate.Lock()
	ctx := s.turnContext
	s.gate.Unlock()
	if ctx != nil {
		return ctx
	}
	return goCtx(fallback)
}
func (s *Session) abortTarget() *cursor.Run {
	s.gate.Lock()
	s.abortGeneration++
	cancel := s.turnCancel
	var run *cursor.Run
	if agent := s.activeAgent(); agent != nil {
		run = agent.Active()
	}
	s.gate.Unlock()
	if cancel != nil {
		cancel()
	}
	return run
}

func (s *Session) publishAgent(agent *cursor.Agent) bool {
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.turnContext == nil || s.turnContext.Err() != nil || s.turnGeneration != s.abortGeneration {
		return false
	}
	s.setAgent(agent)
	return true
}

// scope is exact logical identity, not cwd-only matching. Empty session files
// additionally use the host session ID plus this process's nonce.
func scopeKey(file, cwd, id, nonce string) (string, bool) {
	known := strings.TrimSpace(file) != "" || strings.TrimSpace(id) != ""
	if strings.TrimSpace(file) == "" && strings.TrimSpace(id) != "" {
		file = "ephemeral:" + nonce + ":" + id
	}
	return conversationKey(file, cwd, "", nonce), known
}
func (e *Engine) scope(ctx sdk.Context) (string, bool) {
	cwd := ctx.Cwd()
	if cwd == "" {
		cwd = e.cfg.Workspace
	}
	file := ""
	if f, err := ctx.GetSessionFile(); err == nil && f != nil {
		file = *f
	}
	id := ""
	if strings.TrimSpace(file) == "" {
		id, _ = ctx.GetSessionID()
	}
	return scopeKey(file, cwd, id, e.nonce)
}
