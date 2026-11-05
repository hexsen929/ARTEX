package agent

import (
	"errors"
	"time"

	"github.com/Autumn-27/artex/llmguard"
)

type compactionRoundKey struct{}
type compactionRound struct{ calls int }

var errCompactionRoundBudget = errors.New("compaction round budget reached")

func (c *Compactor) deferCompaction(id int64, err error) {
	if errors.Is(err, errCompactionRoundBudget) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures[id] = min(c.failures[id]+1, 5)
	delay := time.Minute * time.Duration(1<<c.failures[id])
	if kind := llmguard.Classify(err); kind == llmguard.Quota || kind == llmguard.Auth || kind == llmguard.ModelUnavailable {
		delay = 30 * time.Minute
	}
	c.nextRun[id] = time.Now().Add(delay)
}
