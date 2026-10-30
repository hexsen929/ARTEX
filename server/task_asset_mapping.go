package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/db"
)

const topologyMappingAssociationKind = "topology_mapping_task"

// This native record is written only by the task handler, not inferred from a
// model's answer or the conversation's editable title. No new execution grant
// is attached to it; asset_mapper remains a database-only read role.
func appendTopologyMappingAssociation(pg *db.DB, conversationID, taskID int64) error {
	_, err := pg.AppendConvActivity(conversationID, db.Activity{
		Worker: "system", Kind: topologyMappingAssociationKind,
		Summary: fmt.Sprintf("只读拓扑梳理 · 任务 #%d", taskID), Detail: strconv.FormatInt(taskID, 10),
	})
	return err
}

type topologyMappingState struct {
	Running        bool   `json:"running"`
	ConversationID int64  `json:"conversation_id,omitempty"`
	Status         string `json:"status"` // none | running | finished | failed | interrupted
	FinishedAt     string `json:"finished_at,omitempty"`
	Association    string `json:"association,omitempty"` // native | legacy_title (unverified historical candidate)
}

func topologyMappingOutcome(running bool, lastUser, lastResult, lastFailure int64, resultFailed bool) string {
	if running {
		return "running"
	}
	if lastFailure > lastUser && lastFailure >= lastResult || lastResult > lastUser && resultFailed {
		return "failed"
	}
	if lastResult > lastUser {
		// A terminal row does not prove that the two reports are complete or
		// correct. Keep the neutral state and let the user inspect the record.
		return "finished"
	}
	return "interrupted"
}

// taskAssetMapping starts a visible topology-mapping conversation for the task.
// The asset relationship graph remains sourced directly from the database and
// does not depend on this conversation. The conversation is the audit trail for
// the two human-readable reports (asset relationships + network topology).
func (s *Server) taskAssetMapping(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if s.m.pg == nil {
		writeErr(w, http.StatusServiceUnavailable, s.chatUnavailableReason())
		return
	}
	if agentInfo, err := s.m.pg.GetAgentByKey("asset_mapper"); err != nil || agentInfo == nil || !agentInfo.Enabled {
		writeErr(w, http.StatusServiceUnavailable, "网络拓扑梳理 Agent 未启用")
		return
	}
	mappingConversation := &db.Conversation{AgentKey: "asset_mapper"}
	if s.resolveChatAgent(mappingConversation) == nil {
		writeErr(w, http.StatusServiceUnavailable, s.chatUnavailableReason(mappingConversation))
		return
	}
	taskID, _ := strconv.ParseInt(t.ID, 10, 64)
	message := fmt.Sprintf(`请重新梳理任务 #%d 的网络拓扑，并在同一次会话中输出两份相互独立的结果。

三层区域是本任务拓扑的硬约束，不是可忽略的排版：
- internet（互联网区）：公网根域、子域名、公共 IP 和外部入口。
- boundary（边界区）：对外暴露的服务、网关、Web 应用或端点；只有存在公开主机/URL证据时才归入此层。
- internal（内网区）：私有 IP、回环/链路本地地址及其承载的业务应用、端点和数据库。
节点摘要中的 zone/zone_reason 是后端根据真实地址和资产证据计算的权威归类；不得仅按 kind 改写区域。没有 zone 或证据不足时必须标记“待确认”，不能臆造跨区连接。

输出必须严格使用下面两个一级标题，不能合并成一份：
## 资产关系结果
- 只根据当前任务及其直接关联任务的真实资产数据，保留资产库中的层级和归属关系。
- 这部分是资产关系图的说明，不要声称本次 Agent 改写了资产关系，也不要补充摘要中没有的资产。

## 网络拓扑结果
- 只整理互联网入口、边界服务、内网业务三层网络关系，并说明每个节点所属区域。
- 只展示有真实资产证据的连接；数据库节点标注可识别的数据库类型。

两份结果都只能使用当前任务及其直接关联任务的真实资产数据；不要探测目标、不要修改资产库，也不要把没有证据的连接描述成已验证通路。`, taskID)
	if as := s.m.Assets(); as != nil {
		if graph, err := as.BuildCoverageGraph(taskID, t.ExpID); err == nil {
			type nodeSample struct {
				Key, Kind, Label, Domain, IP, Zone, ZoneReason string
				Port                                           int
				Tested, InScope                                bool
			}
			counts := map[string]int{}
			sample := make([]nodeSample, 0, 60)
			for _, node := range graph.Nodes {
				counts[node.Kind]++
				if len(sample) < 60 {
					sample = append(sample, nodeSample{
						Key: node.Key, Kind: node.Kind, Label: node.Label, Domain: node.Domain,
						IP: node.IP, Port: node.Port, Tested: node.Tested, InScope: node.InScope,
						Zone: node.Zone, ZoneReason: node.ZoneReason,
					})
				}
			}
			edges := graph.Edges
			truncatedEdges := len(edges) > 80
			if truncatedEdges {
				edges = edges[:80]
			}
			snapshot, _ := json.Marshal(map[string]any{
				"nodes_total": len(graph.Nodes), "edges_total": len(graph.Edges),
				"counts_by_kind": counts, "node_sample": sample, "edge_sample": edges,
				"truncated": len(graph.Nodes) > len(sample) || truncatedEdges,
			})
			message += "\n\n页面刚读取的任务资产图真实摘要如下；只依据摘要和任务范围解释，避免把全局资产误当成本任务资产。未列出的节点不要逐一补写：\n" + string(snapshot)
		}
	}
	title := fmt.Sprintf("资产与网络梳理 · 任务 #%d", taskID)
	conversation, err := s.m.pg.CreateConversation("asset_mapper", title, nil)
	if err != nil || conversation == nil {
		writeErr(w, http.StatusInternalServerError, "创建拓扑梳理会话失败")
		return
	}
	if err := appendTopologyMappingAssociation(s.m.pg, conversation.ID, taskID); err != nil {
		_ = s.m.pg.DeleteConversation(conversation.ID) // no run has been started
		writeErr(w, http.StatusInternalServerError, "保存拓扑梳理任务关联失败")
		return
	}
	if _, err := s.m.pg.AppendConvActivity(conversation.ID, db.Activity{
		Worker: "asset_mapper", Kind: "user", Summary: title, Detail: message,
	}); err != nil {
		_ = s.m.pg.DeleteConversation(conversation.ID) // no run has been started
		writeErr(w, http.StatusInternalServerError, "写入拓扑梳理会话失败")
		return
	}
	busyKey := s.convBusyKey(conversation.ID)
	s.chatMu.Lock()
	s.chatBusy[busyKey] = true
	s.chatMu.Unlock()
	s.runConversation(conversation, message, busyKey, message)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "accepted", "agent_key": "asset_mapper", "conversation_id": conversation.ID,
	})
}

func (s *Server) taskAssetMappingStatus(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	title := fmt.Sprintf("资产与网络梳理 · 任务 #%s", t.ID)
	legacyTitle := fmt.Sprintf("资产梳理 · 任务 #%s", t.ID)
	var id, lastUser, lastResult, lastFailure int64
	var resultFailed bool
	var nativeAssociation bool
	var terminalAt time.Time
	err := s.m.pg.QueryRowContext(r.Context(), `SELECT c.id,
 EXISTS(SELECT 1 FROM conversation_activities a WHERE a.conversation_id=c.id AND a.kind=$1 AND a.worker='system' AND a.detail=$2),
 COALESCE((SELECT max(a.id) FROM conversation_activities a WHERE a.conversation_id=c.id AND a.kind='user'),0),
 COALESCE((SELECT a.id FROM conversation_activities a WHERE a.conversation_id=c.id AND a.kind='result' ORDER BY a.id DESC LIMIT 1),0),
 COALESCE((SELECT a.is_error FROM conversation_activities a WHERE a.conversation_id=c.id AND a.kind='result' ORDER BY a.id DESC LIMIT 1),false),
 COALESCE((SELECT max(a.id) FROM conversation_activities a WHERE a.conversation_id=c.id AND a.is_error AND a.kind IN ('result','text')),0),
 COALESCE((SELECT a.created_at FROM conversation_activities a WHERE a.conversation_id=c.id AND a.kind IN ('result','text') ORDER BY a.id DESC LIMIT 1),c.updated_at)
FROM conversations c
WHERE c.agent_key='asset_mapper' AND (
 EXISTS(SELECT 1 FROM conversation_activities a WHERE a.conversation_id=c.id AND a.kind=$1 AND a.worker='system' AND a.detail=$2)
 OR (c.title IN ($3,$4) AND NOT EXISTS(SELECT 1 FROM conversation_activities a WHERE a.conversation_id=c.id AND a.kind=$1)))
ORDER BY c.created_at DESC,c.id DESC LIMIT 1`, topologyMappingAssociationKind, t.ID, title, legacyTitle).Scan(&id, &nativeAssociation, &lastUser, &lastResult, &resultFailed, &lastFailure, &terminalAt)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusOK, topologyMappingState{Status: "none"})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "拓扑梳理状态读取失败")
		return
	}
	s.chatMu.Lock()
	running := s.chatBusy[s.convBusyKey(id)]
	s.chatMu.Unlock()
	state := topologyMappingState{Running: running, ConversationID: id, Status: topologyMappingOutcome(running, lastUser, lastResult, lastFailure, resultFailed)}
	state.Association = "legacy_title"
	if nativeAssociation {
		state.Association = "native"
	}
	if !running && (lastResult > lastUser || lastFailure > lastUser) {
		state.FinishedAt = terminalAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, state)
}
