package degrade

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// passthroughFakeEngine is a fakeEngine that also serves the three
// single-attempt passthrough surfaces, counting calls.
type passthroughFakeEngine struct {
	fakeEngine
	audio, images, media int
}

func (f *passthroughFakeEngine) AudioPassthrough(_ context.Context, req router.AudioRequest) (*router.AudioResponse, error) {
	f.audio++
	return &router.AudioResponse{StatusCode: 200, ContentType: "audio/mpeg", Body: []byte("ID3"), Model: req.Model}, nil
}

func (f *passthroughFakeEngine) Images(_ context.Context, req provider.ImageRequest) (*provider.ImageResponse, error) {
	f.images++
	return &provider.ImageResponse{}, nil
}

func (f *passthroughFakeEngine) MediaPassthrough(_ context.Context, req provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error) {
	f.media++
	return &provider.MediaPassthroughResponse{}, nil
}

// TestWrapperForwardsPassthroughs proves the serving wrapper exposes
// and forwards audio, images and media passthrough. Without these
// methods the server's optional-interface checks failed on the
// wrapper and those routes answered 503 in production.
func TestWrapperForwardsPassthroughs(t *testing.T) {
	inner := &passthroughFakeEngine{}
	eng := WrapWithSemantic(inner, cache.NewExactCache(10), nil, DefaultPolicy())

	ae, ok := eng.(interface {
		AudioPassthrough(context.Context, router.AudioRequest) (*router.AudioResponse, error)
	})
	if !ok {
		t.Fatal("wrapped engine does not expose AudioPassthrough")
	}
	ie, ok := eng.(interface {
		Images(context.Context, provider.ImageRequest) (*provider.ImageResponse, error)
	})
	if !ok {
		t.Fatal("wrapped engine does not expose Images")
	}
	me, ok := eng.(interface {
		MediaPassthrough(context.Context, provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error)
	})
	if !ok {
		t.Fatal("wrapped engine does not expose MediaPassthrough")
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ { // twice: never served from a cache
		if _, err := ae.AudioPassthrough(ctx, router.AudioRequest{Operation: router.AudioSpeech, Model: "tts-1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := ie.Images(ctx, provider.ImageRequest{}); err != nil {
			t.Fatal(err)
		}
		if _, err := me.MediaPassthrough(ctx, provider.MediaPassthroughRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if inner.audio != 2 || inner.images != 2 || inner.media != 2 {
		t.Fatalf("inner calls audio=%d images=%d media=%d, want 2 each (forwarded, never cached)", inner.audio, inner.images, inner.media)
	}
}

// TestWrapperPassthroughWithoutCapabilityErrors proves a wrapped
// engine lacking a surface gets an honest error, never a fake answer.
func TestWrapperPassthroughWithoutCapabilityErrors(t *testing.T) {
	se, _ := wrapTest(&fakeEngine{}, DefaultPolicy())
	ctx := context.Background()
	if _, err := se.AudioPassthrough(ctx, router.AudioRequest{}); err == nil {
		t.Error("AudioPassthrough on an engine without audio returned nil error")
	}
	if _, err := se.Images(ctx, provider.ImageRequest{}); err == nil {
		t.Error("Images on an engine without images returned nil error")
	}
	if _, err := se.MediaPassthrough(ctx, provider.MediaPassthroughRequest{}); err == nil {
		t.Error("MediaPassthrough on an engine without media returned nil error")
	}
}
