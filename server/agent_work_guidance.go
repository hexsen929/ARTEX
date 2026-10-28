package server

import "github.com/Autumn-27/artex/agent"

// Runtime guidance is shipped with the executable, not written into editable
// prompt versions. Upgrading therefore preserves custom bodies and history.
func agentWorkGuidanceMetadata(key string) (version, guidance string) {
	guidance = agent.AgentWorkGuidance(key)
	if guidance != "" {
		version = agent.AgentWorkGuidanceVersion
	}
	return version, guidance
}
