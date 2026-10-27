package sidequestion

import (
	"errors"
	"fmt"
)

// ErrInvalidParent identifies a permanent checkpoint identity error. Callers
// must not retry the same malformed parent as if it were a database outage.
var ErrInvalidParent = errors.New("invalid side-question parent")

// Validate mirrors the session table's parent-shape constraint while requiring
// positive identifiers. Approval records retain their existing optional context;
// conversation checkpoints and task checkpoints are mutually exclusive parents.
func (p Parent) Validate() error {
	if p.ConversationID < 0 || p.TaskID < 0 || p.ExplorationID < 0 || p.IntentID < 0 || p.ApprovalID < 0 {
		return fmt.Errorf("%w: identifiers must not be negative", ErrInvalidParent)
	}
	if p.ApprovalID > 0 {
		return nil
	}
	if p.ConversationID > 0 {
		if p.TaskID != 0 || p.ExplorationID != 0 || p.IntentID != 0 {
			return fmt.Errorf("%w: conversation and task ownership must not be mixed", ErrInvalidParent)
		}
		return nil
	}
	if p.TaskID > 0 && p.ExplorationID > 0 {
		return nil
	}
	return fmt.Errorf("%w: a conversation, task/exploration or approval is required", ErrInvalidParent)
}
