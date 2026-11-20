package catalog

import "testing"

func TestIsDeprecatedSeed(t *testing.T) {
	// Every seed entry must carry a retirement date and the source
	// it was checked against; a bare ID with no date is a guess.
	for id, d := range DeprecatedModels {
		if d.ID != id {
			t.Errorf("key %q holds entry for %q", id, d.ID)
		}
		if d.Retired == "" {
			t.Errorf("deprecated model %q has no retirement date", id)
		}
		if d.Source == "" {
			t.Errorf("deprecated model %q has no source", id)
		}
	}
	if len(DeprecatedModels) == 0 {
		t.Fatal("deprecated seed table is empty")
	}
}

func TestIsDeprecatedExactMatch(t *testing.T) {
	if _, ok := IsDeprecated("gpt-4-0314"); !ok {
		t.Error("gpt-4-0314 should be deprecated")
	}
	if d, ok := IsDeprecated("gpt-4-0314"); !ok || d.Retired != "2026-03-26" {
		t.Errorf("gpt-4-0314 retired = %q, want 2026-03-26", d.Retired)
	}
	// No prefix/substring guessing: a fine-tune or a longer ID that
	// merely contains a retired ID is a different model.
	if _, ok := IsDeprecated("ft-gpt-4-0314"); ok {
		t.Error("ft-gpt-4-0314 must not match by prefix")
	}
	if _, ok := IsDeprecated("gpt-4-031"); ok {
		t.Error("gpt-4-031 must not match by prefix")
	}
	if _, ok := IsDeprecated("gpt-4o"); ok {
		t.Error("gpt-4o must not be deprecated by the seed table")
	}
	if _, ok := IsDeprecated(""); ok {
		t.Error("empty ID must not be deprecated")
	}
}
