package provider

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tctien342/pig-cursor-sdk/gen/sdk/v1/sdkv1connect"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func TestCancellationPrecedesAlreadyDrainedReadSide(t *testing.T) {
	for _, doneOnly := range []bool{false, true} {
		name := "drain-error"
		if doneOnly {
			name = "done-without-terminal-result"
		}
		t.Run(name, func(t *testing.T) {
			service := &scopedCancelServer{cancelled: make(chan string, 2), failDrain: !doneOnly, doneOnly: doneOnly}
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
			wait, cancelWait := context.WithTimeout(context.Background(), time.Second)
			defer cancelWait()
			for {
				if _, ok := run.Next(wait); !ok {
					break
				}
			}
			if run.RunID() == "" || run.Terminal() != nil || (doneOnly && !run.Finished()) {
				t.Fatalf("fixture did not close an identified read side without a terminal result: status=%+v error=%v", run.Status(), run.DrainErr())
			}
			if (run.DrainErr() == nil) != doneOnly {
				t.Fatal("fixture did not produce expected read outcome")
			}
			caller, cancel := context.WithCancel(context.Background())
			cancel()
			if !cancelInterruptedTurn(caller, run) || !run.Status().CancelConfirmed {
				t.Fatal("closed read side suppressed upstream cancellation")
			}
			select {
			case id := <-service.cancelled:
				if id != run.RunID() {
					t.Fatal("cancelled a different run")
				}
			default:
				t.Fatal("no cancellation RPC")
			}
		})
	}
}
