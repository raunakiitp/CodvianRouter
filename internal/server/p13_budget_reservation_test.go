// P13: budget reservation and relay spend accounting.
//
// Two guarantees are pinned here:
//
//  1. Relay budget enforcement: the /v1/messages relay path refuses
//     a capped key BEFORE it dials the upstream (402), and records
//     the spend of a served relayed request so the budget actually
//     moves (AUD-SEC-002 — a budgeted key must not be able to spend
//     through the relay path without its accrued spend moving).
//
//  2. Reservation refund: a path that takes a pre-send budget
//     reservation (here /v1/responses) must release it on a failed
//     upstream, so the reservation does not linger and the key is
//     not blocked by a phantom hold.
package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// anthropicRelayReply is a minimal valid Anthropic messages reply the
// relay mock returns (usage: 100 in / 50 out).
const anthropicRelayReply = `{"id":"msg_01","type":"message","role":"assistant","content":[{"type":"text","text":"Hello"}],"model":"claude-sonnet-4-5","stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":50}}`

func relayBody() string {
	return `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"` +
		strings.Repeat("hello world ", 200) + `"}]}`
}

// TestRelayBudgetRefusedAtCeiling: a key at its ceiling is refused
// before the relay dials the upstream (402).
func TestRelayBudgetRefusedAtCeiling(t *testing.T) {
	s := newKeyBudgetSetup(t)
	mock := &anthropicMock{jsonBody: anthropicRelayReply}
	srv := newMessagesServer(t, &fakeEngine{}, mock,
		WithConfig(s.cfg), WithLedger(s.led), WithPriceBook(s.book),
		WithLedgerPrices(ledger.Prices{ledger.PriceKey{Model: "claude-sonnet-4-5"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}))

	// A $0 budget puts the key at its ceiling immediately.
	if err := s.led.SetKeyBudget(context.Background(), s.id, 0, ledger.UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}

	rec := doReq(t, srv, "POST", "/v1/messages", relayBody(), s.raw)
	if rec.Code != 402 {
		t.Fatalf("status = %d, want 402 (relay must refuse a capped key pre-send)", rec.Code)
	}
}

// TestRelaySpendRecordedMovesBudget: a served relayed request records
// its spend, so a budgeted key's accrued spend actually moves
// (AUD-SEC-002). Without the ledger row the relay path would be a
// budget bypass.
func TestRelaySpendRecordedMovesBudget(t *testing.T) {
	s := newKeyBudgetSetup(t)
	mock := &anthropicMock{jsonBody: anthropicRelayReply}
	srv := newMessagesServer(t, &fakeEngine{}, mock,
		WithConfig(s.cfg), WithLedger(s.led), WithPriceBook(s.book),
		WithLedgerPrices(ledger.Prices{ledger.PriceKey{Model: "claude-sonnet-4-5"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}))

	if err := s.led.SetKeyBudget(context.Background(), s.id, 10.0, ledger.UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}

	rec := doReq(t, srv, "POST", "/v1/messages", relayBody(), s.raw)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	spent, err := s.led.SpendFor(context.Background(), s.id, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendFor: %v", err)
	}
	// 100 prompt * $10/1M + 50 completion * $30/1M = $0.001 + $0.0015 = $0.0025.
	want := 100*10.0/1e6 + 50*30.0/1e6
	if spent < want-1e-9 || spent > want+1e-9 {
		t.Fatalf("relay spend = %v, want %v (the served relay request must move the budget)", spent, want)
	}
}

// TestBudgetReservationRefundedOnFailedUpstream: a path that takes a
// pre-send reservation (/v1/responses) releases it when the upstream
// fails, so no phantom reservation lingers against the key's budget.
func TestBudgetReservationRefundedOnFailedUpstream(t *testing.T) {
	s := newKeyBudgetSetup(t)
	s.eng.fakeEngine.chatErr = errors.New("upstream exploded")
	srv := s.server(t)

	if err := s.led.SetKeyBudget(context.Background(), s.id, 10.0, ledger.UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}

	rec := doReq(t, srv, "POST", "/v1/responses", `{"model":"priced-model","input":"hi"}`, s.raw)
	if rec.Code == 200 {
		t.Fatalf("status = 200, want a failure status: %s", rec.Body.String())
	}

	reserved, err := s.led.ReservedSpend(context.Background(), s.id, time.Now())
	if err != nil {
		t.Fatalf("ReservedSpend: %v", err)
	}
	if reserved != 0 {
		t.Errorf("reserved after failed request = %v, want 0 (the reservation must be refunded)", reserved)
	}
}
