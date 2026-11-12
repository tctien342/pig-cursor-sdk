// Probe the bridge with the exact agent spec the extension builds, so a
// rejection can be traced to one field instead of guessed at.
//
//	go run ./cmd/specprobe -model grok-4.7 -params context=500k -tools 1
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func main() {
	model := flag.String("model", "default", "Cursor model id")
	params := flag.String("params", "", "comma list of id=value model params")
	nTools := flag.Int("tools", 1, "number of custom tools to advertise")
	bigSchemas := flag.Bool("bigSchemas", false, "give every tool a large nested schema")
	prompt := flag.String("prompt", "Reply with exactly: ok", "user message")
	flag.Parse()

	key := os.Getenv("CURSOR_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "CURSOR_API_KEY required")
		os.Exit(2)
	}
	cwd, _ := os.Getwd()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	c, err := cursor.New(ctx, cursor.Options{BridgeBin: cursor.FindBinary(), Workspace: cwd, APIKey: key, Logger: log})
	if err != nil {
		fmt.Fprintln(os.Stderr, "spawn:", err)
		os.Exit(1)
	}
	defer c.Close()

	spec := cursor.AgentSpec{APIKey: key, Model: *model, Cwd: cwd}
	for _, p := range strings.Split(*params, ",") {
		if p == "" {
			continue
		}
		id, value, _ := strings.Cut(p, "=")
		spec.ModelParams = append(spec.ModelParams, cursor.ModelParam{ID: strings.TrimSpace(id), Value: strings.TrimSpace(value)})
	}
	for i := 0; i < *nTools; i++ {
		schema := map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}
		if *bigSchemas {
			props := map[string]any{}
			for j := 0; j < 30; j++ {
				props[fmt.Sprintf("field_%d", j)] = map[string]any{
					"type":        "object",
					"description": strings.Repeat("A long description. ", 40),
					"properties": map[string]any{
						"inner": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"a", "b", "c"}}},
					},
				}
			}
			schema = map[string]any{"type": "object", "properties": props, "required": []string{"field_0"}}
		}
		spec.Tools = append(spec.Tools, cursor.ToolSpec{Name: fmt.Sprintf("probe_tool_%d", i), Description: "probe", InputSchema: schema})
	}

	fmt.Printf("spec: model=%s params=%d tools=%d\n", spec.Model, len(spec.ModelParams), len(spec.Tools))
	agent, err := c.CreateAgent(ctx, spec)
	if err != nil {
		fmt.Println("CreateAgent FAILED:", err)
		fmt.Println("  connect code:", connect.CodeOf(err))
		os.Exit(1)
	}
	fmt.Println("agent:", agent.ID())

	run, err := agent.Send(ctx, *prompt, nil)
	if err != nil {
		fmt.Println("Send FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("run:", run.RunID())
	text := ""
	for i := 0; ; i++ {
		ev, ok := run.Next(ctx)
		if !ok {
			break
		}
		switch ev.Kind {
		case cursor.EventTextDelta:
			text += ev.Text
		case cursor.EventStatus:
			if ev.Status != nil {
				fmt.Printf("status %s: %s\n", ev.Status.Status, ev.Status.Message)
			}
		case cursor.EventToolCallStarted:
			host := ev.Pending != nil
			name := ""
			if ev.Pending != nil {
				name = ev.Pending.ToolName
			}
			fmt.Println("tool call: host=", host, name)
		}
		if i > 4000 {
			break
		}
	}
	if err := run.DrainErr(); err != nil {
		fmt.Println("stream error:", err)
		fmt.Println("  connect code:", connect.CodeOf(err))
		var ce *connect.Error
		if errors.As(err, &ce) {
			for _, d := range ce.Details() {
				v, err := d.Value()
				if err != nil {
					fmt.Println("  detail:", d.Type(), "unreadable:", err)
					continue
				}
				b, _ := protojson.Marshal(v)
				fmt.Println("  detail:", d.Type(), string(b))
			}
			fmt.Println("  message:", ce.Message())
		}
		os.Exit(1)
	}
	fmt.Printf("text: %q\n", text)
	if t := run.Terminal(); t != nil {
		fmt.Printf("terminal: %s err=%q\n", t.Status, t.ErrorMessage)
	}
}
