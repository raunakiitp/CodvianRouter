package sla

import "testing"

func TestScoreCompliantBeatsViolating(t *testing.T) {
	p := DefaultPolicy()
	good := Candidate{LatencyMs: 100, HasLatency: true, ErrorRate: 0, HasErrorRate: true, CostPer1MTokens: 1, HasCost: true, Health: 1, HasHealth: true}
	bad := Candidate{LatencyMs: 5000, HasLatency: true, ErrorRate: 0.5, HasErrorRate: true, CostPer1MTokens: 50, HasCost: true, Health: 0, HasHealth: true}
	vg, sg := Score(good, p, 50)
	vb, sb := Score(bad, p, 50)
	if vg != 0 {
		t.Errorf("compliant violation = %v, want 0", vg)
	}
	if vb <= 0 {
		t.Errorf("violating violation = %v, want > 0", vb)
	}
	if sg <= sb {
		t.Errorf("compliant score %v <= violating score %v", sg, sb)
	}
	if !Better(vg, sg, vb, sb, false) || !Better(vg, sg, vb, sb, true) {
		t.Errorf("compliant should beat violating under soft and hard policy")
	}
}

func TestScoreUnknownIsNeutral(t *testing.T) {
	p := DefaultPolicy()
	unknown := Candidate{}
	v, s := Score(unknown, p, 0)
	if v != 0 {
		t.Errorf("unknown violation = %v, want 0 (no violation provable)", v)
	}
	// 0.5 on every component: .35*.5 + .35*.5 + .15*.5 + .10*.5 + .05*.5 = 0.5
	if s != 0.5 {
		t.Errorf("unknown score = %v, want 0.5 (neutral on every component)", s)
	}
}

func TestScoreZeroTolerance(t *testing.T) {
	p := Policy{TargetP95Ms: 2000, MaxErrorRate: 0}
	zero := Candidate{LatencyMs: 100, HasLatency: true, ErrorRate: 0, HasErrorRate: true, CostPer1MTokens: 1, HasCost: true, Health: 1, HasHealth: true}
	nonzero := Candidate{LatencyMs: 100, HasLatency: true, ErrorRate: 0.01, HasErrorRate: true, CostPer1MTokens: 1, HasCost: true, Health: 1, HasHealth: true}
	_, sz := Score(zero, p, 1)
	vn, sn := Score(nonzero, p, 1)
	if vn <= 0 {
		t.Errorf("non-zero error under zero tolerance: violation = %v, want > 0", vn)
	}
	if sn >= sz {
		t.Errorf("non-zero error score %v >= zero-error score %v under zero tolerance", sn, sz)
	}
}

func TestScoreCostCap(t *testing.T) {
	p := Policy{TargetP95Ms: 2000, MaxErrorRate: 0.05, MaxCostPer1MTokens: 5}
	cheap := Candidate{LatencyMs: 100, HasLatency: true, ErrorRate: 0, HasErrorRate: true, CostPer1MTokens: 1, HasCost: true, Health: 1, HasHealth: true}
	pricey := Candidate{LatencyMs: 100, HasLatency: true, ErrorRate: 0, HasErrorRate: true, CostPer1MTokens: 50, HasCost: true, Health: 1, HasHealth: true}
	vc, sc := Score(cheap, p, 50)
	vp, sp := Score(pricey, p, 50)
	if vc != 0 || vp <= 0 {
		t.Errorf("violations cheap=%v pricey=%v, want 0 and > 0", vc, vp)
	}
	if sc <= sp {
		t.Errorf("cheap score %v <= pricey score %v under a cost cap", sc, sp)
	}
}

func TestPolicyResolveDefaults(t *testing.T) {
	p := Policy{MaxErrorRate: -1}.Resolve()
	if p.TargetP95Ms != 2000 || p.MaxErrorRate != 0.05 || p.MaxCostPer1MTokens != 0 || p.HardConstraints {
		t.Errorf("resolved policy = %+v, want defaults 2000/0.05/0/false", p)
	}
	// MaxErrorRate 0 is a real zero-tolerance policy, not
	// "unset": Resolve must not rewrite it to the default.
	p = Policy{TargetP95Ms: 500, MaxErrorRate: 0, MaxCostPer1MTokens: -1}.Resolve()
	if p.TargetP95Ms != 500 || p.MaxErrorRate != 0 || p.MaxCostPer1MTokens != 0 {
		t.Errorf("explicit values changed by Resolve: %+v", p)
	}
}
