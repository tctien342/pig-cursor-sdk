// Package cursor is the Cursor sdk.v1 adapter: a managed cursor-sdk-bridge
// process, generated Connect clients, the adapter-side custom-tool callback
// server, and the run/event model the pig provider builds on.
package cursor

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	"github.com/tctien342/pig-cursor-sdk/gen/sdk/v1/sdkv1connect"
	"github.com/tctien342/pig-cursor-sdk/internal/bridge"
)

// Client owns one bridge process and the Connect clients attached to it.
type Client struct {
	bridge *bridge.Handle

	Agent   sdkv1connect.SdkAgentServiceClient
	Cursor  sdkv1connect.SdkCursorServiceClient
	Control sdkv1connect.SdkBridgeControlServiceClient

	callbacks *callbackServer
	log       *slog.Logger

	// agents routes a bridge tool callback to the agent that owns the run.
	agents sync.Map // agentID -> *Agent

	mu      sync.Mutex
	version *sdkpb.GetVersionResponse
}

// Options configures a Client.
type Options struct {
	BridgeBin string
	Workspace string
	APIKey    string
	Logger    *slog.Logger
	// ToolCallbackHost defaults to 127.0.0.1; the bridge only calls loopback.
	ToolCallbackHost string
}

// New starts the bridge and dials it. The callback server is listening before
// the bridge starts, so a tool callback can never arrive at a closed port.
func New(ctx context.Context, o Options) (*Client, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	cb := newCallbackServer(o.Logger)
	if err := cb.listen(o.ToolCallbackHost); err != nil {
		return nil, fmt.Errorf("tool callback server: %w", err)
	}
	h, err := bridge.Spawn(ctx, o.BridgeBin, o.Workspace,
		[]string{"CURSOR_API_KEY=" + o.APIKey}, cb.url(), cb.token)
	if err != nil {
		cb.close()
		return nil, err
	}

	httpc := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	auth := bearerAuth(h.Token)
	c := &Client{
		bridge:    h,
		Agent:     sdkv1connect.NewSdkAgentServiceClient(httpc, h.URL, connect.WithInterceptors(auth)),
		Cursor:    sdkv1connect.NewSdkCursorServiceClient(httpc, h.URL, connect.WithInterceptors(auth)),
		Control:   sdkv1connect.NewSdkBridgeControlServiceClient(httpc, h.URL, connect.WithInterceptors(auth)),
		callbacks: cb,
		log:       o.Logger,
	}
	// One dispatcher for the whole process: the bridge addresses callbacks by
	// agent id, so any number of sessions can share this bridge client.
	cb.register(func(ctx context.Context, req ToolCallRequest) (map[string]any, error) {
		v, ok := c.agents.Load(req.AgentID)
		if !ok {
			return nil, fmt.Errorf("cursor: no agent %q is registered for tool callbacks", req.AgentID)
		}
		return v.(*Agent).handleToolCallback(ctx, req)
	})
	return c, nil
}

func (c *Client) bindAgent(a *Agent)    { c.agents.Store(a.id, a) }
func (c *Client) unbindAgent(id string) { c.agents.Delete(id) }

// bearerAuth adds the bridge token to unary AND streaming calls.
// authInterceptor attaches the bridge bearer token to every call, unary and
// streaming. A connect.UnaryInterceptorFunc is not enough: it only wraps unary
// calls, so Send/ObserveRun would reach the bridge with no Authorization header
// and come back "Unauthorized".
type authInterceptor struct{ token string }

func (a authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("authorization", "Bearer "+a.token)
		return next(ctx, req)
	}
}

// WrapStreamingClient sets the header before the stream's first message, which
// is when connect-go sends the request.
func (a authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("authorization", "Bearer "+a.token)
		return conn
	}
}

func (a authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func bearerAuth(token string) connect.Interceptor { return authInterceptor{token: token} }

// FindBinary resolves the cursor-sdk-bridge executable (env override, bundled
// download, then PATH).
func FindBinary() string { return bridge.FindBinary() }

// InstallPath is where the bridge binary is expected when CURSOR_SDK_BRIDGE_BIN
// is unset; callers show it in setup instructions.
func InstallPath() string { return bridge.InstallPath() }

// StateDir is this extension's writable state directory.
func StateDir() string { return bridge.StateDir() }

// BinPath reports the resolved bridge binary path.
func (c *Client) BinPath() string { return c.bridge.Bin }

// PID reports the bridge process id.
func (c *Client) PID() int { return c.bridge.Ready.PID }

// Version returns the cached GetVersion response.
func (c *Client) Version(ctx context.Context) (*sdkpb.GetVersionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version != nil {
		return c.version, nil
	}
	res, err := c.Control.GetVersion(ctx, connect.NewRequest(&sdkpb.GetVersionRequest{}))
	if err != nil {
		return nil, err
	}
	c.version = res.Msg
	return c.version, nil
}

// ProtocolVersion validates the bridge speaks sdk.v1.
func (c *Client) ProtocolVersion(ctx context.Context) (string, error) {
	v, err := c.Version(ctx)
	if err != nil {
		return "", err
	}
	if v.GetProtocolVersion() != "sdk.v1" {
		return v.GetProtocolVersion(), fmt.Errorf("bridge protocol %q is not sdk.v1", v.GetProtocolVersion())
	}
	return v.GetProtocolVersion(), nil
}

// Ping checks bridge liveness.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Control.Ping(ctx, connect.NewRequest(&sdkpb.PingRequest{}))
	return err
}

// Me resolves the account behind the API key.
func (c *Client) Me(ctx context.Context, apiKey string) (*sdkpb.SdkUser, error) {
	res, err := c.Cursor.Me(ctx, connect.NewRequest(&sdkpb.MeRequest{
		Options: &sdkpb.CursorRequestOptions{ApiKey: apiKey},
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetUser(), nil
}

// Models lists the account's model catalog.
func (c *Client) Models(ctx context.Context, apiKey string) ([]*sdkpb.SdkModel, error) {
	res, err := c.Cursor.ListModels(ctx, connect.NewRequest(&sdkpb.ListModelsRequest{
		Options: &sdkpb.CursorRequestOptions{ApiKey: apiKey},
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetItems(), nil
}

// Tools returns the callback registration surface.
func (c *Client) Tools() *ToolCallbacks { return &ToolCallbacks{cb: c.callbacks} }

// Close shuts the bridge down and stops the callback server.
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.bridge.Close()
	c.callbacks.close()
}

// ---- custom tool callbacks -------------------------------------------------

// ToolCallRequest is one host-owned tool execution the bridge is waiting on.
type ToolCallRequest struct {
	ToolName   string
	Args       map[string]any
	ToolCallID string
	AgentID    string

	// ArrivedAt is when the callback reached us; used for latency accounting.
	ArrivedAt time.Time
}

// ToolCallbacks is the adapter-side SdkCustomToolCallbackService surface.
type ToolCallbacks struct{ cb *callbackServer }

// Register installs the handler a run uses for its custom tools. A run must
// register before Send so an early callback cannot be dropped.
func (t *ToolCallbacks) Register(handler func(context.Context, ToolCallRequest) (map[string]any, error)) (undo func()) {
	return t.cb.register(handler)
}

type callbackServer struct {
	token   string
	urlStr  string
	ln      net.Listener
	srv     *http.Server
	log     *slog.Logger
	mu      sync.RWMutex
	handler *handlerEntry
}

// handlerEntry wraps the callback so registration can be undone by identity.
type handlerEntry struct {
	fn func(context.Context, ToolCallRequest) (map[string]any, error)
}

func newCallbackServer(log *slog.Logger) *callbackServer {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic("callback token: " + err.Error())
	}
	return &callbackServer{token: base64.RawURLEncoding.EncodeToString(buf), log: log}
}

func (s *callbackServer) listen(host string) error {
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		return err
	}
	s.ln = ln
	s.urlStr = "http://" + ln.Addr().String()
	mux := http.NewServeMux()
	path, handler := sdkv1connect.NewSdkCustomToolCallbackServiceHandler(&callbackImpl{s: s})
	mux.Handle(path, s.requireToken(handler))
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("tool callback server", "err", err)
		}
	}()
	return nil
}

func (s *callbackServer) url() string { return s.urlStr }

func (s *callbackServer) close() {
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	}
}

func (s *callbackServer) register(h func(context.Context, ToolCallRequest) (map[string]any, error)) func() {
	entry := &handlerEntry{fn: h}
	s.mu.Lock()
	s.handler = entry
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if s.handler == entry {
			s.handler = nil
		}
		s.mu.Unlock()
	}
}

func (s *callbackServer) get() *handlerEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.handler
}

// requireToken validates the bearer token the bridge presents, exactly as the
// bridge validates ours.
func (s *callbackServer) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"invalid tool callback token"}`))
	})
}

type callbackImpl struct{ s *callbackServer }

func (c *callbackImpl) CallCustomTool(ctx context.Context, req *connect.Request[sdkpb.CallCustomToolRequest]) (*connect.Response[sdkpb.CallCustomToolResponse], error) {
	h := c.s.get()
	if h == nil || h.fn == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no host tool handler is registered"))
	}
	msg := req.Msg
	args := msg.GetArgs().AsMap()
	if args == nil {
		args = map[string]any{}
	}
	out, err := h.fn(ctx, ToolCallRequest{
		ToolName:   msg.GetToolName(),
		Args:       args,
		ToolCallID: msg.GetToolCallId(),
		AgentID:    msg.GetAgentId(),
		ArrivedAt:  time.Now(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	st, serr := structFromMap(out)
	if serr != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, serr)
	}
	return connect.NewResponse(&sdkpb.CallCustomToolResponse{Result: st}), nil
}

var _ sdkv1connect.SdkCustomToolCallbackServiceHandler = (*callbackImpl)(nil)
