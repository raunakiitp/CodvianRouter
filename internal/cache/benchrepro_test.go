package cache

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Bench prompts, copied from cmd/codvian-bench (topics x frames):
// the rig assumes token overlap between distinct prompts stays far
// below the 0.85 semantic-cache threshold, so a latency run of 1,000
// distinct prompts is (nearly) cache-clean. This test pins that
// assumption against the real similarity math: if EmbedText or
// Cosine ever changes so same-topic / same-frame pairs cross the
// threshold, the benchmark's latency numbers stop meaning "added
// latency" and start meaning "cache hits".
var benchTopics = []string{
	"photosynthesis in desert cacti", "tidal locking of exoplanets",
	"sourdough starter hydration ratios", "Byzantine fault tolerance proofs",
	"migrating monarch butterflies", "welding aluminium bicycle frames",
	"Old Norse saga manuscripts", "fermentation of kimchi at altitude",
	"orbital rendezvous mathematics", "handloom weaving in Kanchipuram",
	"quantum error correction codes", "composting kitchen scraps safely",
	"medieval siege engine design", "deep-sea hydrothermal vent ecology",
	"restoring vintage fountain pens", "solar sail navigation principles",
	"beekeeping in rooftop apiaries", "cartography of uncharted islands",
	"ferrofluid sculpture techniques", "Andean potato domestication history",
	"luthier wood selection secrets", "glacier retreat measurements",
	"Morse code transmission speed", "bonsai pruning seasonal schedules",
	"ant colony optimisation algorithms", "Victorian greenhouse heating",
	"kayak rolling Eskimo technique", "fungal mycelium network signalling",
	"brass instrument mouthpiece acoustics", "Saharan caravan trade routes",
	"origami tessellation folding patterns", "coral bleaching recovery studies",
	"blacksmith forge temperature control", "Arctic permafrost thaw feedback",
	"calligraphy ink grinding methods", "seismic wave propagation models",
	"urban pigeon navigation abilities", "tapestry weaving loom setups",
	"volcanic ash soil fertility", "lighthouse lens Fresnel design",
	"mushroom foraging safety rules", "river delta sediment deposition",
	"typewriter ribbon replacement steps", "penguin huddling thermodynamics",
	"stained glass leading techniques", "monsoon rainfall prediction models",
	"harpsichord tuning temperaments", "cave painting pigment chemistry",
	"saffron harvesting labour costs", "wind turbine blade erosion",
}

var benchFrames = []string{
	"Explain %s in one sentence.",
	"What causes %s?",
	"Give one little-known fact about %s.",
	"Summarise %s briefly.",
	"How does %s work?",
	"Why does %s matter in practice?",
	"What is the hardest part of %s?",
	"Describe %s to a curious beginner.",
	"What would surprise most people about %s?",
	"List the three key ideas behind %s.",
	"How is %s measured or tested?",
	"What is a common misconception about %s?",
	"Where did %s originate?",
	"What are the risks of getting %s wrong?",
	"Compare %s with its closest alternative.",
	"When is %s the right approach?",
	"What tools are used for %s?",
	"What does the future hold for %s?",
	"How long does %s usually take?",
	"Who relies on %s every day?",
}

func benchPrompt(i int) string {
	return fmt.Sprintf(benchFrames[(i/len(benchTopics))%len(benchFrames)], benchTopics[i%len(benchTopics)])
}

func TestBenchPromptsStayBelowSemanticThreshold(t *testing.T) {
	// Every same-topic pair (20 frames) and every same-frame pair
	// (50 topics) must score below the 0.85 hit threshold, with
	// margin: the rig's "distinct prompts" claim depends on it.
	for topic := 0; topic < len(benchTopics); topic++ {
		base := EmbedText(benchPrompt(topic))
		for frame := 1; frame < len(benchFrames); frame++ {
			other := EmbedText(benchPrompt(frame*len(benchTopics) + topic))
			if sim := Cosine(base, other); sim >= DefaultSemanticThreshold {
				t.Fatalf("same-topic pair (topic %d, frames 0/%d) scores %v, want < %v", topic, frame, sim, DefaultSemanticThreshold)
			}
		}
	}
	for frame := 0; frame < len(benchFrames); frame++ {
		base := EmbedText(benchPrompt(frame * len(benchTopics)))
		for topic := 1; topic < len(benchTopics); topic++ {
			other := EmbedText(benchPrompt(frame*len(benchTopics) + topic))
			if sim := Cosine(base, other); sim >= DefaultSemanticThreshold {
				t.Fatalf("same-frame pair (frame %d, topics 0/%d) scores %v, want < %v", frame, topic, sim, DefaultSemanticThreshold)
			}
		}
	}
}

// TestBenchPromptCycleHitsAreExactOnly documents the RPS cycle's
// cache behaviour: with 1,000 distinct prompts cycling through a
// 500-entry cache, the only hits are exact repeats of prompts
// still resident from the fill (the second half of the cycle) —
// never fuzzy same-topic matches (see the threshold test above).
// A hit refreshes its own entry (store-on-hit), so once the
// second half starts hitting it keeps hitting; the first half
// keeps missing. The count is exact in isolation (no interleaved
// stores): 500 hits per 1,000-prompt cycle after the fill.
func TestBenchPromptCycleHitsAreExactOnly(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	for i := 0; i < 1000; i++ {
		c.SetText(ctx, benchPrompt(i), SemanticEntry{Response: CachedResponse{Content: "x"}}, time.Hour)
	}
	hits := 0
	for i := 0; i < 1000; i++ {
		if _, ok := c.GetText(ctx, benchPrompt(i)); ok {
			hits++
		}
	}
	if hits != 500 {
		t.Fatalf("cycle hits = %d, want 500 (exact repeats of resident prompts only)", hits)
	}
}
