// Package modality implements the modality bridges (B3-24,
// PARITY "Modality bridges (vision/audio/video reroute)"): when a
// request carries content in a modality its target model cannot
// serve, the request is rerouted to a model that can — the same
// shape as OmniRoute's Vision/Audio/VideoBridge guardrails
// (registry.ts:308-310), which reroute image-bearing requests to
// the fastest available vision-capable model and describe media
// via a capable model for combos.
//
// What this slice builds: request-modality detection, the
// capability check, and the reroute decision + pool pick. What it
// deliberately does NOT build yet: describing media through a
// vision model / transcribing audio through an STT model and
// replacing the parts with text (OmniRoute's combo behaviour),
// the bridge result cache, and the modality-bridge stats — those
// need a describe/transcribe call path that does not exist here
// yet. Audio and video detection is implemented, but no
// capability source reports audio/video support today, so those
// modalities always report unknown and fail open (the request
// proceeds unbridged); only vision has a confirmed-capability
// source (Model.VisionKnown, fed by the models.dev sync).
package modality

import "github.com/raunakiitp/codvian-router/internal/provider"

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
// (supported, known). Vision reads Model.Vision/VisionKnown —
// false with known=false means "never reported", never a claim
// of text-only. Audio and video have no capability source today,
// so they always report unknown.
func Supports(m provider.Model, mod Modality) (supported, known bool) {
	if mod == Vision {
		return m.Vision, m.VisionKnown
	}
	return false, false
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
