package planusage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The whole feature is conditional on Cursor Desktop being present and logged in,
// so the "not there" answers have to be classified, not swallowed.
func TestCheckClassifiesMissingCursor(t *testing.T) {
	t.Setenv("PI_CURSOR_STORE_PATH", filepath.Join(t.TempDir(), "absent.vscdb"))
	if got, why := Check(); got != NoInstall || why == "" {
		t.Fatalf("Check() = %q (%q), want %q with a reason", got, why, NoInstall)
	}
}

func TestCheckClassifiesInstalledButLoggedOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db := openStore(t, path)
	if _, err := db.Exec(`insert into ItemTable (key, value) values ('cursorAuth/cachedEmail', 'someone@example.com')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	t.Setenv("PI_CURSOR_STORE_PATH", path)
	got, why := Check()
	if got != NoLogin || why == "" {
		t.Fatalf("Check() = %q (%q), want %q with a reason", got, why, NoLogin)
	}
}

func TestTokenReadsLoginAndNeverTheWrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db := openStore(t, path)
	if _, err := db.Exec(`insert into ItemTable (key, value) values ('cursorAuth/accessToken', 'tok-123')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	t.Setenv("PI_CURSOR_STORE_PATH", path)
	got, err := Token()
	if err != nil {
		t.Fatalf("Token(): %v", err)
	}
	if got != "tok-123" {
		t.Fatalf("Token() = %q", got)
	}
	if _, why := Check(); why != "" {
		t.Fatalf("Check() reason = %q, want empty when a token exists", why)
	}
}

// A plan snapshot is parsed from Cursor's live-shaped JSON. Numbers matter: the
// included allowance and the granted bonus are separate pots, and the footer says
// different things depending on which one is being spent.
func TestFetchParsesCursorResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/GetCurrentPeriodUsage":
			fmt.Fprint(w, `{"billingCycleStart":"1793324262000","billingCycleEnd":"1796002662000",
			  "planUsage":{"totalSpend":7513,"includedSpend":2000,"bonusSpend":5513,"limit":2000,"autoPercentUsed":7.07}}`)
		case "/GetPlanInfo":
			fmt.Fprint(w, `{"planInfo":{"planName":"Team","price":"$40/mo"}}`)
		case "/GetHardLimit":
			fmt.Fprint(w, `{"hardLimit":14}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()
	t.Setenv("PI_CURSOR_PLAN_ENDPOINT", srv.URL)

	snap, err := Fetch(context.Background(), "tok")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.PlanName != "Team" || snap.Price != "$40/mo" {
		t.Fatalf("plan = %q %q", snap.PlanName, snap.Price)
	}
	if snap.TotalSpend != 7513 || snap.IncludedSpend != 2000 || snap.BonusSpend != 5513 || snap.IncludedLimit != 2000 {
		t.Fatalf("spend = total %d incl %d bonus %d limit %d", snap.TotalSpend, snap.IncludedSpend, snap.BonusSpend, snap.IncludedLimit)
	}
	if !snap.Exhausted() || snap.Remaining() != 0 {
		t.Fatalf("allowance should read exhausted, remaining %d", snap.Remaining())
	}
	if got, want := snap.CycleEnd.Format("2006-01-02"), "2026-12-01"; got > want {
		t.Fatalf("cycle end %s implausible", got)
	}
	if snap.HardLimit != 14 {
		t.Fatalf("hard limit = %d", snap.HardLimit)
	}
}

func TestFetchReportsRejectedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("PI_CURSOR_PLAN_ENDPOINT", srv.URL)

	_, err := Fetch(context.Background(), "stale")
	if err == nil {
		t.Fatal("want an error for a rejected token")
	}
}

func TestRemainingAndExhausted(t *testing.T) {
	partial := Snapshot{IncludedLimit: 2000, IncludedSpend: 500}
	if partial.Exhausted() || partial.Remaining() != 1500 {
		t.Fatalf("partial allowance: exhausted=%v remaining=%d", partial.Exhausted(), partial.Remaining())
	}
	over := Snapshot{IncludedLimit: 2000, IncludedSpend: 2500}
	if !over.Exhausted() || over.Remaining() != 0 {
		t.Fatalf("overspent allowance must not report negative remaining, got %d", over.Remaining())
	}
}

func TestEpochMillis(t *testing.T) {
	if got := epochMillis("1793324262000"); got.IsZero() || got.Year() < 2020 {
		t.Fatalf("epochMillis parsed %v", got)
	}
	if got := epochMillis(""); !got.IsZero() {
		t.Fatalf("empty epoch should be zero time, got %v", got)
	}
}

// openStore creates a stand-in for Cursor's storage database, same table name.
func openStore(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`create table ItemTable (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	// Give the fixture a plausible age so time-based code paths stay honest.
	if _, err := db.Exec(`insert into ItemTable (key, value) values ('telemetry.firstSessionDate', ?)`,
		json.RawMessage([]byte(`"`+time.Now().Format(time.RFC3339)+`"`))); err != nil {
		t.Fatal(err)
	}
	return db
}
