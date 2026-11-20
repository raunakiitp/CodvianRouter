package config

import (
	"strings"
	"testing"
)

// Shortlist (CHECKLIST 199-200): the operator's curated model
// shortlist. Validate rejects empty and duplicate entries; entries
// naming models no provider offers are NOT rejected (the shortlist
// is a display pin, not a routing rule — a pinned model that
// disappears is simply not shown). Clone deep-copies the list so
// edits to one copy cannot move the other's pins.
func TestShortlistValidate(t *testing.T) {
	base := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		return cfg
	}
	if cfg := base(); cfg.Shortlist != nil {
		t.Errorf("default shortlist = %v, want nil (no pins)", cfg.Shortlist)
	}
	valid := base()
	valid.Shortlist = []string{"gpt-4o", "prov-a/llama-3"}
	if err := valid.Validate(); err != nil {
		t.Errorf("valid shortlist rejected: %v", err)
	}
	for _, tc := range []struct {
		name      string
		shortlist []string
		wantSub   string
	}{
		{"empty entry", []string{"gpt-4o", ""}, "empty"},
		{"whitespace entry", []string{"  "}, "empty"},
		{"duplicate model", []string{"gpt-4o", "gpt-4o"}, "duplicate"},
		{"duplicate pair", []string{"prov-a/llama-3", "prov-a/llama-3"}, "duplicate"},
		{"model and pair are distinct", []string{"gpt-4o", "prov-a/gpt-4o"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			cfg.Shortlist = tc.shortlist
			err := cfg.Validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Errorf("shortlist %v rejected: %v", tc.shortlist, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("shortlist %v: err = %v, want an error containing %q", tc.shortlist, err, tc.wantSub)
			}
		})
	}
	// An entry naming a model no provider offers is ignored, never
	// an error.
	cfg := base()
	cfg.Shortlist = []string{"model-no-provider-offers"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("unknown-model shortlist entry rejected: %v", err)
	}
}

func TestShortlistCloneDeepCopies(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Shortlist = []string{"gpt-4o", "prov-a/llama-3"}
	cloned := cfg.Clone()
	if len(cloned.Shortlist) != 2 || cloned.Shortlist[0] != "gpt-4o" || cloned.Shortlist[1] != "prov-a/llama-3" {
		t.Fatalf("cloned shortlist = %v, want [gpt-4o prov-a/llama-3]", cloned.Shortlist)
	}
	cloned.Shortlist[0] = "changed"
	if cfg.Shortlist[0] != "gpt-4o" {
		t.Errorf("editing the clone moved the original pin: %v", cfg.Shortlist)
	}
	cfg.Shortlist[1] = "changed-too"
	if cloned.Shortlist[1] != "prov-a/llama-3" {
		t.Errorf("editing the original moved the clone's pin: %v", cloned.Shortlist)
	}
}
