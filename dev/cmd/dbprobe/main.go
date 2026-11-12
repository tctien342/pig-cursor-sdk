// Command dbprobe checks one thing: can Go read Cursor's desktop session token
// out of state.vscdb without CGO and while Cursor is running?
//
//	go run ./cmd/dbprobe [path-to-state.vscdb]
//
// It prints key names and value lengths only — never a secret.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func main() {
	path := os.Args[1]
	if len(os.Args) < 2 {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, "Library/Application Support/Cursor/User/globalStorage/state.vscdb")
	}
	st, err := os.Stat(path)
	if err != nil {
		fmt.Println("stat:", err)
		os.Exit(1)
	}
	fmt.Printf("db: %s (%.1f MB)\n", path, float64(st.Size())/1e6)

	// Read-only, with a busy timeout: Cursor holds this database open.
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query(`select key, length(value) from ItemTable where key in
		('cursorAuth/accessToken','cursorAuth/refreshToken','cursorAuth/cachedEmail','cursorAuth/cachedUserId','cursorAuth/cachedTeam')`)
	if err != nil {
		fmt.Println("query:", err)
		os.Exit(1)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var k string
		var l int
		if err := rows.Scan(&k, &l); err != nil {
			fmt.Println("scan:", err)
			continue
		}
		fmt.Printf("  %-28s %d chars\n", k, l)
		n++
	}
	fmt.Println("keys found:", n)
}
