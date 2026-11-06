package agent

import (
	"context"
	"encoding/json"
	"fmt"

	actool "github.com/Autumn-27/norma/tool"
)

// taskToolRegistry is an immutable, per-native-run capability source. It does
// not grant permissions: the server still selects tools from its enabled,
// role-bound catalog rows. The pointer is also the invocation's run token, so
// reusing an old tool in a new run with identical numeric IDs is rejected.
type taskToolRegistry struct {
	role    string
	owner   RunInfo
	toolset *ToolSet
	tools   map[string]actool.CoreTool
	err     error
}

type taskToolRegistryKey struct{}
type taskToolRegistryDisabled struct{}

// IsTaskDomainReadTool identifies existing native task readers. No
// writes, execution tools or newly enabled catalog defaults are added here.
func IsTaskDomainReadTool(name string) bool {
	switch name {
	case "graph_overview", "list_findings", "list_facts", "node_detail", "list_goals",
		"get_worker_output", "get_worker_trace", "list_worker_traces", "search_all_worker_traces",
		"list_assets", "list_companies":
		return true
	default:
		return false
	}
}

func nativeTaskToolRole(role string) bool {
	return role == "worker" || role == "planner" || role == "mainagent"
}

// WithoutTaskToolSet shadows an inherited native-run registry. A Chat with
// task attribution is not a native Worker/Planner/Main run and must never gain
// exploration tools just because its RunInfo mentions a parent task.
func WithoutTaskToolSet(ctx context.Context) context.Context {
	return context.WithValue(ctx, taskToolRegistryKey{}, taskToolRegistryDisabled{})
}

// WithTaskToolSet publishes only readers from this exact configured ToolSet.
// The ToolSet is not cloned or rebound: its stores, owner and callbacks remain
// run-local, and server-level registries remain taskless and immutable.
func WithTaskToolSet(ctx context.Context, role string, ts *ToolSet) context.Context {
	if !nativeTaskToolRole(role) {
		return WithoutTaskToolSet(ctx)
	}
	reg := &taskToolRegistry{role: role, owner: RunInfoFrom(ctx), toolset: ts, tools: map[string]actool.CoreTool{}}
	if !reg.matchesStore() {
		reg.err = fmt.Errorf("本轮任务工具实例与任务归属不一致，未装配探索图读取工具")
		return context.WithValue(ctx, taskToolRegistryKey{}, reg)
	}
	candidates := []actool.CoreTool{
		ts.graphOverview(), ts.listFindings(), ts.listFacts(), ts.nodeDetail(), ts.listGoals(),
		ts.getWorkerOutput(), ts.getWorkerTrace(), ts.listWorkerTraces(), ts.searchAllWorkerTraces(),
		ts.listAssets(), ts.listCompanies(),
	}
	// Respect run-local feature ceilings, including coverage opt-outs. This is
	// deliberately separate from catalog seeding and never changes bindings.
	for _, tool := range ts.DropCoverageTools(candidates) {
		if !IsTaskDomainReadTool(tool.Name()) || !tool.IsReadOnly(nil) {
			reg.err = fmt.Errorf("本轮任务只读工具候选元数据不一致")
			break
		}
		reg.tools[tool.Name()] = &taskDomainReadTool{CoreTool: tool, owner: reg}
	}
	return context.WithValue(ctx, taskToolRegistryKey{}, reg)
}

func (r *taskToolRegistry) matchesStore() bool {
	return r.toolset != nil && r.toolset.ts != nil && r.owner.TaskID > 0 && r.owner.ExplorationID > 0 &&
		r.toolset.taskID == r.owner.TaskID && r.toolset.ts.ID() == r.owner.ExplorationID
}

// TaskDomainTool resolves a catalog-authorized missing reader. managed reports
// whether this name belongs to the task-reader set. A nil tool without an
// error means the caller is legitimately taskless; omit it and report an
// assembly warning rather than advertising a permanently unusable nil-store
// implementation. A present but inconsistent registry fails closed.
func TaskDomainTool(ctx context.Context, role, name string) (tool actool.CoreTool, managed bool, err error) {
	if !IsTaskDomainReadTool(name) {
		return nil, false, nil
	}
	r, _ := ctx.Value(taskToolRegistryKey{}).(*taskToolRegistry)
	if r == nil {
		return nil, true, nil
	}
	if r.err != nil {
		return nil, true, r.err
	}
	if role != r.role || !nativeTaskToolRole(role) || RunInfoFrom(ctx) != r.owner || !r.matchesStore() {
		return nil, true, fmt.Errorf("本轮任务工具归属已变化，未装配探索图读取工具")
	}
	tool, ok := r.tools[name]
	if !ok {
		return nil, true, fmt.Errorf("本轮任务工具受功能限制，不使用全局任务工具补回")
	}
	return tool, true, nil
}

type taskDomainReadTool struct {
	actool.CoreTool
	owner *taskToolRegistry
}

func (t *taskDomainReadTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if ctx.Err() != nil {
		return actool.Errorf("任务工具调用已取消，未读取探索图。"), nil
	}
	r, _ := ctx.Value(taskToolRegistryKey{}).(*taskToolRegistry)
	if r != t.owner || r == nil || r.err != nil || RunInfoFrom(ctx) != r.owner || !r.matchesStore() {
		return actool.Errorf("任务工具实例不属于本轮任务上下文，未读取探索图。"), nil
	}
	return t.CoreTool.Call(ctx, in, tc)
}
