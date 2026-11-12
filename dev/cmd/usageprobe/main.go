// Command usageprobe answers one question with evidence: which of Cursor's usage
// surfaces actually return numbers for the agents this key has.
//
//	CURSOR_API_KEY=… go run ./dev/cmd/usageprobe [limit]
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func main() {
	key := os.Getenv("CURSOR_API_KEY")
	if key == "" {
		fmt.Println("CURSOR_API_KEY is required")
		os.Exit(2)
	}
	limit := uint32(20)
	if len(os.Args) > 1 {
		var n int
		if _, err := fmt.Sscan(os.Args[1], &n); err == nil && n > 0 {
			limit = uint32(n)
		}
	}

	client, err := cursor.New(context.Background(), cursor.Options{APIKey: key, Workspace: wd(), BridgeBin: cursor.FindBinary()})
	if err != nil {
		fmt.Println("bridge:", err)
		os.Exit(1)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	me, err := client.Me(ctx, key)
	if err != nil {
		fmt.Println("Me:", err)
	} else {
		fmt.Printf("account: %s (api key %q)\n", me.GetUserEmail(), me.GetApiKeyName())
	}

	agents, err := client.ListAgents(ctx, key, limit)
	if err != nil {
		fmt.Println("ListAgents:", err)
		os.Exit(1)
	}
	fmt.Printf("ListAgents(limit %d): %d agent(s)\n", limit, len(agents))
	for i, a := range agents {
		if i >= 5 {
			fmt.Println("  … (truncated)")
			break
		}
		fmt.Printf("  %s  created=%s name=%q\n", a.ID, a.CreatedAt.Format(time.RFC3339), a.Name)
	}

	var sum, runs, ok, bad int
	for _, a := range agents {
		u, err := client.Usage(ctx, a.ID)
		if err != nil {
			if bad == 0 {
				fmt.Println("  first GetUsage error:", err)
			}
			bad++
			continue
		}
		if u == nil {
			bad++
			continue
		}
		ok++
		runs += u.Runs
		sum += int(u.Tokens.TotalTokens)
	}
	fmt.Printf("GetUsage: %d answered, %d failed | total tokens %d, runs %d\n", ok, bad, sum, runs)

	// And what one of OUR own agents reports, since that is the footer's case.
	if id := lastAgentID(); id != "" {
		u, err := client.Usage(ctx, id)
		fmt.Printf("this extension's agent %s: err=%v", id, err)
		if err == nil && u != nil {
			fmt.Printf(" tokens=%d runs=%d cents=%.2f(%s)", u.Tokens.TotalTokens, u.Runs, u.CostCents, u.CostLabel)
		}
		fmt.Println()
	}
}

func wd() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// lastAgentID reads the id this extension last recorded for an agent, to test the
// exact id shape the footer's usage call uses.
func lastAgentID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(home + "/.pig/agent/pig-cursor-sdk/agents.json")
	if err != nil {
		return ""
	}
	// Tiny scan: last "agentId":"…" occurrence.
	var last string
	for i := 0; i < len(raw); i++ {
		if string(raw[i]) == `"agentId":"` {
			j := i + len(`"agentId":"`)
			k := j
			for k < len(raw) && raw[k] != '"' {
				k++
			}
			last = string(raw[j:k])
			i = k
		}
	}
	return last
}
