// Package planusage reads Cursor's plan-usage picture — "how much of my
// allowance have I used this cycle" — which the bridge protocol cannot answer for
// an API key.
//
// Why it is shaped this way:
//
//   - sdk.v1 has no plan-limit or account-usage RPC usable by a key. GetUsage is
//     cloud-only, ListAgents lists nothing for a key whose agents are local, and
//     limit windows appear only inside a refusal error.
//   - Cursor's own app reads these numbers from aiserver.v1.DashboardService,
//     authenticated with the *desktop login* token, and that service answers 401
//     for every API-key form of auth.
//
// So this package uses the desktop session, but only with the user's explicit opt
// in, because the token belongs to another application's private storage. It is
// read read-only, held in memory, and never written to a log or the transcript.
package planusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"database/sql"
	_ "modernc.org/sqlite" // pure Go, so the extension still builds without CGO
)

// Endpoint is Cursor's own API, used by the desktop app. Not a public contract:
// fields can change or disappear without notice, which is why every consumer of
// this package treats a failure as "no data", never as "zero usage".
const Endpoint = "https://api2.cursor.sh/aiserver.v1.DashboardService"

// ErrNoToken means Cursor Desktop is not installed or not logged in on this
// machine, or its store could not be read.
var ErrNoToken = errors.New("no Cursor desktop login token found")

// Snapshot is the plan-usage picture, in the units Cursor uses (US cents).
type Snapshot struct {
	PlanName      string `json:"planName"`
	Price         string `json:"price"`
	CycleStart    time.Time
	CycleEnd      time.Time
	TotalSpend    int64 // cents, all sources
	IncludedSpend int64 // cents used from the plan allowance
	IncludedLimit int64 // cents the plan includes
	BonusSpend    int64 // cents of free overage Cursor granted
	PercentUsed   float64
	HardLimit     int64  // cents, 0 when unset
	Account       string // cached email, for labelling the readout
	At            time.Time
}

// Remaining returns the cents left in the included allowance: never negative, and
// zero once the allowance is exhausted, which is the common case once bonus
// spending starts.
func (s Snapshot) Remaining() int64 {
	r := s.IncludedLimit - s.IncludedSpend
	if r < 0 {
		return 0
	}
	return r
}

// Exhausted reports whether the included allowance is fully used.
func (s Snapshot) Exhausted() bool {
	return s.IncludedLimit > 0 && s.IncludedSpend >= s.IncludedLimit
}

// Availability classifies why plan usage can or cannot be read, so the UI can say
// something true instead of showing nothing.
type Availability string

const (
	// Available means Cursor Desktop is installed, holds a login token, and this
	// package can read it.
	Available Availability = "available"
	// NoInstall means Cursor Desktop's storage was not found on this machine.
	NoInstall Availability = "cursor-desktop-not-installed"
	// NoLogin means Cursor is installed but no session token is stored: the user
	// has never logged in, or logged out.
	NoLogin Availability = "cursor-desktop-not-logged-in"
	// Unreadable means the store exists but could not be opened (permissions, or a
	// SQLite failure while Cursor holds it).
	Unreadable Availability = "cursor-storage-unreadable"
)

// Check reports whether the plan readout can work, without contacting the
// network. It is cheap: a stat, and one indexed read of one key.
func Check() (Availability, string) {
	if p := StorePath(); p == "" {
		return NoInstall, "no Cursor Desktop storage found at the default location for this OS"
	}
	if _, err := Token(); err != nil {
		if errors.Is(err, ErrNoToken) {
			return NoLogin, "Cursor Desktop is installed but holds no login token — sign in inside the Cursor app"
		}
		return Unreadable, err.Error()
	}
	return Available, ""
}

// StorePath returns Cursor Desktop's storage database for this OS, or "" when the
// app is not installed. PI_CURSOR_STORE_PATH overrides it, for tests and for a
// non-standard install; pointing it at a missing file is how the "Cursor is not
// installed" path gets exercised without touching anyone's real login.
func StorePath() string {
	if v := strings.TrimSpace(os.Getenv("PI_CURSOR_STORE_PATH")); v != "" {
		if st, err := os.Stat(v); err == nil && !st.IsDir() {
			return v
		}
		return ""
	}
	var base string
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, "Library", "Application Support", "Cursor")
	case "windows":
		base = os.Getenv("APPDATA")
		if base == "" {
			return ""
		}
		base = filepath.Join(base, "Cursor")
	default: // linux and friends
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			base = filepath.Join(xdg, "Cursor")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return ""
			}
			base = filepath.Join(home, ".config", "Cursor")
		}
	}
	p := filepath.Join(base, "User", "globalStorage", "state.vscdb")
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		return ""
	}
	return p
}

// Token reads the desktop access token from Cursor's own storage, read-only.
// The value is returned for the caller to use in memory; nothing here logs it.
func Token() (string, error) {
	path := StorePath()
	if path == "" {
		return "", ErrNoToken
	}
	// mode=ro plus query_only keeps this a pure read even though the Cursor app
	// holds the database open; busy_timeout stops the read failing on a write.
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", fmt.Errorf("open Cursor storage: %w", err)
	}
	defer db.Close()

	var tok string
	err = db.QueryRow(`select value from ItemTable where key = 'cursorAuth/accessToken'`).Scan(&tok)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoToken
	}
	if err != nil {
		return "", fmt.Errorf("read Cursor login: %w", err)
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", ErrNoToken
	}
	return tok, nil
}

// Fetch reads the plan-usage snapshot with a desktop token.
func Fetch(ctx context.Context, token string) (*Snapshot, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	c := &http.Client{Timeout: 10 * time.Second}
	snap := &Snapshot{At: time.Now()}

	var period struct {
		BillingCycleStart string `json:"billingCycleStart"`
		BillingCycleEnd   string `json:"billingCycleEnd"`
		PlanUsage         struct {
			TotalSpend      float64 `json:"totalSpend"`
			IncludedSpend   float64 `json:"includedSpend"`
			BonusSpend      float64 `json:"bonusSpend"`
			Limit           float64 `json:"limit"`
			AutoPercentUsed float64 `json:"autoPercentUsed"`
		} `json:"planUsage"`
	}
	if err := call(ctx, c, token, "GetCurrentPeriodUsage", &period); err != nil {
		return nil, err
	}
	snap.TotalSpend = int64(period.PlanUsage.TotalSpend)
	snap.IncludedSpend = int64(period.PlanUsage.IncludedSpend)
	snap.BonusSpend = int64(period.PlanUsage.BonusSpend)
	snap.IncludedLimit = int64(period.PlanUsage.Limit)
	snap.PercentUsed = period.PlanUsage.AutoPercentUsed
	snap.CycleStart = epochMillis(period.BillingCycleStart)
	snap.CycleEnd = epochMillis(period.BillingCycleEnd)

	var plan struct {
		PlanInfo struct {
			PlanName string `json:"planName"`
			Price    string `json:"price"`
		} `json:"planInfo"`
	}
	if err := call(ctx, c, token, "GetPlanInfo", &plan); err == nil {
		snap.PlanName = plan.PlanInfo.PlanName
		snap.Price = plan.PlanInfo.Price
	}

	var hard struct {
		HardLimit float64 `json:"hardLimit"`
	}
	if err := call(ctx, c, token, "GetHardLimit", &hard); err == nil {
		snap.HardLimit = int64(hard.HardLimit)
	}

	if v, err := email(); err == nil {
		snap.Account = v
	}
	return snap, nil
}

// email is the cached account label from the same store, so the readout can say
// whose plan it is without another network call.
func email() (string, error) {
	path := StorePath()
	if path == "" {
		return "", ErrNoToken
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)")
	if err != nil {
		return "", err
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`select value from ItemTable where key = 'cursorAuth/cachedEmail'`).Scan(&v); err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

// endpoint is overridable for tests; production uses Cursor's API.
func endpoint() string {
	if v := strings.TrimSpace(os.Getenv("PI_CURSOR_PLAN_ENDPOINT")); v != "" {
		return v
	}
	return Endpoint
}

func call(ctx context.Context, c *http.Client, token, method string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint()+"/"+method, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("x-cursor-client-type", "desktop")
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return errors.New("Cursor rejected the desktop login token (log out and back in inside the Cursor app)")
	case res.StatusCode >= 300:
		return fmt.Errorf("%s: HTTP %d %s", method, res.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

func epochMillis(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(n)
}
