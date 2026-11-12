package provider

import (
	"context"
	"strings"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

type AbortReport struct {
	ScopeKnown   bool
	Matched      int
	Stopped      int
	Unconfirmed  int
	Acknowledged int
}

// AbortSession does not use cwd alone or every branch in this process. If the
// host cannot identify the logical session, its own Abort is the safe fallback.
func (e *Engine) AbortSession(ctx sdk.Context) AbortReport {
	scope, known := e.scope(ctx)
	return e.abortScope(scope, known)
}
func (e *Engine) abortScope(scope string, known bool) AbortReport {
	report := AbortReport{ScopeKnown: known}
	if !known {
		return report
	}
	e.mu.Lock()
	sessions := make([]*Session, 0)
	for key, s := range e.sessions {
		if strings.HasPrefix(key, scope) {
			sessions = append(sessions, s)
		}
	}
	e.mu.Unlock()
	type abortWork struct {
		session *Session
		run     *cursor.Run
	}
	targets := make([]abortWork, 0, len(sessions))
	for _, s := range sessions {
		report.Matched++
		if run := s.abortTarget(); run != nil {
			targets = append(targets, abortWork{s, run})
		}
	}
	// One shared bound, even if several model branches in this logical session
	// have work. All local waiters were released before any network request.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan int, len(targets))
	for _, target := range targets {
		status := target.run.Status()
		if status.TerminalStatus != "" || status.CancelConfirmed {
			target.session.recordRunStatus(status)
			continue
		}
		report.Stopped++
		go func(target abortWork) {
			err := target.run.CancelRunContext(ctx, "aborted by the user")
			target.session.recordRunStatus(target.run.Status())
			if err != nil {
				results <- 2
			} else if target.run.Status().CancelConfirmed {
				results <- 1
			} else {
				results <- 0
			}
		}(target)
	}
	for i := 0; i < report.Stopped; i++ {
		switch <-results {
		case 2:
			report.Unconfirmed++
		case 1:
			report.Acknowledged++
		}
	}
	return report
}
