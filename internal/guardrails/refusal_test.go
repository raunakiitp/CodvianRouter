package guardrails

import "testing"

// TestRefusalCodes pins the contract (CHECKLIST 147, research-log
// entry 179): the policy-refusal code is -32001, the upstream
// failure code is -32603, and the two are distinct — no other code
// may ever equal either.
func TestRefusalCodes(t *testing.T) {
	if PolicyRefusalCode != -32001 {
		t.Fatalf("PolicyRefusalCode = %d, want -32001", PolicyRefusalCode)
	}
	if UpstreamFailureCode != -32603 {
		t.Fatalf("UpstreamFailureCode = %d, want -32603", UpstreamFailureCode)
	}
	if PolicyRefusalCode == UpstreamFailureCode {
		t.Fatal("policy-refusal and upstream-failure codes must differ")
	}
}

// TestStoreEvaluationsByPhaseAction proves Check counts one
// evaluation per call, broken down by phase and action (CHECKLIST
// 147): a clean request is request/allowed, a redacted-but-passing
// response is response/redacted, a blocked MCP tool call is
// tool_args/blocked — and the phases never bleed into each other.
func TestStoreEvaluationsByPhaseAction(t *testing.T) {
	s := NewStoreWith(Config{BlockPII: true, BlockSecrets: true, BlockInjection: false})

	if res := s.Check("hello, write a poem", PhaseRequest); res.Blocked {
		t.Fatal("clean request must not block")
	}
	if res := s.Check("reach me at a@example.com", PhaseResponse); !res.Blocked {
		t.Fatal("PII response must block under BlockPII")
	}
	// Blocking off for secrets here: the store still counts the
	// evaluation, as redacted rather than blocked.
	s2 := NewStore()
	if res := s2.Check("my key is sk-proj-abcdefghij1234567890", PhaseMCPToolArgs); res.Blocked {
		t.Fatal("secret tool args must not block with blocking off")
	}
	// And with blocking on, the same tool_args check blocks.
	s3 := NewStoreWith(Config{BlockSecrets: true})
	if res := s3.Check("my key is sk-proj-abcdefghij1234567890", PhaseMCPToolArgs); !res.Blocked || res.BlockReason != KindSecret {
		t.Fatalf("secret tool args must block with reason secret, got %+v", res)
	}

	evals := func(st *Store) map[string]uint64 {
		m := map[string]uint64{}
		for _, e := range st.Stats().Evaluations {
			m[e.Phase+"/"+e.Action] = e.Count
		}
		return m
	}
	if got := evals(s); got["request/allowed"] != 1 || got["response/blocked"] != 1 || len(got) != 2 {
		t.Fatalf("store evaluations = %+v, want request/allowed 1 + response/blocked 1", got)
	}
	if got := evals(s2); got["tool_args/redacted"] != 1 || len(got) != 1 {
		t.Fatalf("store evaluations = %+v, want tool_args/redacted 1", got)
	}
	if got := evals(s3); got["tool_args/blocked"] != 1 || len(got) != 1 {
		t.Fatalf("store evaluations = %+v, want tool_args/blocked 1", got)
	}
	// The legacy totals still agree with the evaluations: the two
	// blocked calls are the two blocked counts.
	if st := s.Stats(); st.Checked != 2 || st.Blocked != 1 {
		t.Fatalf("store stats = checked %d blocked %d, want 2/1", st.Checked, st.Blocked)
	}
	// A fresh store reports an honest empty evaluation list, not null.
	if got := NewStore().Stats().Evaluations; len(got) != 0 {
		t.Fatalf("fresh store evaluations = %+v, want empty", got)
	}
}
