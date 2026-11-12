package provider

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tctien342/pig-cursor-sdk/gen/sdk/v1/sdkv1connect"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func TestDoctorShowsPendingUntilCancellationRPCCompletes(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	service := &scopedCancelServer{cancelled: make(chan string, 2), cancelEntered: make(chan struct{}), cancelRelease: release}
	_, handler := sdkv1connect.NewSdkAgentServiceHandler(service)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &cursor.Client{Agent: sdkv1connect.NewSdkAgentServiceClient(server.Client(), server.URL)}
	agent, err := client.CreateAgent(context.Background(), cursor.AgentSpec{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close(context.Background())
	run, err := agent.Send(context.Background(), "wait", nil)
	if err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, ok := run.Next(wait); !ok {
		t.Fatal("missing identity")
	}
	result := make(chan error, 1)
	go func() { result <- run.CancelRunContext(context.Background(), "test cancellation") }()
	awaitSignal(t, service.cancelEntered)
	snapshot := func() string {
		return FormatDoctor(DoctorReport{ScopeKnown: true, Runs: []DoctorRun{{Status: run.Status()}}})
	}
	if text := snapshot(); !strings.Contains(text, "cancellation pending upstream") || strings.Contains(text, "acknowledged upstream") {
		unblock()
		t.Fatalf("pending RPC misreported: %s", text)
	}
	unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not complete")
	}
	if text := snapshot(); !strings.Contains(text, "cancellation acknowledged upstream") {
		t.Fatal(text)
	}
	terminal := FormatDoctor(DoctorReport{ScopeKnown: true, Runs: []DoctorRun{{Status: cursor.RunStatus{TerminalStatus: "FINISHED", CancelFailed: true}}}})
	if !strings.Contains(terminal, "run: finished") {
		t.Fatal("real terminal state lost to cancellation diagnostic")
	}
}
