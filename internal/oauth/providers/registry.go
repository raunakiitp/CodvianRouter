package providers

import (
	"fmt"
	"sort"
	"sync"
)

// handlers holds the implemented interactive-flow handlers, keyed by
// catalog provider ID.
var handlers = map[string]Handler{}

var (
	registryMu sync.RWMutex
	// infos lists every CategoryOAuth catalog provider in a stable
	// order — implemented and honestly-unimplemented alike.
	infos []Info
)

func register(h Handler) {
	registryMu.Lock()
	defer registryMu.Unlock()
	in := h.Info()
	if _, dup := handlers[in.ID]; dup {
		panic(fmt.Sprintf("providers: duplicate handler for %q", in.ID))
	}
	handlers[in.ID] = h
	infos = append(infos, in)
}

func registerImportOnly(in Info) {
	registryMu.Lock()
	defer registryMu.Unlock()
	infos = append(infos, in)
}

// All returns every CategoryOAuth provider's registry entry — the
// implemented handlers AND the honestly-unimplemented ones (Reason
// says why each stays unimplemented). The order is stable.
func All() []Info {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Info, len(infos))
	copy(out, infos)
	return out
}

// Get returns the handler for a catalog provider ID. For the
// honestly-unimplemented providers the error wraps ErrNotImplemented
// and carries the documented reason; for the import-only providers it
// wraps ErrImportOnly. An unknown ID (not a catalog OAuth provider at
// all) errors as unknown.
func Get(id string) (Handler, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if h, ok := handlers[id]; ok {
		return h, nil
	}
	for _, in := range infos {
		if in.ID == id {
			if in.ImportOnly {
				return nil, fmt.Errorf("%w: %q connects via paste-token import (no interactive flow)", ErrImportOnly, id)
			}
			return nil, fmt.Errorf("%w: %q — %s", ErrNotImplemented, id, in.Reason)
		}
	}
	return nil, fmt.Errorf("providers: %q is not a catalog OAuth provider", id)
}

// ImplementedInfos returns the registry entries that have a real
// handler (interactive flow or allowlisted import).
func ImplementedInfos() []Info {
	registryMu.RLock()
	defer registryMu.RUnlock()
	var out []Info
	for _, in := range infos {
		if in.Implemented {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func init() {
	register(&claudeHandler{})
	register(&copilotHandler{})
	register(&clineHandler{info: clineInfo()})
	register(&clineHandler{info: clinePassInfo()})
	register(&openaiPKCEHandler{info: codexInfo()})
	register(&openaiPKCEHandler{info: chatgptInfo()})
	register(&kimiHandler{})
	register(&gitlabDuoHandler{})
	register(&awsDeviceHandler{info: kiroInfo()})
	register(&awsDeviceHandler{info: amazonQInfo()})
	register(&cursorHandler{})
	register(&gheCopilotHandler{})
	register(&xaiDeviceHandler{info: xaiOAuthInfo()})
	register(&xaiDeviceHandler{info: grokCLIInfo()})
	register(&zedHandler{info: zedInfo()})
	register(&zedHandler{info: zedHostedInfo()})
	registerImportOnly(Info{
		ID:          "antigravity",
		Name:        "Antigravity",
		Flow:        FlowPasteImport,
		Implemented: true,
		ImportOnly:  true,
		DocsURL:     pasteImportDocsURL,
		Reason:      "",
	})
	registerImportOnly(Info{
		ID:          "agy",
		Name:        "Antigravity CLI",
		Alias:       "agy",
		Flow:        FlowPasteImport,
		Implemented: true,
		ImportOnly:  true,
		DocsURL:     pasteImportDocsURL,
		Reason:      "",
	})
	// The remaining 13 catalog OAuth providers have no publicly
	// documented OAuth surface; the registry lists them with the
	// reason so the refusal is explainable, never silent.
	for _, u := range unimplemented {
		registerImportOnly(Info{
			ID:          u.id,
			Name:        u.name,
			Alias:       u.alias,
			Flow:        FlowPasteImport,
			Implemented: false,
			Reason:      u.reason,
		})
	}
}
