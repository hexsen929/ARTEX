package server

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/Autumn-27/artex/db"
)

func credentialNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// credentialItem is deliberately a whitelist; a decrypted store record is
// never passed to the general JSON encoder by an HTTP handler.
func credentialItem(c *db.CredentialRecord) map[string]any {
	return map[string]any{
		"id": c.ID, "task_id": c.TaskID, "task_name": c.TaskName, "task_available": c.TaskAvailable,
		"username": c.Username, "cred_type": c.CredType, "secret_masked": db.MaskSecret(c.Secret),
		"domain": c.Domain, "source": c.Source, "target": c.Target, "target_label": c.TargetLabel, "target_url": c.TargetURL,
		"source_url": c.SourceURL, "source_intent_id": c.SourceIntentID, "source_exploration_id": c.SourceExplorationID,
		"verified": c.VerificationStatus == "verified", "verification_status": c.VerificationStatus,
		"verification_at": c.VerificationAt, "verification_method": c.VerificationMethod,
		"verification_evidence": c.VerificationEvidence, "verification_by": c.VerificationBy,
		"verification_history": c.VerificationHistory,
		"host_asset_id":        c.HostAssetID, "created_at": c.CreatedAt,
	}
}

func credentialSameOrigin(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	} // Bearer-only non-browser clients remain valid.
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	host := r.Host
	if value, _, err := net.SplitHostPort(host); err == nil {
		host = value
	}
	host = strings.Trim(host, "[]")
	// Preserve Next.js development access only on the exact same loopback host.
	return strings.EqualFold(u.Hostname(), host) && (host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()))
}

func decodeCredentialRequest(w http.ResponseWriter, r *http.Request, out any) bool {
	contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if !strings.EqualFold(contentType, "application/json") {
		writeErr(w, 415, "请使用 application/json")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeErr(w, 400, "凭据请求格式不正确或超出长度限制")
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeErr(w, 400, "凭据请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func credentialActor(r *http.Request, key []byte) string {
	if claims, err := parseAccessToken(extractToken(r), key); err == nil && claims.Subject != "" {
		return claims.Subject
	}
	return "unknown"
}

// credentialOperation holds the runtime resource fence while the handler is
// active. Mutations also take the DB task-row lock inside their transaction.
func (s *Server) credentialOperation(w http.ResponseWriter, r *http.Request, id int64) (*db.CredentialRecord, func(), string) {
	if s.credStore == nil {
		writeErr(w, 503, "凭据子系统未就绪")
		return nil, nil, "unavailable"
	}
	c, err := s.credStore.Get(r.Context(), id)
	if err != nil {
		writeErr(w, 500, "凭据读取失败")
		return nil, nil, "read_error"
	}
	if c == nil {
		writeErr(w, 404, "凭据不存在")
		return nil, nil, "not_found"
	}
	if !c.TaskAvailable {
		writeErr(w, 409, "所属任务已删除、归档或正在归档处理中")
		return c, nil, "task_unavailable"
	}
	done, err := s.beginIntranetTaskOperation(c.TaskID)
	if err != nil {
		writeErr(w, 409, "所属任务暂不接受凭据操作")
		return c, nil, "task_blocked"
	}
	// Recheck after taking the runtime fence, including DB-only archive queues.
	current, err := s.credStore.Get(r.Context(), id)
	if err != nil || current == nil || !current.TaskAvailable {
		done()
		if err != nil {
			writeErr(w, 500, "凭据读取失败")
			return c, nil, "read_error"
		}
		writeErr(w, 409, "凭据或所属任务当前不可操作")
		return c, nil, "task_unavailable"
	}
	return current, done, "ok"
}

func (s *Server) credentialMutationError(w http.ResponseWriter, err error) string {
	switch {
	case errors.Is(err, db.ErrCredentialVerificationContainsSecret):
		writeErr(w, 400, "验证说明中请勿填写凭据原值")
		return "verification_contains_secret"
	case errors.Is(err, db.ErrCredentialMetadataInvalid):
		writeErr(w, 400, "目标、认证域或来源地址格式不正确")
		return "invalid_metadata"
	case errors.Is(err, db.ErrCredentialVerificationInvalid):
		writeErr(w, 400, "验证结论需要有效状态、验证方式及证据")
		return "invalid_verification"
	case errors.Is(err, db.ErrCredentialNotFound):
		writeErr(w, 404, "凭据不存在")
		return "not_found"
	case errors.Is(err, db.ErrCredentialUnavailable):
		writeErr(w, 409, "所属任务当前不可操作")
		return "task_unavailable"
	default:
		writeErr(w, 500, "凭据保存失败")
		return "write_error"
	}
}

func (s *Server) intranetRevealCredential(w http.ResponseWriter, r *http.Request) {
	credentialNoStore(w)
	var id, taskID int64
	action, result := "reveal", "invalid_request"
	defer func() { s.auditIntranet(r, "credential_%s id=%d task=%d result=%s", action, id, taskID, result) }()
	if !credentialSameOrigin(r) {
		result = "origin_denied"
		writeErr(w, 403, "凭据操作来源不匹配")
		return
	}
	var err error
	id, err = pathID(r, "id")
	if err != nil {
		writeErr(w, 400, "凭据 ID 不正确")
		return
	}
	var input struct {
		Purpose string `json:"purpose"`
	}
	if !decodeCredentialRequest(w, r, &input) {
		return
	}
	if input.Purpose != "view" && input.Purpose != "copy" {
		writeErr(w, 400, "请选择查看或复制凭据")
		return
	}
	action = "reveal_" + input.Purpose
	c, done, outcome := s.credentialOperation(w, r, id)
	if c != nil {
		taskID = c.TaskID
	}
	if done == nil {
		result = outcome
		return
	}
	defer done()
	result = "ok"
	writeJSON(w, 200, map[string]any{"id": c.ID, "secret": c.Secret})
}

func (s *Server) intranetUpdateCredentialVerification(w http.ResponseWriter, r *http.Request) {
	credentialNoStore(w)
	var id, taskID int64
	result := "invalid_request"
	defer func() { s.auditIntranet(r, "credential_verification id=%d task=%d result=%s", id, taskID, result) }()
	if !credentialSameOrigin(r) {
		result = "origin_denied"
		writeErr(w, 403, "凭据操作来源不匹配")
		return
	}
	var err error
	id, err = pathID(r, "id")
	if err != nil {
		writeErr(w, 400, "凭据 ID 不正确")
		return
	}
	var input struct {
		Status   string `json:"status"`
		Method   string `json:"method"`
		Evidence string `json:"evidence"`
	}
	if !decodeCredentialRequest(w, r, &input) {
		return
	}
	status, method, evidence, err := db.NormalizeCredentialVerification(input.Status, input.Method, input.Evidence)
	if err != nil {
		result = s.credentialMutationError(w, err)
		return
	}
	c, done, outcome := s.credentialOperation(w, r, id)
	if c != nil {
		taskID = c.TaskID
	}
	if done == nil {
		result = outcome
		return
	}
	defer done()
	if err = s.credStore.SetVerification(r.Context(), id, status, method, evidence, credentialActor(r, s.jwtKey)); err != nil {
		result = s.credentialMutationError(w, err)
		return
	}
	updated, err := s.credStore.Get(r.Context(), id)
	if err != nil || updated == nil {
		result = "read_error"
		writeErr(w, 500, "凭据已保存，刷新列表后查看")
		return
	}
	result = "ok"
	writeJSON(w, 200, credentialItem(updated))
}

func (s *Server) intranetUpdateCredentialMetadata(w http.ResponseWriter, r *http.Request) {
	credentialNoStore(w)
	var id, taskID int64
	result := "invalid_request"
	defer func() { s.auditIntranet(r, "credential_metadata id=%d task=%d result=%s", id, taskID, result) }()
	if !credentialSameOrigin(r) {
		result = "origin_denied"
		writeErr(w, 403, "凭据操作来源不匹配")
		return
	}
	var err error
	id, err = pathID(r, "id")
	if err != nil {
		writeErr(w, 400, "凭据 ID 不正确")
		return
	}
	var input struct {
		Target    *string `json:"target"`
		Domain    *string `json:"domain"`
		SourceURL *string `json:"source_url"`
	}
	if !decodeCredentialRequest(w, r, &input) {
		return
	}
	if input.Target == nil && input.Domain == nil && input.SourceURL == nil {
		writeErr(w, 400, "至少填写一项凭据元数据")
		return
	}
	c, done, outcome := s.credentialOperation(w, r, id)
	if c != nil {
		taskID = c.TaskID
	}
	if done == nil {
		result = outcome
		return
	}
	defer done()
	if err = s.credStore.PatchMetadata(r.Context(), id, input.Target, input.Domain, input.SourceURL); err != nil {
		result = s.credentialMutationError(w, err)
		return
	}
	updated, err := s.credStore.Get(r.Context(), id)
	if err != nil || updated == nil {
		result = "read_error"
		writeErr(w, 500, "凭据已保存，刷新列表后查看")
		return
	}
	result = "ok"
	writeJSON(w, 200, credentialItem(updated))
}
