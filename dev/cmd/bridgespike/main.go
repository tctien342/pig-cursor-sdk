// Command bridgespike is the decisive feasibility spike for the Go rewrite.
//
// It answers the one question the whole architecture rests on: can a Cursor
// custom-tool callback stay parked across a pig provider-turn boundary (the
// tool call is shown to pig, pig executes its own tool, the callback is
// resolved on the next turn) and then let the same Cursor run continue?
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tctien342/pig-cursor-sdk/internal/bridge"
)

var t0 = time.Now()

func logf(format string, args ...any) {
	fmt.Printf("[%6dms] "+format+"\n", append([]any{time.Since(t0).Milliseconds()}, args...)...)
}

func cursorAPIKey() string {
	if k := os.Getenv("CURSOR_API_KEY"); k != "" {
		return k
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".pig/agent/auth.json"),
		filepath.Join(home, ".pi/agent/auth.json"),
	} {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var auth map[string]json.RawMessage
		if json.Unmarshal(raw, &auth) != nil {
			continue
		}
		var cred struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		}
		if json.Unmarshal(auth["cursor"], &cred) == nil && cred.Key != "" {
			return strings.TrimSpace(cred.Key)
		}
	}
	return ""
}

// ---- parked custom-tool callback server ------------------------------------

type parked struct {
	srv      *http.Server
	ln       net.Listener
	token    string
	arrivals chan *call
	resolve  chan map[string]any
	mu       sync.Mutex
	seen     []string
}

type call struct {
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	ToolCallID string          `json:"toolCallId"`
	AgentID    string          `json:"agentId"`
	arrivedAt  time.Time
}

func newParked() (*parked, error) {
	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	p := &parked{
		token:    base64.RawURLEncoding.EncodeToString(tok),
		arrivals: make(chan *call, 8),
		resolve:  make(chan map[string]any, 8),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sdk.v1.SdkCustomToolCallbackService/CallCustomTool", p.handle)
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		return nil, lerr
	}
	p.ln = ln
	p.srv = &http.Server{Handler: mux}
	go func() { _ = p.srv.Serve(p.ln) }()
	return p, nil
}

func (p *parked) url() string { return "http://" + p.ln.Addr().String() }

func (p *parked) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("authorization") != "Bearer "+p.token {
		logf("CALLBACK rejected: bad bearer token")
		http.Error(w, `{"code":"unauthenticated","message":"bad token"}`, http.StatusUnauthorized)
		return
	}
	var c call
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, `{"code":"invalid_argument","message":"decode"}`, http.StatusBadRequest)
		return
	}
	c.arrivedAt = time.Now()
	p.mu.Lock()
	p.seen = append(p.seen, c.ToolName)
	p.mu.Unlock()
	logf("CALLBACK arrived tool=%s id=%s args=%s  (PARKING: pig would run this tool now)",
		c.ToolName, c.ToolCallID, truncate(c.Args, 160))
	p.arrivals <- &c

	// Park: hold the HTTP response open until the host result arrives.
	select {
	case res := <-p.resolve:
		b, _ := json.Marshal(res)
		logf("CALLBACK resolved after %dms tool=%s result=%s", time.Since(c.arrivedAt).Milliseconds(), c.ToolName, truncate(b, 160))
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(b)
	case <-time.After(5 * time.Minute):
		logf("CALLBACK timed out tool=%s", c.ToolName)
		http.Error(w, `{"code":"deadline_exceeded","message":"parked too long"}`, http.StatusGatewayTimeout)
	}
}

func truncate(raw []byte, n int) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- main ------------------------------------------------------------------

func main() {
	var (
		model     = flag.String("model", "", "cursor model id (default: first from ListModels)")
		workspace = flag.String("workspace", ".", "agent cwd")
		tool      = flag.String("tool", "spike_host_tool", "custom tool name to advertise")
		parkFor   = flag.Duration("park-for", 3*time.Second, "how long to hold a callback parked (simulates pig tool execution)")
		parkAll   = flag.Bool("park-all", false, "park every callback (proves the run blocks while parked)")
		abort     = flag.Bool("abort-while-parked", false, "cancel the run while a callback is parked")
		restrict  = flag.Bool("restrict-tools", false, "send tools:{names:[]} (drops Cursor built-ins AND the custom-tool mechanism)")
		raw       = flag.String("dump-raw", "", "write raw Send frames as JSONL")
		_         = flag.Bool("keep-stream", false, "do not break the Send stream on park; resolve and finish the run on the same stream")
	)
	flag.Parse()

	key := cursorAPIKey()
	if key == "" {
		fmt.Fprintln(os.Stderr, "no cursor API key (env CURSOR_API_KEY or auth.json)")
		os.Exit(1)
	}
	wd, _ := filepath.Abs(*workspace)

	logf("spawning bridge for %s", wd)
	h, err := bridge.Spawn(context.Background(), bridge.FindBinary(), wd,
		[]string{"CURSOR_API_KEY=" + key}, "", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "spawn:", err)
		os.Exit(1)
	}
	defer h.Close()
	logf("bridge ready url=%s serverVersion=%s pid=%d", h.URL, h.ServerVersion, h.PID)

	ctx := context.Background()
	ping, _, err := h.Call(ctx, "sdk.v1.SdkBridgeControlService", "Ping", map[string]any{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ping:", err, h.StderrTail())
		os.Exit(1)
	}
	logf("ping %s", truncate(ping, 60))

	ver, _, err := h.Call(ctx, "sdk.v1.SdkBridgeControlService", "GetVersion", map[string]any{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "getversion:", err)
		os.Exit(1)
	}
	var version struct {
		BridgeVersion   string   `json:"bridgeVersion"`
		ProtocolVersion string   `json:"protocolVersion"`
		Capabilities    []string `json:"capabilities"`
	}
	_ = json.Unmarshal(ver, &version)
	logf("version bridge=%s protocol=%s capabilities=%s", version.BridgeVersion, version.ProtocolVersion, strings.Join(version.Capabilities, ","))

	me, _, err := h.Call(ctx, "sdk.v1.SdkCursorService", "Me", map[string]any{"options": map[string]any{"apiKey": key}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "me:", err, "| stderr:", h.StderrTail())
		os.Exit(1)
	}
	var who struct {
		User struct {
			APIKeyName string `json:"apiKeyName"`
			UserEmail  string `json:"userEmail"`
		} `json:"user"`
	}
	_ = json.Unmarshal(me, &who)
	logf("auth ok key=%s user=%s", who.User.APIKeyName, who.User.UserEmail)

	mdl, _, err := h.Call(ctx, "sdk.v1.SdkCursorService", "ListModels", map[string]any{"options": map[string]any{"apiKey": key}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "listmodels:", err)
		os.Exit(1)
	}
	var models struct {
		Items []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			Parameters  []struct {
				ID          string                   `json:"id"`
				Values      []struct{ Value string } `json:"values"`
				DisplayName string                   `json:"displayName"`
			} `json:"parameters"`
		} `json:"items"`
	}
	_ = json.Unmarshal(mdl, &models)
	ids := make([]string, 0, len(models.Items))
	for _, m := range models.Items {
		ids = append(ids, m.ID)
	}
	logf("catalog: %d models: %s", len(ids), strings.Join(ids, " "))
	if *model == "" && len(ids) > 0 {
		*model = ids[0]
	}
	logf("using model %s", *model)

	cb, err := newParked()
	if err != nil {
		fmt.Fprintln(os.Stderr, "callback server:", err)
		os.Exit(1)
	}
	if _, _, err := h.Call(ctx, "sdk.v1.SdkBridgeControlService", "SetToolCallback", map[string]any{
		"url": cb.url(), "authToken": cb.token,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "settoolcallback:", err)
		os.Exit(1)
	}
	logf("tool callback registered at %s", cb.url())

	prompt := flag.Arg(0)
	if prompt == "" {
		prompt = fmt.Sprintf("Call the %s custom tool exactly once with command \"echo parked-spike\", then tell me what it returned in one short sentence.", *tool)
	}

	agentOpts := map[string]any{
		"model":  map[string]any{"id": *model},
		"apiKey": key,
		"local": map[string]any{
			"cwd": []string{wd},
			"customTools": map[string]any{*tool: map[string]any{
				"description": "Run a command on the user's machine through the host agent.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string", "description": "Shell command to run"},
					},
					"required": []string{"command"},
				},
			}},
		},
	}
	if *restrict {
		agentOpts["tools"] = map[string]any{"names": []string{}}
		logf("restricting built-in tools to the empty set")
	}
	agent, _, err := h.Call(ctx, "sdk.v1.SdkAgentService", "CreateAgent", map[string]any{"options": agentOpts})
	if err != nil {
		fmt.Fprintln(os.Stderr, "createagent:", err, "| stderr:", h.StderrTail())
		os.Exit(1)
	}
	var created struct {
		AgentID string `json:"agentId"`
	}
	_ = json.Unmarshal(agent, &created)
	logf("agent %s", created.AgentID)
	defer func() {
		_, _, _ = h.Call(context.Background(), "sdk.v1.SdkAgentService", "CloseAgent", map[string]any{"agentId": created.AgentID})
	}()

	// drain decodes one stream frame into the spike's log and bookkeeping.
	var mu sync.Mutex
	var lastOffset string
	var runID string
	seen := map[string]bool{}
	var rawDump *os.File
	if *raw != "" {
		rawDump, _ = os.Create(*raw)
	}
	drain := func(label string, dedup bool) func(bridge.Frame) error {
		return func(f bridge.Frame) error {
			if rawDump != nil && label == "send" {
				fmt.Fprintf(rawDump, "%s\n", f.JSON)
			}
			var env map[string]json.RawMessage
			_ = json.Unmarshal(f.JSON, &env)
			var flat map[string]any
			if json.Unmarshal(f.JSON, &flat) == nil {
				if id := findRunID(flat); id != "" {
					mu.Lock()
					runID = id
					mu.Unlock()
				}
			}
			if off := string(env["offset"]); off != "" && off != `""` {
				mu.Lock()
				lastOffset = off
				mu.Unlock()
			}
			for _, key := range []string{"sdkMessage", "interactionUpdate", "step", "result", "done"} {
				val, ok := env[key]
				if !ok {
					continue
				}
				if dedup {
					sig := key + "|" + string(val)
					mu.Lock()
					dup := seen[sig]
					seen[sig] = true
					mu.Unlock()
					if dup {
						logf("%s DUP %s %s", label, key, truncate(val, 90))
						continue
					}
				}
				switch key {
				case "sdkMessage":
					var m struct {
						Type    string          `json:"type"`
						RunID   string          `json:"run_id"`
						Message json.RawMessage `json:"message"`
					}
					_ = json.Unmarshal(val, &m)
					if m.Type == "text" || m.Type == "thinking" {
						var t struct {
							Text string `json:"text"`
						}
						_ = json.Unmarshal(m.Message, &t)
						logf("%s %s %q", label, m.Type, truncate([]byte(t.Text), 100))
					} else {
						logf("%s sdk_message %s %s", label, m.Type, truncate(m.Message, 200))
					}
				case "interactionUpdate":
					var m struct {
						Type   string          `json:"type"`
						Update json.RawMessage `json:"update"`
					}
					_ = json.Unmarshal(val, &m)
					if m.Type == "token-delta" {
						continue
					}
					logf("%s delta %s %s", label, m.Type, truncate(m.Update, 140))
				case "step":
					var m struct {
						Type string          `json:"type"`
						Step json.RawMessage `json:"step"`
					}
					_ = json.Unmarshal(val, &m)
					logf("%s step %s %s", label, m.Type, truncate(m.Step, 200))
				case "result":
					var r struct {
						Status string          `json:"status"`
						RunID  string          `json:"runId"`
						Result json.RawMessage `json:"result"`
					}
					_ = json.Unmarshal(val, &r)
					logf("%s RESULT %s %s", label, r.Status, truncate(r.Result, 220))
				case "done":
					logf("%s DONE", label)
				}
			}
			return nil
		}
	}

	// Phase 1: Send, and stop reading the live stream as soon as a tool callback
	// is parked. This is exactly the pig boundary: the provider turn ends, pig
	// runs its own tool, and the Cursor run waits on our parked HTTP response.
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	breakOnPark := make(chan struct{})
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- h.Stream(streamCtx, "sdk.v1.SdkAgentService", "Send", map[string]any{
			"agentId": created.AgentID,
			"message": map[string]any{"text": prompt},
			"options": map[string]any{"enableDeltas": true, "enableSteps": true},
		}, func(f bridge.Frame) error {
			if err := drain("send", false)(f); err != nil {
				return err
			}
			select {
			case <-breakOnPark:
				return errBreakStream
			default:
			}
			return nil
		})
	}()

	keepFlag := flag.Lookup("keep-stream")
	keepStream := keepFlag != nil && keepFlag.Value.String() == "true"
	select {
	case c := <-cb.arrivals:
		if keepStream {
			logf("PARKED (keep-stream): tool=%s — leaving the Send stream open and resolving later", c.ToolName)
			time.Sleep(*parkFor)
			logf("RESOLVING callback with the host tool result")
			cb.resolve <- map[string]any{"result": map[string]any{"value": fmt.Sprintf("[host tool output, %dms] parked-tool-output ok", time.Since(c.arrivedAt).Milliseconds())}}
			if err := <-sendDone; err != nil {
				logf("send ended: %v", err)
			}
			logf("send stream finished with run=%s", runID)
			cb.mu.Lock()
			logf("callbacks seen: %d [%s]", len(cb.seen), strings.Join(cb.seen, ","))
			cb.mu.Unlock()
			fmt.Println("SPIKE_DONE")
			return
		}
		logf("PARKED: tool=%s — closing the Send stream to end this provider turn", c.ToolName)
		logf("PARKED: tool=%s — closing the Send stream to end this provider turn", c.ToolName)
		close(breakOnPark)
		if err := <-sendDone; err != nil && err != errBreakStream {
			logf("send stream ended: %v", err)
		} else {
			logf("Send stream closed at %dms (run still alive, parked on the host tool)", time.Since(c.arrivedAt).Milliseconds())
		}
		time.Sleep(*parkFor) // pig would be running its own tool right now
		if *abort {
			logf("ABORT while parked: CancelRun")
			_, _, err := h.Call(ctx, "sdk.v1.SdkAgentService", "CancelRun", map[string]any{"agentId": created.AgentID, "runId": runID})
			logf("CancelRun err=%v", err)
			cb.resolve <- map[string]any{"result": map[string]any{"value": "cancelled by user"}}
		} else {
			logf("RESOLVING callback with the host tool result")
			cb.resolve <- map[string]any{"result": map[string]any{"value": fmt.Sprintf("[host tool output, %dms] parked-tool-output ok", time.Since(c.arrivedAt).Milliseconds())}}
		}
		if *parkAll || *abort {
			break
		}
		// Phase 2: the next provider turn resumes the same run through the durable
		// stream and keeps streaming the rest of the turn.
		time.Sleep(2 * time.Second)
		mu.Lock()
		id := runID
		mu.Unlock()
		logf("RESUME via ObserveRun run=%s (durable offsets only)", id)
		if err := h.Stream(context.Background(), "sdk.v1.SdkAgentService", "ObserveRun", map[string]any{
			"runId": id,
		}, drain("observe", true)); err != nil {
			logf("observe error: %v", err)
		}
	case err := <-sendDone:
		if err != nil {
			logf("send ended without a parked callback: %v", err)
		}
	}

	cb.mu.Lock()
	logf("callbacks seen: %d [%s]", len(cb.seen), strings.Join(cb.seen, ","))
	cb.mu.Unlock()
	mu.Lock()
	logf("last durable offset: %s", lastOffset)
	mu.Unlock()
	fmt.Println("SPIKE_DONE")
	_, _ = hex.EncodeToString(nil), lastOffset
}

var errBreakStream = errors.New("spike: breaking stream on park")

// findRunID walks a decoded frame for a run id at any depth.
func findRunID(v any) string {
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t["run_id"].(string); ok && s != "" {
			return s
		}
		if s, ok := t["runId"].(string); ok && s != "" {
			return s
		}
		for _, child := range t {
			if s := findRunID(child); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range t {
			if s := findRunID(child); s != "" {
				return s
			}
		}
	}
	return ""
}
