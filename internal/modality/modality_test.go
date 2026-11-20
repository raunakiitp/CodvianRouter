package modality

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

func imgMsg(url string) provider.Message {
	return provider.Message{Role: "user", Parts: []provider.ContentPart{
		{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: url}},
	}}
}

func audioMsg() provider.Message {
	return provider.Message{Role: "user", Parts: []provider.ContentPart{
		{Type: "input_audio"},
	}}
}

func textMsg(s string) provider.Message {
	return provider.Message{Role: "user", Parts: []provider.ContentPart{
		{Type: "text", Text: s},
	}}
}

func TestDetectModalities(t *testing.T) {
	msgs := []provider.Message{textMsg("hi"), imgMsg("https://x/img.png"), audioMsg(),
		{Role: "user", Parts: []provider.ContentPart{{Type: "video_url"}}}}
	got := Detect(msgs)
	if len(got) != 3 || got[0] != Vision || got[1] != Audio || got[2] != Video {
		t.Fatalf("Detect = %v, want [vision audio video]", got)
	}
	if got := Detect([]provider.Message{textMsg("hi")}); len(got) != 0 {
		t.Fatalf("text-only Detect = %v, want nil", got)
	}
}

func TestSupportsAudioVideo(t *testing.T) {
	m := provider.Model{Audio: true, AudioKnown: true, Video: false, VideoKnown: true}
	if ok, known := Supports(m, Audio); !ok || !known {
		t.Fatalf("audio Supports = %v,%v want true,true", ok, known)
	}
	if ok, known := Supports(m, Video); ok || !known {
		t.Fatalf("video Supports = %v,%v want false,true", ok, known)
	}
	if _, known := Supports(provider.Model{}, Audio); known {
		t.Fatal("unknown audio must report known=false")
	}
	if _, known := Supports(provider.Model{}, Video); known {
		t.Fatal("unknown video must report known=false")
	}
}

func TestDecideAudioRerouteAndFailOpen(t *testing.T) {
	pool := []Candidate{
		{ProviderID: "p", ModelID: "audio-model", Model: provider.Model{ID: "audio-model", Audio: true, AudioKnown: true}},
	}
	// Target confirmed audio-incapable, pool has a capable model: reroute.
	target := provider.Model{ID: "text-only", Audio: false, AudioKnown: true}
	d, alt := Decide(target, []Modality{Audio}, pool)
	if d != Reroute || alt.ModelID != "audio-model" {
		t.Fatalf("audio Decide = %v %q, want Reroute audio-model", d, alt.ModelID)
	}
	// Target audio capability unknown: fail open, never reroute.
	d, _ = Decide(provider.Model{ID: "unknown"}, []Modality{Audio}, pool)
	if d != Direct {
		t.Fatalf("unknown audio Decide = %v, want Direct", d)
	}
	// Target blocked, no capable pool model: unbridged, proceed.
	d, _ = Decide(target, []Modality{Audio}, nil)
	if d != Unbridged {
		t.Fatalf("no-pool audio Decide = %v, want Unbridged", d)
	}
	// Vision still reroutes as before.
	vpool := []Candidate{
		{ProviderID: "p", ModelID: "vis", Model: provider.Model{ID: "vis", Vision: true, VisionKnown: true}},
	}
	d, alt = Decide(provider.Model{ID: "t", Vision: false, VisionKnown: true}, []Modality{Vision}, vpool)
	if d != Reroute || alt.ModelID != "vis" {
		t.Fatalf("vision Decide = %v %q, want Reroute vis", d, alt.ModelID)
	}
}

func TestDescribeCandidate(t *testing.T) {
	pool := []Candidate{
		{ProviderID: "p", ModelID: "vis", Model: provider.Model{ID: "vis", Vision: true, VisionKnown: true}},
	}
	target := provider.Model{ID: "t", Vision: false, VisionKnown: true}
	c, ok := DescribeCandidate(target, []Modality{Vision}, pool)
	if !ok || c.ModelID != "vis" {
		t.Fatalf("DescribeCandidate = %q,%v want vis,true", c.ModelID, ok)
	}
	// Target not blocked: no describer needed.
	if _, ok := DescribeCandidate(provider.Model{ID: "v", Vision: true, VisionKnown: true}, []Modality{Vision}, pool); ok {
		t.Fatal("capable target must not need a describer")
	}
	// No capable pool model: no describer.
	if _, ok := DescribeCandidate(target, []Modality{Vision}, nil); ok {
		t.Fatal("empty pool must not yield a describer")
	}
}

func TestDescribePromptAndRequest(t *testing.T) {
	if p := DescribePrompt(Vision); !strings.Contains(p, "Describe this image") {
		t.Fatalf("vision prompt = %q", p)
	}
	if p := DescribePrompt(Audio); !strings.Contains(p, "Transcribe") {
		t.Fatalf("audio prompt = %q", p)
	}
	req := BuildDescribeRequest("vis-model", []provider.ContentPart{imgMsg("https://x/i.png").Parts[0]}, Vision)
	if req.Model != "vis-model" || len(req.Messages) != 1 || len(req.Messages[0].Parts) != 2 {
		t.Fatalf("describe request = %+v", req)
	}
	if req.Messages[0].Parts[0].Type != "image_url" || req.Messages[0].Parts[1].Type != "text" {
		t.Fatalf("describe request parts = %+v", req.Messages[0].Parts)
	}
	if req.MaxTokens != 1024 || req.Temperature != 0 {
		t.Fatalf("describe request MaxTokens=%d Temperature=%v", req.MaxTokens, req.Temperature)
	}
	audioReq := BuildDescribeRequest("stt", []provider.ContentPart{{Type: "input_audio"}}, Audio)
	if audioReq.MaxTokens != 4096 {
		t.Fatalf("transcribe MaxTokens=%d want 4096", audioReq.MaxTokens)
	}
}

func TestReplaceMedia(t *testing.T) {
	msgs := []provider.Message{imgMsg("https://x/i.png"), textMsg("keep me")}
	orig := msgs[0].Parts[0]
	out := ReplaceMedia(msgs, Vision, []Description{{MsgIdx: 0, PartIdx: 0, Text: "a red bicycle"}})
	if out[0].Parts[0].Type != "text" || out[0].Parts[0].Text != "[Image description: a red bicycle]" {
		t.Fatalf("replaced part = %+v", out[0].Parts[0])
	}
	if out[1].Parts[0].Text != "keep me" {
		t.Fatalf("unrelated message changed: %+v", out[1])
	}
	if msgs[0].Parts[0] != orig {
		t.Fatal("ReplaceMedia mutated its input")
	}
	// Wrong-modality and out-of-range descriptions are ignored.
	out = ReplaceMedia(msgs, Audio, []Description{{MsgIdx: 0, PartIdx: 0, Text: "x"}, {MsgIdx: 9, PartIdx: 0, Text: "y"}})
	if out[0].Parts[0].Type != "image_url" {
		t.Fatalf("wrong-modality replace applied: %+v", out[0].Parts[0])
	}
}

func TestCacheKey(t *testing.T) {
	p1 := imgMsg("https://x/a.png").Parts[0]
	p2 := imgMsg("https://x/b.png").Parts[0]
	k1, ok1 := CacheKey(Vision, p1)
	k1b, ok1b := CacheKey(Vision, p1)
	k2, ok2 := CacheKey(Vision, p2)
	if !ok1 || !ok1b || !ok2 || k1 != k1b || k1 == k2 {
		t.Fatalf("cache keys: %q,%v %q,%v %q,%v", k1, ok1, k1b, ok1b, k2, ok2)
	}
	// Audio parts carry no payload: never keyed, so two different
	// clips can never share a key.
	if _, ok := CacheKey(Audio, audioMsg().Parts[0]); ok {
		t.Fatal("audio part must not produce a cache key")
	}
	if _, ok := CacheKey(Vision, provider.ContentPart{Type: "text", Text: "hi"}); ok {
		t.Fatal("text part must not produce a vision cache key")
	}
}

func TestCachePutGetExpiryEviction(t *testing.T) {
	c := NewCache(2, time.Hour)
	c.Put("k1", "one", "m1")
	c.Put("k2", "two", "m1")
	if c.Len() != 2 {
		t.Fatalf("Len = %d want 2", c.Len())
	}
	if v, ok := c.Get("k1"); !ok || v != "one" {
		t.Fatalf("Get k1 = %q,%v", v, ok)
	}
	c.Put("k3", "three", "m1") // evicts oldest (k1)
	if _, ok := c.Get("k1"); ok {
		t.Fatal("oldest entry must be evicted past the cap")
	}
	if v, ok := c.Get("k2"); !ok || v != "two" {
		t.Fatalf("Get k2 = %q,%v", v, ok)
	}

	short := NewCache(10, 20*time.Millisecond)
	short.Put("k", "v", "m")
	time.Sleep(40 * time.Millisecond)
	if _, ok := short.Get("k"); ok {
		t.Fatal("expired entry must miss")
	}

	var nilCache *Cache
	if _, ok := nilCache.Get("k"); ok {
		t.Fatal("nil cache Get must miss")
	}
	nilCache.Put("k", "v", "m") // must not panic
	if nilCache.Len() != 0 {
		t.Fatal("nil cache Len must be 0")
	}
}

func TestStats(t *testing.T) {
	s := &Stats{}
	s.RecordDescribe(true)
	s.RecordDescribe(false)
	s.RecordTranscribe(true)
	s.RecordCacheHit()
	s.RecordCacheMiss()
	s.RecordCacheMiss()
	s.RecordDecision(Direct)
	s.RecordDecision(Reroute)
	s.RecordDecision(Unbridged)
	snap := s.Snapshot()
	if snap.DescribesAttempted != 2 || snap.DescribesSucceeded != 1 || snap.DescribesFailed != 1 {
		t.Fatalf("describe stats = %+v", snap)
	}
	if snap.TranscribesAttempted != 1 || snap.TranscribesSucceeded != 1 {
		t.Fatalf("transcribe stats = %+v", snap)
	}
	if snap.CacheHits != 1 || snap.CacheMisses != 2 {
		t.Fatalf("cache stats = %+v", snap)
	}
	if snap.Direct != 1 || snap.Reroute != 1 || snap.Unbridged != 1 {
		t.Fatalf("decision stats = %+v", snap)
	}
	var nilStats *Stats
	nilStats.RecordDescribe(true) // must not panic
	if got := nilStats.Snapshot(); got != (Snapshot{}) {
		t.Fatalf("nil stats snapshot = %+v", got)
	}
}

type fakeDescriber struct {
	describes, transcribes int
	text                   string
	err                    error
	lastReq                provider.ChatRequest
}

func (f *fakeDescriber) Describe(ctx context.Context, model Candidate, req provider.ChatRequest) (string, error) {
	f.describes++
	f.lastReq = req
	return f.text, f.err
}

func (f *fakeDescriber) Transcribe(ctx context.Context, model Candidate, req provider.ChatRequest) (string, error) {
	f.transcribes++
	f.lastReq = req
	return f.text, f.err
}

func TestDescribeMediaCacheHit(t *testing.T) {
	ctx := context.Background()
	model := Candidate{ProviderID: "p", ModelID: "vis", Model: provider.Model{ID: "vis", Vision: true, VisionKnown: true}}
	msgs := []provider.Message{imgMsg("https://x/i.png")}
	cache := NewCache(10, time.Hour)
	stats := &Stats{}
	d := &fakeDescriber{text: "a red bicycle"}

	out, n, err := DescribeMedia(ctx, d, cache, stats, model, Vision, msgs)
	if err != nil || n != 1 || d.describes != 1 {
		t.Fatalf("first describe: out=%v n=%d err=%v describes=%d", out, n, err, d.describes)
	}
	if out[0].Parts[0].Text != "[Image description: a red bicycle]" {
		t.Fatalf("replaced text = %q", out[0].Parts[0].Text)
	}
	if d.lastReq.Model != "vis" {
		t.Fatalf("describer got model %q", d.lastReq.Model)
	}
	// Second call with the same image: cache hit, describer silent.
	out, n, err = DescribeMedia(ctx, d, cache, stats, model, Vision, msgs)
	if err != nil || n != 1 || d.describes != 1 {
		t.Fatalf("cached describe: n=%d err=%v describes=%d", n, err, d.describes)
	}
	if out[0].Parts[0].Text != "[Image description: a red bicycle]" {
		t.Fatalf("cached text = %q", out[0].Parts[0].Text)
	}
	snap := stats.Snapshot()
	if snap.CacheHits != 1 || snap.CacheMisses != 1 || snap.DescribesSucceeded != 1 {
		t.Fatalf("stats = %+v", snap)
	}
	if cache.Len() != 1 {
		t.Fatalf("cache Len = %d want 1", cache.Len())
	}
}

func TestDescribeMediaTranscribeAndError(t *testing.T) {
	ctx := context.Background()
	model := Candidate{ProviderID: "p", ModelID: "stt", Model: provider.Model{ID: "stt", Audio: true, AudioKnown: true}}
	msgs := []provider.Message{audioMsg()}
	stats := &Stats{}
	d := &fakeDescriber{text: "hello world"}

	out, n, err := DescribeMedia(ctx, d, nil, stats, model, Audio, msgs)
	if err != nil || n != 1 || d.transcribes != 1 || d.describes != 0 {
		t.Fatalf("transcribe: n=%d err=%v transcribes=%d describes=%d", n, err, d.transcribes, d.describes)
	}
	if out[0].Parts[0].Text != "[Audio transcript: hello world]" {
		t.Fatalf("transcript text = %q", out[0].Parts[0].Text)
	}

	// Describer error: original messages back, error out, no partial replace.
	d.err = errors.New("upstream 500")
	orig := []provider.Message{imgMsg("https://x/i.png")}
	vmodel := Candidate{ProviderID: "p", ModelID: "vis"}
	_, n, err = DescribeMedia(ctx, d, nil, stats, vmodel, Vision, orig)
	if err == nil || n != 0 {
		t.Fatalf("error path: n=%d err=%v", n, err)
	}
	if got := stats.Snapshot(); got.DescribesFailed != 1 {
		t.Fatalf("failed stat = %+v", got)
	}

	// Nil describer is an error; no media is a no-op.
	if _, _, err := DescribeMedia(ctx, nil, nil, nil, vmodel, Vision, orig); err == nil {
		t.Fatal("nil describer must error")
	}
	if _, n, err := DescribeMedia(ctx, d, nil, nil, vmodel, Vision, []provider.Message{textMsg("hi")}); err != nil || n != 0 {
		t.Fatalf("no-media path: n=%d err=%v", n, err)
	}
}
