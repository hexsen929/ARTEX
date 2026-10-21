package server

import (
	"context"
	"testing"
)

func newTestServer(t *testing.T, parent context.Context, manager *Manager, skillDir, dataDir, keyDir string) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	s := New(ctx, manager, skillDir, dataDir, keyDir)
	t.Cleanup(func() {
		cancel()
		for _, task := range manager.List() {
			s.engine.StopTask(task.ID)
		}
		s.archiveWG.Wait()
		if s.side != nil && s.side.done != nil {
			<-s.side.done
		}
	})
	return s
}
