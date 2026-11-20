// Per-key access schedule, throttle delay and max sessions
// (M5.10): the server side.
//
// The access schedule is OmniRoute's validateKeyAccessSchedule
// (apiKeyPolicy.ts:540): a key outside its allowed hours is
// refused with a 403, before any budget is touched — it runs in
// the policy chain right after the banned/expired checks and
// before the usage-limit check. The throttle delay and the
// max-sessions cap are DIVERGENCES (documented in
// internal/config/gatewaykey.go): OmniRoute stores both fields
// but enforces neither for gateway keys in the current checkout,
// so our enforcement is the honest reading of what the fields
// promise, not a port of an OmniRoute behaviour.
package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// IsWithinAccessSchedule reports whether now falls inside one
// key's allowed-hours window. A faithful port of OmniRoute's
// isWithinSchedule (apiKeyPolicy.ts:131): disabled → true;
// invalid timezone → true (fail open, never block on a bad zone);
// the day check uses the CURRENT day in the zone, and an
// overnight window (until < from) is open when the local minute
// is at/after from OR before until.
//
// QUIRK (ported, not fixed): the days check runs before the
// overnight rule and names only the current day, so "Fri
// 22:00–06:00" is open Friday evening but NOT Saturday 01:00 —
// Saturday is not in the day list. The pool schedules (M5.9)
// handle the wrap properly (days = the day the window starts
// on); the per-key schedule matches OmniRoute exactly. Go's
// time never yields hour 24, so the "24:xx" → "00:xx"
// normalisation OmniRoute needs for Intl is a no-op here.
func IsWithinAccessSchedule(s *config.AccessScheduleConfig, now time.Time) bool {
	if s == nil || !s.Enabled {
		return true
	}
	loc := time.Local
	if s.TZ != "" {
		l, err := time.LoadLocation(s.TZ)
		if err != nil {
			return true // invalid timezone — fail open
		}
		loc = l
	}
	local := now.In(loc)
	localDay := int(local.Weekday())
	localMinutes := local.Hour()*60 + local.Minute()
	found := false
	for _, d := range s.Days {
		if d == localDay {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	from, err := parseHHMM(s.From)
	if err != nil {
		from = 0
	}
	until, err := parseHHMM(s.Until)
	if err != nil {
		until = 24 * 60
	}
	if until < from {
		return localMinutes >= from || localMinutes < until
	}
	return localMinutes >= from && localMinutes < until
}

// parseHHMM parses an "HH:MM" clock reading into minutes from
// midnight.
func parseHHMM(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("not HH:MM: %q", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("not HH:MM: %q", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("not HH:MM: %q", s)
	}
	return h*60 + m, nil
}

// checkKeyAccessSchedule enforces the caller's allowed-hours
// window. It writes the refusal and returns true when the
// request must stop here. No schedule, or a schedule that is
// disabled, passes. The message is OmniRoute's exact wording.
func (s *Server) checkKeyAccessSchedule(w http.ResponseWriter, r *http.Request) bool {
	keyID := s.callerKeyID(r)
	if keyID == "" || s.cfg == nil {
		return false
	}
	key, ok := s.cfg.FindGatewayKey(keyID)
	if !ok || key.AccessSchedule == nil || !key.AccessSchedule.Enabled {
		return false
	}
	if IsWithinAccessSchedule(key.AccessSchedule, time.Now()) {
		return false
	}
	sch := key.AccessSchedule
	writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
		Message: fmt.Sprintf("Access denied outside allowed hours (%s–%s %s)", sch.From, sch.Until, sch.TZ),
		Type:    "key_access_schedule",
	}})
	return true
}

// throttleKeyDelay waits out the caller's per-request throttle
// delay, if any. It runs after every refusal gate (a refused
// request is never delayed) and before the engine is called.
// False means the request context went away while waiting —
// the caller must stop.
func (s *Server) throttleKeyDelay(r *http.Request) bool {
	keyID := s.callerKeyID(r)
	if keyID == "" || s.cfg == nil {
		return true
	}
	key, ok := s.cfg.FindGatewayKey(keyID)
	if !ok || key.ThrottleDelayMs <= 0 {
		return true
	}
	timer := time.NewTimer(time.Duration(key.ThrottleDelayMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.Context().Done():
		return false
	}
}
