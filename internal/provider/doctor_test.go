package provider

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	"github.com/tctien342/pig-cursor-sdk/gen/sdk/v1/sdkv1connect"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

type doctorPingServer struct {
	sdkv1connect.UnimplementedSdkBridgeControlServiceHandler
	calls atomic.Int32
}

func (s *doctorPingServer) Ping(context.Context, *connect.Request[sdkpb.PingRequest]) (*connect.Response[sdkpb.PingResponse], error) {
	s.calls.Add(1)
	return connect.NewResponse(&sdkpb.PingResponse{}), nil
}

func TestDoctorIsPassiveUntilExistingBridgeProbeRequested(t *testing.T) {
	e := NewEngine(Config{ConfigHome: t.TempDir()})
	report := e.doctorSnapshot("", false, nil, []string{"bash", "read"})
	if e.clientNow() != nil || report.BridgeStarted || !strings.Contains(e.probeExisting(), "skipped") {
		t.Fatal("idle doctor created a client/bridge")
	}
	service := &doctorPingServer{}
	_, handler := sdkv1connect.NewSdkBridgeControlServiceHandler(service)
	server := httptest.NewServer(handler)
	defer server.Close()
	e.client = &cursor.Client{Control: sdkv1connect.NewSdkBridgeControlServiceClient(server.Client(), server.URL)}
	report = e.doctorSnapshot("", false, nil, []string{"bash"})
	if !report.BridgeStarted || service.calls.Load() != 0 {
		t.Fatal("passive snapshot pinged the bridge")
	}
	if probe := e.probeExisting(); !strings.HasPrefix(probe, "ok:") || service.calls.Load() != 1 {
		t.Fatalf("explicit probe failed: %s", probe)
	}
}

func TestDoctorRedactsErrorsUnknownParamsAndRunIdentities(t *testing.T) {
	const secret = "sensitive-api-key-123"
	e := NewEngine(Config{ConfigHome: t.TempDir()})
	e.recordFailure(errors.New("invalid API key " + secret + " /private/project/prompt"))
	report := e.doctorSnapshot("", true, &ControlReport{ModelID: "public-model", ContextWindow: 500000, Params: []Param{{ID: "api_key", Value: secret}, {ID: "context", Value: "500k"}}}, []string{"bash"})
	report.Runs = []DoctorRun{{Status: cursor.RunStatus{ID: secret, CancelFailed: true}, Controls: &ControlReport{ModelID: "public-model", Params: []Param{{ID: "api_key", Value: secret}, {ID: "thinking", Value: "false"}}}}}
	text := FormatDoctor(report)
	for _, value := range []string{secret, "/private/project/prompt", "api_key="} {
		if strings.Contains(text, value) {
			t.Fatalf("diagnostic exposed private data %q", value)
		}
	}
	for _, value := range []string{"authentication unavailable or rejected", "upstream cancellation unconfirmed", "context=500k", "thinking=false"} {
		if !strings.Contains(text, value) {
			t.Fatalf("missing actionable diagnosis: %s", value)
		}
	}
}

func TestDoctorRecentHistoryIsBoundedAndClassified(t *testing.T) {
	e := NewEngine(Config{ConfigHome: t.TempDir()})
	for i := 0; i < 20; i++ {
		e.recordFailure(errors.New("private failure content"))
	}
	report := e.doctorSnapshot("", false, nil, nil)
	if len(report.Recent) > 6 {
		t.Fatal("unbounded failure history")
	}
	if strings.Contains(FormatDoctor(report), "private failure content") {
		t.Fatal("raw error details exposed")
	}
}
