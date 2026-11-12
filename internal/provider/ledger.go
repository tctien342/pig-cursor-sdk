package provider

// The local usage ledger.
//
// Cursor gives an API key per-run token counts and nothing else: GetUsage is
// cloud-only (a local agent answers "Unknown agent"), ListAgents returns nothing
// for a key whose agents are all local, and plan-limit windows arrive only inside
// a refusal error. So "how much have I used this week" cannot be asked of Cursor —
// it has to be added up here from the run frames that do arrive, and labelled as
// ours rather than as an account report.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ledgerRecord is one turn's token usage.
type ledgerRecord struct {
	At      int64   `json:"at"`     // unix seconds
	Model   string  `json:"model"`  // Cursor model id
	Tokens  int64   `json:"tokens"` // total tokens Cursor charged for the turn
	Input   int64   `json:"in"`
	Output  int64   `json:"out"`
	Cents   float64 `json:"cents,omitempty"` // only when Cursor reported cost
	Charged bool    `json:"charged,omitempty"`
}

// UsageWindow is one bucket of the ledger.
type UsageWindow struct {
	Tokens   int64
	Cents    float64
	HasCents bool
	Turns    int
	From     time.Time
	To       time.Time
}

// UsageLedger is an append-only JSON log of turns, pruned by age. Small on purpose:
// one record per turn, oldest dropped past the cap, so a long-lived install cannot
// grow without bound.
type UsageLedger struct {
	mu       sync.Mutex
	path     string
	maxAge   time.Duration
	maxRecs  int
	records  []ledgerRecord
	loaded   bool
	disabled bool
}

// NewUsageLedger opens (and reads) the ledger at path. A missing file is an empty
// ledger, not an error; an unreadable one disables the ledger so a corrupt file
// never takes the provider down with it.
func NewUsageLedger(path string) *UsageLedger {
	return &UsageLedger{path: path, maxAge: 90 * 24 * time.Hour, maxRecs: 20000}
}

func (l *UsageLedger) load() {
	if l.loaded || l.disabled {
		return
	}
	l.loaded = true
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return // no ledger yet
	}
	var dump struct {
		Version int            `json:"version"`
		Records []ledgerRecord `json:"records"`
	}
	if err := json.Unmarshal(raw, &dump); err != nil {
		l.disabled = true
		return
	}
	l.records = dump.Records
	l.prune(time.Now())
}

// Record adds one turn. Empty usage is ignored so a turn that produced no numbers
// does not look like a free one.
func (l *UsageLedger) Record(rec ledgerRecord) {
	if rec.Tokens <= 0 && rec.Cents <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	if l.disabled {
		return
	}
	if rec.At == 0 {
		rec.At = time.Now().Unix()
	}
	l.records = append(l.records, rec)
	l.prune(time.Now())
	l.flushLocked()
}

// Window returns the sum of records inside [now-span, now].
func (l *UsageLedger) Window(span time.Duration, now time.Time) UsageWindow {
	return l.WindowBetween(now.Add(-span), now)
}

// WindowBetween sums records whose timestamp falls in [from, to].
func (l *UsageLedger) WindowBetween(from, to time.Time) UsageWindow {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	var w UsageWindow
	if l.disabled {
		return w
	}
	lo, hi := from.Unix(), to.Unix()
	w.From, w.To = from, to
	for _, r := range l.records {
		if r.At < lo || r.At > hi {
			continue
		}
		w.Turns++
		w.Tokens += r.Tokens
		if r.Cents > 0 {
			w.Cents += r.Cents
			w.HasCents = true
		}
	}
	return w
}

// Since returns the first recorded turn, for "tracking since" lines.
func (l *UsageLedger) Since() (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	if l.disabled || len(l.records) == 0 {
		return time.Time{}, false
	}
	return time.Unix(l.records[0].At, 0), true
}

// Enabled reports whether the ledger is usable (not disabled by a corrupt file).
func (l *UsageLedger) Enabled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	return !l.disabled
}

func (l *UsageLedger) prune(now time.Time) {
	cutoff := now.Add(-l.maxAge).Unix()
	keep := l.records[:0]
	for _, r := range l.records {
		if r.At >= cutoff {
			keep = append(keep, r)
		}
	}
	l.records = keep
	if len(l.records) > l.maxRecs {
		l.records = l.records[len(l.records)-l.maxRecs:]
	}
}

func (l *UsageLedger) flushLocked() {
	if l.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return
	}
	sort.SliceStable(l.records, func(i, j int) bool { return l.records[i].At < l.records[j].At })
	raw, err := json.Marshal(struct {
		Version int            `json:"version"`
		Records []ledgerRecord `json:"records"`
		Updated int64          `json:"updatedAt"`
	}{1, l.records, time.Now().Unix()})
	if err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, l.path)
}
