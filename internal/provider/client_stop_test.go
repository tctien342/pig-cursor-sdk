package provider

import (
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBridgeStartupConcurrentStopWaitsForSingleCleanup(t *testing.T) {
	e := NewEngine(Config{ConfigHome: t.TempDir()})
	e.client = &cursor.Client{}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var closed atomic.Int32
	e.closeClient = func(*cursor.Client) { close(entered); <-release; closed.Add(1) }
	first := make(chan struct{})
	go func() { e.Stop(); close(first) }()
	awaitSignal(t, entered)
	second := make(chan struct{})
	go func() { e.Stop(); close(second) }()
	select {
	case <-second:
		t.Fatal("concurrent Stop returned before shared cleanup completed")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	awaitSignal(t, first)
	awaitSignal(t, second)
	if closed.Load() != 1 {
		t.Fatal("bridge was not disposed exactly once")
	}
}
