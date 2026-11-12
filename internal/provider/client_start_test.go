package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

type clientAnswer struct {
	client *cursor.Client
	err    error
}

func clientInBackground(e *Engine, ctx context.Context) <-chan clientAnswer {
	done := make(chan clientAnswer, 1)
	go func() { client, err := e.Client(ctx, "test-key"); done <- clientAnswer{client, err} }()
	return done
}
func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle operation blocked")
	}
}
func awaitClient(t *testing.T, ch <-chan clientAnswer) clientAnswer {
	t.Helper()
	select {
	case answer := <-ch:
		return answer
	case <-time.After(2 * time.Second):
		t.Fatal("client waiter blocked")
		return clientAnswer{}
	}
}

func TestBridgeStartupCancelledWaiterDoesNotCancelOtherSession(t *testing.T) {
	e := NewEngine(Config{ConfigHome: t.TempDir()})
	defer e.Stop()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	expected := &cursor.Client{}
	e.newClient = func(ctx context.Context, _ cursor.Options) (*cursor.Client, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return expected, nil
		}
	}
	firstCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := clientInBackground(e, firstCtx)
	awaitSignal(t, entered)
	second := clientInBackground(e, context.Background())
	cancel()
	if answer := awaitClient(t, first); !errors.Is(answer.err, context.Canceled) {
		t.Fatalf("first waiter=%+v", answer)
	}
	unblock()
	if answer := awaitClient(t, second); answer.err != nil || answer.client != expected {
		t.Fatalf("another session lost shared startup: %+v", answer)
	}
	if calls.Load() != 1 || e.startErr != "" {
		t.Fatal("cancelled waiter poisoned/restarted initialization")
	}
}

func TestBridgeStartupStopAndDoctorDoNotWaitForReadiness(t *testing.T) {
	e := NewEngine(Config{ConfigHome: t.TempDir()})
	entered := make(chan struct{})
	e.newClient = func(ctx context.Context, _ cursor.Options) (*cursor.Client, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	result := clientInBackground(e, context.Background())
	awaitSignal(t, entered)
	diagnosed := make(chan struct{})
	go func() {
		report := e.doctorSnapshot("", false, nil, nil)
		if !report.BridgeStarting {
			t.Error("startup was not diagnosed")
		}
		close(diagnosed)
	}()
	awaitSignal(t, diagnosed)
	stopped := make(chan struct{})
	go func() { e.Stop(); close(stopped) }()
	awaitSignal(t, stopped)
	if answer := awaitClient(t, result); !errors.Is(answer.err, context.Canceled) {
		t.Fatalf("startup did not terminate: %+v", answer)
	}
	if e.clientNow() != nil || e.startErr != "" {
		t.Fatal("shutdown published a client or cached cancellation")
	}
}

func TestBridgeStartupLateSuccessIsDisposedExactlyOnce(t *testing.T) {
	e := NewEngine(Config{ConfigHome: t.TempDir()})
	entered, release, disposed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var closes atomic.Int32
	e.newClient = func(context.Context, cursor.Options) (*cursor.Client, error) {
		close(entered)
		<-release
		return &cursor.Client{}, nil
	}
	e.closeClient = func(*cursor.Client) { e.mu.Lock(); e.mu.Unlock(); closes.Add(1); close(disposed) }
	answer := clientInBackground(e, context.Background())
	awaitSignal(t, entered)
	e.Stop()
	unblock()
	awaitSignal(t, disposed)
	if got := awaitClient(t, answer); !errors.Is(got.err, context.Canceled) || got.client != nil {
		t.Fatalf("late client escaped shutdown: %+v", got)
	}
	e.Stop()
	if closes.Load() != 1 || e.clientNow() != nil {
		t.Fatal("late startup leaked or was disposed more than once")
	}
}

func TestBridgeStartupFailureDoesNotAutomaticallyRetryAndDeadlineIsRetryable(t *testing.T) {
	e := NewEngine(Config{ConfigHome: t.TempDir()})
	defer e.Stop()
	var calls atomic.Int32
	expected := &cursor.Client{}
	e.newClient = func(context.Context, cursor.Options) (*cursor.Client, error) {
		if calls.Add(1) == 1 {
			return nil, context.DeadlineExceeded
		}
		return expected, nil
	}
	if _, err := e.Client(context.Background(), "test-key"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("startup failure silently retried")
	}
	client, err := e.Client(context.Background(), "test-key")
	if err != nil || client != expected || calls.Load() != 2 {
		t.Fatal("explicit later attempt was poisoned by old timeout")
	}
}
