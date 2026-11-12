package bridge

// A bridge exists to serve exactly one host process. Nothing tells it otherwise:
// when that process is killed — SIGKILL, a crash, a terminal closed — no shutdown
// handler runs, and the bridge survives. It keeps its port, its memory, and
// upstream a Cursor run that stays open on the agent, so the next session that
// resumes that agent is told "already has active run".
//
// So spawning a bridge starts by reaping the ones nobody owns any more. Ownership
// is proven, not guessed: the record stores the owner's pid and the start time
// `ps` reported for it, because pids get recycled and a bare "is this pid alive"
// would happily protect a stranger's process — or, worse, let us signal one.

import (
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// bridgeRecord is one bridge this extension started.
type bridgeRecord struct {
	BridgePID  int    `json:"bridgePid"`
	BridgeBin  string `json:"bridgeBin"`
	OwnerPID   int    `json:"ownerPid"`
	OwnerStart string `json:"ownerStart"`
	Workspace  string `json:"workspace"`
	StartedAt  int64  `json:"startedAt"`
}

// processInfo is what the OS says about a pid. A process is only the one we
// recorded when both the start time and the command still match.
type processInfo struct {
	Exists  bool
	Start   string
	Command string
}

// lookProcess is replaced in tests. It reads `ps`, which is how ownership is
// proven on the Unix hosts this runs on; where `ps` is missing every record reads
// as gone, which drops records and never signals a process.
var lookProcess = func(pid int) processInfo {
	if pid <= 0 {
		return processInfo{}
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return processInfo{}
	}
	info := processInfo{Exists: true, Start: strings.TrimSpace(string(out))}
	if cmd, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output(); err == nil {
		info.Command = strings.TrimSpace(string(cmd))
	}
	return info
}

// ProcessStart is the start time `ps` reports for a pid, "" when it does not
// exist. Pair it with OwnerAlive to decide whether a pid is still the process you
// recorded.
func ProcessStart(pid int) string { return lookProcess(pid).Start }

// OwnerAlive reports whether pid is still the same process it was when its start
// time was recorded. Anything recorded about a process is only trustworthy while
// that holds, because pids are recycled: a bare liveness check would credit a
// stranger's process with owning someone else's agent or bridge.
func OwnerAlive(pid int, recordedStart string) bool {
	if pid <= 0 || recordedStart == "" {
		return false // recorded before ownership was tracked, so it proves nothing
	}
	info := lookProcess(pid)
	return info.Exists && info.Start == recordedStart
}

// reapPlan splits recorded bridges into those that must go and those that must
// not be touched. It is pure so the ownership rules can be tested directly: an
// orphan is only an orphan when its owner is provably gone or provably different.
func reapPlan(records []bridgeRecord, info func(int) processInfo, now int64) (kill, keep []bridgeRecord) {
	for _, rec := range records {
		if rec.BridgePID <= 0 {
			continue // nothing to signal, and nothing worth keeping a line for
		}
		bridge := info(rec.BridgePID)
		if !bridge.Exists || !strings.Contains(bridge.Command, filepath.Base(rec.BridgeBin)) {
			continue // already gone, or its pid now belongs to an unrelated process
		}
		owner := info(rec.OwnerPID)
		if owner.Exists && owner.Start == rec.OwnerStart {
			keep = append(keep, rec)
			continue
		}
		// Either the owner is dead, or the pid came back as a different process —
		// both mean no host is driving this bridge.
		kill = append(kill, rec)
	}
	return kill, keep
}

func recordsPath(dir string) string { return filepath.Join(dir, "bridges.json") }

func readRecords(dir string) []bridgeRecord {
	raw, err := os.ReadFile(recordsPath(dir))
	if err != nil {
		return nil
	}
	var doc struct {
		Version int            `json:"version"`
		Bridges []bridgeRecord `json:"bridges"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return doc.Bridges
}

// writeRecords replaces the file. Losing a record only means one bridge is not
// reaped until its owner exits normally, so a write failure is never fatal, and
// concurrent hosts need no lock: they each add their own line.
func writeRecords(dir string, records []bridgeRecord) {
	if len(records) > 64 {
		records = records[len(records)-64:]
	}
	raw, err := json.MarshalIndent(struct {
		Version int            `json:"version"`
		Bridges []bridgeRecord `json:"bridges"`
	}{1, records}, "", "  ")
	if err != nil {
		return
	}
	tmp := recordsPath(dir) + ".tmp"
	if os.WriteFile(tmp, append(raw, '\n'), 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, recordsPath(dir))
}

// ReapStale kills bridges whose owning process is gone and forgets their records.
func ReapStale(dir string, log *slog.Logger) int {
	records := readRecords(dir)
	if len(records) == 0 {
		return 0
	}
	kill, keep := reapPlan(records, lookProcess, time.Now().Unix())
	if len(kill) == 0 {
		if len(keep) != len(records) {
			writeRecords(dir, keep) // some bridges have exited on their own
		}
		return 0
	}
	for _, rec := range kill {
		if log != nil {
			log.Info("reaping a cursor bridge whose session is gone",
				"bridge", rec.BridgePID, "session", rec.OwnerPID, "workspace", rec.Workspace)
		}
		stopPID(rec.BridgePID)
	}
	writeRecords(dir, keep)
	return len(kill)
}

// stopPID ends a bridge process. It gets SIGTERM first because a bridge that can
// still hear it will close its runs properly; an orphan that ignores it gets
// killed shortly after, off the spawn path, so no turn waits on a dead process.
func stopPID(pid int) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	terminatePID(proc)
	go func() {
		for i := 0; i < 20; i++ {
			time.Sleep(250 * time.Millisecond)
			if !lookProcess(pid).Exists {
				return
			}
		}
		_ = proc.Kill()
	}()
}

// register records a live bridge so a later process can reap it if this one dies
// without shutting down.
func register(dir string, rec bridgeRecord, log *slog.Logger) {
	if rec.BridgePID <= 0 || dir == "" {
		return
	}
	if rec.OwnerPID <= 0 {
		rec.OwnerPID = os.Getpid()
		rec.OwnerStart = lookProcess(rec.OwnerPID).Start
	}
	records := readRecords(dir)
	records = append(records, rec)
	// Drop anything whose bridge has already exited, including this process's own
	// bridges from earlier engines in the same session.
	_, keep := reapPlan(records, lookProcess, time.Now().Unix())
	writeRecords(dir, keep)
	if log != nil && len(keep) > len(records)-1 {
		log.Debug("bridge record count grew", "records", len(keep))
	}
}

// forgetBridge drops one bridge's record, on the shutdown path where it was
// closed properly. It drops only that bridge: another bridge this process still
// runs must stay reapable if this process is then killed.
func forgetBridge(dir string, bridgePID int) {
	records := readRecords(dir)
	if len(records) == 0 {
		return
	}
	out := make([]bridgeRecord, 0, len(records))
	for _, rec := range records {
		if rec.BridgePID == bridgePID {
			continue
		}
		out = append(out, rec)
	}
	writeRecords(dir, out)
}
