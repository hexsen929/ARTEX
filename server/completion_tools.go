package server

import (
	"encoding/json"
	"log"

	"github.com/Autumn-27/artex/agent"
)

// SeedTool only inserts new rows. Add the optional completion protocol parameter
// to existing system tools without resetting any user-owned configuration.
func (s *Server) seedCompletionTools() {
	for _, seed := range agent.BuiltinToolSeeds() {
		if seed.Key != "prove_goal" && seed.Key != "goal_met" {
			continue
		}
		properties, ok := seed.Schema["properties"].(map[string]any)
		if !ok {
			continue
		}
		parameter, err := json.Marshal(properties["remaining_work"])
		if err != nil {
			log.Printf("[completion] marshal %s schema: %v", seed.Key, err)
			continue
		}
		// This idempotent update merges one absent property only. Keeping it
		// conditional also preserves custom schemas after the first upgrade.
		_, err = s.m.pg.Exec(`UPDATE tools SET schema=jsonb_set(schema,'{properties}',
		 COALESCE(schema->'properties','{}'::jsonb)||jsonb_build_object('remaining_work',$2::jsonb)),updated_at=now()
		 WHERE key=$1 AND system AND jsonb_typeof(schema)='object'
		 AND NOT COALESCE(schema->'properties','{}'::jsonb) ? 'remaining_work'`, seed.Key, string(parameter))
		if err != nil {
			log.Printf("[completion] upgrade %s schema: %v", seed.Key, err)
		}
	}
}
