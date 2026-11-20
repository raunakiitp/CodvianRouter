package router

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/modality"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ---------------------------------------------------------------------------
// Modality hooks (B3-24): capability + pool from the registry.
// ---------------------------------------------------------------------------

// testModalityRegistry seeds a registry the way the models.dev sync
// reports it: operator-set flags in the store, then ApplySynced*
// calls that affirmatively report each capability (known=true).
// "ghost" carries an operator-set vision flag with no sync report —
// the bridge must treat it as unknown, never as capable.
func testModalityRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.json")
	store := provider.NewJSONFileStore(path)
	if err := store.Save([]provider.ProviderConfig{
		{ID: "p1", Name: "P1", BaseURL: "https://p1.example/v1", Enabled: true, Models: []provider.Model{
			{ID: "vis", Vision: true},
			{ID: "aud", Audio: true},
			{ID: "vid", Video: true},
			{ID: "all", Vision: true, Audio: true, Video: true},
			{ID: "text"},
			{ID: "ghost", Vision: true},
		}},
		{ID: "p2", Name: "P2", BaseURL: "https://p2.example/v1", Enabled: true, Models: []provider.Model{
			{ID: "aud2", Audio: true},
		}},
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	reg := provider.NewRegistry(store)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, c := range []struct {
		provider, model string
		apply           func(string, string, bool, bool) error
	}{
		{"p1", "vis", reg.ApplySyncedVision},
		{"p1", "aud", reg.ApplySyncedAudio},
		{"p1", "vid", reg.ApplySyncedVideo},
		{"p1", "all", reg.ApplySyncedVision},
		{"p1", "all", reg.ApplySyncedAudio},
		{"p1", "all", reg.ApplySyncedVideo},
		{"p2", "aud2", reg.ApplySyncedAudio},
	} {
		if err := c.apply(c.provider, c.model, true, true); err != nil {
			t.Fatalf("sync %s/%s: %v", c.provider, c.model, err)
		}
	}
	return reg
}

func TestModalityCapabilityFunc(t *testing.T) {
	reg := testModalityRegistry(t)
	hook := ModalityCapabilityFunc(reg)

	m, known := hook("p1", "aud")
	if !known || !m.Audio || !m.AudioKnown || m.Vision || m.Video {
		t.Fatalf("aud record = %+v known=%v, want Audio+AudioKnown only", m, known)
	}
	m, known = hook("p1", "all")
	if !known || !m.Vision || !m.Audio || !m.Video || !m.VisionKnown || !m.AudioKnown || !m.VideoKnown {
		t.Fatalf("all record = %+v known=%v, want every flag+known", m, known)
	}
	// Operator-set flag with no sync report: the model is known,
	// the capability is not proven.
	m, known = hook("p1", "ghost")
	if !known || !m.Vision || m.VisionKnown {
		t.Fatalf("ghost record = %+v known=%v, want Vision without VisionKnown", m, known)
	}
	if m, known := hook("p1", "nope"); known || m != (provider.Model{}) {
		t.Fatalf("unknown model = %+v known=%v, want zero+false", m, known)
	}
	if m, known := hook("nope", "aud"); known || m != (provider.Model{}) {
		t.Fatalf("unknown provider = %+v known=%v, want zero+false", m, known)
	}
	nilHook := ModalityCapabilityFunc(nil)
	if m, known := nilHook("p1", "aud"); known || m != (provider.Model{}) {
		t.Fatalf("nil registry = %+v known=%v, want zero+false", m, known)
	}
}

func TestModalityPoolFunc(t *testing.T) {
	reg := testModalityRegistry(t)
	pool := ModalityPoolFunc(reg)()
	var got []string
	for _, c := range pool {
		got = append(got, c.ProviderID+"/"+c.ModelID)
	}
	want := []string{"p1/vis", "p1/aud", "p1/vid", "p1/all", "p2/aud2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pool = %v, want %v (text-only and ghost excluded)", got, want)
	}
	// Pool candidates carry the full record, so Decide can filter.
	for _, c := range pool {
		if c.ModelID == "aud" && (!c.Model.Audio || !c.Model.AudioKnown) {
			t.Fatalf("aud candidate record = %+v, want Audio+AudioKnown", c.Model)
		}
	}
	if got := ModalityPoolFunc(nil)(); len(got) != 0 {
		t.Fatalf("nil registry pool = %v, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// Bridge reroute (B3-24): maybeBridgeModality with modality hooks.
// ---------------------------------------------------------------------------

// testModalityHooks returns inline capability/pool hooks over a fixed
// record table, so bridge tests need no registry.
func testModalityHooks() (func(providerID, model string) (provider.Model, bool), func() []modality.Candidate) {
	recs := map[string]provider.Model{
		"text-model":   {ID: "text-model", AudioKnown: true, VideoKnown: true, VisionKnown: true},
		"audio-model":  {ID: "audio-model", AudioKnown: true, Audio: true},
		"video-model":  {ID: "video-model", VideoKnown: true, Video: true},
		"vision-model": {ID: "vision-model", VisionKnown: true, Vision: true},
	}
	hook := func(providerID, model string) (provider.Model, bool) {
		m, ok := recs[model]
		return m, ok
	}
	pool := func() []modality.Candidate {
		var out []modality.Candidate
		for _, id := range []string{"audio-model", "video-model", "vision-model"} {
			pid := strings.TrimSuffix(id, "-model")
			out = append(out, modality.Candidate{ProviderID: pid, ModelID: id, Model: recs[id]})
		}
		return out
	}
	return hook, pool
}

func mediaReq(model string, part provider.ContentPart) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: RoleUser, Parts: []provider.ContentPart{part}}},
	}
}

func TestMaybeBridgeModalityAudioVideo(t *testing.T) {
	hook, pool := testModalityHooks()
	cfg := testConfig()
	cfg.ModalityCapability = hook
	cfg.ModalityPool = pool
	cfg.ChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: strings.TrimSuffix(model, "-model"), Model: model}}
	}
	eng := NewEngine(map[string]provider.Client{}, nil, cfg)
	audioPart := provider.ContentPart{Type: "input_audio"}
	videoPart := provider.ContentPart{Type: "video_url"}
	imagePart := provider.ContentPart{Type: "image_url"}

	t.Run("audio-reroutes", func(t *testing.T) {
		chain := []Attempt{{ProviderID: "text", Model: "text-model"}}
		got, req := eng.maybeBridgeModality(chain, mediaReq("text-model", audioPart), nil, "")
		if req.Model != "audio-model" || len(got) == 0 || got[0].ProviderID != "audio" {
			t.Fatalf("audio reroute = chain %v model %q, want audio/audio-model", got, req.Model)
		}
	})
	t.Run("video-reroutes", func(t *testing.T) {
		chain := []Attempt{{ProviderID: "text", Model: "text-model"}}
		got, req := eng.maybeBridgeModality(chain, mediaReq("text-model", videoPart), nil, "")
		if req.Model != "video-model" || len(got) == 0 || got[0].ProviderID != "video" {
			t.Fatalf("video reroute = chain %v model %q, want video/video-model", got, req.Model)
		}
	})
	t.Run("vision-still-reroutes", func(t *testing.T) {
		chain := []Attempt{{ProviderID: "text", Model: "text-model"}}
		got, req := eng.maybeBridgeModality(chain, mediaReq("text-model", imagePart), nil, "")
		if req.Model != "vision-model" || len(got) == 0 || got[0].ProviderID != "vision" {
			t.Fatalf("vision reroute = chain %v model %q, want vision/vision-model", got, req.Model)
		}
	})
	t.Run("unknown-head-fails-open", func(t *testing.T) {
		chain := []Attempt{{ProviderID: "text", Model: "unknown-model"}}
		got, req := eng.maybeBridgeModality(chain, mediaReq("unknown-model", audioPart), nil, "")
		if req.Model != "unknown-model" || len(got) != 1 || got[0].Model != "unknown-model" {
			t.Fatalf("unknown head = chain %v model %q, want untouched", got, req.Model)
		}
	})
	t.Run("no-capable-alternative", func(t *testing.T) {
		// Pool without the audio model: Unbridged proceeds as-is.
		cfg2 := testConfig()
		cfg2.ModalityCapability = hook
		cfg2.ModalityPool = func() []modality.Candidate {
			return []modality.Candidate{{ProviderID: "vision", ModelID: "vision-model", Model: provider.Model{ID: "vision-model", VisionKnown: true, Vision: true}}}
		}
		eng2 := NewEngine(map[string]provider.Client{}, nil, cfg2)
		chain := []Attempt{{ProviderID: "text", Model: "text-model"}}
		got, req := eng2.maybeBridgeModality(chain, mediaReq("text-model", audioPart), nil, "")
		if req.Model != "text-model" || len(got) != 1 {
			t.Fatalf("no alternative = chain %v model %q, want untouched", got, req.Model)
		}
	})
	t.Run("text-only-untouched", func(t *testing.T) {
		chain := []Attempt{{ProviderID: "text", Model: "text-model"}}
		got, req := eng.maybeBridgeModality(chain, testReq(), nil, "")
		if req.Model != "" || len(got) != 1 {
			t.Fatalf("text-only = chain %v model %q, want untouched", got, req.Model)
		}
	})
	t.Run("legacy-vision-only-fallback", func(t *testing.T) {
		// Callers wiring only the old hooks keep vision-only
		// behaviour: vision reroutes, audio fails open.
		cfg3 := testConfig()
		cfg3.VisionCapability = func(providerID, model string) (bool, bool) {
			// Head is confirmed non-vision; the pool candidate
			// carries its own confirmed-capable record.
			return false, true
		}
		// Only the vision model is in the legacy pool.
		cfg3.VisionPool = func() []modality.Candidate {
			return []modality.Candidate{{ProviderID: "vision", ModelID: "vision-model", Model: provider.Model{ID: "vision-model", VisionKnown: true, Vision: true}}}
		}
		cfg3.ChainFor = cfg.ChainFor
		eng3 := NewEngine(map[string]provider.Client{}, nil, cfg3)
		chain := []Attempt{{ProviderID: "text", Model: "text-model"}}
		// Vision reroutes under the legacy hooks.
		got, req := eng3.maybeBridgeModality(chain, mediaReq("text-model", imagePart), nil, "")
		if req.Model != "vision-model" {
			t.Fatalf("legacy vision = model %q, want vision-model", req.Model)
		}
		// Audio fails open under the legacy hooks (no audio source).
		got, req = eng3.maybeBridgeModality(chain, mediaReq("text-model", audioPart), nil, "")
		if req.Model != "text-model" || len(got) != 1 {
			t.Fatalf("legacy audio = chain %v model %q, want untouched", got, req.Model)
		}
	})
	t.Run("no-hooks-untouched", func(t *testing.T) {
		eng4 := NewEngine(map[string]provider.Client{}, nil, testConfig())
		chain := []Attempt{{ProviderID: "text", Model: "text-model"}}
		got, req := eng4.maybeBridgeModality(chain, mediaReq("text-model", audioPart), nil, "")
		if req.Model != "text-model" || len(got) != 1 {
			t.Fatalf("no hooks = chain %v model %q, want untouched", got, req.Model)
		}
	})
}

// TestEngineChatBridgesAudio proves the live path: an audio-bearing
// request through Engine.Chat reroutes off a confirmed
// audio-incapable head to the audio-capable model.
func TestEngineChatBridgesAudio(t *testing.T) {
	hook, pool := testModalityHooks()
	text := errClient(errors.New("text must not serve audio"))
	audio := okClient("heard it")
	cfg := testConfig()
	cfg.ModalityCapability = hook
	cfg.ModalityPool = pool
	cfg.ChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: strings.TrimSuffix(model, "-model"), Model: model}}
	}
	eng := NewEngine(map[string]provider.Client{"text": text, "audio": audio}, nil, cfg)
	req := mediaReq("text-model", provider.ContentPart{Type: "input_audio"})
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "heard it" {
		t.Fatalf("content = %q, want %q", resp.Content, "heard it")
	}
	if text.chatCalls() != 0 {
		t.Fatalf("text client calls = %d, want 0 (must never dial the incapable head)", text.chatCalls())
	}
	if audio.chatCalls() != 1 {
		t.Fatalf("audio client calls = %d, want 1", audio.chatCalls())
	}
	if got := audio.reqs[0].Model; got != "audio-model" {
		t.Fatalf("audio request model = %q, want audio-model", got)
	}
}

// ---------------------------------------------------------------------------
// Engine describer (B3-24): DescribeMedia over the upstream chat path.
// ---------------------------------------------------------------------------

func TestEngineDescriberDescribe(t *testing.T) {
	ctx := context.Background()
	cap := okClient("a red bicycle")
	eng := NewEngine(map[string]provider.Client{"cap": cap}, nil, testConfig())
	d := eng.Describer()
	if d == nil {
		t.Fatal("Describer() = nil, want a real describer")
	}
	cand := modality.Candidate{ProviderID: "cap", ModelID: "vis", Model: provider.Model{ID: "vis", Vision: true, VisionKnown: true}}
	msgs := []provider.Message{{Role: RoleUser, Parts: []provider.ContentPart{
		{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: "https://x/i.png"}},
	}}}
	cache := modality.NewCache(10, time.Hour)
	stats := &modality.Stats{}
	// Cache miss: the description really runs upstream.
	out, n, err := modality.DescribeMedia(ctx, d, cache, stats, cand, modality.Vision, msgs)
	if err != nil || n != 1 {
		t.Fatalf("DescribeMedia = n %d err %v, want 1 nil", n, err)
	}
	if got := out[0].Parts[0].Text; !strings.Contains(got, "a red bicycle") {
		t.Fatalf("replaced text = %q, want it to carry the upstream description", got)
	}
	if cap.chatCalls() != 1 {
		t.Fatalf("upstream calls = %d, want 1", cap.chatCalls())
	}
	if got := cap.reqs[0].Model; got != "vis" {
		t.Fatalf("upstream request model = %q, want vis", got)
	}
	// Cache hit: the upstream is not called again.
	if _, n, err := modality.DescribeMedia(ctx, d, cache, stats, cand, modality.Vision, msgs); err != nil || n != 1 {
		t.Fatalf("second DescribeMedia = n %d err %v, want 1 nil", n, err)
	}
	if cap.chatCalls() != 1 {
		t.Fatalf("upstream calls after cache hit = %d, want still 1", cap.chatCalls())
	}
}

func TestEngineDescriberTranscribe(t *testing.T) {
	ctx := context.Background()
	cap := okClient("hello world")
	eng := NewEngine(map[string]provider.Client{"cap": cap}, nil, testConfig())
	cand := modality.Candidate{ProviderID: "cap", ModelID: "stt", Model: provider.Model{ID: "stt", Audio: true, AudioKnown: true}}
	msgs := []provider.Message{{Role: RoleUser, Parts: []provider.ContentPart{{Type: "input_audio"}}}}
	out, n, err := modality.DescribeMedia(ctx, eng.Describer(), nil, &modality.Stats{}, cand, modality.Audio, msgs)
	if err != nil || n != 1 {
		t.Fatalf("DescribeMedia audio = n %d err %v, want 1 nil", n, err)
	}
	if got := out[0].Parts[0].Text; !strings.Contains(got, "hello world") {
		t.Fatalf("replaced text = %q, want it to carry the upstream transcript", got)
	}
	if cap.chatCalls() != 1 {
		t.Fatalf("upstream calls = %d, want 1", cap.chatCalls())
	}
}

func TestEngineDescriberError(t *testing.T) {
	ctx := context.Background()
	eng := NewEngine(map[string]provider.Client{"cap": errClient(errors.New("upstream down"))}, nil, testConfig())
	cand := modality.Candidate{ProviderID: "cap", ModelID: "vis", Model: provider.Model{ID: "vis", Vision: true, VisionKnown: true}}
	msgs := []provider.Message{{Role: RoleUser, Parts: []provider.ContentPart{
		{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: "https://x/i.png"}},
	}}}
	out, n, err := modality.DescribeMedia(ctx, eng.Describer(), nil, &modality.Stats{}, cand, modality.Vision, msgs)
	if err == nil {
		t.Fatal("DescribeMedia with a failing upstream = nil error, want an error")
	}
	if n != 0 || len(out) != 1 || out[0].Parts[0].Type != "image_url" {
		t.Fatalf("failed describe = n %d out %+v, want original messages untouched", n, out)
	}
}

func TestEngineDescriberNil(t *testing.T) {
	var eng *Engine
	if d := eng.Describer(); d != nil {
		t.Fatalf("nil engine Describer() = %v, want nil", d)
	}
	eng2 := NewEngine(map[string]provider.Client{"cap": okClient("x")}, nil, testConfig())
	req := modality.BuildDescribeRequest("vis", []provider.ContentPart{{Type: "image_url"}}, modality.Vision)
	if _, err := eng2.Describer().Describe(context.Background(), modality.Candidate{}, req); err == nil {
		t.Fatal("Describe with an empty candidate = nil error, want an error")
	}
}
