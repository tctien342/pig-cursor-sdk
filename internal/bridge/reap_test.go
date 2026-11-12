package bridge

import (
	"os"
	"path/filepath"
	"testing"
)

// proc is a fake `ps`: which pids exist, since when, and running what.
type proc map[int]processInfo

func (p proc) look(pid int) processInfo { return p[pid] }

func rec(bridgePID, ownerPID int, ownerStart string) bridgeRecord {
	return bridgeRecord{
		BridgePID:  bridgePID,
		BridgeBin:  "/home/u/.pig/agent/pig-cursor-sdk/bin/cursor-sdk-bridge",
		OwnerPID:   ownerPID,
		OwnerStart: ownerStart,
		Workspace:  "/work/repo",
	}
}

const bridgeCmd = "/home/u/.pig/agent/pig-cursor-sdk/bin/cursor-sdk-bridge --workspace /work/repo"

// The whole point of recording a start time is that a pid alone proves nothing: a
// dead host's pid gets reused, and "is this pid alive" would then protect an
// unrelated process and leave the orphan holding a Cursor run forever.
func TestReapPlanOnlyKillsBridgesWhoseOwnerIsGone(t *testing.T) {
	own := rec(200, 100, "Mon Jan 1 10:00:00 2026")
	orphan := rec(201, 101, "Mon Jan 1 09:00:00 2026")
	recycled := rec(202, 102, "Mon Jan 1 09:00:00 2026")
	exited := rec(203, 103, "Mon Jan 1 09:00:00 2026")
	pidReused := rec(204, 104, "Mon Jan 1 09:00:00 2026")

	table := proc{
		100: {Exists: true, Start: "Mon Jan 1 10:00:00 2026", Command: "pig"},
		200: {Exists: true, Start: "Mon Jan 1 10:00:01 2026", Command: bridgeCmd},
		// 101 is gone: its bridge must go with it.
		201: {Exists: true, Start: "Mon Jan 1 09:00:01 2026", Command: bridgeCmd},
		// 102 came back as a different process.
		102: {Exists: true, Start: "Tue Feb 2 11:11:11 2026", Command: "google-chrome"},
		202: {Exists: true, Start: "Mon Jan 1 09:00:02 2026", Command: bridgeCmd},
		103: {Exists: true, Start: "Mon Jan 1 09:00:00 2026", Command: "pig"},
		// 203 already exited on its own; 204's pid is now someone else's.
		104: {Exists: true, Start: "Mon Jan 1 09:00:00 2026", Command: "pig"},
		204: {Exists: true, Start: "Wed Mar 3 08:00:00 2026", Command: "/sbin/launchd"},
	}

	kill, keep := reapPlan([]bridgeRecord{own, orphan, recycled, exited, pidReused}, table.look, 0)

	if len(keep) != 1 || keep[0].BridgePID != 200 {
		t.Fatalf("a bridge with a live owner must never be touched, kept %+v", keep)
	}
	got := map[int]bool{}
	for _, r := range kill {
		got[r.BridgePID] = true
	}
	if !got[201] {
		t.Error("the bridge whose session died should be reaped")
	}
	if !got[202] {
		t.Error("the bridge whose session pid was recycled should be reaped")
	}
	if len(kill) != 2 {
		t.Errorf("only two bridges qualify for killing, got %+v", kill)
	}
}

func TestReapRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeRecords(dir, []bridgeRecord{rec(10, 1, "start"), rec(11, 2, "start")})
	got := readRecords(dir)
	if len(got) != 2 || got[0].BridgePID != 10 || got[1].OwnerPID != 2 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	force := readRecords(filepath.Join(dir, "nope.json"))
	if force != nil {
		t.Fatalf("a missing file should read as no records, got %+v", force)
	}

	forgetBridge(dir, 10)
	if got = readRecords(dir); len(got) != 1 || got[0].BridgePID != 11 {
		t.Fatalf("forgetBridge should drop exactly one bridge, got %+v", got)
	}
}

// A garbage file must not make the reaper signal arbitrary processes.
func TestReapRecordsIgnoreCorruption(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(recordsPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readRecords(dir); got != nil {
		t.Fatalf("corrupt file should read as empty, got %+v", got)
	}
	if n := ReapStale(dir, nil); n != 0 {
		t.Fatalf("nothing should be reaped from a corrupt file, got %d", n)
	}
}
