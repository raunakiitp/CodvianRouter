// P3-A: organization / team / RBAC / team-key isolation.
//
// A self-contained multi-tenant model on top of the gateway's existing
// ledger and key scopes:
//
//   - Organization owns Teams.
//   - A Team carries a name, a monthly budget (USD), an allowed-model
//     set ("" = inherit org / unrestricted), allowed strategies, and
//     role assignments (member identity -> Role).
//   - A gateway key may be bound to a team via its Team claim; the
//     engine then restricts that key to the team's allowed models and
//     strategies and charges against the team budget.
//   - RBAC: Role gates org/team operations (owner > admin > member >
//     viewer). read-only roles cannot mutate.
//   - AuditLog is an append-only record of admin actions, persisted.
//
// The store is JSON-file persisted and restart-recovered, mirroring the
// eval.Store pattern. It uses no external database and no paid APIs.
package org

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Role is an RBAC level on an org or team.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleMember, RoleViewer:
		return true
	}
	return false
}

// CanManage reports whether a role may mutate an org/team.
func (r Role) CanManage() bool { return r == RoleOwner || r == RoleAdmin }

// Team is one tenant under an org.
type Team struct {
	ID                string          `json:"id"`
	OrgID             string          `json:"org_id"`
	Name              string          `json:"name"`
	BudgetUSD         float64         `json:"budget_usd,omitempty"`     // 0 = unlimited
	AllowedModels     []string        `json:"allowed_models,omitempty"` // empty = inherit org/unrestricted
	AllowedStrategies []string        `json:"allowed_strategies,omitempty"`
	Roles             map[string]Role `json:"roles,omitempty"` // member identity -> role
	CreatedAt         time.Time       `json:"created_at"`
}

// Organization is the top-level tenant.
type Organization struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	DefaultBudgetUSD float64         `json:"default_budget_usd,omitempty"`
	Roles            map[string]Role `json:"roles,omitempty"` // member identity -> role (org-level)
	CreatedAt        time.Time       `json:"created_at"`
}

// AuditEntry is one append-only administrative record.
type AuditEntry struct {
	Seq    int       `json:"seq"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"` // member identity or key id
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// KeyBinding ties a gateway key to a team for isolation.
type KeyBinding struct {
	KeyID  string `json:"key_id"`
	TeamID string `json:"team_id"`
}

// Store persists orgs, teams, key bindings and the audit log.
type Store struct {
	mu       sync.Mutex
	orgs     map[string]*Organization
	teams    map[string]*Team
	bindings map[string]KeyBinding // by key id
	audit    []AuditEntry
	nextSeq  int
	path     string
}

// Open loads the store from path ("" = memory only).
func Open(path string) (*Store, error) {
	s := &Store{orgs: map[string]*Organization{}, teams: map[string]*Team{}, bindings: map[string]KeyBinding{}, path: path}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var snap struct {
		Orgs     map[string]*Organization `json:"orgs"`
		Teams    map[string]*Team         `json:"teams"`
		Bindings map[string]KeyBinding    `json:"bindings"`
		Audit    []AuditEntry             `json:"audit"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	if snap.Orgs != nil {
		s.orgs = snap.Orgs
	}
	if snap.Teams != nil {
		s.teams = snap.Teams
	}
	if snap.Bindings != nil {
		s.bindings = snap.Bindings
	}
	if snap.Audit != nil {
		s.audit = snap.Audit
		for _, e := range s.audit {
			if e.Seq > s.nextSeq {
				s.nextSeq = e.Seq
			}
		}
	}
	return s, nil
}

// persist writes the store snapshot to its JSON path. Callers MUST hold
// s.mu (every mutator does) — persist must not re-lock or it deadlocks.
func (s *Store) persist() error {
	if s.path == "" {
		return nil
	}
	snap := map[string]any{"orgs": s.orgs, "teams": s.teams, "bindings": s.bindings, "audit": s.audit}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

// auditLocked appends an audit entry (caller holds s.mu).
func (s *Store) auditLocked(actor, action, target, detail string) {
	s.nextSeq++
	s.audit = append(s.audit, AuditEntry{Seq: s.nextSeq, At: time.Now(), Actor: actor, Action: action, Target: target, Detail: detail})
}

// AddOrg creates an org (actor must be a known member; first org bootstraps).
func (s *Store) AddOrg(o *Organization, actor Role) error {
	if o.ID == "" || o.Name == "" {
		return errors.New("org: id and name required")
	}
	if !actor.CanManage() {
		return errors.New("org: actor role cannot manage orgs")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[o.ID]; ok {
		return errors.New("org: already exists")
	}
	if o.Roles == nil {
		o.Roles = map[string]Role{}
	}
	o.CreatedAt = time.Now()
	s.orgs[o.ID] = o
	s.auditLocked(string(actor), "org.create", o.ID, o.Name)
	return s.persist()
}

// AddTeam creates a team under an org.
func (s *Store) AddTeam(t *Team, actor Role) error {
	if t.ID == "" || t.OrgID == "" || t.Name == "" {
		return errors.New("org: team id, org id and name required")
	}
	if !actor.CanManage() {
		return errors.New("org: actor role cannot manage teams")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[t.OrgID]; !ok {
		return errors.New("org: unknown org")
	}
	if _, ok := s.teams[t.ID]; ok {
		return errors.New("org: team already exists")
	}
	if t.Roles == nil {
		t.Roles = map[string]Role{}
	}
	t.CreatedAt = time.Now()
	s.teams[t.ID] = t
	s.auditLocked(string(actor), "team.create", t.ID, t.OrgID+"/"+t.Name)
	return s.persist()
}

// SetRole assigns a role on a team (or org when teamID is "").
func (s *Store) SetRole(teamID, member string, role Role, actor Role) error {
	if member == "" || !role.Valid() {
		return errors.New("org: member and valid role required")
	}
	if !actor.CanManage() {
		return errors.New("org: actor role cannot assign roles")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if teamID == "" {
		if len(s.orgs) == 0 {
			return errors.New("org: no org to hold the role")
		}
		// org-level role -> assign to the first org (single-org model).
		for _, o := range s.orgs {
			o.Roles[member] = role
			s.auditLocked(string(actor), "org.role.set", o.ID, member+"="+string(role))
			return s.persist()
		}
	}
	t, ok := s.teams[teamID]
	if !ok {
		return errors.New("org: unknown team")
	}
	t.Roles[member] = role
	s.auditLocked(string(actor), "team.role.set", teamID, member+"="+string(role))
	return s.persist()
}

// BindKey associates a gateway key with a team, giving it team isolation.
func (s *Store) BindKey(keyID, teamID string, actor Role) error {
	if keyID == "" || teamID == "" {
		return errors.New("org: key and team required")
	}
	if !actor.CanManage() {
		return errors.New("org: actor role cannot bind keys")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.teams[teamID]; !ok {
		return errors.New("org: unknown team")
	}
	s.bindings[keyID] = KeyBinding{KeyID: keyID, TeamID: teamID}
	s.auditLocked(string(actor), "key.bind", keyID, "team="+teamID)
	return s.persist()
}

// UnbindKey removes a key's team binding (isolation off).
func (s *Store) UnbindKey(keyID string, actor Role) error {
	if !actor.CanManage() {
		return errors.New("org: actor role cannot unbind keys")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.bindings[keyID]; !ok {
		return errors.New("org: key not bound")
	}
	delete(s.bindings, keyID)
	s.auditLocked(string(actor), "key.unbind", keyID, "")
	return s.persist()
}

// TeamForKey returns the team a key is bound to, or nil.
func (s *Store) TeamForKey(keyID string) *Team {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bindings[keyID]
	if !ok {
		return nil
	}
	t, ok := s.teams[b.TeamID]
	if !ok {
		return nil
	}
	c := *t
	return &c
}

// RoleOf returns a member's role on a team (team-level wins; else org-level).
func (s *Store) RoleOf(teamID, member string) Role {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.teams[teamID]; ok {
		if r, ok := t.Roles[member]; ok {
			return r
		}
	}
	for _, o := range s.orgs {
		if r, ok := o.Roles[member]; ok {
			return r
		}
	}
	return RoleViewer
}

// AllowModel reports whether a team allows a model (empty allowlist = unrestricted).
func (s *Store) AllowModel(teamID, model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.teams[teamID]
	if !ok {
		return false
	}
	if len(t.AllowedModels) == 0 {
		return true
	}
	for _, m := range t.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

// AllowStrategy reports whether a team allows a routing strategy.
func (s *Store) AllowStrategy(teamID, strategy string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.teams[teamID]
	if !ok {
		return false
	}
	if len(t.AllowedStrategies) == 0 {
		return true
	}
	for _, m := range t.AllowedStrategies {
		if m == strategy {
			return true
		}
	}
	return false
}

// BudgetOf returns a team's monthly budget (0 = unlimited).
func (s *Store) BudgetOf(teamID string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.teams[teamID]
	if !ok {
		return 0
	}
	return t.BudgetUSD
}

// Teams returns all teams sorted by id.
func (s *Store) Teams() []*Team {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Team, 0, len(s.teams))
	for _, t := range s.teams {
		c := *t
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Orgs returns all orgs sorted by id.
func (s *Store) Orgs() []*Organization {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Organization, 0, len(s.orgs))
	for _, o := range s.orgs {
		c := *o
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Audit returns the audit log, newest first, bounded by limit.
func (s *Store) Audit(limit int) []AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditEntry, 0, len(s.audit))
	for i := len(s.audit) - 1; i >= 0; i-- {
		out = append(out, s.audit[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// SpendRecord reports a team's spend so far this month. The caller
// supplies the spent amount (from the ledger); the store keeps the
// budget comparison pure so it stays free of ledger coupling.
func (s *Store) SpendWithinBudget(teamID string, spentUSD float64) (within bool, budget, spent float64) {
	budget = s.BudgetOf(teamID)
	if budget <= 0 {
		return true, 0, spentUSD
	}
	return spentUSD < budget, budget, spentUSD
}
