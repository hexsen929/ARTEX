package server

import (
	"context"
	"fmt"
	"strconv"
	"sync"
)

// Resource APIs participate in the same archive/delete barrier as Agent runs.
// An authenticated manual operation must not create or revive resources after
// the archiver's final resource check.
func (s *Server) beginIntranetTaskOperation(taskID int64) (func(), error) {
	if taskID <= 0 || s.engine == nil {
		return func() {}, nil
	}
	id := strconv.FormatInt(taskID, 10)
	if s.m == nil {
		return nil, fmt.Errorf("任务运行时未就绪")
	}
	if _, ok := s.m.Task(id); !ok {
		return nil, fmt.Errorf("任务 %s 已删除或归档", id)
	}
	if !s.engine.beginTaskOperation(id) {
		return nil, fmt.Errorf("任务 %s 正在删除或归档，暂不接受资源操作", id)
	}
	var once sync.Once
	return func() { once.Do(func() { s.engine.decInflight(id) }) }, nil
}

// A session can be explicitly shared by a handoff. Fence all owning/bound tasks,
// not just its creating task, so a manual probe cannot revive it during a child
// task's archive snapshot.
func (s *Server) beginIntranetSessionOperation(ctx context.Context, sessionID int64) (func(), error) {
	if s.engine == nil {
		return func() {}, nil
	}
	if s.m == nil || s.m.pg == nil {
		return nil, fmt.Errorf("会话任务运行时未就绪")
	}
	rows, err := s.m.pg.QueryContext(ctx, `SELECT DISTINCT v.task_id FROM (
SELECT created_by_task AS task_id FROM sessions WHERE id=$1
UNION SELECT task_id FROM task_session_bindings WHERE session_id=$1
) v JOIN tasks t ON t.id=v.task_id WHERE t.deleted_at IS NULL ORDER BY v.task_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("读取会话关联任务失败: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var releases []func()
	var once sync.Once
	release := func() {
		once.Do(func() {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
		})
	}
	for _, id := range ids {
		done, err := s.beginIntranetTaskOperation(id)
		if err != nil {
			release()
			return nil, err
		}
		releases = append(releases, done)
	}
	return release, nil
}
