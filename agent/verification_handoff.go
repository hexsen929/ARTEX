package agent

import (
	"errors"

	"github.com/Autumn-27/artex/db"
)

var ErrVerificationHandoff = errors.New(db.VerificationHandoffCode)

// Repeated rejected updates end this run with a deterministic diagnosis. No
// additional model request, tool execution, fabricated evidence or replay.
type verificationHandoffError struct{ failure *db.VerificationFieldFailure }

func (e *verificationHandoffError) Error() string {
	return db.VerificationHandoffCode + ": " + e.failure.Message()
}

func (e *verificationHandoffError) Unwrap() []error {
	return []error{ErrToolLoopGuard, ErrVerificationHandoff}
}

func automaticToolHandoff(err error) bool {
	return errors.Is(err, ErrTrafficReadBudget) || errors.Is(err, ErrVerificationHandoff)
}
