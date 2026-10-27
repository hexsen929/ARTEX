package sidequestion

import (
	"errors"
	"testing"
)

func TestParentValidationMatchesExclusiveOwnershipAndApprovalCompatibility(t *testing.T) {
	valid := []Parent{
		{ConversationID: 7},
		{TaskID: 11, ExplorationID: 13},
		{TaskID: 11, ExplorationID: 13, IntentID: 17},
		{ApprovalID: 19},
		// The existing SQL approval alternative permits associated context.
		{ApprovalID: 19, ConversationID: 7, TaskID: 11, ExplorationID: 13, IntentID: 17},
	}
	for _, parent := range valid {
		if err := parent.Validate(); err != nil {
			t.Fatalf("valid parent rejected: %+v: %v", parent, err)
		}
	}
	invalid := []Parent{
		{}, {ConversationID: -1}, {TaskID: -1, ExplorationID: 13},
		{ApprovalID: -1, ConversationID: 7}, {ApprovalID: 19, IntentID: -1},
		{TaskID: 11}, {ExplorationID: 13}, {IntentID: 17},
		{ConversationID: 7, TaskID: 11},
		{ConversationID: 7, ExplorationID: 13},
		{ConversationID: 7, IntentID: 17},
		{ConversationID: 7, TaskID: 11, ExplorationID: 13, IntentID: 17},
	}
	for _, parent := range invalid {
		if err := parent.Validate(); !errors.Is(err, ErrInvalidParent) {
			t.Fatalf("invalid parent not classified: %+v: %v", parent, err)
		}
	}
}
