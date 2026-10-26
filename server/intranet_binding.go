package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/db"
)

// intranetUnbindSession revokes one explicit handoff grant. It never removes the
// shared session, its creating task's ownership or another task's grant. The
// route is mounted behind requireAuth with the other task management APIs.
// DELETE /api/tasks/{task}/sessions/{session}/binding
func (s *Server) intranetUnbindSession(w http.ResponseWriter, r *http.Request) {
	if s.m == nil || s.m.pg == nil || s.sessStore == nil {
		writeErr(w, http.StatusServiceUnavailable, "会话任务子系统未就绪")
		return
	}
	taskID, err := pathID(r, "task")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sessionID, err := pathID(r, "session")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := s.m.Task(strconv.FormatInt(taskID, 10)); !ok {
		writeErr(w, http.StatusNotFound, "任务不存在或已归档")
		return
	}
	done, err := s.beginIntranetTaskOperation(taskID)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	defer done()
	record, err := s.sessStore.Get(r.Context(), sessionID)
	if err != nil {
		s.auditIntranet(r, "unbind task=%d session=%d 读取失败", taskID, sessionID)
		writeErr(w, http.StatusInternalServerError, "会话读取失败")
		return
	}
	if record == nil {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	if record.CreatedByTask == taskID {
		s.auditIntranet(r, "unbind task=%d session=%d 拒绝移除创建者所有权", taskID, sessionID)
		writeErr(w, http.StatusConflict, "本任务是会话所有者，解除关联只适用于显式移交的共享会话；不会删除或停止该会话")
		return
	}
	unbound, err := s.sessStore.UnbindTask(r.Context(), taskID, sessionID)
	switch {
	case errors.Is(err, db.ErrSessionTaskOwner):
		writeErr(w, http.StatusConflict, "本任务是会话所有者，不能解除自身所有权")
		return
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "任务或会话不存在")
		return
	case err != nil:
		s.auditIntranet(r, "unbind task=%d session=%d 解绑失败", taskID, sessionID)
		writeErr(w, http.StatusInternalServerError, "解除会话关联失败")
		return
	}
	s.auditIntranet(r, "unbind task=%d session=%d unbound=%v(会话和其他任务授权保留)", taskID, sessionID, unbound)
	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": strconv.FormatInt(taskID, 10), "session_id": strconv.FormatInt(sessionID, 10), "unbound": unbound,
	})
}
