package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) HasUserOrg(ctx context.Context, userID string, orgID uuid.UUID) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM user_org WHERE user_id = $1 AND org_id = $2`,
		userID, orgID,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("user_org lookup: %w", err)
	}
	return n > 0, nil
}

// ListUserOrgs returns the org_ids the given user is a member of. Empty
// slice if none; caller decides whether 0/many is fatal for the use case.
func (s *Store) ListUserOrgs(ctx context.Context, userID string) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT org_id FROM user_org WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("user_org list: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetOrgShortID resolves an org_id UUID to its short_id text used in RW
// schema and role names (e.g. schema_org_0a3f8f72, yfinance_org_0a3f8f72).
// Returns ErrOrgNotFound if absent.
func (s *Store) GetOrgShortID(ctx context.Context, orgID uuid.UUID) (string, error) {
	var shortID string
	err := s.pool.QueryRow(ctx,
		`SELECT short_id FROM organisations WHERE org_id = $1`,
		orgID,
	).Scan(&shortID)
	if err != nil {
		return "", fmt.Errorf("organisations lookup: %w", err)
	}
	return shortID, nil
}

// CreateOrg inserts a new organisations row. short_id is derived from
// the leading 8 hex chars of the org_id (same convention as the seed
// orgs `00000000` and `test2222`). On the rare prefix collision a
// 23505 error bubbles up — caller should retry with a fresh UUID.
// base_currency is the org's default; per-portfolio currency on the
// portfolios table can override.
func (s *Store) CreateOrg(ctx context.Context, name, baseCurrency string) (orgID uuid.UUID, shortID string, err error) {
	orgID = uuid.New()
	shortID = orgID.String()[:8]
	_, err = s.pool.Exec(ctx,
		`INSERT INTO organisations (org_id, short_id, name, base_currency)
		 VALUES ($1, $2, $3, $4)`,
		orgID, shortID, name, baseCurrency,
	)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("insert organisations: %w", err)
	}
	return orgID, shortID, nil
}

// OrgMembership is one row returned by ListOrgMembershipsForUser. Used
// by /api/me to render the org switcher even before the user enters
// Grafana.
type OrgMembership struct {
	OrgID    uuid.UUID
	ShortID  string
	Name     string
	Role     string
	Currency string
}

// ListOrgMembershipsForUser returns every (org, role) pair the user
// belongs to. Empty slice when the user is unaffiliated.
func (s *Store) ListOrgMembershipsForUser(ctx context.Context, userID string) ([]OrgMembership, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT o.org_id, o.short_id, o.name, uo.role, o.base_currency
		   FROM user_org uo
		   JOIN organisations o ON o.org_id = uo.org_id
		  WHERE uo.user_id = $1
		  ORDER BY o.created_at`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list org memberships: %w", err)
	}
	defer rows.Close()
	var out []OrgMembership
	for rows.Next() {
		var m OrgMembership
		if err := rows.Scan(&m.OrgID, &m.ShortID, &m.Name, &m.Role, &m.Currency); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UserIDByExternalID resolves a (provider, external_id) pair to the
// canonical user_id without any org membership check. Returns "" +
// false when the mapping doesn't exist; that signals the onboarding
// path that this Grafana user has never been seen before.
func (s *Store) UserIDByExternalID(ctx context.Context, provider, externalID string) (string, bool, error) {
	var userID string
	err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM user_external_ids
		  WHERE provider = $1 AND external_id = $2`,
		provider, externalID,
	).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("user_external_ids lookup: %w", err)
	}
	return userID, true, nil
}

// EnsureUserExternalID inserts a (provider, external_id) -> user_id
// row if it doesn't already exist. Returns the canonical user_id
// regardless of whether it was inserted or already present. Used by
// the onboarding middleware to lazily provision a control_db user
// the first time a brand-new Grafana session shows up.
//
// Conflict semantics: if (provider, external_id) ALREADY maps to a
// different user_id, that mapping wins — we never overwrite. Caller
// gets the existing user_id back.
func (s *Store) EnsureUserExternalID(ctx context.Context, provider, externalID, proposedUserID, createdBy string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var existing string
	err = tx.QueryRow(ctx,
		`SELECT user_id FROM user_external_ids WHERE provider = $1 AND external_id = $2`,
		provider, externalID,
	).Scan(&existing)
	switch {
	case err == nil:
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return existing, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through
	default:
		return "", fmt.Errorf("probe user_external_ids: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO user_external_ids (provider, external_id, user_id, created_by)
		 VALUES ($1, $2, $3, $4)`,
		provider, externalID, proposedUserID, createdBy,
	); err != nil {
		return "", fmt.Errorf("insert user_external_ids: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return proposedUserID, nil
}

// AddUserToOrg inserts or updates a (user_id, org_id) -> role row.
// Same on-conflict semantics as LinkUser's inner upsert.
func (s *Store) AddUserToOrg(ctx context.Context, userID string, orgID uuid.UUID, role string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO user_org (user_id, org_id, role)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (user_id, org_id) DO UPDATE SET role = EXCLUDED.role`,
		userID, orgID, role,
	)
	if err != nil {
		return fmt.Errorf("upsert user_org: %w", err)
	}
	return nil
}

// PluginInstall captures the row shape the catalog endpoint joins onto
// the static manifest. Mirrors plugin_installs after the 0024 migration.
type PluginInstall struct {
	OrgID            uuid.UUID
	PluginID         string
	PlatformToken    string
	GrantedAt        time.Time
	UninstallState   string // "" | "in_progress" | "failed"
	UninstallOffEvts int
	UninstallOffData int
	UninstallTotal   *int // null until enumeration completes
	UninstallDone    int
	UninstallError   string
}

// ListPluginInstallsForOrg returns every plugin_installs row for the
// org. The catalog endpoint joins this against install.DefaultManifests
// to produce the {installed, required, ...} list.
func (s *Store) ListPluginInstallsForOrg(ctx context.Context, orgID uuid.UUID) ([]PluginInstall, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT org_id, plugin_id, platform_token, granted_at,
		        COALESCE(uninstall_state, ''),
		        uninstall_offset_events,
		        uninstall_offset_data,
		        uninstall_keys_total,
		        uninstall_keys_done,
		        COALESCE(uninstall_last_error, '')
		   FROM plugin_installs WHERE org_id = $1
		  ORDER BY plugin_id`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("list plugin_installs: %w", err)
	}
	defer rows.Close()
	var out []PluginInstall
	for rows.Next() {
		var p PluginInstall
		if err := rows.Scan(
			&p.OrgID, &p.PluginID, &p.PlatformToken, &p.GrantedAt,
			&p.UninstallState,
			&p.UninstallOffEvts, &p.UninstallOffData,
			&p.UninstallTotal, &p.UninstallDone,
			&p.UninstallError,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListAllPluginInstalls returns every (org, plugin) install across the
// whole control_db. Used by the control-plane boot path to push the
// per-(grafana_org) AppPluginConfig to Grafana on every restart so the
// state in Grafana matches the DB exactly (replaces the old static
// provisioning + env_file path).
func (s *Store) ListAllPluginInstalls(ctx context.Context) ([]PluginInstall, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT org_id, plugin_id, platform_token, granted_at,
		        COALESCE(uninstall_state, ''),
		        uninstall_offset_events,
		        uninstall_offset_data,
		        uninstall_keys_total,
		        uninstall_keys_done,
		        COALESCE(uninstall_last_error, '')
		   FROM plugin_installs ORDER BY org_id, plugin_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list plugin_installs (all): %w", err)
	}
	defer rows.Close()
	var out []PluginInstall
	for rows.Next() {
		var p PluginInstall
		if err := rows.Scan(
			&p.OrgID, &p.PluginID, &p.PlatformToken, &p.GrantedAt,
			&p.UninstallState,
			&p.UninstallOffEvts, &p.UninstallOffData,
			&p.UninstallTotal, &p.UninstallDone,
			&p.UninstallError,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkUninstallStarted flips a row to in_progress (and resets any prior
// progress / error state from a previous failed attempt). Idempotent
// inside a retry.
func (s *Store) MarkUninstallStarted(ctx context.Context, orgID uuid.UUID, pluginID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE plugin_installs
		    SET uninstall_state         = 'in_progress',
		        uninstall_started_at    = NOW(),
		        uninstall_offset_events = 0,
		        uninstall_offset_data   = 0,
		        uninstall_keys_total    = NULL,
		        uninstall_keys_done     = 0,
		        uninstall_last_error    = NULL
		  WHERE org_id = $1 AND plugin_id = $2`,
		orgID, pluginID,
	)
	return err
}

// UpdateUninstallProgress writes a checkpoint after a page of tombstones
// completes. Lets the worker resume from this offset after a
// control-plane restart.
func (s *Store) UpdateUninstallProgress(ctx context.Context, orgID uuid.UUID, pluginID string, offEvts, offData, done int, total *int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE plugin_installs
		    SET uninstall_offset_events = $3,
		        uninstall_offset_data   = $4,
		        uninstall_keys_done     = $5,
		        uninstall_keys_total    = COALESCE($6, uninstall_keys_total)
		  WHERE org_id = $1 AND plugin_id = $2`,
		orgID, pluginID, offEvts, offData, done, total,
	)
	return err
}

// MarkUninstallFailed records an error and pauses the worker for this
// row. Operator can inspect the row; subsequent uninstall calls reset
// the state via MarkUninstallStarted.
func (s *Store) MarkUninstallFailed(ctx context.Context, orgID uuid.UUID, pluginID, errStr string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE plugin_installs
		    SET uninstall_state = 'failed',
		        uninstall_last_error = $3
		  WHERE org_id = $1 AND plugin_id = $2`,
		orgID, pluginID, errStr,
	)
	return err
}

// DeletePluginInstall drops the row outright. Final step of a
// successful uninstall.
func (s *Store) DeletePluginInstall(ctx context.Context, orgID uuid.UUID, pluginID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM plugin_installs WHERE org_id = $1 AND plugin_id = $2`,
		orgID, pluginID,
	)
	return err
}

// RoleForUserOrg returns the caller's role in the given org, or empty
// string + false if they have no membership. Used by the plugin
// endpoints to enforce admin-only writes.
func (s *Store) RoleForUserOrg(ctx context.Context, userID string, orgID uuid.UUID) (string, bool, error) {
	var role string
	err := s.pool.QueryRow(ctx,
		`SELECT role FROM user_org WHERE user_id = $1 AND org_id = $2`,
		userID, orgID,
	).Scan(&role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("user_org role lookup: %w", err)
	}
	return role, true, nil
}

func (s *Store) HasPluginInstall(ctx context.Context, orgID uuid.UUID, pluginID string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM plugin_installs WHERE org_id = $1 AND plugin_id = $2`,
		orgID, pluginID,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("plugin_installs lookup: %w", err)
	}
	return n > 0, nil
}

// VerifyPluginToken returns true iff the given platform_token matches the row
// in plugin_installs for (orgID, pluginID). Comparison is constant-time at
// the storage layer; the secret comparison happens by SQL equality which
// already returns a single boolean — the leak surface is the SQL planner's
// timing, identical for matched vs missing tokens.
func (s *Store) VerifyPluginToken(ctx context.Context, orgID uuid.UUID, pluginID, token string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM plugin_installs
		  WHERE org_id = $1 AND plugin_id = $2 AND platform_token = $3`,
		orgID, pluginID, token,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("plugin_installs token check: %w", err)
	}
	return n > 0, nil
}

// UserOrgByExternalID resolves an upstream IdP handle (a Grafana JWT
// sub like "user:2", a Kinde sub, ...) to the (user_id, role) pair the
// caller has for the given org. The lookup joins user_external_ids onto
// user_org so the canonical user_id is what gets returned, never the
// external id. Returns false if no membership.
func (s *Store) UserOrgByExternalID(ctx context.Context, provider, externalID string, orgID uuid.UUID) (userID string, role string, found bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT uo.user_id, uo.role
		   FROM user_external_ids x
		   JOIN user_org uo ON uo.user_id = x.user_id
		  WHERE x.provider = $1 AND x.external_id = $2 AND uo.org_id = $3`,
		provider, externalID, orgID,
	).Scan(&userID, &role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("user_external_ids lookup: %w", err)
	}
	return userID, role, true, nil
}

// LinkResult reports which rows the call materially touched. The link
// endpoint's response uses it so an operator can see whether the call
// created a new external mapping vs. just updated the role.
type LinkResult struct {
	ExternalIDInserted bool
	UserOrgRoleSet     bool
}

// LinkUser writes the (provider, external_id) -> user_id mapping and
// makes sure user_id has the requested role in org_id. Both writes
// happen in a single transaction so a partial failure rolls back.
//
// Conflict semantics:
//   - (provider, external_id) already maps to the SAME user_id -> noop
//     for the external_ids row, role-only update on user_org.
//   - (provider, external_id) maps to a DIFFERENT user_id -> returns
//     ErrExternalIDPointsElsewhere. Caller must DELETE the existing
//     mapping first (the unlink endpoint handles that).
//   - user_org already has the (user_id, org_id) pair -> role is
//     overwritten.
func (s *Store) LinkUser(ctx context.Context, provider, externalID, userID string, orgID uuid.UUID, role, createdBy string) (LinkResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LinkResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var existingUserID string
	err = tx.QueryRow(ctx,
		`SELECT user_id FROM user_external_ids WHERE provider = $1 AND external_id = $2`,
		provider, externalID,
	).Scan(&existingUserID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Insert the mapping fresh.
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_external_ids (provider, external_id, user_id, created_by)
			 VALUES ($1, $2, $3, $4)`,
			provider, externalID, userID, createdBy,
		); err != nil {
			return LinkResult{}, fmt.Errorf("insert user_external_ids: %w", err)
		}
	case err != nil:
		return LinkResult{}, fmt.Errorf("probe user_external_ids: %w", err)
	default:
		if existingUserID != userID {
			return LinkResult{}, ErrExternalIDPointsElsewhere
		}
		// Same canonical user_id; no external_ids change needed.
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO user_org (user_id, org_id, role)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (user_id, org_id) DO UPDATE SET role = EXCLUDED.role`,
		userID, orgID, role,
	); err != nil {
		return LinkResult{}, fmt.Errorf("upsert user_org: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return LinkResult{}, fmt.Errorf("commit: %w", err)
	}
	return LinkResult{
		ExternalIDInserted: errors.Is(err, pgx.ErrNoRows) || existingUserID == "",
		UserOrgRoleSet:     true,
	}, nil
}

// UnlinkResult reports what UnlinkUser materially removed so the
// endpoint can tell the operator whether the org_membership row went
// away too (it only goes away when no other mapping resolves to the
// same canonical user_id).
type UnlinkResult struct {
	OrgMembershipRemoved bool
}

// UnlinkUser removes a (provider, external_id) -> user_id mapping and,
// if no other mapping resolves to the same canonical user_id, also
// drops the user_org row for (user_id, org_id). Single transaction.
//
// Returns ErrUserExternalIDNotFound if the mapping doesn't exist (or
// resolves to a different user_id than the caller asked for) so a typo
// surfaces rather than no-op'ing.
func (s *Store) UnlinkUser(ctx context.Context, provider, externalID, userID string, orgID uuid.UUID) (UnlinkResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return UnlinkResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	tag, err := tx.Exec(ctx,
		`DELETE FROM user_external_ids
		  WHERE provider = $1 AND external_id = $2 AND user_id = $3`,
		provider, externalID, userID,
	)
	if err != nil {
		return UnlinkResult{}, fmt.Errorf("delete user_external_ids: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return UnlinkResult{}, ErrUserExternalIDNotFound
	}

	var remaining int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM user_external_ids WHERE user_id = $1`,
		userID,
	).Scan(&remaining); err != nil {
		return UnlinkResult{}, fmt.Errorf("count remaining external_ids: %w", err)
	}

	var membershipRemoved bool
	if remaining == 0 {
		t, err := tx.Exec(ctx,
			`DELETE FROM user_org WHERE user_id = $1 AND org_id = $2`,
			userID, orgID,
		)
		if err != nil {
			return UnlinkResult{}, fmt.Errorf("delete user_org: %w", err)
		}
		membershipRemoved = t.RowsAffected() > 0
	}

	if err := tx.Commit(ctx); err != nil {
		return UnlinkResult{}, fmt.Errorf("commit: %w", err)
	}
	return UnlinkResult{OrgMembershipRemoved: membershipRemoved}, nil
}

// CountOrgAdmins returns the number of distinct users with role='admin'
// in org_id. Callers use it to refuse an unlink that would drop the org
// to zero admins.
func (s *Store) CountOrgAdmins(ctx context.Context, orgID uuid.UUID) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM user_org WHERE org_id = $1 AND role = 'admin'`,
		orgID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count org admins: %w", err)
	}
	return n, nil
}

// WriteAuditLog appends one row to audit_log. Errors are returned but
// callers typically log-and-continue: an audit-write failure must not
// block the user-visible operation.
func (s *Store) WriteAuditLog(ctx context.Context, entry AuditEntry) error {
	targetJSON, err := json.Marshal(entry.Target)
	if err != nil {
		return fmt.Errorf("marshal audit target: %w", err)
	}
	var ipArg any
	if entry.RequestIP != "" {
		ipArg = entry.RequestIP
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO audit_log (actor, actor_source, action, target, result, request_ip)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6)`,
		entry.Actor, entry.ActorSource, entry.Action, string(targetJSON), entry.Result, ipArg,
	); err != nil {
		return fmt.Errorf("insert audit_log: %w", err)
	}
	return nil
}

// AuditEntry mirrors the audit_log row. RequestIP is optional ("" maps
// to a SQL NULL).
type AuditEntry struct {
	Actor       string
	ActorSource string
	Action      string
	Target      map[string]any
	Result      string
	RequestIP   string
}

// ErrExternalIDPointsElsewhere signals that an admin tried to link a
// provider+external_id pair that already resolves to a different
// canonical user_id. The caller must DELETE the old mapping first
// (this prevents implicit silent re-pointing of an external handle).
var ErrExternalIDPointsElsewhere = errors.New("external_id already mapped to a different user_id")

// ErrUserExternalIDNotFound signals that an unlink targeted a row that
// either doesn't exist or maps the external_id to a different
// canonical user_id than the caller specified.
var ErrUserExternalIDNotFound = errors.New("user_external_ids row not found")

// PortfolioInput is the upsert payload for the canonical control_db.portfolios
// table. Shape mirrors the plugin's v5 UpsertPortfolio (postgres_repo.go)
// with org_id promoted into the primary key.
type PortfolioInput struct {
	OrgID        uuid.UUID
	PortfolioID  uuid.UUID
	BaseCurrency string
	Attributes   map[string]any
	UpdatedBy    string
}

func (s *Store) UpsertPortfolio(ctx context.Context, in PortfolioInput) error {
	attrsJSON, err := json.Marshal(coalesceMap(in.Attributes))
	if err != nil {
		return fmt.Errorf("marshal attributes: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO portfolios
		    (org_id, portfolio_id, base_currency, attributes, updated_at, updated_by)
		VALUES ($1, $2, $3, $4::jsonb, NOW(), $5)
		ON CONFLICT (org_id, portfolio_id) DO UPDATE
		  SET base_currency = EXCLUDED.base_currency,
		      attributes    = EXCLUDED.attributes,
		      updated_at    = EXCLUDED.updated_at,
		      updated_by    = EXCLUDED.updated_by`,
		in.OrgID, in.PortfolioID, in.BaseCurrency, string(attrsJSON), in.UpdatedBy,
	)
	if err != nil {
		return fmt.Errorf("upsert portfolio: %w", err)
	}
	return nil
}

// PortfolioRecord is a read projection of control_db.portfolios for the
// plugin's reference pages. JSON tags match portfolio-admin's PortfolioEntity
// so a GET response unmarshals straight through. updated_at is epoch micros.
type PortfolioRecord struct {
	PortfolioID  string            `json:"portfolio_id"`
	BaseCurrency string            `json:"base_currency"`
	Attributes   map[string]string `json:"attributes"`
	UpdatedAt    int64             `json:"updated_at"`
}

// ListPortfoliosForOrg returns every portfolio owned by org_id, ordered by
// portfolio_id. Returns an empty (non-nil) slice when the org has none.
func (s *Store) ListPortfoliosForOrg(ctx context.Context, orgID uuid.UUID) ([]PortfolioRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT portfolio_id, base_currency, attributes, updated_at
		   FROM portfolios WHERE org_id = $1 ORDER BY portfolio_id`,
		orgID,
	)
	if err != nil {
		return nil, fmt.Errorf("list portfolios: %w", err)
	}
	defer rows.Close()
	out := []PortfolioRecord{}
	for rows.Next() {
		rec, err := scanPortfolio(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// GetPortfolio returns one portfolio scoped to (org_id, portfolio_id).
// ok=false when no such row exists for the org — callers map this to 404;
// a foreign-org id is simply not found (no cross-org disclosure).
func (s *Store) GetPortfolio(ctx context.Context, orgID, portfolioID uuid.UUID) (PortfolioRecord, bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT portfolio_id, base_currency, attributes, updated_at
		   FROM portfolios WHERE org_id = $1 AND portfolio_id = $2`,
		orgID, portfolioID,
	)
	if err != nil {
		return PortfolioRecord{}, false, fmt.Errorf("get portfolio: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return PortfolioRecord{}, false, rows.Err()
	}
	rec, err := scanPortfolio(rows)
	if err != nil {
		return PortfolioRecord{}, false, err
	}
	return rec, true, nil
}

// scanPortfolio reads one portfolios row into a PortfolioRecord: it decodes
// the JSONB attributes column into a string map (portfolio attributes are
// string-valued by contract) and renders updated_at as epoch microseconds,
// the unit portfolio-admin's frontend expects.
func scanPortfolio(rows pgx.Rows) (PortfolioRecord, error) {
	var (
		rec       PortfolioRecord
		pid       uuid.UUID
		attrs     []byte
		updatedAt time.Time
	)
	if err := rows.Scan(&pid, &rec.BaseCurrency, &attrs, &updatedAt); err != nil {
		return PortfolioRecord{}, fmt.Errorf("scan portfolio: %w", err)
	}
	rec.PortfolioID = pid.String()
	rec.Attributes = map[string]string{}
	if len(attrs) > 0 {
		if err := json.Unmarshal(attrs, &rec.Attributes); err != nil {
			return PortfolioRecord{}, fmt.Errorf("decode portfolio attributes: %w", err)
		}
	}
	rec.UpdatedAt = updatedAt.UnixMicro()
	return rec, nil
}

func coalesceMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// PluginSource is one user-added per-plugin manifest URL.
type PluginSource struct {
	ManifestURL string
	Publisher   string
	Enabled     bool
	AddedAt     time.Time
}

func (s *Store) ListPluginSources(ctx context.Context) ([]PluginSource, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT manifest_url, publisher, enabled, added_at FROM plugin_sources ORDER BY added_at`)
	if err != nil {
		return nil, fmt.Errorf("list plugin_sources: %w", err)
	}
	defer rows.Close()
	var out []PluginSource
	for rows.Next() {
		var p PluginSource
		if err := rows.Scan(&p.ManifestURL, &p.Publisher, &p.Enabled, &p.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) CreatePluginSource(ctx context.Context, url, publisher string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO plugin_sources (manifest_url, publisher) VALUES ($1, $2)`, url, publisher)
	if err != nil {
		return fmt.Errorf("create plugin_source: %w", err)
	}
	return nil
}

// DeletePluginSource removes a user-added source. Returns (deleted, error).
func (s *Store) DeletePluginSource(ctx context.Context, url string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM plugin_sources WHERE manifest_url = $1`, url)
	if err != nil {
		return false, fmt.Errorf("delete plugin_source: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
