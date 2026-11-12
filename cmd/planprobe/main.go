// Command planprobe measures and prints what the plan readout would show, so the
// cost and the data can be checked without starting pig.
//
//	go run ./cmd/planprobe
package main

import (
	"context"
	"fmt"
	"time"

	planusage "github.com/tctien342/pig-cursor-sdk/internal/planusage"
)

func main() {
	t0 := time.Now()
	avail, why := planusage.Check()
	fmt.Printf("Check: %s (%s) in %dms  store=%q\n", avail, why, time.Since(t0).Milliseconds(), planusage.StorePath())
	if avail != planusage.Available {
		return
	}

	t1 := time.Now()
	tok, err := planusage.Token()
	if err != nil {
		fmt.Println("Token:", err)
		return
	}
	fmt.Printf("Token: read in %dms (%d chars, value not shown)\n", time.Since(t1).Milliseconds(), len(tok))

	t2 := time.Now()
	snap, err := planusage.Fetch(context.Background(), tok)
	if err != nil {
		fmt.Println("Fetch:", err)
		return
	}
	fmt.Printf("Fetch: %dms\n", time.Since(t2).Milliseconds())
	fmt.Printf("plan %s %s | used $%.2f of $%.2f included | bonus $%.2f | total $%.2f | %.2f%%\n",
		snap.PlanName, snap.Price,
		float64(snap.IncludedSpend)/100, float64(snap.IncludedLimit)/100,
		float64(snap.BonusSpend)/100, float64(snap.TotalSpend)/100, snap.PercentUsed)
	fmt.Printf("cycle %s -> %s | hard limit $%d | account %s | remaining $%.2f exhausted=%v\n",
		snap.CycleStart.Local().Format(time.RFC3339), snap.CycleEnd.Local().Format(time.RFC3339),
		snap.HardLimit, snap.Account, float64(snap.Remaining())/100, snap.Exhausted())
}
