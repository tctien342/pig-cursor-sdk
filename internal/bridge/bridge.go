// Package bridge owns the cursor-sdk-bridge child process: locate, spawn,
// ready-line handshake, bearer token, unary/streaming Connect calls, shutdown.
//
// JSON mode (content-type application/json) is used for unary RPCs and the
// Connect streaming envelope (application/connect+json) for server streams, so
// the contract stays readable while the protobuf codegen lands.
package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Ready struct {
	SchemaVersion int    `json:"schemaVersion"`
	ServerVersion string `json:"serverVersion"`
	PID           int    `json:"pid"`
	Transport     string `json:"transport"`
	Protocol      string `json:"protocol"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	URL           string `json:"url"`
	AuthTokenFile string `json:"authTokenFile"`
	WorkspaceRef  string `json:"workspaceRef"`
	StateRoot     string `json:"stateRoot"`

	Token string `json:"-"`
}

type Handle struct {
	Ready
	Bin    string
	cmd    *exec.Cmd
	client *http.Client
	stderr []string // bounded tail of bridge diagnostics
}

// Spawn starts the bridge for workspace and waits for the ready line.
func Spawn(ctx context.Context, bin, workspace string, extraEnv []string, toolCallbackURL, toolCallbackToken string) (*Handle, error) {
	if bin == "" {
		return nil, errors.New("bridge binary not found")
	}
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("bridge binary: %w", err)
	}
	args := []string{"--workspace", workspace}
	if toolCallbackURL != "" {
		args = append(args, "--tool-callback-url", toolCallbackURL, "--tool-callback-auth-token", toolCallbackToken)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Env = append(cmd.Env, "CURSOR_SDK_CLIENT_LANGUAGE=go")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	h := &Handle{Bin: bin, cmd: cmd, client: &http.Client{Timeout: 2 * time.Minute}}
	// Recorded before anything else can go wrong: if this process is killed, a
	// later one has to be able to find the bridge this one left behind.
	register(StateDir(), bridgeRecord{
		BridgePID:  cmd.Process.Pid,
		BridgeBin:  bin,
		OwnerPID:   os.Getpid(),
		OwnerStart: lookProcess(os.Getpid()).Start,
		Workspace:  workspace,
		StartedAt:  time.Now().Unix(),
	}, nil)
	readyCh := make(chan Ready, 1)
	errCh := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if len(h.stderr) > 200 {
				h.stderr = h.stderr[1:]
			}
			h.stderr = append(h.stderr, line)
			if rest, ok := strings.CutPrefix(line, "cursor-sdk-bridge ready "); ok {
				var r Ready
				if err := json.Unmarshal([]byte(rest), &r); err != nil {
					errCh <- fmt.Errorf("ready line JSON: %w", err)
					return
				}
				readyCh <- r
				continue // keep draining stderr forever: a full pipe blocks the bridge
			}
		}
	}()

	var ready Ready
	select {
	case ready = <-readyCh:
	case err := <-errCh:
		_ = cmd.Process.Kill()
		return nil, err
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("bridge did not become ready in 30s; stderr tail:\n%s", strings.Join(h.stderr, "\n"))
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return nil, ctx.Err()
	}

	if ready.SchemaVersion != 1 {
		h.Close()
		return nil, fmt.Errorf("unsupported ready schemaVersion %d", ready.SchemaVersion)
	}
	if ready.Transport != "tcp" || ready.Protocol != "connect" {
		h.Close()
		return nil, fmt.Errorf("unexpected transport/protocol %q/%q", ready.Transport, ready.Protocol)
	}
	if ready.URL == "" {
		ready.URL = fmt.Sprintf("http://%s:%d", ready.Host, ready.Port)
	}
	raw, err := os.ReadFile(ready.AuthTokenFile)
	if err != nil {
		h.Close()
		return nil, fmt.Errorf("auth token file: %w", err)
	}
	ready.Token = strings.TrimSpace(string(raw))
	if ready.Token == "" {
		h.Close()
		return nil, errors.New("empty bridge auth token")
	}
	h.Ready = ready
	return h, nil
}

// StderrTail returns captured bridge diagnostics for error messages. It never
// includes the ready line (older bridges inlined the token there).
func (h *Handle) StderrTail() string {
	var kept []string
	for _, l := range h.stderr {
		if strings.HasPrefix(l, "cursor-sdk-bridge ready ") {
			continue
		}
		kept = append(kept, l)
	}
	if len(kept) > 20 {
		kept = kept[len(kept)-20:]
	}
	return strings.Join(kept, "\n")
}

// Call performs one unary RPC with a JSON body and returns the decoded reply.
// status is non-zero when the RPC failed; the error body is returned too.
func (h *Handle) Call(ctx context.Context, service, method string, body any) (json.RawMessage, int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL+"/"+service+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+h.Token)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s/%s: %w", service, method, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return out, resp.StatusCode, fmt.Errorf("%s/%s: http %d: %s", service, method, resp.StatusCode, string(out))
	}
	return out, resp.StatusCode, nil
}

// Frame is one message of a Connect server stream.
type Frame struct {
	Flags byte
	JSON  json.RawMessage
}

// Stream performs a server-streaming RPC: reqBody is sent as one framed
// message and frames are delivered to fn until the end-of-stream frame.
func (h *Handle) Stream(ctx context.Context, service, method string, reqBody any, fn func(Frame) error) error {
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	framed := make([]byte, 5+len(raw))
	framed[0] = 0
	binary.BigEndian.PutUint32(framed[1:5], uint32(len(raw)))
	copy(framed[5:], raw)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL+"/"+service+"/"+method, bytes.NewReader(framed))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/connect+json")
	req.Header.Set("accept", "application/connect+json")
	req.Header.Set("authorization", "Bearer "+h.Token)
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", service, method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return fmt.Errorf("%s/%s: http %d: %s", service, method, resp.StatusCode, string(body))
	}
	var head [5]byte
	for {
		if _, err := io.ReadFull(resp.Body, head[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		n := binary.BigEndian.Uint32(head[1:5])
		if n > 128*1024*1024 {
			return fmt.Errorf("%s/%s: frame too large (%d bytes)", service, method, n)
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(resp.Body, payload); err != nil {
			return err
		}
		if head[0]&0x02 != 0 { // end of stream: {"error":{...}} only on failure
			if len(payload) > 0 && !bytes.Contains(payload, []byte(`"error"`)) {
				continue
			}
			var end struct {
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(payload, &end); err != nil {
				return fmt.Errorf("end stream frame: %w: %s", err, payload)
			}
			if end.Error != nil {
				return fmt.Errorf("stream ended: %s: %s", end.Error.Code, end.Error.Message)
			}
			return nil
		}
		if err := fn(Frame{Flags: head[0], JSON: payload}); err != nil {
			return err
		}
	}
}

// Close shuts the bridge down gracefully, then kills it.
func (h *Handle) Close() {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _ = h.Call(ctx, "sdk.v1.SdkBridgeControlService", "Shutdown", map[string]any{"graceSeconds": 1})

	forgetBridge(StateDir(), h.cmd.Process.Pid)

	done := make(chan struct{})
	go func() { _ = h.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
}

// StateDir is this extension's writable state directory.
func StateDir() string { return filepath.Dir(filepath.Dir(InstallPath())) }

// InstallPath is where this extension keeps its own bridge binary, separate
// from any copy another integration may have installed.
func InstallPath() string {
	home := strings.TrimSpace(os.Getenv("PIG_HOME"))
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".pig")
		} else {
			home = ".pig"
		}
	}
	return filepath.Join(home, "agent", "pig-cursor-sdk", "bin", "cursor-sdk-bridge")
}

// FindBinary resolves the bridge executable: env override, then this
// extension's install location, then a directory next to the running binary,
// then a development checkout, then PATH.
func FindBinary() string {
	if v := strings.TrimSpace(os.Getenv("CURSOR_SDK_BRIDGE_BIN")); v != "" {
		return v
	}
	exe, _ := os.Executable()
	candidates := []string{
		InstallPath(),
		filepath.Join(filepath.Dir(exe), "cursor-sdk-bridge"),
		"upstream/bridge/bin/cursor-sdk-bridge",
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath("cursor-sdk-bridge"); err == nil {
		return p
	}
	return ""
}
