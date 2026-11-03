package db

// Learning outcomes are host-selected, body-free categories. Never persist a
// provider error, response, target, or model-selected string in these fields.
const memoryKnownResultSQL = `('saved','empty','duplicate','input_invalid','input_sensitive',
 'output_invalid','output_truncated','output_rejected','output_sensitive',
 'source_changed','settings_changed','cleared','provider_error','timeout',
 'storage_error','lease_expired','budget_deferred','no_experience','failed','skipped','legacy_unknown')`

const memoryFailedResultSQL = `('output_invalid','output_truncated','provider_error','timeout',
 'storage_error','lease_expired','budget_deferred','settings_changed','failed')`

// NormalizeMemoryResult makes unknown/empty historical values observable
// without exposing their old contents or inventing a successful empty result.
func NormalizeMemoryResult(result string) string {
	switch result {
	case "saved", "empty", "duplicate", "input_invalid", "input_sensitive",
		"output_invalid", "output_truncated", "output_rejected", "output_sensitive",
		"source_changed", "settings_changed", "cleared", "provider_error", "timeout",
		"storage_error", "lease_expired", "budget_deferred", "no_experience", "failed", "skipped", "legacy_unknown":
		return result
	default:
		return "legacy_unknown"
	}
}

func memorySkipResult(result string) bool {
	switch result {
	case "input_invalid", "input_sensitive", "output_rejected", "output_sensitive",
		"source_changed", "settings_changed", "cleared", "lease_expired":
		return true
	default:
		return false
	}
}

func memoryFailResult(result string) bool {
	switch result {
	case "output_invalid", "output_truncated", "provider_error", "timeout", "storage_error", "budget_deferred", "settings_changed", "failed":
		return true
	default:
		return false
	}
}
