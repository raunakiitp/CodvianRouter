// Package modality implements the modality bridges (B3-24,
// PARITY "Modality bridges (vision/audio/video reroute)"): when a
// request carries content in a modality its target model cannot
// serve, the request is rerouted to a model that can — the same
// shape as the sector-typical Vision/Audio/VideoBridge guardrails
// (registry.ts:308-310), which reroute image-bearing requests to
// the fastest available vision-capable model and describe media
// via a capable model for combos.
//
// What this slice builds: request-modality detection, the
// capability check, and the reroute decision + pool pick — plus
// the describe/transcribe machinery (the sector-typical combo
// behaviour): building the describe/transcribe request for a
// capable model, replacing media parts with the returned text,
// a result cache for descriptions, and bridge stats. The
// describe/transcribe TRANSPORT lives behind the MediaDescriber
// interface: this package owns the request shape, the cache and
// the stats; the router owns the upstream call and wires a real
// describer (Engine.Describer, backed by its upstream chat path).
// The live request path is modality-aware too: the router's
// ModalityCapability/ModalityPool hooks report the full
// Vision/Audio/Video record, so audio/video reroute live and
// fail open when unknown, exactly as vision does.
// Audio and video capability now HAS a source: the models.dev
// [modalities] table (the same table vision comes from) lists
// audio/video input, and the sync carries it into
// Model.Audio/AudioKnown and Model.Video/VideoKnown — no
// capability is guessed from a model id.
package modality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Modality is one input modality a request can require.
type Modality int

const (
	// Vision is image input (image_url content parts).
	Vision Modality = iota
	// Audio is audio input (input_audio content parts).
	Audio
	// Video is video input (video_url / video content parts).
	Video
)

// Candidate is one pool model the bridge may reroute to.
type Candidate struct {
	ProviderID string
	ModelID    string
	Model      provider.Model
}

// Detect scans message parts for media and returns the required
// modalities, deduped, in Vision/Audio/Video order. Text-only
// requests return nil. Parts the wire layer does not understand
// yet still decode (ContentPart carries the raw type), so a
// client-sent input_audio or video part is detected even though
// no wire converter forwards it today.
func Detect(msgs []provider.Message) []Modality {
	var seen [3]bool
	for _, m := range msgs {
		for _, p := range m.Parts {
			switch p.Type {
			case "image_url":
				seen[Vision] = true
			case "input_audio":
				seen[Audio] = true
			case "video_url", "video":
				seen[Video] = true
			}
		}
	}
	var out []Modality
	for i, s := range seen {
		if s {
			out = append(out, Modality(i))
		}
	}
	return out
}

// Supports reports whether a model serves a modality:
// (supported, known). Vision reads Model.Vision/VisionKnown,
// audio reads Model.Audio/AudioKnown, video reads
// Model.Video/VideoKnown — false with known=false means "never
// reported", never a claim of incapability.
func Supports(m provider.Model, mod Modality) (supported, known bool) {
	switch mod {
	case Vision:
		return m.Vision, m.VisionKnown
	case Audio:
		return m.Audio, m.AudioKnown
	case Video:
		return m.Video, m.VideoKnown
	default:
		return false, false
	}
}

// Decision is the bridge outcome for one request.
type Decision int

const (
	// Direct: the target serves everything required, or nothing
	// required is known to block it — proceed as requested.
	Direct Decision = iota
	// Reroute: the target is confirmed incapable of a required
	// modality and a capable alternative exists in the pool.
	Reroute
	// Unbridged: the target is confirmed incapable but no pool
	// model serves the blocking modality — proceed unbridged;
	// the upstream refuses honestly, exactly as before.
	Unbridged
)

// Decide resolves the bridge for one request: the target model,
// the modalities the request requires, and the candidate pool.
// A target is only ever rerouted away from on a CONFIRMED
// incapability (known && !supported); unknown capability fails
// open. A reroute candidate must be confirmed capable of every
// modality that blocked the target, and confirmed incapable of
// none of the required modalities. The first qualifying pool
// candidate wins — pool order is the ranking.
func Decide(target provider.Model, required []Modality, pool []Candidate) (Decision, Candidate) {
	if len(required) == 0 {
		return Direct, Candidate{}
	}
	var blocked []Modality
	for _, mod := range required {
		ok, known := Supports(target, mod)
		if known && !ok {
			blocked = append(blocked, mod)
		}
	}
	if len(blocked) == 0 {
		return Direct, Candidate{}
	}
	for _, c := range pool {
		if servesAll(c.Model, blocked) && incapableOfNone(c.Model, required) {
			return Reroute, c
		}
	}
	return Unbridged, Candidate{}
}

// servesAll reports whether a model is confirmed capable of every
// listed modality.
func servesAll(m provider.Model, mods []Modality) bool {
	for _, mod := range mods {
		ok, known := Supports(m, mod)
		if !known || !ok {
			return false
		}
	}
	return true
}

// incapableOfNone reports whether a model is confirmed incapable
// of none of the listed modalities (unknown never disqualifies).
func incapableOfNone(m provider.Model, mods []Modality) bool {
	for _, mod := range mods {
		ok, known := Supports(m, mod)
		if known && !ok {
			return false
		}
	}
	return true
}

// String names the modality for cache keys, stats labels and traces.
func (m Modality) String() string {
	switch m {
	case Vision:
		return "vision"
	case Audio:
		return "audio"
	case Video:
		return "video"
	default:
		return "unknown"
	}
}

// mediaNoun is the label used when a media part is replaced by
// text: "[Image description: ...]", "[Audio transcript: ...]",
// "[Video description: ...]".
func (m Modality) mediaNoun() string {
	switch m {
	case Vision:
		return "Image description"
	case Audio:
		return "Audio transcript"
	case Video:
		return "Video description"
	default:
		return "Media description"
	}
}

// isMediaPart reports whether part carries media of the given modality.
func isMediaPart(p provider.ContentPart, mod Modality) bool {
	switch mod {
	case Vision:
		return p.Type == "image_url"
	case Audio:
		return p.Type == "input_audio"
	case Video:
		return p.Type == "video_url" || p.Type == "video"
	default:
		return false
	}
}

// DescribePrompt is the instruction sent to a capable model with
// the media parts: describe what the media shows (vision/video)
// or transcribe it exactly (audio). The reply is text only — the
// caller replaces the media parts with it.
func DescribePrompt(mod Modality) string {
	switch mod {
	case Audio:
		return "Transcribe the audio exactly, word for word. Reply with the transcript only, no preamble."
	case Video:
		return "Describe this video in detail: what it shows, any text or speech in it, and anything else a reader of the conversation would need. Reply with the description only, no preamble."
	default:
		return "Describe this image in detail: what it shows, any text visible in it, and anything else a reader of the conversation would need. Reply with the description only, no preamble."
	}
}

// describeMaxTokens bounds one describe/transcribe reply: a
// description is short by construction; a transcript may run long.
func describeMaxTokens(mod Modality) int {
	if mod == Audio {
		return 4096
	}
	return 1024
}

// BuildDescribeRequest builds the chat request that asks modelID
// (a model confirmed capable of mod) to describe/transcribe the
// given media parts: the parts verbatim, plus the instruction as
// a trailing text part. Temperature 0 keeps the text as
// deterministic as the model allows.
func BuildDescribeRequest(modelID string, parts []provider.ContentPart, mod Modality) provider.ChatRequest {
	parts = append(append([]provider.ContentPart{}, parts...), provider.ContentPart{Type: "text", Text: DescribePrompt(mod)})
	return provider.ChatRequest{
		Model:       modelID,
		Messages:    []provider.Message{{Role: "user", Parts: parts}},
		MaxTokens:   describeMaxTokens(mod),
		Temperature: 0,
	}
}

// DescribeCandidate returns a pool model confirmed capable of
// every modality that blocked the target — the model a
// describe/transcribe call should use when the request cannot be
// rerouted (Decide says Unbridged) but the media can still be
// turned into text. The selection is the reroute's own predicate
// (servesAll + incapableOfNone): a describer that cannot serve
// the blocking modality is no describer at all. False when the
// target is not blocked, or when no pool model qualifies.
func DescribeCandidate(target provider.Model, required []Modality, pool []Candidate) (Candidate, bool) {
	var blocked []Modality
	for _, mod := range required {
		ok, known := Supports(target, mod)
		if known && !ok {
			blocked = append(blocked, mod)
		}
	}
	if len(blocked) == 0 {
		return Candidate{}, false
	}
	for _, c := range pool {
		if servesAll(c.Model, blocked) && incapableOfNone(c.Model, required) {
			return c, true
		}
	}
	return Candidate{}, false
}

// Description is one media part's text replacement: the part at
// (MsgIdx, PartIdx) becomes a text part carrying Text.
type Description struct {
	MsgIdx  int
	PartIdx int
	Text    string
}

// ReplaceMedia swaps each described media part for a text part
// carrying its description ("[Image description: ...]",
// "[Audio transcript: ...]"). The input messages are never
// mutated: the returned slice is a copy. Descriptions naming a
// part that is not a media part of mod, or an out-of-range
// position, are ignored.
func ReplaceMedia(msgs []provider.Message, mod Modality, descs []Description) []provider.Message {
	out := make([]provider.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		out[i].Parts = append([]provider.ContentPart{}, m.Parts...)
	}
	byPos := make(map[[2]int]string, len(descs))
	for _, d := range descs {
		byPos[[2]int{d.MsgIdx, d.PartIdx}] = d.Text
	}
	for i := range out {
		for j := range out[i].Parts {
			text, ok := byPos[[2]int{i, j}]
			if !ok || !isMediaPart(out[i].Parts[j], mod) {
				continue
			}
			out[i].Parts[j] = provider.ContentPart{Type: "text", Text: fmt.Sprintf("[%s: %s]", mod.mediaNoun(), text)}
		}
	}
	return out
}

// DefaultCacheMaxEntries and DefaultCacheTTL bound a bridge
// description cache: descriptions are small text, so a few
// hundred entries cover a session; entries older than the TTL
// are re-described (a model upgrade may describe better).
const (
	DefaultCacheMaxEntries = 512
	DefaultCacheTTL        = time.Hour
)

// Cache is a content-hash-keyed store of describe/transcribe
// results: the same image described twice costs one upstream
// call. Keys come from CacheKey; entries expire after the TTL
// and the oldest entry is evicted past the cap. Safe for
// concurrent use.
type Cache struct {
	mu         sync.Mutex
	entries    map[string]cacheEntry
	seq        uint64
	maxEntries int
	ttl        time.Duration
}

type cacheEntry struct {
	text    string
	modelID string
	at      time.Time
	seq     uint64
}

// NewCache builds a bridge cache; non-positive maxEntries/TTL
// take the defaults.
func NewCache(maxEntries int, ttl time.Duration) *Cache {
	if maxEntries <= 0 {
		maxEntries = DefaultCacheMaxEntries
	}
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Cache{entries: make(map[string]cacheEntry), maxEntries: maxEntries, ttl: ttl}
}

// Get returns a cached description, or false on a miss or an
// expired entry (expired entries are dropped).
func (c *Cache) Get(key string) (string, bool) {
	if c == nil || key == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.at) > c.ttl {
		delete(c.entries, key)
		return "", false
	}
	return e.text, true
}

// Put stores a description under key, evicting the oldest entry
// past the cap.
func (c *Cache) Put(key, text, modelID string) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.maxEntries {
		var oldest string
		oldestSeq := ^uint64(0)
		for k, e := range c.entries {
			if e.seq < oldestSeq {
				oldestSeq, oldest = e.seq, k
			}
		}
		delete(c.entries, oldest)
	}
	c.seq++
	c.entries[key] = cacheEntry{text: text, modelID: modelID, at: time.Now(), seq: c.seq}
}

// Len reports the live entry count.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// CacheKey derives the cache key for one media part: the modality
// plus a hash of the part's distinguishing payload. Only image
// parts carry a payload today (ImageURL.URL); audio and video
// parts have no payload field in wire.ContentPart, so they return
// no key — two different clips must never share one. Callers
// skip the cache for such parts (always describe, never store).
func CacheKey(mod Modality, part provider.ContentPart) (string, bool) {
	if mod != Vision || part.Type != "image_url" || part.ImageURL == nil || part.ImageURL.URL == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(mod.String() + "\x00" + part.ImageURL.URL))
	return hex.EncodeToString(sum[:]), true
}

// Stats counts bridge outcomes for the dashboard: describe and
// transcribe attempts, cache hits and misses, and the reroute
// decisions the bridge made. Safe for concurrent use; Snapshot
// is the read shape.
type Stats struct {
	mu       sync.Mutex
	snapshot Snapshot
}

// Snapshot is one read of the bridge stats.
type Snapshot struct {
	DescribesAttempted   int
	DescribesSucceeded   int
	DescribesFailed      int
	TranscribesAttempted int
	TranscribesSucceeded int
	TranscribesFailed    int
	CacheHits            int
	CacheMisses          int
	Direct               int
	Reroute              int
	Unbridged            int
}

// RecordDescribe records one describe attempt (vision/video).
func (s *Stats) RecordDescribe(ok bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.DescribesAttempted++
	if ok {
		s.snapshot.DescribesSucceeded++
	} else {
		s.snapshot.DescribesFailed++
	}
}

// RecordTranscribe records one transcribe attempt (audio).
func (s *Stats) RecordTranscribe(ok bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.TranscribesAttempted++
	if ok {
		s.snapshot.TranscribesSucceeded++
	} else {
		s.snapshot.TranscribesFailed++
	}
}

// RecordCacheHit records one description served from the cache.
func (s *Stats) RecordCacheHit() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.CacheHits++
}

// RecordCacheMiss records one description that missed the cache.
func (s *Stats) RecordCacheMiss() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.CacheMisses++
}

// RecordDecision records one bridge decision.
func (s *Stats) RecordDecision(d Decision) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch d {
	case Direct:
		s.snapshot.Direct++
	case Reroute:
		s.snapshot.Reroute++
	case Unbridged:
		s.snapshot.Unbridged++
	}
}

// Snapshot returns the current counts.
func (s *Stats) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot
}

// MediaDescriber executes one describe/transcribe call against a
// capable model: it sends req (built by BuildDescribeRequest) and
// returns the model's reply text. The modality package owns the
// request shape, the cache and the stats; the implementation owns
// the transport (the router wires a real describer backed by its
// upstream chat path).
type MediaDescriber interface {
	// Describe asks model to describe the media in req.
	Describe(ctx context.Context, model Candidate, req provider.ChatRequest) (string, error)
	// Transcribe asks model to transcribe the audio in req.
	Transcribe(ctx context.Context, model Candidate, req provider.ChatRequest) (string, error)
}

// mediaPart is one media part's location in a message list.
type mediaPart struct {
	msgIdx  int
	partIdx int
	part    provider.ContentPart
}

// collectMediaParts lists the parts of one modality across all messages.
func collectMediaParts(msgs []provider.Message, mod Modality) []mediaPart {
	var out []mediaPart
	for i, m := range msgs {
		for j, p := range m.Parts {
			if isMediaPart(p, mod) {
				out = append(out, mediaPart{msgIdx: i, partIdx: j, part: p})
			}
		}
	}
	return out
}

// DescribeMedia replaces the media parts of one modality in msgs
// with text produced by a capable model: each part is described
// (vision/video) or transcribed (audio) by model, consulting the
// cache first and recording stats. Parts already cached are never
// re-sent. On any describer error it returns the original
// messages and the error — the caller keeps the request unbridged
// rather than sending a half-described one. A nil cache disables
// caching; a nil stats disables recording; a nil describer is an
// error.
func DescribeMedia(ctx context.Context, d MediaDescriber, cache *Cache, stats *Stats, model Candidate, mod Modality, msgs []provider.Message) ([]provider.Message, int, error) {
	if d == nil {
		return msgs, 0, fmt.Errorf("modality: no describer wired")
	}
	parts := collectMediaParts(msgs, mod)
	if len(parts) == 0 {
		return msgs, 0, nil
	}
	verb := "describe"
	if mod == Audio {
		verb = "transcribe"
	}
	descs := make([]Description, 0, len(parts))
	for _, mp := range parts {
		key, keyed := CacheKey(mod, mp.part)
		if keyed && cache != nil {
			if text, ok := cache.Get(key); ok {
				stats.RecordCacheHit()
				descs = append(descs, Description{MsgIdx: mp.msgIdx, PartIdx: mp.partIdx, Text: text})
				continue
			}
		}
		stats.RecordCacheMiss()
		req := BuildDescribeRequest(model.ModelID, []provider.ContentPart{mp.part}, mod)
		var text string
		var err error
		if mod == Audio {
			text, err = d.Transcribe(ctx, model, req)
		} else {
			text, err = d.Describe(ctx, model, req)
		}
		if err != nil {
			if mod == Audio {
				stats.RecordTranscribe(false)
			} else {
				stats.RecordDescribe(false)
			}
			return msgs, 0, fmt.Errorf("modality: %s %s: %w", verb, mod, err)
		}
		if mod == Audio {
			stats.RecordTranscribe(true)
		} else {
			stats.RecordDescribe(true)
		}
		if keyed && cache != nil {
			cache.Put(key, text, model.ModelID)
		}
		descs = append(descs, Description{MsgIdx: mp.msgIdx, PartIdx: mp.partIdx, Text: text})
	}
	return ReplaceMedia(msgs, mod, descs), len(descs), nil
}
