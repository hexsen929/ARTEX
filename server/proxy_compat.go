package server

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// resolveTaskUpstreamChecked distinguishes a task-selected tunnel from global
// fallback in the same ledger snapshot. Errors never authorize fallback or a
// global pause of a task whose actual route is unknown.
func (m *Manager) resolveTaskUpstreamChecked(ctx context.Context, taskID int64) (upstream string, global bool, err error) {
	if m.taskProxyInitErr != nil {
		return "", false, fmt.Errorf("task proxy configuration invalid: %w", m.taskProxyInitErr)
	}
	if m.taskProxy == nil || taskID <= 0 {
		return m.GlobalProxy(), true, nil
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	recs, err := m.taskProxy.store.ListByTask(ctx, taskID)
	if err != nil {
		return "", false, err
	}
	selected, err := pickUpstreamChecked(recs, "")
	if err != nil {
		return "", false, err
	}
	if selected != "" {
		return selected, false, nil
	}
	return m.GlobalProxy(), true, nil
}

func (s *Server) taskUsesGlobalProxy(taskID string) bool {
	id, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || id <= 0 || s == nil || s.m == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, global, err := s.m.resolveTaskUpstreamChecked(ctx, id)
	return err == nil && global
}

// Existing task-local MITMs hold their own upstream snapshot. Pool changes must
// refresh those snapshots too, while their tunnel-first selection stays intact.
func (m *Manager) refreshTaskProxyRoutes() {
	if m.taskProxy == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m.taskProxy.RefreshAll(ctx)
}
