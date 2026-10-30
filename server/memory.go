package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/memcontext"
	longmemory "github.com/Autumn-27/artex/memory"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/google/uuid"
)

// registerMemoryRoutes stays behind the server's ordinary administrator JWT
// middleware. Model runs do not receive these write/settings endpoints as tools.
func (s *Server) registerMemoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/memories/settings", s.getMemorySettings)
	mux.HandleFunc("PUT /api/memories/settings", s.putMemorySettings)
	mux.HandleFunc("GET /api/memories/usage", s.getMemoryUsage)
	mux.HandleFunc("POST /api/memories/extract", s.extractMemoryHistory)
	mux.HandleFunc("GET /api/memories", s.listMemories)
	mux.HandleFunc("POST /api/memories", s.createMemory)
	mux.HandleFunc("GET /api/memories/{id}", s.getMemory)
	mux.HandleFunc("PATCH /api/memories/{id}", s.patchMemory)
	mux.HandleFunc("DELETE /api/memories/{id}", s.deleteMemory)
}

func decodeMemoryRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 24<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		writeErr(w, http.StatusBadRequest, "记忆请求格式无效或正文过大")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		writeErr(w, http.StatusBadRequest, "请求只接受一个 JSON 对象")
		return false
	}
	return true
}

func memoryAPIError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrMemoryConflict), errors.Is(err, db.ErrMemoryStale):
		writeErr(w, http.StatusConflict, "记忆或来源已变化，请刷新后重试")
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, http.StatusNotFound, "记忆或来源不存在")
	case errors.Is(err, db.ErrMemoryInput), errors.Is(err, longmemory.ErrInvalid), errors.Is(err, longmemory.ErrSensitive):
		writeErr(w, http.StatusBadRequest, "记忆内容无效、包含敏感信息或指令性内容，本次未保存")
	default:
		// Driver/provider errors can contain source fragments or credentials.
		writeErr(w, http.StatusServiceUnavailable, "记忆服务暂不可用；写入结果请重新读取核对")
	}
}

func memoryID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "记忆 ID 无效")
		return 0, false
	}
	return id, true
}

func memoryQueryID(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, db.ErrMemoryInput
	}
	return id, nil
}

func memoryLimit(r *http.Request, fallback, maximum int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return fallback
	}
	return min(n, maximum)
}

func (s *Server) memoryModel(settings db.MemorySettings) (llm.Provider, agent.Config, error) {
	if settings.ProfileID > 0 {
		p, cfg, ok := s.providerForProfile(settings.ProfileID)
		if !ok || p == nil || cfg.ConfigError != nil {
			return nil, cfg, db.ErrMemoryUnavailable
		}
		return p, cfg, nil
	}
	s.cfgMu.Lock()
	p, cfg, on := s.llmDirect, s.llmCfg, s.llmOn
	s.cfgMu.Unlock()
	if !on || p == nil || cfg.ConfigError != nil {
		return nil, cfg, db.ErrMemoryUnavailable
	}
	// Use the selected concrete provider, not a cross-channel failover pool.
	return p, cfg, nil
}

type memorySettingsView struct {
	db.MemorySettings
	ModelAvailable bool   `json:"model_available"`
	ModelError     string `json:"model_error,omitempty"`
}

func (s *Server) memorySettingsView(settings db.MemorySettings) memorySettingsView {
	_, _, err := s.memoryModel(settings)
	v := memorySettingsView{MemorySettings: settings, ModelAvailable: err == nil}
	if err != nil {
		v.ModelError = "未配置可用的提炼模型；已有记忆仍可检索，自动学习等待模型可用"
	}
	return v
}

func (s *Server) getMemorySettings(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	settings, err := pg.GetMemorySettings(r.Context())
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.memorySettingsView(settings))
}

func (s *Server) putMemorySettings(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var settings db.MemorySettings
	if !decodeMemoryRequest(w, r, &settings) {
		return
	}
	if settings.Epoch <= 0 {
		memoryAPIError(w, db.ErrMemoryInput)
		return
	}
	if settings.ProfileID > 0 {
		if _, _, err := s.memoryModel(settings); err != nil {
			writeErr(w, http.StatusBadRequest, "所选记忆提炼模型不可用，请选择有效配置")
			return
		}
	}
	stored, err := pg.UpdateMemorySettings(r.Context(), settings, settings.Epoch)
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	s.wakeMemoryWorker()
	writeJSON(w, http.StatusOK, s.memorySettingsView(stored))
}

func (s *Server) listMemories(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, err := memoryQueryID(r, "task_id")
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	convID, err := memoryQueryID(r, "conversation_id")
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	q := r.URL.Query().Get("q")
	if !utf8.ValidString(q) || len(q) > 512 {
		memoryAPIError(w, db.ErrMemoryInput)
		return
	}
	limit := memoryLimit(r, 30, 100)
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
	}
	if err != nil || offset < 0 || offset > 1000000 || offset%limit != 0 {
		memoryAPIError(w, db.ErrMemoryInput)
		return
	}
	items, total, err := pg.ListMemoryEntries(r.Context(), db.MemoryFilter{
		Query: q, State: r.URL.Query().Get("status"), Kind: r.URL.Query().Get("kind"), TaskID: taskID, ConversationID: convID,
	}, offset/limit, limit)
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	if items == nil {
		items = []db.MemoryEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

func (s *Server) getMemory(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := memoryID(w, r)
	if !ok {
		return
	}
	entry, err := pg.GetMemoryEntry(r.Context(), id)
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	if entry == nil {
		memoryAPIError(w, sql.ErrNoRows)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) validateMemoryInput(input db.MemoryInput) error {
	if err := longmemory.ValidateContent(input.Title, input.Content); err != nil {
		return err
	}
	metadata, err := json.Marshal(input)
	if err != nil {
		return db.ErrMemoryInput
	}
	if _, err := longmemory.SanitizeSource(string(metadata)); err != nil {
		return err
	}
	if s.m != nil && s.m.egress != nil {
		if _, hit := s.m.egress.Check(string(metadata)); hit {
			return longmemory.ErrSensitive
		}
	}
	return nil
}

func (s *Server) createMemory(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var input db.MemoryInput
	if !decodeMemoryRequest(w, r, &input) {
		return
	}
	if err := s.validateMemoryInput(input); err != nil {
		memoryAPIError(w, err)
		return
	}
	entry, err := pg.CreateManualMemory(r.Context(), input)
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) patchMemory(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := memoryID(w, r)
	if !ok {
		return
	}
	var patch struct {
		Version       int64           `json:"version"`
		Title         *string         `json:"title"`
		Content       *string         `json:"content"`
		Kind          *string         `json:"kind"`
		State         *string         `json:"status"`
		Confidence    *string         `json:"confidence"`
		Target        *string         `json:"target"`
		Applicability *string         `json:"applicability"`
		Tags          *[]string       `json:"tags"`
		ExpiresAt     json.RawMessage `json:"expires_at"`
	}
	if !decodeMemoryRequest(w, r, &patch) {
		return
	}
	if patch.Version <= 0 {
		memoryAPIError(w, db.ErrMemoryInput)
		return
	}
	// PATCH from the state toggle need not repeat the body, but validation still
	// covers the complete resulting record before it can become active again.
	old, err := pg.GetMemoryEntry(r.Context(), id)
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	if old == nil {
		memoryAPIError(w, sql.ErrNoRows)
		return
	}
	input := db.MemoryInput{Title: old.Title, Content: old.Content, Kind: old.Kind, State: old.State,
		Confidence: old.Confidence, Applicability: old.Applicability, Target: old.Target, Tags: old.Tags, ExpiresAt: old.ExpiresAt}
	if input.State == "expired" {
		input.State = "active"
	} // expiry remains unchanged unless explicitly edited
	if patch.Title != nil {
		input.Title = *patch.Title
	}
	if patch.Content != nil {
		input.Content = *patch.Content
	}
	if patch.Kind != nil {
		input.Kind = *patch.Kind
	}
	if patch.State != nil {
		input.State = *patch.State
	}
	if patch.Confidence != nil {
		input.Confidence = *patch.Confidence
	}
	if patch.Applicability != nil {
		input.Applicability = *patch.Applicability
	}
	if patch.Target != nil {
		input.Target = *patch.Target
	}
	if patch.Tags != nil {
		input.Tags = *patch.Tags
	}
	if len(patch.ExpiresAt) > 0 {
		if err := json.Unmarshal(patch.ExpiresAt, &input.ExpiresAt); err != nil {
			memoryAPIError(w, db.ErrMemoryInput)
			return
		}
	}
	// An edit does not move source ownership to a user-supplied task/session.
	input.OriginTaskID, input.OriginConversationID = old.OriginTaskID, old.OriginConversationID
	input.AgentKey, input.SessionID = old.AgentKey, old.SessionID
	if err := s.validateMemoryInput(input); err != nil {
		memoryAPIError(w, err)
		return
	}
	entry, err := pg.EditMemoryEntry(r.Context(), id, input, patch.Version)
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := memoryID(w, r)
	if !ok {
		return
	}
	var input struct {
		Version int64 `json:"version"`
	}
	if !decodeMemoryRequest(w, r, &input) {
		return
	}
	if input.Version <= 0 {
		memoryAPIError(w, db.ErrMemoryInput)
		return
	}
	if err := pg.DeleteMemoryEntry(r.Context(), id, input.Version); err != nil {
		memoryAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) getMemoryUsage(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	taskID, err := memoryQueryID(r, "task_id")
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	convID, err := memoryQueryID(r, "conversation_id")
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if len(sessionID) > 128 || !utf8.ValidString(sessionID) {
		memoryAPIError(w, db.ErrMemoryInput)
		return
	}
	items, err := pg.ListMemoryAuditFiltered(r.Context(), taskID, convID, sessionID, memoryLimit(r, 50, 200), true)
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	views := make([]map[string]any, 0, len(items))
	for _, item := range items {
		views = append(views, map[string]any{"id": item.ID, "entry_id": item.EntryID, "entry_version": item.EntryVersion,
			"task_id": item.TaskID, "conversation_id": item.ConversationID, "session_id": item.SessionID,
			"agent_key": item.AgentKey, "created_at": item.CreatedAt, "result": item.Action})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views})
}

func (s *Server) extractMemoryHistory(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var input struct {
		TaskID int64 `json:"task_id"`
	}
	if !decodeMemoryRequest(w, r, &input) {
		return
	}
	if input.TaskID < 0 {
		memoryAPIError(w, db.ErrMemoryInput)
		return
	}
	settings, err := pg.GetMemorySettings(r.Context())
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	if !settings.Enabled || !settings.AutoExtract {
		writeErr(w, http.StatusConflict, "请先开启记忆和自动提炼；本次未排队")
		return
	}
	if _, _, err := s.memoryModel(settings); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "提炼模型不可用；本次未排队")
		return
	}
	sources, err := pg.FindMemoryHistorySources(r.Context(), input.TaskID, 50)
	if err != nil {
		memoryAPIError(w, err)
		return
	}
	queued := 0
	for _, source := range sources {
		ok, err := pg.QueueMemoryExtraction(r.Context(), source)
		if err != nil {
			memoryAPIError(w, err)
			return
		}
		if ok {
			queued++
		}
	}
	s.wakeMemoryWorker()
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": queued})
}

func memoryConversationID(session string) int64 {
	if !strings.HasPrefix(session, "conv-") {
		return 0
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(session, "conv-"), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

func memorySessionID(ctx context.Context, info agent.RunInfo, role string) string {
	if info.TaskID == 0 {
		return info.SessionID
	}
	if info.IntentID > 0 {
		return fmt.Sprintf("intent:%d", info.IntentID)
	}
	if role == "planner" {
		return "plan"
	}
	if role == "mainagent" {
		seg := int64(0)
		if _, suffix, ok := strings.Cut(transcript.SessionIDFrom(ctx), "-main-s"); ok {
			seg, _ = strconv.ParseInt(suffix, 10, 64)
		}
		return fmt.Sprintf("main:%d", max(seg, 0))
	}
	return info.SessionID
}

// Only recent human/task text is a retrieval query. Tool outputs and system
// instructions are not allowed to manufacture a new query or sharing identity.
func memoryRecallQuery(req llm.CompletionRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != llm.RoleUser {
			continue
		}
		var text strings.Builder
		for _, block := range req.Messages[i].Content {
			if block.Type == llm.BlockText {
				text.WriteString(block.Text)
				text.WriteByte(' ')
			}
		}
		q := strings.TrimSpace(text.String())
		if q == "" {
			continue
		}
		if !utf8.ValidString(q) {
			return ""
		}
		if len(q) > 512 {
			q = q[:512]
			for !utf8.ValidString(q) {
				q = q[:len(q)-1]
			}
		}
		if _, err := longmemory.SanitizeSource(q); err != nil {
			return ""
		}
		return q
	}
	return ""
}

func (s *Server) wireMemoryRecall() {
	if s.m == nil || s.m.pg == nil {
		agent.MemoryRecall = nil
		return
	}
	agent.MemoryRecall = s.recallMemory
}

func (s *Server) recallMemory(ctx context.Context, info agent.RunInfo, role string, req llm.CompletionRequest) (agent.MemoryRecallResult, error) {
	empty := agent.MemoryRecallResult{}
	if role == db.CredentialAgentKey || role == "asset_mapper" {
		return empty, nil
	}
	convID := memoryConversationID(info.SessionID)
	if info.TaskID <= 0 && convID <= 0 {
		return empty, nil
	}
	query := memoryRecallQuery(req)
	if query == "" {
		return empty, nil
	}
	if s.m.egress != nil {
		if _, hit := s.m.egress.Check(query); hit {
			return empty, nil
		}
	}
	sessionID := memorySessionID(ctx, info, role)
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	entries, settings, err := s.m.pg.SearchMemory(lookupCtx, db.MemorySearchInput{
		Query: query, TaskID: info.TaskID, ConversationID: convID, AgentKey: role, SessionID: sessionID,
	})
	if err != nil {
		return empty, errors.New("记忆读取暂不可用，本次继续使用原任务上下文")
	}
	if !settings.Enabled || len(entries) == 0 {
		return empty, nil
	}
	budget := min(settings.MaxTokens, memcontext.ByteBudget(ctx))
	if budget <= 0 {
		budget = min(settings.MaxTokens, 1600)
	}
	var refs []memcontext.Reference
	var selected []map[string]any
	seen := map[string]bool{}
	block := ""
	for _, entry := range entries {
		if len(selected) >= settings.MaxEntries {
			break
		}
		identity := strings.Join([]string{entry.Title, entry.Content, entry.Kind, entry.Target, entry.Applicability}, "\x00")
		if seen[identity] {
			continue
		}
		if err := s.validateMemoryInput(db.MemoryInput{Title: entry.Title, Content: entry.Content, Kind: entry.Kind,
			Target: entry.Target, Applicability: entry.Applicability, Tags: entry.Tags}); err != nil {
			continue
		}
		next := append(append([]map[string]any(nil), selected...), map[string]any{
			"id": entry.ID, "title": entry.Title, "content": entry.Content, "kind": entry.Kind,
			"applicability": entry.Applicability, "confidence": entry.Confidence, "target": entry.Target,
			"origin_task_id": entry.OriginTaskID, "origin_conversation_id": entry.OriginConversationID,
			"source_kind": entry.SourceKind, "source_id": entry.SourceID, "source_version": entry.SourceVersion,
		})
		body, marshalErr := json.Marshal(next)
		if marshalErr != nil {
			continue
		}
		nextRefs := append(append([]memcontext.Reference(nil), refs...), memcontext.Reference{ID: entry.ID, Version: entry.Version})
		if len(memcontext.FormatBlock(string(body), nextRefs)) > budget {
			if entry.Automatic {
				continue
			} // automated notes must supply their actual short content
			// A long note still contributes an explicitly labelled INDEX, never
			// a silently truncated claim that could lose its qualifications.
			index := map[string]any{"id": entry.ID, "title": entry.Title, "kind": entry.Kind,
				"origin_task_id": entry.OriginTaskID, "origin_conversation_id": entry.OriginConversationID,
				"representation": "index_only", "content_omitted": true,
				"notice": "正文未注入，仅供历史索引；不作为当前目标事实或证据，完整内容见记忆中心。"}
			next = append(append([]map[string]any(nil), selected...), index)
			body, marshalErr = json.Marshal(next)
			if marshalErr != nil || len(memcontext.FormatBlock(string(body), nextRefs)) > budget {
				continue
			}
		}
		selected, refs, block = next, nextRefs, string(body)
		seen[identity] = true
	}
	if len(refs) == 0 {
		return empty, nil
	}
	admission := db.MemoryAdmission{SettingsEpoch: settings.Epoch, TaskID: info.TaskID, ConversationID: convID,
		AgentKey: role, SessionID: sessionID, CallKey: uuid.NewString()}
	for _, ref := range refs {
		admission.References = append(admission.References, db.MemoryRef{ID: ref.ID, Version: ref.Version})
	}
	return agent.MemoryRecallResult{Block: block, References: refs, Revalidate: func(callCtx context.Context) error {
		checkCtx, cancel := context.WithTimeout(callCtx, 2*time.Second)
		defer cancel()
		if err := s.m.pg.ValidateMemoryAdmission(checkCtx, admission); err != nil {
			return errors.New("记忆或来源已失效；本次请求停止，请开始新一轮对话")
		}
		return nil
	}}, nil
}
