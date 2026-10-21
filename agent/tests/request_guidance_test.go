//go:build ignore

package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/intercept"
)

func TestFlagObjectiveFollowsExplicitRequest(t *testing.T) {
	for _, tc := range []struct {
		name, objective, request string
		wantError                bool
	}{
		{"ordinary", "检查登录和上传漏洞", "测试普通应用", false},
		{"hallucinated", "尽力获取有效漏洞与flag", "全面测试应用，整理证据", true},
		{"missing source", "获取flag", "", true},
		{"explicit Chinese", "获取应用中的flag", "尝试获取应用中的flag", false},
		{"explicit English", "retrieve the flag", "find the flag in the lab", false},
		{"get English", "get the flag", "get the flag from the challenge", false},
		{"not English", "get the flag", "do not search for flags", true},
		{"verb substring", "get the flag", "prior target flags are in the transcript", true},
		{"CTF", "获取flag", "这是 CTF 靶场任务", false},
		{"CTF denied", "获取flag", "这是CTF参考资料，但不要搜索flag", true},
		{"denied", "获取flag", "测试普通应用，不需要获取flag", true},
		{"history revoked", "获取flag", "旧任务获取flag的要求已撤销", true},
		{"remove old request", "获取flag", "删除旧任务中的获取flag目标", true},
		{"CLI", "读取命令行 flag 参数", "检查工具配置", false},
		{"CLI English", "check CLI flags", "检查工具配置", false},
		{"quantity beside switch", "flag数量10；检查命令行开关", "检查工具配置", true},
		{"quantity same clause", "flag数量10并检查命令行开关", "检查工具配置", true},
		{"feature", "read feature flags", "检查工具配置", false},
		{"switch", "修改flag标志位", "检查工具配置", false},
		{"cleanup", "删除默认flag引导", "清理错误提示词", false},
		{"mixed maintenance", "获取flag；检查命令行开关", "测试普通应用", true},
		{"mixed denial", "获取flag\n不要扩大测试范围", "测试普通应用", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateFlagObjective(tc.objective, tc.request); (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestSetGoalsRejectsUnrequestedFlagsBeforePersistence(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	for _, tc := range []struct {
		name, root, message, source, deny, objective string
		wantError                                    bool
	}{
		{"normal inspection", "测试普通应用", "", "", "", "检查登录入口", false},
		{"root without flags", "测试普通应用", "", "", "", "获取flag", true},
		{"explicit root", "尝试获取flag", "", "", "", "获取flag", false},
		{"explicit human", "测试普通应用", "这是CTF任务", intercept.BackgroundUserMessage, "", "获取flag", false},
		{"follow-up preserves CTF", "尝试获取flag", "继续", intercept.BackgroundUserMessage, "", "获取flag", false},
		{"follow-up removes flags", "尝试获取flag", "删除获取flag目标", intercept.BackgroundUserMessage, "", "获取flag", true},
		{"model summary", "测试普通应用", "获取flag", "worker_summary", "", "获取flag", true},
		{"current denial overrides root", "尝试获取flag", "不要获取flag", intercept.BackgroundUserMessage, "", "获取flag", true},
		{"legacy root revoked", "尝试获取flag", "", "", "禁止flag/CTF专项搜索，旧目标里的flag已撤销", "获取flag", true},
		{"technical deny unrelated", "这是CTF任务", "", "", "禁止修改feature flags", "获取flag", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, err := d.CreateTask("request fixture", tc.root, nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
			store := d.Exploration(task.ExplorationID)
			if tc.deny != "" {
				if _, err := store.AddConstraint("deny", tc.deny, "human"); err != nil {
					t.Fatal(err)
				}
			}
			tools := NewToolSet(store, "goals")
			tools.SetTaskID(task.ID)
			notified := 0
			tools.notify = func() { notified++ }
			ctx := t.Context()
			if tc.message != "" {
				ctx = intercept.WithReviewContext(ctx, "", intercept.ReviewBackground{Source: tc.source, Text: tc.message})
			}
			input, _ := json.Marshal(goalItem{Text: tc.objective, GoalType: "inspection", RequiredChecks: []string{"检查已授权的入口"}})
			result, err := tools.setGoals().Call(ctx, input, nil)
			if err != nil || result.IsError != tc.wantError {
				t.Fatalf("result=%s err=%v", result.Flatten(), err)
			}
			var count int
			if err := d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal'`, task.ExplorationID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.wantError {
				want = 0
			}
			if count != want || notified != want {
				t.Fatalf("persisted=%d notified=%d want=%d", count, notified, want)
			}
		})
	}
}

func TestSetGoalsChecksRequiredChecksAndKeepsBatchAlignment(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("batch fixture", "测试普通应用", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	tools := NewToolSet(d.Exploration(task.ExplorationID), "goals")
	input := json.RawMessage(`{"goals":[{"text":"检查登录","goal_type":"inspection","required_checks":["获取flag"]},{"text":"检查上传","goal_type":"inspection","required_checks":["测试授权上传入口"]}]}`)
	result, err := tools.setGoals().Call(t.Context(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		IDs    []int64           `json:"ids"`
		Errors map[string]string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(result.Flatten()), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.IDs) != 2 || out.IDs[0] != 0 || out.IDs[1] <= 0 || out.Errors["0"] == "" || len(out.Errors) != 1 {
		t.Fatal(out)
	}
}

func TestTaskRequestGuidanceSurvivesSavedPrompts(t *testing.T) {
	old := PromptOverride
	t.Cleanup(func() { PromptOverride = old })
	PromptOverride = func(string) (string, bool) { return "保存的自定义正文", true }
	for role, prompt := range map[string]string{
		"auto":      chatSystem("auto", "/data", "/data"),
		"pentest":   chatSystem("pentest", "/data", "/data"),
		"planner":   plannerSystem("test app", "/data", "/data"),
		"worker":    workerSystem("", "", "/data", "/data"),
		"mainagent": mainAgentSystem("test app", "/data", "/data"),
	} {
		if !strings.HasPrefix(prompt, "保存的自定义正文") || strings.Count(prompt, taskRequestGuidance) != 1 {
			t.Fatalf("%s lost prompt body or request guidance", role)
		}
	}
}
