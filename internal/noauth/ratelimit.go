package noauth

import (
	"net/http"
	"sync"
	"time"
)

// IPLimiter enforces the provider's own documented per-IP request
// budgets (Mechanism.Limits) client-side, before a request is dialed.
// It is a set of fixed windows: a request is allowed only when every
// documented window has budget left. AI Horde's own docs state a
// request over a limit gets 429; refusing locally is the polite
// equivalent — the request never reaches the shared queue.
//
// Fixed windows are an approximation of the provider's flask-limiter
// rolling limits, chosen deliberately conservative in the direction
// of refusing early (the window never lets a burst through that the
// provider would refuse).
type IPLimiter struct {
	mu      sync.Mutex
	limits  []Limit
	windows map[string][]*ipWindow // key (IP) -> one window per Limit
	now     func() time.Time       // injectable clock for tests
}

// ipWindow counts requests inside the current fixed window.
type ipWindow struct {
	count    int
	deadline time.Time // window end
}

// NewIPLimiter builds a limiter for the given documented limits.
func NewIPLimiter(limits []Limit) *IPLimiter {
	return &IPLimiter{
		limits:  append([]Limit(nil), limits...),
		windows: map[string][]*ipWindow{},
		now:     time.Now,
	}
}

// Allow records one request for ip and reports whether every
// documented window still has budget. Over-limit requests are
// refused (the caller maps the refusal to the provider's own error,
// e.g. AI Horde's 429 TooManyPrompts).
func (l *IPLimiter) Allow(ip string) bool {
	return l.allowAt(ip, l.now())
}

func (l *IPLimiter) allowAt(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ws, ok := l.windows[ip]
	if !ok || len(ws) != len(l.limits) {
		ws = make([]*ipWindow, len(l.limits))
		for i := range ws {
			ws[i] = &ipWindow{}
		}
		l.windows[ip] = ws
	}
	for i, lim := range l.limits {
		w := ws[i]
		if !now.Before(w.deadline) {
			w.count = 0
			w.deadline = now.Add(lim.Window)
		}
		if w.count >= lim.Requests {
			return false
		}
	}
	for i := range ws {
		ws[i].count++
	}
	return true
}

// Status codes the limiter refusal maps onto, for callers that proxy
// the provider's own error semantics.
const (
	// StatusOverLimit is HTTP 429: AI Horde's docs state "A request
	// over a limit gets 429".
	StatusOverLimit = http.StatusTooManyRequests
)
