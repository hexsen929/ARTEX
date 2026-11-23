package server

import "strings"

// The original trigger uses these literal, case-sensitive terms, then admits
// the payout analyst at most once per task for this scheduler lifetime.
var payoutDirectionKeywords = []string{"支付", "收款", "出金", "提现", "充值", "结算", "商户", "分账", "代付", "码商", "跑分", "USDT", "虚拟币", "支付链", "回调地址"}

func moneyDirection(values ...string) bool {
	for _, value := range values {
		for _, word := range payoutDirectionKeywords {
			if strings.Contains(value, word) {
				return true
			}
		}
	}
	return false
}
func (sc *Scheduler) markPayoutFired(task int64) bool {
	if task <= 0 {
		return false
	}
	sc.payoutMu.Lock()
	defer sc.payoutMu.Unlock()
	if sc.payoutFired == nil {
		sc.payoutFired = map[int64]bool{}
	}
	if sc.payoutFired[task] {
		return false
	}
	sc.payoutFired[task] = true
	return true
}
