package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Reproduce the opposing lock order without starting a proxy or requiring PG:
// refresh owns taskProxy.mu and may need Manager.GlobalProxy's m.mu.RLock;
// Close must release m.mu before waiting for taskProxy.mu.
func TestManagerCloseDoesNotHoldGlobalLockDuringTaskProxyTeardown(t *testing.T) {
	proxies := &taskProxyManager{proxies: map[int64]*taskProxyInst{}}
	manager := &Manager{taskProxy: proxies, trafficOn: true}
	proxies.mu.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(proxies.mu.Unlock) }
	t.Cleanup(release)

	done := make(chan error, 1)
	go func() { done <- manager.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if manager.mu.TryRLock() {
			closing := manager.closing
			manager.mu.RUnlock()
			if closing {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("manager Close retained its global lock while waiting for task-proxy teardown")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("Close returned before the blocked task-proxy teardown completed")
	default:
	}

	// A request arriving during shutdown must fail clearly, not try to create a
	// fresh listener, return a different egress route or wait for the proxy lock.
	request := make(chan error, 1)
	go func() {
		_, _, err := manager.TaskProxyForTaskChecked(context.Background(), 42)
		request <- err
	}()
	select {
	case err := <-request:
		if err == nil || !strings.Contains(err.Error(), "closing") {
			t.Fatalf("request during manager shutdown must report closing, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("new task-proxy request blocked behind shutdown")
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("closing an empty manager: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("manager Close remained blocked after task-proxy lock was released")
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("repeated manager Close: %v", err)
	}
	if manager.TrafficEnabled() {
		t.Fatal("closing manager still advertises traffic capture as enabled")
	}
}
