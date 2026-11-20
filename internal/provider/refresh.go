// Live model-list refresh (CHECKLIST 200): re-probe every
// provider's model list from the upstream, right now, and report
// honestly per provider what happened — never silent staleness.
//
// A refresh is a live ListModels call per enabled provider (the same
// call the registry makes when the provider is added). Disabled
// providers are skipped, never probed. A provider whose list fetch
// fails keeps its previously stored models: a failed refresh must
// never empty the catalog — the operator keeps routing on the last
// known list, and the failure is reported per provider with its
// reason, so a dead provider cannot silently vanish from the
// catalog. A provider whose fetch succeeds replaces its stored list
// with the live one, merged so per-model metadata the upstream does
// not report (context window, provider-reported prices, the
// operator's deprecated flag) survives the refresh for models that
// are still listed; models the upstream no longer lists are dropped
// (they cannot be served any more); genuinely new models arrive
// with unknown window/prices (never guessed).
//
// The refreshed lists are persisted (one save for the whole refresh)
// so a restart keeps them. If the persist fails, the in-memory
// update is rolled back and the error is returned: the caller gets
// per-provider results AND the persist failure, never a half-saved
// refresh presented as done.
package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ModelRefreshStatus is the outcome of one provider's refresh.
type ModelRefreshStatus string

const (
	// ModelRefreshOK: the live list was fetched and stored.
	ModelRefreshOK ModelRefreshStatus = "ok"
	// ModelRefreshFailed: the live list could not be fetched; the
	// stored list is untouched. Error names the reason.
	ModelRefreshFailed ModelRefreshStatus = "failed"
	// ModelRefreshSkipped: the provider was not probed (disabled).
	// Error names the reason.
	ModelRefreshSkipped ModelRefreshStatus = "skipped"
)

// ModelRefreshResult is one provider's refresh outcome: its id and
// display name, the status, the model IDs now stored for it (the
// live list on success, the previously stored list otherwise), and
// the failure/skip reason ("" on success). Error never carries an
// upstream response body: an upstream HTTP failure is reduced to
// its status ("upstream HTTP 429"), and anything else is the
// transport error's own message.
type ModelRefreshResult struct {
	ProviderID   string             `json:"provider_id"`
	ProviderName string             `json:"provider_name"`
	Status       ModelRefreshStatus `json:"status"`
	Models       []string           `json:"models"`
	Error        string             `json:"error,omitempty"`
}

// refreshProviderTimeout bounds one provider's live ListModels call:
// a hung upstream must not stall the whole refresh (the handler
// bounds the total separately).
const refreshProviderTimeout = 15 * time.Second

// ModelListRefreshInterval is the scheduled background refresh
// cadence for stored model lists (CT-7): every 24h each enabled
// provider's list is re-probed live (see
// runScheduledModelRefreshLoop in cmd/codvian-router). A stored
// list older than this is stale (see Provider.ModelsListStale)
// and GET /v1/models says so per row. There is deliberately no
// config knob for the interval: no existing config section owns
// a refresh cadence, and 24h matches how slowly upstream model
// catalogs change — new models arriving a day late is the honest
// cost of not hammering every upstream hourly.
const ModelListRefreshInterval = 24 * time.Hour

// RefreshModels re-probes every provider's model list live and
// returns one result per provider, in registry order. It never
// returns a nil slice. The second return is a persist failure only:
// per-provider fetch failures are reported in the results, not as
// an error, so one dead provider never hides the other providers'
// fresh lists.
func (r *Registry) RefreshModels(ctx context.Context) ([]ModelRefreshResult, error) {
	providers := r.List()
	results := make([]ModelRefreshResult, len(providers))
	// Probe concurrently (bounded): a slow upstream must not
	// serialize the whole refresh behind it. Each result lands in
	// its own index, so the order stays registry order.
	const maxConcurrent = 8
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for i, p := range providers {
		if p == nil {
			results[i] = ModelRefreshResult{ProviderID: "", ProviderName: "", Status: ModelRefreshSkipped, Models: []string{}, Error: "provider record is missing"}
			continue
		}
		wg.Add(1)
		go func(i int, p *Provider) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = r.refreshOne(ctx, p)
		}(i, p)
	}
	wg.Wait()

	// Apply the successful fetches under one write lock, then
	// persist once. A successful fetch also stamps
	// ModelsFetchedAt (CT-7): the whole refresh run shares one
	// timestamp, so every provider refreshed in this run shows
	// the same fetch time. A persist failure rolls the
	// in-memory lists AND their stamps back, so the registry
	// never claims a refresh it did not save.
	r.mu.Lock()
	previous := make(map[string][]Model, len(results))
	previousFetchedAt := make(map[string]time.Time, len(results))
	fetchedAt := time.Now().UTC()
	for _, res := range results {
		if res.Status != ModelRefreshOK {
			continue
		}
		p, ok := r.providers[res.ProviderID]
		if !ok {
			continue
		}
		previous[res.ProviderID] = p.Models
		previousFetchedAt[res.ProviderID] = p.ModelsFetchedAt
		p.Models = mergeModelLists(p.Models, res.Models)
		p.ModelsFetchedAt = fetchedAt
	}
	saveErr := r.saveLocked()
	if saveErr != nil {
		for id, models := range previous {
			if p, ok := r.providers[id]; ok {
				p.Models = models
				p.ModelsFetchedAt = previousFetchedAt[id]
			}
		}
	}
	r.mu.Unlock()
	if saveErr != nil {
		return results, fmt.Errorf("persisting refreshed model lists: %w", saveErr)
	}
	return results, nil
}

// refreshOne probes one provider's live model list. It never mutates
// the registry: the caller applies successful results.
func (r *Registry) refreshOne(ctx context.Context, p *Provider) ModelRefreshResult {
	ids := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	if !p.Enabled {
		return ModelRefreshResult{ProviderID: p.ID, ProviderName: p.Name, Status: ModelRefreshSkipped, Models: ids, Error: "provider disabled"}
	}
	probeCtx, cancel := context.WithTimeout(ctx, refreshProviderTimeout)
	defer cancel()
	client, err := r.ClientFor(p.ID)
	if err != nil {
		return ModelRefreshResult{ProviderID: p.ID, ProviderName: p.Name, Status: ModelRefreshFailed, Models: ids, Error: refreshErrorText(err)}
	}
	live, err := client.ListModels(probeCtx)
	if err != nil {
		return ModelRefreshResult{ProviderID: p.ID, ProviderName: p.Name, Status: ModelRefreshFailed, Models: ids, Error: refreshErrorText(err)}
	}
	return ModelRefreshResult{ProviderID: p.ID, ProviderName: p.Name, Status: ModelRefreshOK, Models: live}
}

// mergeModelLists merges a live ID list into the stored models:
// models still listed keep their stored record (context window,
// provider-reported prices, deprecated flag — the upstream list
// carries none of those); new IDs arrive as bare records (unknown
// window/prices, never guessed); IDs the upstream no longer lists
// are dropped. The order is the live list's order.
func mergeModelLists(stored []Model, live []string) []Model {
	byID := make(map[string]Model, len(stored))
	for _, m := range stored {
		if m.ID != "" {
			byID[m.ID] = m
		}
	}
	out := make([]Model, 0, len(live))
	seen := make(map[string]bool, len(live))
	for _, id := range live {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if m, ok := byID[id]; ok {
			out = append(out, m)
		} else {
			out = append(out, Model{ID: id})
		}
	}
	return out
}

// refreshErrorText reduces a refresh failure to a short, safe
// message: an upstream HTTP failure is its status only ("upstream
// HTTP 429" — never the response body, which can echo request
// material), whether the client reports it as an *UpstreamError, a
// *RateLimitError (429) or a *QuotaError (402 / quota 4xx); anything
// else is the error's own message (transport errors carry no
// bodies).
func refreshErrorText(err error) string {
	var ue *UpstreamError
	if errors.As(err, &ue) && ue != nil {
		return fmt.Sprintf("upstream HTTP %d", ue.StatusCode)
	}
	var rle *RateLimitError
	if errors.As(err, &rle) && rle != nil {
		return "upstream HTTP 429"
	}
	var qe *QuotaError
	if errors.As(err, &qe) && qe != nil {
		return fmt.Sprintf("upstream HTTP %d", qe.StatusCode)
	}
	return err.Error()
}
