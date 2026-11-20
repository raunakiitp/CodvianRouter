package router

import (
	"context"
	"fmt"

	"github.com/raunakiitp/codvian-router/internal/modality"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// engineDescriber adapts the Engine to modality.MediaDescriber
// (B3-24 modality bridges): Describe/Transcribe send the request
// through the engine's normal upstream chat path
// (ChatWithChainStrategy) pinned to the candidate's provider and
// model, and return the reply text. Pinning the chain matters:
// the candidate was picked from the pool because THAT provider's
// record for the model is confirmed capable — re-resolving the
// chain could hand the media to another provider whose record for
// the same model is unknown or incapable. The call is a real
// upstream call, subject to the same contract, budget and failover
// treatment as any other chat — never a stub, never canned text.
// It cannot recurse into the bridge: the bridge runs only in the
// Chat/ChatStream entry points, never inside ChatWithChain.
type engineDescriber struct {
	e *Engine
}

// Describer returns the engine's MediaDescriber, backed by its
// upstream chat path. A nil engine yields a nil describer (the
// modality package treats a nil describer as an error, so callers
// fail loudly instead of silently skipping the description).
func (e *Engine) Describer() modality.MediaDescriber {
	if e == nil {
		return nil
	}
	return &engineDescriber{e: e}
}

// Describe asks model to describe the media in req.
func (d *engineDescriber) Describe(ctx context.Context, model modality.Candidate, req provider.ChatRequest) (string, error) {
	return d.run(ctx, model, req)
}

// Transcribe asks model to transcribe the audio in req.
func (d *engineDescriber) Transcribe(ctx context.Context, model modality.Candidate, req provider.ChatRequest) (string, error) {
	return d.run(ctx, model, req)
}

func (d *engineDescriber) run(ctx context.Context, model modality.Candidate, req provider.ChatRequest) (string, error) {
	if d == nil || d.e == nil {
		return "", fmt.Errorf("router: no engine for media describe")
	}
	if model.ProviderID == "" || model.ModelID == "" {
		return "", fmt.Errorf("router: describe candidate has no provider/model")
	}
	req.Model = model.ModelID
	chain := []Attempt{{ProviderID: model.ProviderID, Model: model.ModelID}}
	resp, _, err := d.e.ChatWithChain(ctx, chain, req)
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", fmt.Errorf("router: describe got no response")
	}
	return resp.Content, nil
}
