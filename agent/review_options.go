package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

type ReviewOptions struct {
	Duration time.Duration
	MaxTurns int
}

func (o ReviewOptions) Normalized() ReviewOptions {
	if o.Duration <= 0 {
		o.Duration = 180 * time.Second
	}
	if o.MaxTurns <= 0 {
		o.MaxTurns = 16
	}
	return o
}

func (p *Planner) SetReviewOptions(fn func() ReviewOptions) { p.reviewOptionsFn = fn }
func (p *Planner) SetReviewSignature(fn func() string)      { p.reviewSignatureFn = fn }
func (p *Planner) CompletionReviewOptions() ReviewOptions {
	if p.reviewOptionsFn != nil {
		return p.reviewOptionsFn().Normalized()
	}
	return (ReviewOptions{}).Normalized()
}
func (p *Planner) CompletionReviewSignature() string {
	profile := p.model
	if p.reviewSignatureFn != nil {
		profile = p.reviewSignatureFn()
	}
	opts := p.CompletionReviewOptions()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d", profile, opts.Duration, opts.MaxTurns)))
	return hex.EncodeToString(sum[:])
}
