package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func (s *Server) pgListProfiles(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	ps, err := pg.ListProfiles()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"profiles": llmProfileDTOs(ps)})
}

func (s *Server) pgSaveProfile(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	// APIKey has json:"-" on db.LLMProfile so it is never leaked to the UI on
	// read; accept it here via a sibling field for create/update. Streaming is a
	// *bool shadowing the embedded json:"streaming": an absent field must default
	// to streaming (true), which a plain bool zero value (false = non-streaming)
	// would get wrong — legacy/partial clients that never send it stay streaming.
	var body struct {
		db.LLMProfile
		APIKey    string `json:"api_key"`
		Streaming *bool  `json:"streaming"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p := body.LLMProfile
	p.APIKey = body.APIKey
	p.Streaming = body.Streaming == nil || *body.Streaming
	// 输出上限:负数无意义,归零(= 不发送该字段)。字段名开关只有 Chat Completions
	// 用得上——anthropic 与 openai-responses 各自定死了字段名,存下来只会误导后续读者,
	// 故非 openai 格式一律清空。未知取值同样清空,避免把 DB CHECK 的报错甩给用户。
	if p.MaxTokens < 0 {
		p.MaxTokens = 0
	}
	if p.Format != "openai" || p.MaxTokensField != llm.MaxTokensFieldCompletion {
		p.MaxTokensField = ""
	}
	id, err := pg.SaveProfile(&p)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Editing a profile rebuilds any task pinned to it on its next round. Reapply
	// the active profile too: the global fallback and explicit task chains must
	// repopulate the same provider-cache entry and therefore share one limiter.
	s.invalidateProfileAgents()
	// Hot-apply. Not just when the ACTIVE profile changed: with failover on, any
	// profile's key/model/priority/pool_exclude edit reshapes the chain, so the
	// engine's provider has to be rebuilt either way.
	s.reapplyActiveProfile()
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) pgDeleteProfile(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, _ := pathInt(r, "id")
	if err := pg.DeleteProfileContext(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, db.ErrActiveLLMProfileDelete):
			writeErr(w, 409, "当前激活的 LLM 配置不能删除，请先激活其他配置")
		case errors.Is(err, db.ErrLLMProfileReferencesChanged):
			writeErr(w, 409, "LLM 配置正在被任务或会话修改，请重试")
		case errors.Is(err, context.DeadlineExceeded):
			writeErr(w, 409, "等待 LLM 配置引用释放超时，请重试")
		case errors.Is(err, db.ErrLLMProfileNotFound):
			writeErr(w, 404, err.Error())
		default:
			writeErr(w, 500, err.Error())
		}
		return
	}
	s.invalidateProfileAgents() // drop cached agents for the removed profile
	s.llmHealth.Reset(id)       // its breaker state is meaningless now (row is FK-cascaded away)
	s.restoreTasksAfterProfileDelete(pg)
	// Cache invalidation also removed the active profile's shared provider entry.
	// Reapply after task-state sync so the wake-up observes the post-delete chain.
	s.reapplyActiveProfile()
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (s *Server) restoreTasksAfterProfileDelete(pg *db.DB) {
	for _, task := range s.m.List() {
		if taskID, err := strconv.ParseInt(task.ID, 10, 64); err == nil {
			if pt, err := pg.GetTask(taskID); err == nil && pt != nil {
				s.syncTaskLLMState(pt)
				// Deleting the active entry may advance the task to a ready successor,
				// or clear the explicit chain and restore its Agent/global fallback.
				// Resume only intents that were blocked by the exhausted chain; a
				// paused task remains paused and terminal tasks remain immutable.
				if !isTerminalStatus(task.lifecycleSnapshot().Status) && s.taskRuntimeAvailable(task, "planner", "worker") {
					if _, reopenErr := task.Store.ReopenIntentsByBlockedReason(db.IntentBlockedLLMQuota); reopenErr != nil {
						log.Printf("[llm-profile] task %s reopen quota-blocked intents after profile delete: %v", task.ID, reopenErr)
					}
				}
				task.Notify()
			}
		}
	}
}

func (s *Server) pgActivateProfile(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body struct {
		ID int64 `json:"id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := pg.SetActiveProfile(body.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.invalidateProfileAgents() // active change may affect pinned-task fallbacks
	s.reapplyActiveProfile()    // switch the running engine to the newly activated profile
	writeJSON(w, 200, map[string]any{"ok": true})
}
