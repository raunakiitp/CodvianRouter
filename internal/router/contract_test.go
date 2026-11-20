package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// contractClients builds three fake clients — cheap, mid, pricey —
// each answering with its own id as content, so the test can tell
// exactly who served. fail maps a provider id to an error its
// Chat returns (a 429 fails over to the next target in the chain).
func contractClients(t *testing.T, fail map[string]error) (map[string]provider.Client, map[string]*fakeClient) {
	t.Helper()
	fakes := map[string]*fakeClient{}
	clients := map[string]provider.Client{}
	for _, id := range []string{"cheap", "mid", "pricey"} {
		f := &fakeClient{}
		if err, ok := fail[id]; ok {
			f.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
				return nil, err
			}
		} else {
			id := id
			f.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
				return &provider.ChatResponse{Content: "served-by-" + id, Model: req.Model}, nil
			}
		}
		fakes[id] = f
		clients[id] = f
	}
	return clients, fakes
}

func contractChain() []Attempt {
	return []Attempt{
		{ProviderID: "cheap", Model: "m"},
		{ProviderID: "mid", Model: "m"},
		{ProviderID: "pricey", Model: "m"},
	}
}

func contractReq(routing *provider.RequestRouting) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Routing:  routing,
	}
}

func fptr(v float64) *float64 { return &v }

// contractPrices is the ContractPriceFunc for the tests: cheap
// $1/$2, mid $5/$10, pricey $50/$100 (input/output USD per 1M
// tokens). Unknown is the set of providers with no price at all.
func contractPrices(unknown map[string]bool, halfKnown map[string]bool) func(string, string) (config.ModelPrice, bool) {
	return func(providerID, model string) (config.ModelPrice, bool) {
		if unknown[providerID] {
			return config.ModelPrice{}, false
		}
		var in, out float64
		switch providerID {
		case "cheap":
			in, out = 1, 2
		case "mid":
			in, out = 5, 10
		case "pricey":
			in, out = 50, 100
		}
		if halfKnown[providerID] {
			// Only the output leg is known: an input ceiling
			// cannot be verified for this pair.
			return config.ModelPrice{OutputUSDPer1M: fptr(out)}, true
		}
		return config.ModelPrice{InputUSDPer1M: fptr(in), OutputUSDPer1M: fptr(out)}, true
	}
}

func TestContractOrder(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{})
	resp, trace, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		Order: []string{"pricey", "cheap"},
	}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served-by-pricey" {
		t.Fatalf("content = %q, want served-by-pricey (first in order)", resp.Content)
	}
	if fakes["cheap"].chats != 0 || fakes["mid"].chats != 0 {
		t.Fatalf("chats cheap=%d mid=%d, want 0/0: order must not try anyone before pricey", fakes["cheap"].chats, fakes["mid"].chats)
	}
	if trace.Contract == nil {
		t.Fatalf("trace has no contract")
	}
	if len(trace.Contract.Order) != 2 || trace.Contract.Order[0] != "pricey" {
		t.Fatalf("trace contract order = %v, want [pricey cheap]", trace.Contract.Order)
	}
	if len(trace.Contract.Excluded) != 1 || trace.Contract.Excluded[0].Provider != "mid" || trace.Contract.Excluded[0].Reason != "not_in_order" {
		t.Fatalf("trace exclusions = %+v, want [{mid not_in_order}]", trace.Contract.Excluded)
	}
}

func TestContractOnly(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{})
	resp, trace, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		Only: []string{"mid"},
	}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served-by-mid" {
		t.Fatalf("content = %q, want served-by-mid", resp.Content)
	}
	if fakes["cheap"].chats != 0 || fakes["pricey"].chats != 0 {
		t.Fatalf("chats cheap=%d pricey=%d, want 0/0", fakes["cheap"].chats, fakes["pricey"].chats)
	}
	if len(trace.Contract.Excluded) != 2 {
		t.Fatalf("exclusions = %+v, want 2 (cheap, pricey)", trace.Contract.Excluded)
	}
	for _, x := range trace.Contract.Excluded {
		if x.Reason != "not_in_only" {
			t.Fatalf("exclusion %+v: reason want not_in_only", x)
		}
	}
}

func TestContractExclude(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{})
	resp, trace, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		Exclude: []string{"cheap"},
	}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served-by-mid" {
		t.Fatalf("content = %q, want served-by-mid (cheap excluded, mid next)", resp.Content)
	}
	if fakes["cheap"].chats != 0 {
		t.Fatalf("cheap chats = %d, want 0", fakes["cheap"].chats)
	}
	if len(trace.Contract.Excluded) != 1 || trace.Contract.Excluded[0].Reason != "excluded" {
		t.Fatalf("exclusions = %+v, want [{cheap excluded}]", trace.Contract.Excluded)
	}
}

func TestContractSortMapsToStrategy(t *testing.T) {
	for _, tc := range []struct {
		sort     string
		strategy string
	}{
		{"price", "cheapest"},
		{"latency", "least-latency"},
		{"reliability", "most-reliable"},
	} {
		hooks := &recordingHookFactory{}
		clients, _ := contractClients(t, nil)
		e := NewEngine(clients, nil, Config{StrategyHook: hooks.hook})
		_, _, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{Sort: tc.sort}))
		if err != nil {
			t.Fatalf("sort=%q: %v", tc.sort, err)
		}
		if got := hooks.last(); got != tc.strategy {
			t.Fatalf("sort=%q: strategy hook got %q, want %q", tc.sort, got, tc.strategy)
		}
		if info, ok := e.LastRouting(); !ok || info.Strategy != tc.strategy {
			t.Fatalf("sort=%q: LastRouting = %+v, want strategy %q", tc.sort, info, tc.strategy)
		}
	}
}

func TestContractSortBeatsChainStrategy(t *testing.T) {
	hooks := &recordingHookFactory{}
	clients, _ := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{
		StrategyHook: hooks.hook,
		ChainStrategy: func(model string) string {
			return "fastest"
		},
	})
	_, _, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{Sort: "price"}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if got := hooks.last(); got != "cheapest" {
		t.Fatalf("strategy hook got %q, want cheapest: contract sort must beat the chain override", got)
	}
}

func TestContractMaxPrice(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{ContractPriceFunc: contractPrices(nil, nil)})
	// Chain order is pricey-first: the ceiling must exclude it
	// before any attempt, so mid serves.
	chain := []Attempt{{ProviderID: "pricey", Model: "m"}, {ProviderID: "mid", Model: "m"}, {ProviderID: "cheap", Model: "m"}}
	resp, trace, err := e.ChatWithChain(context.Background(), chain, contractReq(&provider.RequestRouting{
		MaxPrice: &provider.MaxPrice{Input: fptr(6)},
	}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served-by-mid" {
		t.Fatalf("content = %q, want served-by-mid", resp.Content)
	}
	if fakes["pricey"].chats != 0 {
		t.Fatalf("pricey chats = %d, want 0: over-ceiling provider must never be dialled", fakes["pricey"].chats)
	}
	if len(trace.Contract.Excluded) != 1 || trace.Contract.Excluded[0].Reason != "over_max_price" {
		t.Fatalf("exclusions = %+v, want [{pricey over_max_price}]", trace.Contract.Excluded)
	}
}

func TestContractMaxPriceNothingFitsIs402(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{ContractPriceFunc: contractPrices(nil, nil)})
	_, trace, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		MaxPrice: &provider.MaxPrice{Input: fptr(0.5)},
	}))
	if err == nil {
		t.Fatalf("expected a 402, got nil error")
	}
	var ce *ContractError
	if !errors.As(err, &ce) {
		t.Fatalf("error = %T %v, want *ContractError", err, err)
	}
	if ce.Status != 402 {
		t.Fatalf("status = %d, want 402", ce.Status)
	}
	if !strings.Contains(ce.Msg, "max_price") {
		t.Fatalf("message = %q, want it to name max_price", ce.Msg)
	}
	for id, f := range fakes {
		if f.chats != 0 {
			t.Fatalf("provider %s chats = %d, want 0: nothing may be dialled when nothing fits", id, f.chats)
		}
	}
	if trace == nil || trace.Contract == nil || trace.Contract.Error == "" {
		t.Fatalf("trace contract error not recorded: %+v", trace)
	}
	if len(trace.Attempts) != 0 {
		t.Fatalf("attempts = %d, want 0 on a rejected contract", len(trace.Attempts))
	}
}

func TestContractMaxPriceUnknownPriceExcluded(t *testing.T) {
	clients, _ := contractClients(t, nil)
	// Only cheap has a known price; mid and pricey are unknown.
	e := NewEngine(clients, nil, Config{ContractPriceFunc: contractPrices(map[string]bool{"mid": true, "pricey": true}, nil)})
	resp, trace, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		MaxPrice: &provider.MaxPrice{Input: fptr(100)},
	}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served-by-cheap" {
		t.Fatalf("content = %q, want served-by-cheap (only known-priced provider)", resp.Content)
	}
	reasons := map[string]string{}
	for _, x := range trace.Contract.Excluded {
		reasons[x.Provider] = x.Reason
	}
	if reasons["mid"] != "price_unknown" || reasons["pricey"] != "price_unknown" {
		t.Fatalf("exclusions = %+v, want mid+pricey price_unknown", trace.Contract.Excluded)
	}
}

func TestContractMaxPriceHalfKnownPriceExcluded(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	// mid's input price is unknown (only output known): an input
	// ceiling cannot be verified for it, so it is excluded even
	// though its output price would fit.
	e := NewEngine(clients, nil, Config{ContractPriceFunc: contractPrices(nil, map[string]bool{"mid": true})})
	resp, trace, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		MaxPrice: &provider.MaxPrice{Input: fptr(100)},
	}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served-by-cheap" {
		t.Fatalf("content = %q, want served-by-cheap", resp.Content)
	}
	if fakes["mid"].chats != 0 {
		t.Fatalf("mid chats = %d, want 0: half-known price must not pass an input ceiling", fakes["mid"].chats)
	}
	found := false
	for _, x := range trace.Contract.Excluded {
		if x.Provider == "mid" && x.Reason == "price_unknown" {
			found = true
		}
	}
	if !found {
		t.Fatalf("exclusions = %+v, want mid price_unknown", trace.Contract.Excluded)
	}
}

func TestContractDataCollectionDeny(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{
		TrainingSafe: func(id string) bool { return id == "cheap" },
	})
	resp, trace, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		DataCollection: "deny",
	}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served-by-cheap" {
		t.Fatalf("content = %q, want served-by-cheap (only verified provider)", resp.Content)
	}
	if fakes["mid"].chats != 0 || fakes["pricey"].chats != 0 {
		t.Fatalf("chats mid=%d pricey=%d, want 0/0", fakes["mid"].chats, fakes["pricey"].chats)
	}
	for _, x := range trace.Contract.Excluded {
		if x.Reason != "data_collection_deny" {
			t.Fatalf("exclusion %+v: reason want data_collection_deny", x)
		}
	}
}

func TestContractDataCollectionDenyFailsClosedWithoutVerifier(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{})
	_, _, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		DataCollection: "deny",
	}))
	if err == nil {
		t.Fatalf("expected a 400, got nil error")
	}
	var ce *ContractError
	if !errors.As(err, &ce) || ce.Status != 400 {
		t.Fatalf("error = %T %v, want *ContractError 400", err, err)
	}
	for id, f := range fakes {
		if f.chats != 0 {
			t.Fatalf("provider %s chats = %d, want 0: deny with no verifier must dial nobody", id, f.chats)
		}
	}
}

func TestContractValidation(t *testing.T) {
	clients, _ := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{ContractPriceFunc: contractPrices(nil, nil)})
	cases := []struct {
		name    string
		routing *provider.RequestRouting
		want    string
	}{
		{"unknown provider in only", &provider.RequestRouting{Only: []string{"nope"}}, `unknown provider "nope"`},
		{"unknown provider in exclude", &provider.RequestRouting{Exclude: []string{"ghost"}}, `unknown provider "ghost"`},
		{"unknown provider in order", &provider.RequestRouting{Order: []string{"ghost"}}, `unknown provider "ghost"`},
		{"only and exclude overlap", &provider.RequestRouting{Only: []string{"cheap"}, Exclude: []string{"cheap"}}, `"cheap"`},
		{"order and sort conflict", &provider.RequestRouting{Order: []string{"cheap"}, Sort: "price"}, "conflict"},
		{"bad sort", &provider.RequestRouting{Sort: "fast"}, `unknown sort "fast"`},
		{"bad data_collection", &provider.RequestRouting{DataCollection: "sometimes"}, `unknown value "sometimes"`},
		{"negative max_price", &provider.RequestRouting{MaxPrice: &provider.MaxPrice{Input: fptr(-1)}}, "non-negative"},
		{"empty max_price", &provider.RequestRouting{MaxPrice: &provider.MaxPrice{}}, "at least one of input or output"},
		{"order provider does not serve model", &provider.RequestRouting{Order: []string{"cheap"}}, "does not serve"},
		{"empty provider name", &provider.RequestRouting{Only: []string{"  "}}, "must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The "does not serve" case needs a chain without
			// cheap; every other case uses the full chain.
			chain := contractChain()
			if tc.name == "order provider does not serve model" {
				chain = []Attempt{{ProviderID: "mid", Model: "m"}, {ProviderID: "pricey", Model: "m"}}
			}
			_, _, err := e.ChatWithChain(context.Background(), chain, contractReq(tc.routing))
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			var ce *ContractError
			if !errors.As(err, &ce) {
				t.Fatalf("error = %T %v, want *ContractError", err, err)
			}
			if ce.Status != 400 {
				t.Fatalf("status = %d, want 400", ce.Status)
			}
			if !strings.Contains(ce.Msg, tc.want) {
				t.Fatalf("message = %q, want it to contain %q", ce.Msg, tc.want)
			}
		})
	}
}

func TestContractCombined(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{ContractPriceFunc: contractPrices(nil, nil)})
	// order [mid, cheap] + a ceiling cheap and mid both fit:
	// pricey is out for being over the ceiling (the price filter
	// runs before the order stage), mid serves first.
	resp, trace, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		Order:    []string{"mid", "cheap"},
		MaxPrice: &provider.MaxPrice{Input: fptr(6)},
	}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served-by-mid" {
		t.Fatalf("content = %q, want served-by-mid", resp.Content)
	}
	if fakes["pricey"].chats != 0 || fakes["cheap"].chats != 0 {
		t.Fatalf("chats pricey=%d cheap=%d, want 0/0", fakes["pricey"].chats, fakes["cheap"].chats)
	}
	if len(trace.Contract.Excluded) != 1 || trace.Contract.Excluded[0].Provider != "pricey" || trace.Contract.Excluded[0].Reason != "over_max_price" {
		t.Fatalf("exclusions = %+v, want [{pricey over_max_price}]", trace.Contract.Excluded)
	}
	if trace.Contract.Strategy != "priority" {
		t.Fatalf("contract strategy = %q, want priority (no sort, no override: the M1.11 default)", trace.Contract.Strategy)
	}
}

func TestContractNoSilentFallback(t *testing.T) {
	// only=[cheap], cheap 429s: the request must fail, never
	// silently fall back to mid or pricey.
	clients, fakes := contractClients(t, map[string]error{"cheap": &provider.RateLimitError{}})
	e := NewEngine(clients, nil, Config{})
	_, _, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		Only: []string{"cheap"},
	}))
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	if fakes["mid"].chats != 0 || fakes["pricey"].chats != 0 {
		t.Fatalf("chats mid=%d pricey=%d, want 0/0: only=[cheap] must never fall back outside the set", fakes["mid"].chats, fakes["pricey"].chats)
	}
}

func TestContractEmptyIsNoop(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{})
	resp, trace, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served-by-cheap" {
		t.Fatalf("content = %q, want served-by-cheap (plain chain order)", resp.Content)
	}
	if trace.Contract != nil {
		t.Fatalf("trace contract = %+v, want nil for an empty contract", trace.Contract)
	}
	if fakes["cheap"].chats != 1 {
		t.Fatalf("cheap chats = %d, want 1", fakes["cheap"].chats)
	}
}

func TestContractStreamingHonorsOnly(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	for id, f := range fakes {
		id := id
		f.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
			if err := onDelta("stream-" + id); err != nil {
				return nil, err
			}
			return &provider.ChatResponse{Content: "stream-" + id, Model: req.Model}, nil
		}
	}
	e := NewEngine(clients, nil, Config{})
	var got string
	_, err := e.ChatStream(context.Background(), contractReq(&provider.RequestRouting{
		Only: []string{"mid"},
	}), func(s string) error { got += s; return nil })
	// ChatStream resolves its chain via chainFor (all clients),
	// so the contract must restrict it there too.
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got != "stream-mid" {
		t.Fatalf("streamed content = %q, want stream-mid", got)
	}
	if fakes["cheap"].streams != 0 || fakes["pricey"].streams != 0 {
		t.Fatalf("streams cheap=%d pricey=%d, want 0/0", fakes["cheap"].streams, fakes["pricey"].streams)
	}
	if tr := e.LastTrace(); tr == nil || tr.Contract == nil || len(tr.Contract.Excluded) != 2 {
		t.Fatalf("last trace contract = %+v, want 2 exclusions", tr)
	}
}

func TestContractRejectionPublishesTrace(t *testing.T) {
	clients, _ := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{})
	_, _, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		Only: []string{"nope"},
	}))
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	tr := e.LastTrace()
	if tr == nil || tr.Contract == nil {
		t.Fatalf("LastTrace = %+v, want a trace carrying the rejected contract", tr)
	}
	if !strings.Contains(tr.Contract.Error, `unknown provider "nope"`) {
		t.Fatalf("contract error = %q, want it to name the unknown provider", tr.Contract.Error)
	}
	if len(tr.Attempts) != 0 {
		t.Fatalf("attempts = %d, want 0", len(tr.Attempts))
	}
}

// TestContractResolvesDisplayNames pins the name-resolution rule:
// clients address providers by the display names they see, while
// chains are keyed by registry ID. The resolver maps a name to its
// ID (exact ID, exact name, case-insensitive name); the trace
// records the resolved IDs, so its provider fields line up with
// the attempt rows. An ambiguous name is a 400, never a guess.
func TestContractResolvesDisplayNames(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	names := map[string]string{"Cheap One": "cheap", "MID": "mid", "pricey": "pricey"}
	resolver := func(name string) (string, error) {
		if id, ok := names[name]; ok {
			return id, nil
		}
		for n, id := range names {
			if strings.EqualFold(n, name) {
				return id, nil
			}
		}
		return "", errors.New("unknown provider " + name)
	}
	e := NewEngine(clients, nil, Config{ResolveProvider: resolver})
	resp, trace, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		Only: []string{"cheap one"},
	}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served-by-cheap" {
		t.Fatalf("content = %q, want served-by-cheap (case-insensitive display name)", resp.Content)
	}
	if trace.Contract == nil || len(trace.Contract.Only) != 1 || trace.Contract.Only[0] != "cheap" {
		t.Fatalf("trace contract only = %v, want [cheap] (the resolved ID)", trace.Contract.Only)
	}
	if fakes["mid"].chats != 0 || fakes["pricey"].chats != 0 {
		t.Fatalf("chats mid=%d pricey=%d, want 0/0", fakes["mid"].chats, fakes["pricey"].chats)
	}
}

func TestContractAmbiguousNameIs400(t *testing.T) {
	clients, _ := contractClients(t, nil)
	ambiguous := func(name string) (string, error) {
		return "", errors.New("ambiguous provider name " + name)
	}
	e := NewEngine(clients, nil, Config{ResolveProvider: ambiguous})
	_, _, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		Only: []string{"same-name"},
	}))
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	var ce *ContractError
	if !errors.As(err, &ce) || ce.Status != 400 {
		t.Fatalf("error = %T %v, want *ContractError 400", err, err)
	}
	if !strings.Contains(ce.Msg, "ambiguous") {
		t.Fatalf("message = %q, want it to name the ambiguity", ce.Msg)
	}
}

// reversingHook is a strategy hook that reverses the chain —
// the stand-in for the balanced hook re-ranking by probe latency
// in serve mode. An explicit contract order must survive it.
func reversingHook(strategy string) func([]Attempt) []Attempt {
	return func(chain []Attempt) []Attempt {
		rev := make([]Attempt, len(chain))
		for i, att := range chain {
			rev[len(chain)-1-i] = att
		}
		return rev
	}
}

// TestContractOrderSurvivesStrategyHook is the regression test for
// the live-smoke finding in the 157-158 gate: the contract order
// was recorded on the trace, but the balanced strategy hook
// re-ranked the chain by health-probe latency afterwards, so a
// request with order [pricey, cheap] was served by cheap. An
// explicit order is final — no scoring may re-rank it.
func TestContractOrderSurvivesStrategyHook(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	e := NewEngine(clients, nil, Config{StrategyHook: reversingHook})
	resp, _, err := e.ChatWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		Order: []string{"pricey", "cheap"},
	}))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served-by-pricey" {
		t.Fatalf("content = %q, want served-by-pricey: the strategy hook must not re-rank an explicit order", resp.Content)
	}
	if fakes["cheap"].chats != 0 || fakes["mid"].chats != 0 {
		t.Fatalf("chats cheap=%d mid=%d, want 0/0", fakes["cheap"].chats, fakes["mid"].chats)
	}
}

func TestContractOrderSurvivesStrategyHookStreaming(t *testing.T) {
	clients, fakes := contractClients(t, nil)
	for id, f := range fakes {
		id := id
		f.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
			if err := onDelta("stream-" + id); err != nil {
				return nil, err
			}
			return &provider.ChatResponse{Content: "stream-" + id, Model: req.Model}, nil
		}
	}
	e := NewEngine(clients, nil, Config{StrategyHook: reversingHook})
	ch, err := e.ChatStreamWithChain(context.Background(), contractChain(), contractReq(&provider.RequestRouting{
		Order: []string{"pricey", "cheap"},
	}))
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	var got string
	for ev := range ch {
		got += ev.Delta
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
	}
	if got != "stream-pricey" {
		t.Fatalf("streamed content = %q, want stream-pricey: the strategy hook must not re-rank an explicit order", got)
	}
	if fakes["cheap"].streams != 0 || fakes["mid"].streams != 0 {
		t.Fatalf("streams cheap=%d mid=%d, want 0/0", fakes["cheap"].streams, fakes["mid"].streams)
	}
}
