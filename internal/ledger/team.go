package ledger

// Team cost centers (M5.12): teams as key groupings with a shared
// rolling 1d/7d/30d USD budget, ported from the reference router's team model
// (src/lib/db/teams.ts, src/lib/usage/teamUsageLimits.ts).
//
// Membership is a history table, not a flag on the key: a key has at
// most one ACTIVE binding (valid_to_ms IS NULL); reassigning closes
// the old binding and opens a new one, so past usage stays attributed
// to the team the key belonged to when it was spent — the same rule
// the reference router's resolveBillingTeamIdForApiKeyAt applies. Usage rows
// carry the team id at record time (usage_events.team_id), resolved
// inside RecordUsage, so the attribution never depends on the
// membership at read time.
//
// The budget is measured on ESTIMATED LIST cost: the team's usage in
// the window repriced at the catalog's published per-model prices,
// exactly as the reference router's getCommittedTeamEstimatedListCostUsd does.
// The metered cost (the operator price book at record time) is
// reported alongside it for the cost-center view, but it never
// decides the budget. Any unpriced usage in the window fails closed:
// the reference router's exceeded rule is `listCost >= max || hasUnpricedUsage`,
// and the same rule is used here — a team whose usage cannot be
// priced cannot prove it is under budget.
//
// Enforcement mode is "soft_committed_usage" in the reference router's words:
// soft because the cost is an estimate, not because the refusal is
// soft — an exceeded team refuses with 400. Windows roll: when now
// passes budget_reset_at the window advances by the duration until
// it is in the future again (advanceTeamBudgetWindow).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TeamBudgetDuration values, matching the reference router's TeamBudgetDuration.
const (
	TeamBudget1d  = "1d"
	TeamBudget7d  = "7d"
	TeamBudget30d = "30d"
)

// Team is one cost center: a named grouping of gateway keys with an
// optional shared rolling budget.
type Team struct {
	ID             string
	Name           string
	Description    string
	Status         string // "active" | "archived"
	MaxBudgetUSD   *float64
	BudgetDuration string // "", "1d", "7d", "30d"
	// BudgetResetAt is the end of the current window (exclusive).
	// Nil when the team has no budget.
	BudgetResetAt *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ArchivedAt    *time.Time
}

// TeamMember is one key currently assigned to a team.
type TeamMember struct {
	KeyID      string
	AssignedAt time.Time
}

func teamDurationDays(d string) (int, bool) {
	switch d {
	case TeamBudget1d:
		return 1, true
	case TeamBudget7d:
		return 7, true
	case TeamBudget30d:
		return 30, true
	}
	return 0, false
}

// normalizeTeam validates a team and fills in the window end for a
// freshly budgeted team (now + duration). now is used for the
// created/updated stamps when they are zero.
func normalizeTeam(t Team, now time.Time) (Team, error) {
	if t.ID == "" {
		return t, errors.New("ledger: team id is required")
	}
	if t.Name == "" {
		return t, errors.New("ledger: team name is required")
	}
	if t.Status == "" {
		t.Status = "active"
	}
	if t.Status != "active" && t.Status != "archived" {
		return t, fmt.Errorf("ledger: team status %q must be active or archived", t.Status)
	}
	if t.MaxBudgetUSD != nil && *t.MaxBudgetUSD <= 0 {
		return t, errors.New("ledger: team max budget must be greater than 0")
	}
	if t.MaxBudgetUSD == nil && t.BudgetDuration != "" {
		return t, errors.New("ledger: team budget duration requires a max budget")
	}
	if t.MaxBudgetUSD != nil && t.BudgetDuration == "" {
		return t, errors.New("ledger: team max budget requires a budget duration")
	}
	days, ok := teamDurationDays(t.BudgetDuration)
	if t.BudgetDuration != "" && !ok {
		return t, fmt.Errorf("ledger: team budget duration %q must be 1d, 7d or 30d", t.BudgetDuration)
	}
	if t.MaxBudgetUSD == nil {
		t.BudgetResetAt = nil
	} else if t.BudgetResetAt == nil {
		end := now.Add(time.Duration(days) * 24 * time.Hour)
		t.BudgetResetAt = &end
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = now
	}
	if t.Status == "archived" && t.ArchivedAt == nil {
		at := now
		t.ArchivedAt = &at
	}
	if t.Status == "active" {
		t.ArchivedAt = nil
	}
	return t, nil
}

func scanTeamRow(row interface{ Scan(...any) error }) (Team, error) {
	var t Team
	var maxBudget sql.NullFloat64
	var duration, status sql.NullString
	var resetAt, archivedAt sql.NullInt64
	var createdAt, updatedAt int64
	if err := row.Scan(&t.ID, &t.Name, &t.Description, &status,
		&maxBudget, &duration, &resetAt, &createdAt, &updatedAt, &archivedAt); err != nil {
		return t, err
	}
	t.Status = status.String
	if maxBudget.Valid {
		v := maxBudget.Float64
		t.MaxBudgetUSD = &v
	}
	t.BudgetDuration = duration.String
	if resetAt.Valid {
		at := time.UnixMilli(resetAt.Int64).UTC()
		t.BudgetResetAt = &at
	}
	t.CreatedAt = time.UnixMilli(createdAt).UTC()
	t.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	if archivedAt.Valid {
		at := time.UnixMilli(archivedAt.Int64).UTC()
		t.ArchivedAt = &at
	}
	return t, nil
}

// SetTeam creates or replaces a team (full-row upsert).
func (l *Ledger) SetTeam(ctx context.Context, team Team) error {
	now := time.Now().UTC()
	t, err := normalizeTeam(team, now)
	if err != nil {
		return err
	}
	var resetAt, archivedAt sql.NullInt64
	if t.BudgetResetAt != nil {
		resetAt = sql.NullInt64{Int64: t.BudgetResetAt.UnixMilli(), Valid: true}
	}
	if t.ArchivedAt != nil {
		archivedAt = sql.NullInt64{Int64: t.ArchivedAt.UnixMilli(), Valid: true}
	}
	var maxBudget sql.NullFloat64
	if t.MaxBudgetUSD != nil {
		maxBudget = sql.NullFloat64{Float64: *t.MaxBudgetUSD, Valid: true}
	}
	_, err = l.db.ExecContext(ctx, `
		INSERT INTO teams
		  (id, name, description, status, max_budget_usd, budget_duration,
		   budget_reset_at_ms, created_at_ms, updated_at_ms, archived_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  name = excluded.name, description = excluded.description,
		  status = excluded.status, max_budget_usd = excluded.max_budget_usd,
		  budget_duration = excluded.budget_duration,
		  budget_reset_at_ms = excluded.budget_reset_at_ms,
		  updated_at_ms = excluded.updated_at_ms,
		  archived_at_ms = excluded.archived_at_ms`,
		t.ID, t.Name, t.Description, t.Status, maxBudget, t.BudgetDuration,
		resetAt, t.CreatedAt.UnixMilli(), now.UnixMilli(), archivedAt)
	if err != nil {
		return fmt.Errorf("ledger: set team: %w", err)
	}
	return nil
}

// GetTeam returns the team by id, or (nil, nil) when it does not exist.
func (l *Ledger) GetTeam(ctx context.Context, id string) (*Team, error) {
	t, err := scanTeamRow(l.db.QueryRowContext(ctx, `
		SELECT id, name, description, status, max_budget_usd, budget_duration,
		       budget_reset_at_ms, created_at_ms, updated_at_ms, archived_at_ms
		FROM teams WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: get team: %w", err)
	}
	return &t, nil
}

// ListTeams returns all teams in name order.
func (l *Ledger) ListTeams(ctx context.Context) ([]Team, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT id, name, description, status, max_budget_usd, budget_duration,
		       budget_reset_at_ms, created_at_ms, updated_at_ms, archived_at_ms
		FROM teams ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("ledger: list teams: %w", err)
	}
	defer rows.Close()
	var out []Team
	for rows.Next() {
		t, err := scanTeamRow(rows)
		if err != nil {
			return nil, fmt.Errorf("ledger: list teams: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list teams: %w", err)
	}
	return out, nil
}

// DeleteTeam removes the team and closes every active binding to it.
// Usage rows keep their recorded team id; they simply never match a
// team again. Prefer archiving (status "archived") over deleting when
// the history matters.
func (l *Ledger) DeleteTeam(ctx context.Context, id string) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: delete team: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE key_team_bindings SET valid_to_ms = ? WHERE team_id = ? AND valid_to_ms IS NULL`,
		time.Now().UnixMilli(), id); err != nil {
		return fmt.Errorf("ledger: delete team: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM teams WHERE id = ?`, id); err != nil {
		return fmt.Errorf("ledger: delete team: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: delete team: %w", err)
	}
	return nil
}

// AssignKeyToTeam binds a key to a team, closing any previous active
// binding first. Assigning to the team the key already belongs to is
// a no-op. The team must exist and be active.
func (l *Ledger) AssignKeyToTeam(ctx context.Context, keyID, teamID string, now time.Time) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: assign key to team: %w", err)
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM teams WHERE id = ?`, teamID).Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("ledger: assign key to team: team %q does not exist", teamID)
		}
		return fmt.Errorf("ledger: assign key to team: %w", err)
	}
	if status != "active" {
		return fmt.Errorf("ledger: assign key to team: team %q is not active", teamID)
	}
	var current sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT team_id FROM key_team_bindings WHERE gateway_key_id = ? AND valid_to_ms IS NULL`,
		keyID).Scan(&current); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("ledger: assign key to team: %w", err)
	}
	if current.Valid && current.String == teamID {
		return nil
	}
	if current.Valid {
		if _, err := tx.ExecContext(ctx, `
			UPDATE key_team_bindings SET valid_to_ms = ?
			WHERE gateway_key_id = ? AND valid_to_ms IS NULL`,
			now.UnixMilli(), keyID); err != nil {
			return fmt.Errorf("ledger: assign key to team: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO key_team_bindings (gateway_key_id, team_id, valid_from_ms, valid_to_ms)
		VALUES (?, ?, ?, NULL)`, keyID, teamID, now.UnixMilli()); err != nil {
		return fmt.Errorf("ledger: assign key to team: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: assign key to team: %w", err)
	}
	return nil
}

// UnassignKeyFromTeam closes the key's active binding. It returns
// false when the key had no active binding.
func (l *Ledger) UnassignKeyFromTeam(ctx context.Context, keyID string, now time.Time) (bool, error) {
	res, err := l.db.ExecContext(ctx, `
		UPDATE key_team_bindings SET valid_to_ms = ?
		WHERE gateway_key_id = ? AND valid_to_ms IS NULL`,
		now.UnixMilli(), keyID)
	if err != nil {
		return false, fmt.Errorf("ledger: unassign key from team: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ledger: unassign key from team: %w", err)
	}
	return n > 0, nil
}

// ActiveTeamForKey returns the key's currently active team, or
// (nil, nil) when the key belongs to no team.
func (l *Ledger) ActiveTeamForKey(ctx context.Context, keyID string) (*Team, error) {
	t, err := scanTeamRow(l.db.QueryRowContext(ctx, `
		SELECT t.id, t.name, t.description, t.status, t.max_budget_usd, t.budget_duration,
		       t.budget_reset_at_ms, t.created_at_ms, t.updated_at_ms, t.archived_at_ms
		FROM key_team_bindings b JOIN teams t ON t.id = b.team_id
		WHERE b.gateway_key_id = ? AND b.valid_to_ms IS NULL AND t.status = 'active'`,
		keyID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: active team for key: %w", err)
	}
	return &t, nil
}

// TeamIDForKeyAt returns the team a key belonged to at a moment in
// time (valid_from <= at < valid_to, or valid_to NULL). It is what
// attributes historical usage to the right team after a reassignment.
func (l *Ledger) TeamIDForKeyAt(ctx context.Context, keyID string, at time.Time) (string, error) {
	var teamID sql.NullString
	err := l.db.QueryRowContext(ctx, `
		SELECT team_id FROM key_team_bindings
		WHERE gateway_key_id = ? AND valid_from_ms <= ? AND (valid_to_ms IS NULL OR valid_to_ms > ?)
		ORDER BY valid_from_ms DESC LIMIT 1`,
		keyID, at.UnixMilli(), at.UnixMilli()).Scan(&teamID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("ledger: team for key at: %w", err)
	}
	return teamID.String, nil
}

// TeamMembers returns the keys currently assigned to a team, in key
// id order.
func (l *Ledger) TeamMembers(ctx context.Context, teamID string) ([]TeamMember, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT gateway_key_id, valid_from_ms FROM key_team_bindings
		WHERE team_id = ? AND valid_to_ms IS NULL ORDER BY gateway_key_id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("ledger: team members: %w", err)
	}
	defer rows.Close()
	var out []TeamMember
	for rows.Next() {
		var m TeamMember
		var from int64
		if err := rows.Scan(&m.KeyID, &from); err != nil {
			return nil, fmt.Errorf("ledger: team members: %w", err)
		}
		m.AssignedAt = time.UnixMilli(from).UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: team members: %w", err)
	}
	return out, nil
}

// AdvanceTeamBudgetWindow rolls a budgeted team's window forward by
// whole durations until its end is in the future, persisting the
// move. A team with no budget, or a window still open, is returned
// unchanged. This is the reference router's advanceTeamBudgetWindow.
func (l *Ledger) AdvanceTeamBudgetWindow(ctx context.Context, team Team, now time.Time) (Team, error) {
	if team.MaxBudgetUSD == nil || team.BudgetResetAt == nil || team.BudgetDuration == "" {
		return team, nil
	}
	days, ok := teamDurationDays(team.BudgetDuration)
	if !ok {
		return team, nil
	}
	end := *team.BudgetResetAt
	if !now.After(end) && !now.Equal(end) {
		return team, nil
	}
	step := time.Duration(days) * 24 * time.Hour
	for !end.After(now) {
		end = end.Add(step)
	}
	team.BudgetResetAt = &end
	if _, err := l.db.ExecContext(ctx, `
		UPDATE teams SET budget_reset_at_ms = ?, updated_at_ms = ? WHERE id = ?`,
		end.UnixMilli(), now.UnixMilli(), team.ID); err != nil {
		return team, fmt.Errorf("ledger: advance team budget window: %w", err)
	}
	return team, nil
}

// TeamWindow returns the [start, end) of the team's current budget
// window. The caller must advance the window first.
func TeamWindow(team Team) (start, end time.Time) {
	if team.BudgetResetAt == nil {
		return time.Time{}, time.Time{}
	}
	days, ok := teamDurationDays(team.BudgetDuration)
	if !ok {
		return time.Time{}, *team.BudgetResetAt
	}
	end = *team.BudgetResetAt
	return end.Add(-time.Duration(days) * 24 * time.Hour), end
}

// UsageSlicesForTeam is UsageSlices scoped to one team's recorded
// usage rows in a window: successful, unshared rows the team owned
// when they were spent.
func (l *Ledger) UsageSlicesForTeam(ctx context.Context, teamID string, start, end time.Time) ([]ModelUsageSlice, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT provider, model, COUNT(*),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cost_usd), 0),
		       MIN(price_known), MIN(tokens_split_known)
		FROM usage_events
		WHERE team_id = ? AND ok = 1 AND shared = 0
		  AND ts >= ? AND ts < ?
		GROUP BY provider, model
		ORDER BY provider, model`,
		teamID, start.UnixMilli(), end.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("ledger: team usage slices: %w", err)
	}
	defer rows.Close()
	var out []ModelUsageSlice
	for rows.Next() {
		var s ModelUsageSlice
		var priceKnown, splitKnown int
		if err := rows.Scan(&s.Provider, &s.Model, &s.Rows,
			&s.PromptTokens, &s.CompletionTokens, &s.TotalTokens, &s.CostUSD,
			&priceKnown, &splitKnown); err != nil {
			return nil, fmt.Errorf("ledger: team usage slices: %w", err)
		}
		s.PriceKnown = priceKnown == 1
		s.SplitKnown = splitKnown == 1
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: team usage slices: %w", err)
	}
	return out, nil
}

// TeamMemberUsageSlice is one (key, provider, model) group's usage
// inside a team's window — the per-key rows of the cost-center
// report.
type TeamMemberUsageSlice struct {
	KeyID string
	ModelUsageSlice
}

// TeamMemberSlices returns the per-key usage breakdown for a team's
// window, in key id order.
func (l *Ledger) TeamMemberSlices(ctx context.Context, teamID string, start, end time.Time) ([]TeamMemberUsageSlice, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT gateway_key_id, provider, model, COUNT(*),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cost_usd), 0),
		       MIN(price_known), MIN(tokens_split_known)
		FROM usage_events
		WHERE team_id = ? AND ok = 1 AND shared = 0
		  AND ts >= ? AND ts < ?
		GROUP BY gateway_key_id, provider, model
		ORDER BY gateway_key_id, provider, model`,
		teamID, start.UnixMilli(), end.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("ledger: team member slices: %w", err)
	}
	defer rows.Close()
	var out []TeamMemberUsageSlice
	for rows.Next() {
		var s TeamMemberUsageSlice
		var priceKnown, splitKnown int
		if err := rows.Scan(&s.KeyID, &s.Provider, &s.Model, &s.Rows,
			&s.PromptTokens, &s.CompletionTokens, &s.TotalTokens, &s.CostUSD,
			&priceKnown, &splitKnown); err != nil {
			return nil, fmt.Errorf("ledger: team member slices: %w", err)
		}
		s.PriceKnown = priceKnown == 1
		s.SplitKnown = splitKnown == 1
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: team member slices: %w", err)
	}
	return out, nil
}

// activeTeamIDForKeyTx resolves a key's active team inside a
// transaction (used by RecordUsage so the attribution is atomic
// with the row insert).
func activeTeamIDForKeyTx(ctx context.Context, tx *sql.Tx, keyID string) (string, error) {
	var teamID sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT team_id FROM key_team_bindings
		WHERE gateway_key_id = ? AND valid_to_ms IS NULL`, keyID).Scan(&teamID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return teamID.String, nil
}
