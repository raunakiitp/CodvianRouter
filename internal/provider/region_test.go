package provider

// Region fields (CHECKLIST 257): operator-declared regions on
// provider/model entries, never guessed — undeclared means
// unknown, never a default.

import "testing"

func TestEffectiveRegion(t *testing.T) {
	cases := []struct {
		name, providerRegion, modelRegion, want string
	}{
		{"model wins over provider", "us-east-1", "eu-west-1", "eu-west-1"},
		{"model inherits provider", "us-east-1", "", "us-east-1"},
		{"both empty is unknown", "", "", ""},
		{"whitespace trims, never defaults", " us-east-1 ", " ", "us-east-1"},
	}
	for _, tc := range cases {
		if got := EffectiveRegion(tc.providerRegion, tc.modelRegion); got != tc.want {
			t.Errorf("%s: EffectiveRegion(%q, %q) = %q, want %q", tc.name, tc.providerRegion, tc.modelRegion, got, tc.want)
		}
	}
}

func TestProviderRegionFor(t *testing.T) {
	p := &Provider{
		Region: "us-east-1",
		Models: []Model{
			{ID: "m", Region: "eu-west-1"},
			{ID: "n"},
		},
	}
	if got := p.RegionFor("m"); got != "eu-west-1" {
		t.Errorf("RegionFor(m) = %q, want eu-west-1 (model wins)", got)
	}
	if got := p.RegionFor("n"); got != "us-east-1" {
		t.Errorf("RegionFor(n) = %q, want us-east-1 (provider fallback)", got)
	}
	if got := p.RegionFor("unlisted"); got != "us-east-1" {
		t.Errorf("RegionFor(unlisted) = %q, want us-east-1 (provider fallback)", got)
	}
	if got := (&Provider{}).RegionFor("m"); got != "" {
		t.Errorf("RegionFor on empty provider = %q, want \"\" (unknown, never a default)", got)
	}
	if got := (*Provider)(nil).RegionFor("m"); got != "" {
		t.Errorf("RegionFor on nil provider = %q, want \"\"", got)
	}
}

func TestProviderRegionRoundTrip(t *testing.T) {
	p := &Provider{
		ID: "prov-a", Name: "A", BaseURL: "https://a.example/v1",
		Region: "ap-south-1",
		Models: []Model{{ID: "m", Region: "eu-west-1"}},
	}
	cfg := p.ToConfig()
	if cfg.Region != "ap-south-1" || len(cfg.Models) != 1 || cfg.Models[0].Region != "eu-west-1" {
		t.Fatalf("ToConfig lost the region: %+v", cfg)
	}
	restored := ProviderFromConfig(cfg)
	if restored.Region != "ap-south-1" || restored.RegionFor("m") != "eu-west-1" {
		t.Fatalf("ProviderFromConfig lost the region: %+v", restored)
	}
	if got := restored.Clone().RegionFor("m"); got != "eu-west-1" {
		t.Fatalf("Clone lost the region: %q", got)
	}
}

func TestValidateRegion(t *testing.T) {
	for _, ok := range []string{"", "us-east-1", "eu-west-1", "ap-south-1", "US-EAST-1"} {
		if err := ValidateRegion(ok); err != nil {
			t.Errorf("ValidateRegion(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"us east 1", "us-east-1 ", " us-east-1", "eu\twest"} {
		if err := ValidateRegion(bad); err == nil {
			t.Errorf("ValidateRegion(%q) = nil, want an error (a region is one token)", bad)
		}
	}
}
