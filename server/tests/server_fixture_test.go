//go:build ignore

package server

import (
	"context"
	"testing"
)

// A fixture must join restored task runtimes before another Server replaces
// agent hooks. Cancellation alone does not prove the old callbacks exited.
func newManagedTestServer(t *testing.T, m *Manager, webDir, skillDir, agentDir string) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, m, webDir, skillDir, agentDir)
	t.Cleanup(func() {
		cancel()
		for _, task := range m.List() {
			s.engine.StopTask(task.ID)
		}
		s.archiveWG.Wait()
		<-s.side.done
	})
	return s
}
