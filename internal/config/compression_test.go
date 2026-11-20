package config

import "testing"

// Lossless-first compression config (CHECKLIST 183-184): the
// per-model lossless list must name real models with no duplicates.
// There is no lossy setting to validate — lossy is per-call only
// and lives in the request body, never in this file.
func TestCompressionConfigValidate(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Compression = &CompressionConfig{LosslessModels: []string{"gpt-4o", "claude-x"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid compression config: %v", err)
	}

	cfg.Compression = &CompressionConfig{LosslessModels: []string{"gpt-4o", ""}}
	if err := cfg.Validate(); err == nil {
		t.Errorf("empty model entry accepted, want rejection")
	}

	cfg.Compression = &CompressionConfig{LosslessModels: []string{"gpt-4o", "gpt-4o"}}
	if err := cfg.Validate(); err == nil {
		t.Errorf("duplicate model entry accepted, want rejection")
	}

	cfg.Compression = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("nil compression block: %v", err)
	}
	cfg.Compression = &CompressionConfig{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty compression block: %v", err)
	}
}

func TestCompressionConfigCloneIndependent(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Compression = &CompressionConfig{LosslessModels: []string{"gpt-4o"}}
	clone := cfg.Clone()
	if clone.Compression == cfg.Compression {
		t.Fatal("Clone shares the Compression pointer")
	}
	clone.Compression.LosslessModels[0] = "changed"
	clone.Compression.LosslessModels = append(clone.Compression.LosslessModels, "extra")
	if cfg.Compression.LosslessModels[0] != "gpt-4o" || len(cfg.Compression.LosslessModels) != 1 {
		t.Fatalf("original compression list mutated by clone: %v", cfg.Compression.LosslessModels)
	}
}
