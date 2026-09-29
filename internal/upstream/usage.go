package upstream

// normalizeUsageCacheAliases keeps cache-hit aliases consistent before the
// response leaves the gateway. Some WorkBuddy responses carry the real hit in
// prompt_tokens_details.cached_tokens while also emitting
// cache_read_input_tokens: 0 and cached_tokens: 0 compatibility aliases.
// Strict downstream parsers may prefer those zero aliases and lose the hit.
func normalizeUsageCacheAliases(usage map[string]any) map[string]any {
	best, ok := bestUsageCacheHitTokens(usage)
	if !ok || best <= 0 {
		return usage
	}

	out := cloneUsageMap(usage)
	out["cache_read_input_tokens"] = best
	out["cached_tokens"] = best
	out["prompt_cache_hit_tokens"] = best

	promptDetails := cloneUsageDetails(out, "prompt_tokens_details")
	promptDetails["cached_tokens"] = best
	out["prompt_tokens_details"] = promptDetails

	// Responses API consumers use this nested form. Preserve it when the
	// upstream already supplies it, but do not invent it for Chat-only clients.
	if _, exists := out["input_tokens_details"]; exists {
		inputDetails := cloneUsageDetails(out, "input_tokens_details")
		inputDetails["cached_tokens"] = best
		out["input_tokens_details"] = inputDetails
	}

	return out
}

func bestUsageCacheHitTokens(usage map[string]any) (float64, bool) {
	paths := []struct {
		section string
		key     string
	}{
		{"prompt_tokens_details", "cached_tokens"},
		{"", "prompt_cache_hit_tokens"},
		{"", "cache_read_input_tokens"},
		{"", "cached_tokens"},
		{"input_tokens_details", "cached_tokens"},
	}

	for _, path := range paths {
		var value any
		if path.section == "" {
			value = usage[path.key]
		} else if details, ok := usage[path.section].(map[string]any); ok {
			value = details[path.key]
		}
		if tokens, ok := positiveUsageNumber(value); ok {
			return tokens, true
		}
	}
	return 0, false
}

func positiveUsageNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, n > 0
	case float32:
		value := float64(n)
		return value, value > 0
	case int:
		return float64(n), n > 0
	case int64:
		return float64(n), n > 0
	case int32:
		return float64(n), n > 0
	case uint:
		return float64(n), n > 0
	case uint64:
		return float64(n), n > 0
	case uint32:
		return float64(n), n > 0
	default:
		return 0, false
	}
}

func cloneUsageMap(usage map[string]any) map[string]any {
	out := make(map[string]any, len(usage))
	for key, value := range usage {
		out[key] = value
	}
	return out
}

func cloneUsageDetails(usage map[string]any, key string) map[string]any {
	out := make(map[string]any)
	details, _ := usage[key].(map[string]any)
	for detailKey, value := range details {
		out[detailKey] = value
	}
	return out
}
