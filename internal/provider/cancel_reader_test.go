package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/tctien342/pig-cursor-sdk/gen/sdk/v1/sdkv1connect"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

type cancelReadTransport struct {
	base    http.RoundTripper
	cancel  chan context.CancelFunc
	wrapped bool
}

func (t *cancelReadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, "/Send") {
		return t.base.RoundTrip(req)
	}
	ctx, cancel := context.WithCancel(req.Context())
	response, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	if t.wrapped {
		response.Body = wrappedCancelBody{response.Body}
	}
	t.cancel <- cancel
	return response, nil
}

type wrappedCancelBody struct{ io.ReadCloser }

func (b wrappedCancelBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if err != nil {
		return n, fmt.Errorf("wrapped reader: %w", err)
	}
	return n, nil
}

func TestCancellationReadSideStopsOwnedRunWithLiveCaller(t *testing.T) {
	for _, kind := range []string{"raw", "wrapped", "terminal-control"} {
		t.Run(kind, func(t *testing.T) {
			terminal := kind == "terminal-control"
			service := &scopedCancelServer{cancelled: make(chan string, 2), terminalResult: terminal}
			_, handler := sdkv1connect.NewSdkAgentServiceHandler(service)
			server := httptest.NewServer(handler)
			defer server.Close()
			base := http.DefaultTransport.(*http.Transport).Clone()
			defer base.CloseIdleConnections()
			transport := &cancelReadTransport{base: base, cancel: make(chan context.CancelFunc, 1), wrapped: kind == "wrapped"}
			client := &cursor.Client{Agent: sdkv1connect.NewSdkAgentServiceClient(&http.Client{Transport: transport}, server.URL)}
			agent, err := client.CreateAgent(context.Background(), cursor.AgentSpec{Model: "test"})
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Close(context.Background())
			run, err := agent.Send(context.Background(), "single-send", nil)
			if err != nil {
				t.Fatal(err)
			}
			stopRead := <-transport.cancel
			defer stopRead()
			wait, cancelWait := context.WithTimeout(context.Background(), time.Second)
			defer cancelWait()
			if _, ok := run.Next(wait); !ok {
				t.Fatal("missing run identity")
			}
			if !terminal {
				stopRead()
			}
			for {
				if _, ok := run.Next(wait); !ok {
					break
				}
			}
			if !terminal && !cursor.IsCancellation(run.DrainErr()) {
				t.Fatalf("actual reader cancellation was discarded: %v", run.DrainErr())
			}
			caller, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			if terminal {
				cancelCaller()
			}
			session := &Session{turnContext: caller}
			engine := NewEngine(Config{ConfigHome: t.TempDir()})
			stream := sdk.CreateAssistantMessageEventStream()
			engine.consumeWithUI(sdk.Context{}, stream, map[string]any{"id": "test"}, &ModelEntry{ID: "test"}, session, run, false, nil, false)
			if stream.Result()["stopReason"] != "aborted" {
				t.Fatal("consume did not preserve aborted outcome")
			}
			if !terminal && (caller.Err() != nil || !run.Status().CancelConfirmed) {
				t.Fatalf("live caller/read cancellation did not reach upstream: %+v", run.Status())
			}
			if terminal {
				select {
				case <-service.cancelled:
					t.Fatal("real terminal result was cancelled again")
				default:
				}
			} else {
				select {
				case id := <-service.cancelled:
					if id != run.RunID() {
						t.Fatal("wrong run cancelled")
					}
				default:
					t.Fatal("no upstream cancellation")
				}
			}
			if service.sends.Load() != 1 {
				t.Fatal("prompt was resent")
			}
		})
	}
}
