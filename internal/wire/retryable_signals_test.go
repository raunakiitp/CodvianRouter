package wire

import "testing"

func TestIsQuotaExhaustedSignal(t *testing.T) {
	yes := []string{
		"You exceeded your current quota, please check your plan",
		"insufficient_quota: you have no credits left",
		"insufficient balance for this request",
		"credits exhausted",
		"balance is $0",
		"out of credits",
		"no credits remaining",
		"QUOTA exceeded",
	}
	for _, s := range yes {
		if !IsQuotaExhaustedSignal(s) {
			t.Errorf("IsQuotaExhaustedSignal(%q) = false, want true", s)
		}
	}
	no := []string{
		"rate limit exceeded: too many requests per minute",
		"model not found",
		"internal server error",
		"concurrency limit reached",
		"",
	}
	for _, s := range no {
		if IsQuotaExhaustedSignal(s) {
			t.Errorf("IsQuotaExhaustedSignal(%q) = true, want false", s)
		}
	}
}

func TestIsConcurrencyLimitSignal(t *testing.T) {
	yes := []string{
		"concurrency limit reached for this model",
		"too many concurrent requests",
		"concurrent request limit exceeded",
		"max concurrency exceeded",
		"too many simultaneous requests in flight",
		"concurrency_limit: 429",
	}
	for _, s := range yes {
		if !IsConcurrencyLimitSignal(s) {
			t.Errorf("IsConcurrencyLimitSignal(%q) = false, want true", s)
		}
	}
	no := []string{
		"rate limit exceeded: too many requests per minute",
		"concurrent users online now",
		"quota exceeded",
		"model not found",
		"",
	}
	for _, s := range no {
		if IsConcurrencyLimitSignal(s) {
			t.Errorf("IsConcurrencyLimitSignal(%q) = true, want false", s)
		}
	}
}
