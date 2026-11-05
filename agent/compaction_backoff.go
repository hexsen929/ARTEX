package agent

import (
	"errors"
	"fmt"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmguard"
)

type compactionRoundKey struct{}
type compactionRound struct{ calls int }

var errCompactionRoundBudget = errors.New("compaction round budget reached")

func boundedCompressionText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func compressionNodeSummary(n *db.Node) string {
	text := nodeSummary(n)
	if len([]rune(text)) <= 768 {
		return text
	}
	return boundedCompressionText(text, 768) + fmt.Sprintf("（摘要截短，完整内容见 node_detail #%d）", n.ID)
}

func boundedCompactionBlocks(blocks []block) []block {
	var out []block
	for _, item := range blocks {
		members := item.Members
		for len(members) > 24 {
			n := 24
			if len(members) == 25 {
				n = 23
			}
			out = append(out, block{Members: members[:n], Anchors: item.Anchors})
			members = members[n:]
		}
		if len(members) > 0 {
			out = append(out, block{Members: members, Anchors: item.Anchors})
		}
	}
	return out
}

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
