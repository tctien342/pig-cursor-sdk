package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tctien342/pig-cursor-sdk/internal/bridge"
)

// agentRegistry keeps the Cursor agent id of each conversation branch on disk.
//
// The identity of an agent is durable information; its live execution is not: a
// new pig process gets a new bridge, so a resumed agent carries Cursor's own
// stored run state and nothing of this process's. When resume fails, the caller
// falls back to building a fresh agent and replaying the transcript, which is
// always correct, only slower.
type agentRegistry struct {
	mu   sync.Mutex
	path string
	doc  agentDoc

	// ownerPID/ownerStart describe this process; owned reports whether a recorded
	// owner is still the same running process. Both are replaced in tests.
	ownerPID   int
	ownerStart string
	owned      func(pid int, start string) bool
}

type agentDoc struct {
	// Version guards the file format against a future shape change.
	Version   int                    `json:"version"`
	Agents    map[string]agentRecord `json:"agents"`
	UpdatedAt int64                  `json:"updatedAt"`
}

type agentRecord struct {
	AgentID   string `json:"agentId"`
	UpdatedAt int64  `json:"updatedAt"`

	// OwnerPID and OwnerStart identify the process believed to hold this agent.
	// An agent is not a shared resource: resuming one another session is still
	// using means colliding with its run. OwnerStart is the process start time as
	// of recording, since a pid that came back is a different process.
	OwnerPID   int    `json:"ownerPid,omitempty"`
	OwnerStart string `json:"ownerStart,omitempty"`
}

// ownership decides what to do with a recorded agent, and is a pure function of
// the record so the rule is testable: an agent recorded by a live process belongs
// to that process; anything else is free to take over.
func (rec agentRecord) resumable(owned func(pid int, start string) bool) bool {
	if rec.OwnerPID == 0 {
		return true // written before ownership was recorded; no one can claim it
	}
	return !owned(rec.OwnerPID, rec.OwnerStart)
}

const agentRegistryTTL = 12 * time.Hour

// newAgentRegistry loads (or creates) the registry inside the extension's state
// directory. An unreadable file is treated as empty rather than fatal: the
// worst case is one extra agent creation.
func newAgentRegistry(dir string) *agentRegistry {
	r := &agentRegistry{
		path: filepath.Join(dir, "agents.json"),
		doc:  agentDoc{Version: 1, Agents: map[string]agentRecord{}},
	}
	r.ownerPID = os.Getpid()
	r.ownerStart = bridge.ProcessStart(r.ownerPID)
	// Our own pid never owns anything from our point of view: a reload closes the
	// running engine's agents and builds a new one in the same process, and that
	// new engine must still be able to resume what the previous one recorded.
	r.owned = func(pid int, start string) bool {
		if pid == r.ownerPID {
			return false
		}
		return bridge.OwnerAlive(pid, start)
	}
	if raw, err := os.ReadFile(r.path); err == nil {
		var doc agentDoc
		if json.Unmarshal(raw, &doc) == nil && doc.Version == 1 && doc.Agents != nil {
			r.doc = doc
		}
	}
	return r
}

// lookup returns a still-fresh agent id for a conversation key, and whether that
// agent is claimed by a process still running. A claimed agent must not be
// resumed: the other session is talking to it, and two processes on one agent
// means one of them is told "already has active run".
func (r *agentRegistry) lookup(key string) (agentID string, free bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.doc.Agents[registryKey(key)]
	if !ok || rec.AgentID == "" {
		return "", false
	}
	if time.Since(time.UnixMilli(rec.UpdatedAt)) > agentRegistryTTL {
		return "", false
	}
	if !rec.resumable(r.owned) {
		return rec.AgentID, false
	}
	return rec.AgentID, true
}

// remember stores the agent id of a conversation key, and claims it for this
// process so a second session in the same directory picks its own agent instead.
func (r *agentRegistry) remember(key, agentID string) {
	if agentID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.doc.Agents == nil {
		r.doc.Agents = map[string]agentRecord{}
	}
	r.doc.Agents[registryKey(key)] = agentRecord{
		AgentID:    agentID,
		UpdatedAt:  time.Now().UnixMilli(),
		OwnerPID:   r.ownerPID,
		OwnerStart: r.ownerStart,
	}
	r.doc.UpdatedAt = time.Now().UnixMilli()
	r.saveLocked()
}

// forget drops a key, used when an agent was closed or resume proved impossible.
func (r *agentRegistry) forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.doc.Agents[registryKey(key)]; !ok {
		return
	}
	delete(r.doc.Agents, registryKey(key))
	r.saveLocked()
}

// saveLocked writes the file; the caller holds r.mu. A write failure is not
// reported: the registry is an optimization, never a correctness dependency.
func (r *agentRegistry) saveLocked() {
	if r.path == "" {
		return
	}
	if len(r.doc.Agents) > 200 {
		// Bound the file: keep the most recently used entries.
		type rec struct {
			k string
			r agentRecord
		}
		all := make([]rec, 0, len(r.doc.Agents))
		for k, v := range r.doc.Agents {
			all = append(all, rec{k, v})
		}
		for i := 1; i < len(all); i++ {
			for j := i; j > 0 && all[j-1].r.UpdatedAt < all[j].r.UpdatedAt; j-- {
				all[j-1], all[j] = all[j], all[j-1]
			}
		}
		r.doc.Agents = map[string]agentRecord{}
		for _, e := range all[:200] {
			r.doc.Agents[e.k] = e.r
		}
	}
	raw, err := json.Marshal(&r.doc)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return
	}
	tmp := r.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, r.path)
}

// registryKey hashes the conversation key: it holds absolute paths, which have
// no business appearing in a file someone else might read.
func registryKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}
