// G19 Area 1 vision tests: models.dev [modalities] parsing and
// Plan vision updates.
package modelsync

import (
	"testing"
)

func TestG19ParseCatalogModalities(t *testing.T) {
	data := []byte(`{
		"openai": {"id": "openai", "name": "OpenAI", "models": {
			"gpt-4o": {"id": "gpt-4o", "modalities": {"input": ["text", "image"], "output": ["text"]}},
			"gpt-4o-text": {"id": "gpt-4o-text", "modalities": {"input": ["text"], "output": ["text"]}},
			"o1": {"id": "o1", "modalities": {"input": ["Text", "IMAGE"], "output": ["text"]}},
			"legacy": {"id": "legacy"}
		}}
	}`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	vision, ok := cat.Providers["openai"].Models["gpt-4o"]
	if !ok {
		t.Fatal("gpt-4o missing from parsed catalog")
	}
	if !vision.ModalitiesReported || !vision.SupportsVision {
		t.Fatalf("gpt-4o reported=%v vision=%v, want true/true", vision.ModalitiesReported, vision.SupportsVision)
	}
	textOnly := cat.Providers["openai"].Models["gpt-4o-text"]
	if !textOnly.ModalitiesReported || textOnly.SupportsVision {
		t.Fatalf("gpt-4o-text reported=%v vision=%v, want true/false", textOnly.ModalitiesReported, textOnly.SupportsVision)
	}
	upper := cat.Providers["openai"].Models["o1"]
	if !upper.SupportsVision {
		t.Fatal("o1 with [Text, IMAGE] input not reported as vision-capable (match must be case-insensitive)")
	}
	legacy := cat.Providers["openai"].Models["legacy"]
	if legacy.ModalitiesReported || legacy.SupportsVision {
		t.Fatalf("legacy (no modalities table) reported=%v vision=%v, want false/false", legacy.ModalitiesReported, legacy.SupportsVision)
	}
}

func TestG19PlanVisionUpdates(t *testing.T) {
	data := []byte(`{
		"openai": {"id": "openai", "name": "OpenAI", "models": {
			"gpt-4o": {"id": "gpt-4o", "modalities": {"input": ["text", "image"], "output": ["text"]}},
			"gpt-4o-text": {"id": "gpt-4o-text", "modalities": {"input": ["text"], "output": ["text"]}},
			"unreported": {"id": "unreported"}
		}}
	}`)
	cat, err := ParseCatalog(data)
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	locals := []LocalProvider{{ID: "openai", Models: []LocalModel{
		{ID: "gpt-4o"}, {ID: "gpt-4o-text"}, {ID: "unreported"},
	}}}
	plan := Plan(cat, locals, func(provider, model string) bool { return false }, nil)
	byModel := map[string]Update{}
	for _, u := range plan.Updates {
		byModel[u.Model] = u
	}
	// Vision-capable model: reported + true, counted.
	u, ok := byModel["gpt-4o"]
	if !ok || !u.VisionReported || !u.Vision {
		t.Fatalf("gpt-4o update = %+v, want VisionReported+Vision", u)
	}
	if plan.Counts.VisionsApplied != 1 {
		t.Fatalf("VisionsApplied = %d, want 1", plan.Counts.VisionsApplied)
	}
	// Text-only model with a reported table: reported + false
	// (clears a previously synced flag).
	u, ok = byModel["gpt-4o-text"]
	if !ok || !u.VisionReported || u.Vision {
		t.Fatalf("gpt-4o-text update = %+v, want VisionReported + Vision=false", u)
	}
	// No modalities table: no vision update at all.
	if u, ok := byModel["unreported"]; ok && u.VisionReported {
		t.Fatalf("unreported update = %+v, want no vision report", u)
	}
}
