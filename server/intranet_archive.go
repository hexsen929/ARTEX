package server

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/db"
)

// checkTaskIntranetArchive refuses to turn live remote resources into detached
// archive data. It never kills a session shared with another task. Historical
// dead/stopped records are safe to serialize and restore as inactive records.
func (s *Server) checkTaskIntranetArchive(ctx context.Context, taskID int64) error {
	if taskID <= 0 {
		return nil
	}
	if s.sessStore != nil {
		recs, err := s.sessStore.ListForTask(ctx, taskID, db.SessionAlive)
		if err != nil {
			return fmt.Errorf("读取任务会话资源失败: %w", err)
		}
		if len(recs) > 0 {
			return fmt.Errorf("任务仍有 %d 个存活会话(含显式移交绑定)，请先回收或解除绑定再归档；不会自动终止共享会话", len(recs))
		}
	}
	if s.m != nil && s.m.pg != nil {
		var activeChecks bool
		if err := s.m.pg.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM finding_checks c JOIN findings f ON f.id=c.finding_id WHERE f.task_id=$1 AND c.status IN ('pending','running'))`, taskID).Scan(&activeChecks); err != nil {
			return fmt.Errorf("读取任务复测资源失败: %w", err)
		}
		if activeChecks {
			return fmt.Errorf("任务仍有正在等待或运行的独立漏洞复测，请先停止或完成复测后归档")
		}
		recs, err := db.NewTunnelStore(s.m.pg).ListByTask(ctx, taskID)
		if err != nil {
			return fmt.Errorf("读取任务隧道资源失败: %w", err)
		}
		for _, rec := range recs {
			if rec.State != db.TunnelStopped {
				return fmt.Errorf("任务仍有未回收隧道 #%d (%s)，请先 teardown 后归档", rec.ID, rec.State)
			}
		}
	}
	if s.stage != nil {
		for _, e := range s.stage.List() {
			if e.TaskID == strconv.FormatInt(taskID, 10) && e.ExpiresAt.After(time.Now()) {
				return fmt.Errorf("任务仍有未过期的受管投递资源，请先回收后归档")
			}
		}
	}
	if s.reverse != nil {
		s.reverse.mu.Lock()
		active := s.reverse.running && s.reverse.taskID == taskID
		s.reverse.mu.Unlock()
		if active {
			return fmt.Errorf("任务仍占用反向连接监听，请先停止监听后归档")
		}
	}
	return nil
}
