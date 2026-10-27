package agent

import "context"

type skillVisibilityContextKey struct{}

type skillVisibilitySelection struct {
	executionKey  string
	visibilityKey string
}

// WithSkillVisibilityAgent selects a role's Skill visibility for one run while
// keeping the execution role, domain tools and MCP authorization unchanged. The
// override is tied to executionKey so inherited contexts cannot change another
// agent's Skill grants.
func WithSkillVisibilityAgent(ctx context.Context, executionKey, visibilityKey string) context.Context {
	return context.WithValue(ctx, skillVisibilityContextKey{}, skillVisibilitySelection{
		executionKey:  executionKey,
		visibilityKey: visibilityKey,
	})
}

// SkillVisibilityAgent returns the selected Skill role for this execution role.
// An absent or empty selection uses the execution role's ordinary permissions.
func SkillVisibilityAgent(ctx context.Context, executionKey string) string {
	selection, ok := ctx.Value(skillVisibilityContextKey{}).(skillVisibilitySelection)
	if ok && selection.executionKey == executionKey && selection.visibilityKey != "" {
		return selection.visibilityKey
	}
	return executionKey
}
