package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestRoutingDecode(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"order":["b","a"],"only":["a","b"],"exclude":["c"],"sort":"price","max_price":{"input":5,"output":15},"data_collection":"deny"}}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Routing == nil {
		t.Fatalf("routing is nil")
	}
	r := req.Routing
	if len(r.Order) != 2 || r.Order[0] != "b" {
		t.Fatalf("order = %v", r.Order)
	}
	if len(r.Only) != 2 || r.Only[1] != "b" {
		t.Fatalf("only = %v", r.Only)
	}
	if len(r.Exclude) != 1 || r.Exclude[0] != "c" {
		t.Fatalf("exclude = %v", r.Exclude)
	}
	if r.Sort != "price" {
		t.Fatalf("sort = %q", r.Sort)
	}
	if r.MaxPrice == nil || r.MaxPrice.Input == nil || *r.MaxPrice.Input != 5 || r.MaxPrice.Output == nil || *r.MaxPrice.Output != 15 {
		t.Fatalf("max_price = %+v", r.MaxPrice)
	}
	if r.DataCollection != "deny" {
		t.Fatalf("data_collection = %q", r.DataCollection)
	}
}

func TestRequestRoutingUnknownFieldRejected(t *testing.T) {
	var req ChatRequest
	err := json.Unmarshal([]byte(`{"model":"m","messages":[],"routing":{"excludes":["a"]}}`), &req)
	if err == nil {
		t.Fatalf("expected an error for an unknown routing field, got nil")
	}
	if !strings.Contains(err.Error(), `unknown field "excludes"`) {
		t.Fatalf("error = %q, want it to name the unknown field", err.Error())
	}
	if !strings.Contains(err.Error(), "known fields:") {
		t.Fatalf("error = %q, want it to list the known fields", err.Error())
	}
}

func TestRequestRoutingNullIsNil(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[],"routing":null}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Routing != nil {
		t.Fatalf("routing = %+v, want nil for JSON null", req.Routing)
	}
}

func TestRequestRoutingWrongShapeRejected(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[],"routing":"cheap"}`), &req); err == nil {
		t.Fatalf("expected an error for a string routing value, got nil")
	}
}

func TestRequestRoutingEmpty(t *testing.T) {
	var nilRouting *RequestRouting
	if !nilRouting.Empty() {
		t.Fatalf("nil routing must be empty")
	}
	zero := RequestRouting{}
	if !zero.Empty() {
		t.Fatalf("zero routing must be empty")
	}
	sorted := RequestRouting{Sort: "price"}
	if sorted.Empty() {
		t.Fatalf("routing with a sort must not be empty")
	}
	priced := RequestRouting{MaxPrice: &MaxPrice{}}
	if priced.Empty() {
		t.Fatalf("routing with a max_price must not be empty")
	}
}

func TestRequestRoutingNeverReachesUpstream(t *testing.T) {
	// The wire clients build their own upstream payload from the
	// ChatRequest fields; the routing contract must never be
	// forwarded to a provider (it is router-internal after decode).
	req := ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Routing:  &RequestRouting{Only: []string{"a"}, Sort: "price"},
	}
	ccReq, err := (&openAIClient{}).toChatCompletionRequest(req)
	if err != nil {
		t.Fatalf("build upstream payload: %v", err)
	}
	payload, err := json.Marshal(ccReq)
	if err != nil {
		t.Fatalf("marshal upstream payload: %v", err)
	}
	if strings.Contains(string(payload), "routing") {
		t.Fatalf("upstream payload carries the routing contract: %s", payload)
	}
}
