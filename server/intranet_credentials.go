// credentials.go 装配凭据一等实体的四个 host 工具(内网渗透期 2,
// INTRANET-PIVOT-DESIGN.md §4.4):登记/脱敏列表/验证记录/复用建议。
// 与期 1a 会话工具同模式:普通工具调用,自动过 guard
// 审批、自动进 activity 留痕;seed 进 tools 表、默认绑 worker。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// initCredentials 建凭据落库访问层(secret AES-GCM,密钥由 jwtKey 派生,
// 与 sessions 不同域标签)。失败只记日志,不拖垮启动。
func (s *Server) initCredentials() {
	if s.m.pg == nil {
		return
	}
	s.credStore = db.NewCredentialStore(s.m.pg, s.jwtKey)
}

// credentialTools 返回四个凭据工具。credStore 为 nil(DB 未就绪)时不提供。
func (s *Server) credentialTools() []actool.CoreTool {
	if s.credStore == nil {
		return nil
	}
	return []actool.CoreTool{
		s.addCredentialTool(),
		s.listCredentialsTool(),
		s.recordCredentialVerificationTool(),
		s.credentialReusePlanTool(),
	}
}

// addCredentialTool 把从目标收割的凭据登记为平台管理的一等实体(加密落库)。
func (s *Server) addCredentialTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "add_credential",
		Description: "【凭据登记】把从目标读到的口令/hash/密钥/ticket 登记为平台管理的凭据(secret AES-GCM 加密落库)。" +
			"一切收割到的凭据都应登记:复用打分(credential_reuse_plan)依赖它推断可横向的目标。" +
			"来源(source)写清楚从哪来(如 \"sqli dump users 表\" / \"/etc/shadow\" / \"mimikatz lsass dump\")," +
			"打分会把 source 里的 ssh/rdp/smb 等服务关键词纳入匹配。新凭据默认为未验证；已有实测结果请用 record_credential_verification 记录方法和证据，不重复登记或凭猜测标记通过。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"username":      strParam("用户名(可空,如 ticket/token 无用户名时留空)"),
				"cred_type":     strParam("凭据类型:password / hash_nt / hash_lm / hash_sha1 / ticket / ssh_key / token"),
				"secret":        strParam("凭据内容(口令明文/hash/私钥/ticket 串;加密存储,不明文落库)"),
				"domain":        strParam("认证域(可选,如 CORP 或 corp.local;不是来源网站域名)"),
				"target":        strParam("凭据所属站点或主机(可选，明确来源的主机名/IP/HTTP(S)地址；与认证域 domain 分开)"),
				"source_url":    strParam("可选的原始来源 HTTP(S) 地址，不含用户名密码；查询参数和片段会移除，不自动访问"),
				"source":        strParam("来源描述(必填):从哪台主机/什么方式得到,如 \"10.0.0.5 /etc/shadow\"、\"sqli dump users 表\""),
				"host_asset_id": map[string]any{"type": "integer", "description": "凭据来源主机的资产 id(可选;用于同网段打分)"},
			},
			"required":             []any{"cred_type", "secret", "source"},
			"additionalProperties": false,
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				Username    string `json:"username"`
				CredType    string `json:"cred_type"`
				Secret      string `json:"secret"`
				Domain      string `json:"domain"`
				Target      string `json:"target"`
				SourceURL   string `json:"source_url"`
				Source      string `json:"source"`
				HostAssetID int64  `json:"host_asset_id"`
			}
			if !decodeCredentialToolInput(in, &a) {
				return actool.Errorf("凭据参数应为有界 JSON 对象，且只包含声明的字段"), nil
			}
			if !db.ValidCredType(a.CredType) {
				return actool.Errorf("cred_type 应为 password/hash_nt/hash_lm/hash_sha1/ticket/ssh_key/token"), nil
			}
			if a.Secret == "" || len(a.Secret) > 64<<10 {
				return actool.Errorf("secret 必填且最多 64 KiB"), nil
			}
			a.Source = strings.TrimSpace(a.Source)
			if a.Source == "" || len(a.Source) > 8192 || len(a.Username) > 1024 || a.HostAssetID < 0 {
				return actool.Errorf("source 必填且最多 8192 字节，用户名最多 1024 字节，资产 ID 应为非负数"), nil
			}
			var err error
			a.Target, a.Domain, a.SourceURL, err = db.NormalizeCredentialMetadata(a.Target, a.Domain, a.SourceURL)
			if err != nil {
				return actool.Errorf("凭据目标、认证域或来源地址不符合格式要求"), nil
			}
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("凭据必须在任务上下文登记(非任务运行无 task 归属)"), nil
			}
			done, err := s.beginIntranetTaskOperation(ri.TaskID)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			defer done()
			rec := &db.CredentialRecord{
				TaskID: ri.TaskID, HostAssetID: a.HostAssetID, Username: a.Username,
				CredType: a.CredType, Secret: a.Secret, Domain: a.Domain, Source: a.Source,
				Target: a.Target, SourceURL: a.SourceURL,
				SourceIntentID: ri.IntentID, SourceExplorationID: ri.ExplorationID,
			}
			id, err := s.credStore.Create(ctx, rec)
			if err != nil {
				return credentialToolStoreError(err, "凭据登记失败，请检查所属任务、资产关联及数据库状态"), nil
			}
			return jsonResult(map[string]any{"credential_id": id})
		},
	})
}

// listCredentialsTool 返回本任务凭据的脱敏列表(secret 只露前后各 2 字符)。
func (s *Server) listCredentialsTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "list_credentials",
		Description: "列出本任务已登记的全部凭据(脱敏:secret 只返回前后 2 字符掩码)。" +
			"横向前先看已有凭据,避免重复收割;要打具体目标用 credential_reuse_plan 拿复用建议。",
		Schema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
		ReadOnly:    func(json.RawMessage) bool { return true },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			// Older callers omit arguments for this zero-parameter read tool.
			// Keep that compatibility local; credential writers remain strict.
			if len(bytes.TrimSpace(in)) == 0 {
				in = json.RawMessage(`{}`)
			}
			if !decodeCredentialToolInput(in, &struct{}{}) {
				return actool.Errorf("list_credentials 只接受空 JSON 对象"), nil
			}
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("list_credentials 需要任务上下文"), nil
			}
			creds, err := s.credStore.ListByTask(ctx, ri.TaskID)
			if err != nil {
				return actool.Errorf("读取凭据失败，请检查数据库状态与加密密钥配置"), nil
			}
			items := []map[string]any{}
			for _, c := range creds {
				items = append(items, map[string]any{
					"id": c.ID, "task_id": c.TaskID, "username": c.Username, "cred_type": c.CredType,
					"secret_masked": db.MaskSecret(c.Secret),
					"domain":        c.Domain, "source": c.Source, "verified": c.Verified,
					"host_asset_id": c.HostAssetID, "created_at": c.CreatedAt,
					"target": c.Target, "target_label": c.TargetLabel, "source_url": c.SourceURL,
					"verification_status": c.VerificationStatus,
					"source_intent_id":    c.SourceIntentID, "source_exploration_id": c.SourceExplorationID,
				})
			}
			return jsonResult(map[string]any{"credentials": items, "count": len(items)})
		},
	})
}

// recordCredentialVerificationTool only records an already-observed result; it
// never attempts a login or sends a request to the credential's target.
func (s *Server) recordCredentialVerificationTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "record_credential_verification",
		Description: "记录本任务凭据的已有实测验证结果，不会自动访问目标或尝试登录。" +
			"status 为 unverified/verified/failed；verified 与 failed 必须给出具体方法和可复核证据，" +
			"未实际验证、仅推测或缺少证据时保持 unverified。证据不要包含完整口令、token 或私钥。" +
			"回到 unverified 只清除当前验证标记，历史记录仍保留。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"credential_id": map[string]any{"type": "integer", "minimum": 1, "description": "本任务已有凭据 ID"},
				"status":        map[string]any{"type": "string", "enum": []any{"unverified", "verified", "failed"}, "description": "已有实测结果；不确定时 unverified"},
				"method":        strParam("实测方法，通过或失败时必填；不包含完整密钥"),
				"evidence":      strParam("可复核的结果与证据位置，通过或失败时必填；不要写入完整密钥"),
			},
			"required":             []any{"credential_id", "status"},
			"additionalProperties": false,
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				CredentialID int64  `json:"credential_id"`
				Status       string `json:"status"`
				Method       string `json:"method"`
				Evidence     string `json:"evidence"`
			}
			if !decodeCredentialToolInput(in, &a) || a.CredentialID <= 0 {
				return actool.Errorf("验证记录参数不合法，需要正数 credential_id 和声明的 JSON 字段"), nil
			}
			status, method, evidence, err := db.NormalizeCredentialVerification(a.Status, a.Method, a.Evidence)
			if err != nil {
				return actool.Errorf("验证状态不合法，验证通过或失败需提供有界的方法与证据"), nil
			}
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("record_credential_verification 需要任务上下文"), nil
			}
			done, err := s.beginIntranetTaskOperation(ri.TaskID)
			if err != nil {
				return actool.Errorf("任务正在删除或归档，暂不接受验证记录"), nil
			}
			defer done()
			rec, err := s.credStore.Get(ctx, a.CredentialID)
			if err != nil {
				return actool.Errorf("读取凭据失败，请检查数据库与加密密钥配置"), nil
			}
			if rec == nil || rec.TaskID != ri.TaskID {
				return actool.Errorf("凭据不存在或不属于当前任务"), nil
			}
			by := fmt.Sprintf("agent:task-%d:exploration-%d:intent-%d", ri.TaskID, ri.ExplorationID, ri.IntentID)
			if err := s.credStore.SetVerification(ctx, rec.ID, status, method, evidence, by); err != nil {
				return credentialToolStoreError(err, "保存验证记录失败，请检查任务与数据库状态"), nil
			}
			return jsonResult(map[string]any{"credential_id": rec.ID, "verification_status": status})
		},
	})
}

// Keep malformed/oversized arguments and unknown fields out of credential
// writers without quoting input or decoder diagnostics back into activity.
func decodeCredentialToolInput(in json.RawMessage, out any) bool {
	in = bytes.TrimSpace(in)
	if len(in) == 0 || len(in) > 128<<10 || in[0] != '{' {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(in))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func credentialToolStoreError(err error, fallback string) actool.Result {
	switch {
	case errors.Is(err, db.ErrCredentialNotFound):
		return actool.Errorf("凭据不存在")
	case errors.Is(err, db.ErrCredentialUnavailable):
		return actool.Errorf("任务已删除或正在归档，暂不接受凭据修改")
	case errors.Is(err, db.ErrCredentialMetadataInvalid):
		return actool.Errorf("凭据目标、认证域、来源地址或资产关联不符合要求")
	case errors.Is(err, db.ErrCredentialVerificationInvalid):
		return actool.Errorf("验证状态、方法或证据不符合要求")
	default:
		return actool.Errorf(fallback)
	}
}

// credentialReusePlanTool 对本任务凭据 × 本任务资产跑服务端复用打分,返回 top 10。
func (s *Server) credentialReusePlanTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "credential_reuse_plan",
		Description: "【凭据复用打分】对本任务已登记凭据 × 本任务已发现主机资产跑复用打分(纯规则:同网段/域凭据/服务重叠/PtH/已验证)," +
			"返回得分最高的 top 10 条横向建议(凭据 × 目标资产 + 每条加分的理由)。建议只代表成功率先验," +
			"实际横向(ssh/wmi/psexec/喷洒)属高动静动作,会过 guard 人工确认且须在 RoE 范围内。",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		ReadOnly:    func(json.RawMessage) bool { return true },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("credential_reuse_plan 需要任务上下文"), nil
			}
			creds, err := s.credStore.ListByTask(ctx, ri.TaskID)
			if err != nil {
				return actool.Errorf("读取凭据失败:" + err.Error()), nil
			}
			if len(creds) == 0 {
				return actool.Text("本任务还没有登记凭据(add_credential 先登记),无复用建议"), nil
			}
			assets, err := s.m.pg.Assets().QueryByTask(ri.TaskID, "ip", 2000, 0)
			if err != nil {
				return actool.Errorf("读取资产失败:" + err.Error()), nil
			}
			cands, byID := reuseCandidates(assets)
			type row struct {
				CredID   int64    `json:"cred_id"`
				Username string   `json:"username"`
				CredType string   `json:"cred_type"`
				AssetID  int64    `json:"asset_id"`
				IP       string   `json:"ip"`
				Score    int      `json:"score"`
				Reasons  []string `json:"reasons"`
			}
			flat := []row{}
			for _, c := range creds {
				var src *db.ReuseCandidate
				if c.HostAssetID != 0 {
					src = byID[c.HostAssetID]
				}
				for _, sug := range db.ScoreReuse(c, src, cands) {
					flat = append(flat, row{c.ID, c.Username, c.CredType, sug.AssetID, sug.IP, sug.Score, sug.Reasons})
				}
			}
			// 全局排序:得分降序,同分 cred_id/asset_id 升序(确定性)。
			for i := 0; i < len(flat); i++ {
				for j := i + 1; j < len(flat); j++ {
					if flat[j].Score > flat[i].Score ||
						(flat[j].Score == flat[i].Score && (flat[j].CredID < flat[i].CredID ||
							(flat[j].CredID == flat[i].CredID && flat[j].AssetID < flat[i].AssetID))) {
						flat[i], flat[j] = flat[j], flat[i]
					}
				}
			}
			if len(flat) > 10 {
				flat = flat[:10]
			}
			return jsonResult(map[string]any{
				"suggestions": flat, "credential_count": len(creds), "asset_count": len(cands),
			})
		},
	})
}

// reuseCandidates 把 ip 资产投影为打分候选(服务名取自 open_ports)。
func reuseCandidates(assets []*db.Asset) ([]db.ReuseCandidate, map[int64]*db.ReuseCandidate) {
	out := []db.ReuseCandidate{}
	byID := map[int64]*db.ReuseCandidate{}
	for _, a := range assets {
		if a == nil || a.IP == "" {
			continue
		}
		services := []string{}
		for _, p := range a.OpenPorts {
			if svc, ok := p["service"].(string); ok && svc != "" {
				services = append(services, svc)
			}
		}
		out = append(out, db.ReuseCandidate{
			AssetID: a.ID, IP: a.IP, CSegment: a.CSegment, Services: services,
		})
		byID[a.ID] = &out[len(out)-1]
	}
	return out, byID
}

// seedCredentialTools 把四个凭据工具 seed 进 tools 表、默认绑定 worker
// (与 seedSessionTools 同一套 SeedTool 首插入语义:老库的用户编辑不被覆盖)。
func (s *Server) seedCredentialTools() {
	workerAgents, _ := json.Marshal([]string{"worker"})
	for _, t := range s.credentialTools() {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, workerAgents); err != nil {
			log.Printf("[tools] seed %s 失败: %v", t.Name(), err)
		}
	}
	s.upgradeCredentialToolMetadata()
}

// SeedTool deliberately preserves existing rows. Add only the new optional
// metadata properties to old system schemas, and replace the exact obsolete
// verification instruction without resetting user descriptions or bindings.
func (s *Server) upgradeCredentialToolMetadata() {
	if s.m == nil || s.m.pg == nil {
		return
	}
	props := s.addCredentialTool().InputSchema()["properties"].(map[string]any)
	for _, name := range []string{"target", "source_url"} {
		raw, err := json.Marshal(props[name])
		if err != nil {
			return
		}
		_, err = s.m.pg.Exec(`UPDATE tools SET
schema=jsonb_set(schema, '{properties}', COALESCE(schema->'properties','{}'::jsonb) || jsonb_build_object($1::text,$2::jsonb), true),
updated_at=now()
WHERE key='add_credential' AND system AND jsonb_typeof(schema)='object'
AND (schema->'properties' IS NULL OR jsonb_typeof(schema->'properties')='object')
AND NOT(COALESCE(schema->'properties','{}'::jsonb) ? $1)`, name, string(raw))
		if err != nil {
			log.Printf("[tools] 凭据元数据 schema 升级失败")
			return
		}
	}
	const oldInstruction = "实测能登录后用 verify=true 重新登记或让平台标记 verified。"
	const newInstruction = "已有实测结果请用 record_credential_verification 记录方法和证据，不重复登记或凭猜测标记通过。"
	if _, err := s.m.pg.Exec(`UPDATE tools SET description=replace(description,$1,$2),updated_at=now()
WHERE key='add_credential' AND system AND strpos(description,$1)>0`, oldInstruction, newInstruction); err != nil {
		log.Printf("[tools] 凭据验证说明升级失败")
	}
}
