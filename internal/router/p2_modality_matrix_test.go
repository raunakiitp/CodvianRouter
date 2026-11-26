package router

// WS9E closure item 2b: the provider × modality × operation
// capability matrix in executable form. endpointForProjection is the
// single seam where the router's endpoint kinds map onto the
// autorouter's surfaces — pinning it proves every modality the
// request layer names (chat, embeddings, rerank, images, audio×3,
// search, ocr, video, music) has a real projection target, and an
// unknown kind fails loudly instead of silently routing as chat.

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestModalityProjectionMatrix pins every modality → autorouter
// surface projection. This is the matrix the request layer routes
// by: a missing case here would send an operation of one modality
// down the chat path.
func TestModalityProjectionMatrix(t *testing.T) {
	pairs := []struct {
		kind provider.EndpointKind
		want autorouter.Endpoint
	}{
		{"", autorouter.EndpointChat},
		{provider.EndpointChatCompletions, autorouter.EndpointChat},
		{provider.EndpointMessages, autorouter.EndpointChat},
		{provider.EndpointResponses, autorouter.EndpointChat},
		{provider.EndpointGeminiGenerateContent, autorouter.EndpointChat},
		{provider.EndpointEmbeddings, autorouter.EndpointEmbeddings},
		{provider.EndpointRerank, autorouter.EndpointRerank},
		{provider.EndpointImages, autorouter.EndpointImages},
		{EndpointAudioSpeech, autorouter.EndpointAudioSpeech},
		{EndpointAudioTranscriptions, autorouter.EndpointAudioTranscriptions},
		{EndpointAudioTranslations, autorouter.EndpointAudioTranslations},
		{provider.EndpointSearch, autorouter.EndpointSearch},
		{provider.EndpointOCR, autorouter.EndpointOCR},
		{provider.EndpointVideo, autorouter.EndpointVideos},
		{provider.EndpointMusic, autorouter.EndpointMusic},
	}
	for _, p := range pairs {
		got, err := endpointForProjection(p.kind)
		if err != nil {
			t.Errorf("%q: unexpected projection error %v — the modality matrix has a hole", p.kind, err)
			continue
		}
		if got != p.want {
			t.Errorf("%q projects to %q, want %q", p.kind, got, p.want)
		}
	}
}

// TestModalityProjectionUnknownKindFailsLoudly: an endpoint kind the
// router does not know must ERROR, not silently degrade to chat —
// routing an unknown operation down the chat path would dial a
// chat-shaped request at whatever provider the chain names.
func TestModalityProjectionUnknownKindFailsLoudly(t *testing.T) {
	_, err := endpointForProjection(provider.EndpointKind("holodeck"))
	if err == nil {
		t.Fatal("unknown endpoint kind accepted — it must fail loudly, not degrade to chat")
	}
	got, err2 := endpointForProjection(provider.EndpointKind(""))
	if err2 != nil || got != autorouter.EndpointChat {
		t.Fatalf("empty kind = (%q, %v), want chat with no error (documented default)", got, err2)
	}
}
