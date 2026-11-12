package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	if raw, err := os.ReadFile(r.path); err == nil {
		var doc agentDoc
		if json.Unmarshal(raw, &doc) == nil && doc.Version == 1 && doc.Agents != nil {
			r.doc = doc
		}
	}
	return r
}

// lookup returns a still-fresh agent id for a conversation key.
func (r *agentRegistry) lookup(key string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.doc.Agents[registryKey(key)]
	if !ok || rec.AgentID == "" {
		return "", false
	}
	if time.Since(time.UnixMilli(rec.UpdatedAt)) > agentRegistryTTL {
		return "", false
	}
	return rec.AgentID, true
}

// remember stores the agent id of a conversation key.
func (r *agentRegistry) remember(key, agentID string) {
	if agentID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.doc.Agents == nil {
		r.doc.Agents = map[string]agentRecord{}
	}
	r.doc.Agents[registryKey(key)] = agentRecord{AgentID: agentID, UpdatedAt: time.Now().UnixMilli()}
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
