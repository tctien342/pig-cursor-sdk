package provider

import "testing"

// Two pig sessions on one conversation must not end up on one Cursor agent: the
// second is told the agent "already has active run", and whichever one exits closes
// the agent out from under the other. The registry is what makes them collide, so
// the registry is where the claim has to be recorded.
func TestAgentRegistryRefusesAnAgentAnotherProcessHolds(t *testing.T) {
	dir := t.TempDir()
	r := newAgentRegistry(dir)
	live := map[int]bool{4242: true}
	r.owned = func(pid int, start string) bool { return live[pid] && start == "start-1" }
	r.ownerPID = 9999
	r.ownerStart = "start-9"

	r.remember("key-a", "agent-1")
	got, free := r.lookup("key-a")
	if got != "agent-1" || !free {
		t.Fatalf("our own record should be resumable, got %q free=%v", got, free)
	}

	// A record written by a process that is still running belongs to that process.
	r.doc.Agents[registryKey("key-a")] = agentRecord{
		AgentID: "agent-2", UpdatedAt: r.doc.Agents[registryKey("key-a")].UpdatedAt,
		OwnerPID: 4242, OwnerStart: "start-1",
	}
	if got, free = r.lookup("key-a"); free {
		t.Fatalf("a live process's agent must not be resumed, got %q free=%v", got, free)
	}

	// Once that process is gone — or its pid has come back as a different process —
	// taking the agent over is correct: nobody is using it.
	live[4242] = false
	if _, free = r.lookup("key-a"); !free {
		t.Fatal("an agent whose owner died should be resumable")
	}
	live[4242] = true
	r.doc.Agents[registryKey("key-a")] = agentRecord{
		AgentID: "agent-3", UpdatedAt: r.doc.Agents[registryKey("key-a")].UpdatedAt,
		OwnerPID: 4242, OwnerStart: "an-earlier-life",
	}
	if _, free = r.lookup("key-a"); !free {
		t.Fatal("a recycled pid must not count as owning an agent")
	}

	// Records from before ownership was tracked stay usable: refusing them would
	// turn every existing install's resume into a replay.
	r.doc.Agents[registryKey("key-a")] = agentRecord{AgentID: "legacy", UpdatedAt: r.doc.Agents[registryKey("key-a")].UpdatedAt}
	if got, free = r.lookup("key-a"); got != "legacy" || !free {
		t.Fatalf("a legacy record should still resume, got %q free=%v", got, free)
	}
}

// A reload builds a fresh engine in the same process, after the old engine closed
// its agents. That new engine is not "another session".
func TestAgentRegistryTreatsItsOwnProcessAsFree(t *testing.T) {
	dir := t.TempDir()
	r := newAgentRegistry(dir)
	r.ownerPID = 7777
	r.ownerStart = "start-7"
	r.remember("key-b", "agent-before-reload")

	before := r.doc.Agents[registryKey("key-b")]
	if before.OwnerPID != 7777 {
		t.Fatalf("remember should claim the agent for this process, got %+v", before)
	}
	if got, free := r.lookup("key-b"); got != "agent-before-reload" || !free {
		t.Fatalf("the same process must be able to resume its own agent, got %q free=%v", got, free)
	}
}
