package config

import "testing"

// G16 Area 11 cost row: per-endpoint price dimension in the price
// registry (config.ModelPrice.EndpointPrices).
func TestG16EndpointPriceMerge(t *testing.T) {
	in := func(v float64) *float64 { return &v }
	base := ModelPrice{
		InputUSDPer1M:  in(2.5),
		OutputUSDPer1M: in(10),
		EndpointPrices: map[string]ModelPrice{
			"embeddings": {InputUSDPer1M: in(0.1)},
		},
	}
	// Chat (no override): base legs unchanged.
	chat := base.PriceForEndpoint("")
	if *chat.InputUSDPer1M != 2.5 || *chat.OutputUSDPer1M != 10 {
		t.Fatalf("chat price = %v/%v, want 2.5/10", *chat.InputUSDPer1M, *chat.OutputUSDPer1M)
	}
	// Embeddings: input leg overridden, output leg falls back to base.
	emb := base.PriceForEndpoint("embeddings")
	if *emb.InputUSDPer1M != 0.1 {
		t.Fatalf("embeddings input = %v, want 0.1", *emb.InputUSDPer1M)
	}
	if *emb.OutputUSDPer1M != 10 {
		t.Fatalf("embeddings output = %v, want base 10", *emb.OutputUSDPer1M)
	}
	// Unknown endpoint: base legs.
	resp := base.PriceForEndpoint("responses")
	if *resp.InputUSDPer1M != 2.5 {
		t.Fatalf("responses input = %v, want base 2.5", *resp.InputUSDPer1M)
	}
	// An endpoint override carries no nested dimension.
	if len(emb.EndpointPrices) != 0 {
		t.Fatalf("endpoint price carries nested endpoint prices: %v", emb.EndpointPrices)
	}
}

func TestG16EndpointPriceValidateAndClone(t *testing.T) {
	in := func(v float64) *float64 { return &v }
	bad := ModelPrice{
		InputUSDPer1M:  in(2.5),
		EndpointPrices: map[string]ModelPrice{"embeddings": {InputUSDPer1M: in(-1)}},
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("negative endpoint leg validated without error")
	}
	good := ModelPrice{
		InputUSDPer1M:  in(2.5),
		EndpointPrices: map[string]ModelPrice{"embeddings": {InputUSDPer1M: in(0.1)}},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid endpoint price rejected: %v", err)
	}
	// Clone deep-copies the endpoint map: mutating the copy's
	// leg never moves the original.
	c := good.clone()
	*c.EndpointPrices["embeddings"].InputUSDPer1M = 99
	if *good.EndpointPrices["embeddings"].InputUSDPer1M != 0.1 {
		t.Fatal("clone shares endpoint price storage with the original")
	}
	// A model priced ONLY at an endpoint is Known (priced for
	// that endpoint), not unknown.
	epOnly := ModelPrice{EndpointPrices: map[string]ModelPrice{"embeddings": {InputUSDPer1M: in(0.1)}}}
	if !epOnly.Known() {
		t.Fatal("endpoint-only price reports Known() = false")
	}
	if (ModelPrice{}).Known() {
		t.Fatal("empty price reports Known() = true")
	}
}
