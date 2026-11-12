package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

type DiagnosticEvent struct {
	At   time.Time
	Kind string
}
type DoctorRun struct {
	Controls *ControlReport
	Status   cursor.RunStatus
}
type DoctorReport struct {
	Selected       *ControlReport
	ScopeKnown     bool
	BridgeStarted  bool
	BridgeStarting bool
	StartFailed    bool
	BridgePID      int
	Protocol       string
	Tools          []string
	Runs           []DoctorRun
	Recent         []DiagnosticEvent
	Probe          string
}

func (e *Engine) recordFailure(err error) {
	if err == nil {
		return
	}
	kind := "provider request failed; see local log"
	switch {
	case errors.Is(err, context.Canceled):
		kind = "request cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		kind = "request timed out"
	case cursor.IsInterruptedStream(err):
		kind = "response stream interrupted"
	case strings.Contains(strings.ToLower(err.Error()), "api key"), strings.Contains(strings.ToLower(err.Error()), "unauthenticated"):
		kind = "authentication unavailable or rejected"
	case strings.Contains(strings.ToLower(err.Error()), "bridge"):
		kind = "bridge unavailable or rejected request"
	}
	e.diagnosticsMu.Lock()
	defer e.diagnosticsMu.Unlock()
	e.recent = append(e.recent, DiagnosticEvent{At: time.Now(), Kind: kind})
	if len(e.recent) > 6 {
		e.recent = append([]DiagnosticEvent(nil), e.recent[len(e.recent)-6:]...)
	}
}

func (s *Session) recordRunStatus(status cursor.RunStatus) {
	key := fmt.Sprintf("%s/%d/%t/%t/%t", status.ID, status.Recoveries, status.CancelFailed, status.CancelConfirmed, status.StreamFailed)
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.lastDiagnostic == key {
		return
	}
	s.lastDiagnostic = key
	var kinds []string
	if status.CancelFailed {
		kinds = append(kinds, "upstream cancellation unconfirmed")
	}
	if status.CancelConfirmed {
		kinds = append(kinds, "upstream cancellation acknowledged")
	}
	if status.StreamFailed {
		kinds = append(kinds, "run stream failed")
	}
	if status.Recoveries > 0 {
		kinds = append(kinds, fmt.Sprintf("stream reattachments: %d", status.Recoveries))
	}
	for _, kind := range kinds {
		s.recent = append(s.recent, DiagnosticEvent{At: time.Now(), Kind: kind})
	}
	if len(s.recent) > 6 {
		s.recent = append([]DiagnosticEvent(nil), s.recent[len(s.recent)-6:]...)
	}
}

// doctorSnapshot is passive. It never creates a client, starts a bridge or reads
// credentials, prompt content, tool arguments, tool output or private paths.
func (e *Engine) doctorSnapshot(scope string, known bool, selected *ControlReport, tools []string) DoctorReport {
	report := DoctorReport{ScopeKnown: known, Selected: selected, Tools: append([]string(nil), tools...), Probe: "not requested (passive snapshot)"}
	sort.Strings(report.Tools)
	if e == nil {
		return report
	}
	e.mu.Lock()
	client := e.client
	report.BridgeStarting = e.starting != nil
	report.StartFailed = e.startErr != ""
	sessions := make([]*Session, 0)
	if known {
		for key, s := range e.sessions {
			if strings.HasPrefix(key, scope) {
				sessions = append(sessions, s)
			}
		}
	}
	e.mu.Unlock()
	if client != nil {
		report.BridgeStarted = true
		report.BridgePID = client.PID()
		report.Protocol = client.CachedProtocol()
	}
	for _, session := range sessions {
		if agent := session.activeAgent(); agent != nil {
			if run := agent.Active(); run != nil {
				report.Runs = append(report.Runs, DoctorRun{Controls: session.controls(), Status: run.Status()})
			}
		}
		session.gate.Lock()
		report.Recent = append(report.Recent, session.recent...)
		session.gate.Unlock()
	}
	e.diagnosticsMu.Lock()
	report.Recent = append(report.Recent, e.recent...)
	e.diagnosticsMu.Unlock()
	sort.Slice(report.Recent, func(i, j int) bool { return report.Recent[i].At.Before(report.Recent[j].At) })
	if len(report.Recent) > 8 {
		report.Recent = report.Recent[len(report.Recent)-8:]
	}
	return report
}

func (e *Engine) Doctor(ctx sdk.Context, entry *ModelEntry, probe bool) DoctorReport {
	var selected *ControlReport
	if entry != nil {
		selection := reportControls(entry, withContextThinking(ModelOverride{}, thinkingFromContext(ctx)))
		selected = &selection
	}
	tools := hostTools(ctx)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	scope, known := "", false
	if e != nil {
		scope, known = e.scope(ctx)
		if e.cfg.DisableHostTools {
			names = nil
		}
	}
	report := e.doctorSnapshot(scope, known, selected, names)
	if probe {
		report.Probe = e.probeExisting()
	}
	return report
}
func (e *Engine) probeExisting() string {
	if e == nil {
		return "skipped: no bridge has been started"
	}
	client := e.clientNow()
	if client == nil || client.Control == nil {
		return "skipped: no bridge has been started"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		return "failed: existing bridge did not acknowledge ping"
	}
	return "ok: existing bridge acknowledged ping"
}

func doctorParameters(report *ControlReport) string {
	safe := []string{}
	for _, param := range report.Params {
		switch param.ID {
		case "thinking", "effort", "reasoning", "reasoning_effort", "context", "fast":
			safe = append(safe, param.ID+"="+clip(oneLine(param.Value), 40))
		}
	}
	sort.Strings(safe)
	return strings.Join(safe, ", ")
}

func FormatDoctor(report DoctorReport) string {
	lines := []string{"Cursor doctor (no credentials or prompt data inspected)"}
	if report.Selected != nil {
		lines = append(lines, fmt.Sprintf("selected: %s · context %d", oneLine(report.Selected.ModelID), report.Selected.ContextWindow))
		lines = append(lines, "selected parameters: "+doctorParameters(report.Selected))
		for _, warning := range report.Selected.Warnings {
			lines = append(lines, "selection warning: "+warning)
		}
	} else {
		lines = append(lines, "selected: no Cursor model selected")
	}
	if report.BridgeStarted {
		lines = append(lines, fmt.Sprintf("bridge: client present · pid %d · cached protocol %s (unprobed unless requested)", report.BridgePID, oneLine(report.Protocol)))
	} else if report.BridgeStarting {
		lines = append(lines, "bridge: initialization in progress (not started by doctor)")
	} else if report.StartFailed {
		lines = append(lines, "bridge: previous startup failed; resolve installation/configuration and /reload")
	} else {
		lines = append(lines, "bridge: not started; this command did not start it")
	}
	lines = append(lines, "probe: "+report.Probe, fmt.Sprintf("pig tools: %d active", len(report.Tools)))
	if len(report.Tools) > 0 {
		names := report.Tools
		if len(names) > 12 {
			names = names[:12]
		}
		safe := make([]string, 0, len(names))
		for _, name := range names {
			safe = append(safe, clip(oneLine(name), 60))
		}
		lines = append(lines, "available: "+strings.Join(safe, ", "))
	} else {
		lines = append(lines, "action: enable suitable pig tools if you want native execution; Cursor built-ins may be used otherwise")
	}
	if !report.ScopeKnown {
		lines = append(lines, "runs: logical session identity unavailable; no other session inspected")
	}
	if report.ScopeKnown && len(report.Runs) == 0 {
		lines = append(lines, "runs: no tracked Cursor run in this session")
	}
	for _, run := range report.Runs {
		state := "running"
		switch {
		case run.Status.TerminalStatus != "":
			state = strings.ToLower(run.Status.TerminalStatus)
		case run.Status.CancelFailed:
			state = "locally stopped; upstream cancellation unconfirmed"
		case run.Status.CancelConfirmed:
			state = "cancellation acknowledged upstream"
		case run.Status.CancelRequested:
			state = "cancellation pending upstream"
		case run.Status.Paused:
			state = "waiting for pig tool results"
		case run.Status.Finished:
			state = "local stream stopped"
		}
		lines = append(lines, fmt.Sprintf("run: %s · %d pending · %d reattachments", state, run.Status.Pending, run.Status.Recoveries))
		if run.Controls != nil {
			lines = append(lines, "last sent controls: "+oneLine(run.Controls.ModelID)+" · "+doctorParameters(run.Controls))
			for _, warning := range run.Controls.Warnings {
				lines = append(lines, "warning: "+warning)
			}
		}
	}
	for _, event := range report.Recent {
		lines = append(lines, "recent "+event.At.Format("15:04:05")+": "+event.Kind)
	}
	lines = append(lines, "actions: /cursor-abort retries unconfirmed cancellation; /cursor-refresh-models refreshes controls; /cursor-doctor --probe checks only an existing bridge")
	return strings.Join(lines, "\n")
}
