package agent

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// add_intent 的 chain_tags:合法值写入意图 payload(不破坏原有契约),非法值/超 2 个报错。
func TestAddOneIntentChainTags(t *testing.T) {
	d := testDB(t)
	defer d.Close()

	task, err := d.CreateTask("chain tags", "test chain_tags", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	tools := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	tools.SetTaskID(task.ID)

	id, err := tools.addOneIntent(intentItem{Summary: "对 example.com 登录口做 sql 注入测试", ChainTags: []string{" Web ", "sqli?"}})
	_ = id
	if err == nil || !strings.Contains(err.Error(), "chain_tags") {
		t.Fatalf("非法 tag 应报 chain_tags 错, got err=%v", err)
	}

	if _, err := tools.addOneIntent(intentItem{Summary: "x", ChainTags: []string{"web", "ad", "priv"}}); err == nil {
		t.Fatal("超过 2 个 tag 应报错")
	}

	id, err = tools.addOneIntent(intentItem{Summary: "对 example.com 登录口做 sql 注入测试", ChainTags: []string{"web", "PRIV"}})
	if err != nil {
		t.Fatalf("合法 tags 不应报错: %v", err)
	}
	n, err := tools.ts.GetNode(id)
	if err != nil || n == nil {
		t.Fatalf("取意图节点: %v", err)
	}
	var payload struct {
		Summary   string   `json:"summary"`
		ChainTags []string `json:"chain_tags"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		t.Fatalf("payload JSON 无效: %v", err)
	}
	if !slices.Equal(payload.ChainTags, []string{"web", "priv"}) {
		t.Fatalf("chain_tags 应精确等于规整后的 [web priv], got: %#v", payload.ChainTags)
	}
	// 原有字段不受影响
	if payload.Summary != "对 example.com 登录口做 sql 注入测试" {
		t.Fatalf("payload summary 被改变, got: %q", payload.Summary)
	}
}

// L2 meta 铁律:静态常量追加在 planner/worker system 的代码固定尾。
func TestSystemPromptMetaRules(t *testing.T) {
	p := plannerSystem("拿下X", "/data", "/data")
	if !strings.Contains(p, "meta 铁律(反问纪律,全程在场)") {
		t.Fatal("plannerSystem 缺 planner 版 meta 铁律")
	}
	w := workerSystem("", "", "/data", "/data", false)
	if !strings.Contains(w, "meta 铁律(观测真伪与卡住自检)") {
		t.Fatal("workerSystem 缺 worker 版 meta 铁律")
	}
}
