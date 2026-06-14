// Package install implements the per-(plugin, org) install/uninstall flow:
// filesystem dir, plugin_installs row, platform_token generation.
//
// Plugins no longer connect to RW pg-wire — the read-gateway is the sole RW
// reader — so Install provisions no per-org RW schema/views/LOGIN role.
// Uninstall still drops those objects for installs created before that cutover.
//
// Idempotent: every step uses IF NOT EXISTS / OR REPLACE / UPSERT so
// re-installs are safe. The platform_token is generated once and preserved
// across re-installs.
//
// v8: the plugin's install footprint is no longer a hardcoded map here.
// Callers resolve a Footprint from the plugin registry (the plugin's
// self-describing control-plane.json) and pass it in. Install snapshots the
// footprint into plugin_installs so Uninstall is self-sufficient even if the
// plugin is later un-published from the registry.
package install

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LogicalView is a per-tenant view declaration: a view named Name inside the
// per-org schema, scoped by `WHERE org_id = '<uuid>'` over SourceTable.
type LogicalView struct {
	Name        string `json:"name"`
	SourceTable string `json:"source_table"` // fully qualified, e.g. "public.instruments"
}

// QueryEntity declares a SQLite table a plugin exposes for read-only querying
// by the plugin-data datasource. Mirrors the datasource's SQLiteEntity shape.
type QueryEntity struct {
	Name       string            `json:"name"`
	Table      string            `json:"table"`
	Scope      string            `json:"scope"`
	ScopeLabel string            `json:"scope_label"`
	TimeCol    string            `json:"time_col"`
	Grain      []string          `json:"grain"`
	Labels     map[string]string `json:"labels"`
	Columns    []string          `json:"columns"`
	Kinds      map[string]string `json:"kinds"`
}

// Footprint is a plugin's install footprint — the subset of its
// self-describing control-plane.json the control plane acts on. It is read
// from the plugin registry at install time and snapshotted into
// plugin_installs. Note: `required` is NOT here — whether a plugin is
// mandatory is control-plane policy, not something a plugin self-declares.
type Footprint struct {
	PluginID       string        `json:"plugin_id"`
	GrafanaSlug    string        `json:"grafana_slug"`
	Type           string        `json:"type"`
	DisplayName    string        `json:"display_name"`
	Description    string        `json:"description"`
	PlatformPlugin bool          `json:"platform_plugin"`
	LogicalViews   []LogicalView `json:"logical_views"`
	QueryEntities  []QueryEntity `json:"query_entities"`
}

// ValidateType returns nil when the footprint's Type is one of the three
// Grafana plugin kinds (app, datasource, panel) and an error otherwise.
func (f Footprint) ValidateType() error {
	switch f.Type {
	case "app", "datasource", "panel":
		return nil
	case "":
		return errors.New("footprint type is empty: must be one of app, datasource, panel")
	default:
		return fmt.Errorf("footprint type %q is not valid: must be one of app, datasource, panel", f.Type)
	}
}

// Installer wires the install flow against control_db (token persistence) and
// RisingWave. The rwPool is retained only for Uninstall, which drops any
// per-org views/LOGIN role left behind by old installs.
type Installer struct {
	controlPool *pgxpool.Pool
	rwPool      *pgxpool.Pool
	pluginsRoot string
}

// RWPool exposes the privileged RisingWave pool so callers (the
// uninstall worker, in particular) can run their own SELECTs without
// minting a second connection pool. Read-only convention.
func (i *Installer) RWPool() *pgxpool.Pool { return i.rwPool }

// PluginsRoot exposes the filesystem root where per-(plugin, org)
// state dirs live. Uninstall removes the per-(plugin, org) subtree.
func (i *Installer) PluginsRoot() string { return i.pluginsRoot }

func New(controlPool, rwPool *pgxpool.Pool, pluginsRoot string) *Installer {
	return &Installer{
		controlPool: controlPool,
		rwPool:      rwPool,
		pluginsRoot: pluginsRoot,
	}
}

// Result is returned to the operator after install. PlatformToken is the
// authenticator the plugin container needs in its env; this is the only time
// the operator sees it.
type Result struct {
	OrgID         uuid.UUID
	PluginID      string
	ShortID       string
	SQLitePath    string
	PlatformToken string
}

// ErrUnknownPlugin is returned when a plugin cannot be resolved.
var ErrUnknownPlugin = errors.New("unknown plugin")

// Install runs the full install flow for the given footprint.
// shortID is the organisation's short_id. Idempotent across reruns. The
// footprint is snapshotted into plugin_installs so Uninstall can run without
// re-reading the registry.
func (i *Installer) Install(ctx context.Context, orgID uuid.UUID, shortID string, fp Footprint) (Result, error) {
	pluginID := fp.PluginID
	if pluginID == "" {
		return Result{}, ErrUnknownPlugin
	}
	fpJSON, err := json.Marshal(fp)
	if err != nil {
		return Result{}, fmt.Errorf("marshal footprint: %w", err)
	}

	// PlatformPlugin (e.g. core-datasource): no per-org data plane (no RW
	// schema/role/views), but it authenticates to the read-gateway with a
	// per-org platform_token like the app plugins, so generate + persist one.
	// The UPSERT preserves an existing non-empty token on reinstall and
	// backfills it when the stored token is empty (e.g. rows from before the
	// core-datasource gained read-gateway auth).
	if fp.PlatformPlugin {
		newToken, err := randomToken(32)
		if err != nil {
			return Result{}, fmt.Errorf("generate platform token: %w", err)
		}
		var persisted string
		err = i.controlPool.QueryRow(ctx, `
			INSERT INTO plugin_installs (org_id, plugin_id, platform_token, footprint, granted_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (org_id, plugin_id) DO UPDATE
			  SET granted_at = NOW(), footprint = EXCLUDED.footprint,
			      platform_token = CASE WHEN plugin_installs.platform_token = ''
			                            THEN EXCLUDED.platform_token
			                            ELSE plugin_installs.platform_token END,
			      uninstall_state = NULL, uninstall_started_at = NULL,
			      uninstall_offset_events = 0, uninstall_offset_data = 0,
			      uninstall_keys_total = NULL, uninstall_keys_done = 0,
			      uninstall_last_error = NULL
			RETURNING platform_token`,
			orgID, pluginID, newToken, fpJSON,
		).Scan(&persisted)
		if err != nil {
			return Result{}, fmt.Errorf("upsert plugin_installs (platform plugin): %w", err)
		}
		return Result{OrgID: orgID, PluginID: pluginID, ShortID: shortID, PlatformToken: persisted}, nil
	}

	dir := filepath.Join(i.pluginsRoot, pluginID, orgID.String())
	sqlitePath := filepath.Join(dir, "data.db")

	// 1. Filesystem scaffold for the plugin's per-org SQLite. Plugin code
	//    opens this path; control plane only ensures it exists.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// No per-org RW schema/role/views are provisioned: plugins no longer
	// connect to RW pg-wire. The read-gateway is the sole RW reader and
	// connects as its own principal to public.*. (Uninstall still drops
	// any per-org views/role left by old installs — see Uninstall.)

	// 2. Generate platform_token; UPSERT preserves any existing token on
	//    rerun so reinstalls do not invalidate plugin containers already in
	//    the field.
	newToken, err := randomToken(32)
	if err != nil {
		return Result{}, fmt.Errorf("generate platform token: %w", err)
	}
	// Re-install clears any lingering uninstall markers (e.g. a prior
	// uninstall that failed mid-flight): installing is the definitive intent,
	// and both the catalog and the reconciler treat a non-empty
	// uninstall_state as "not installed".
	var persisted string
	err = i.controlPool.QueryRow(ctx, `
		INSERT INTO plugin_installs (org_id, plugin_id, platform_token, footprint, granted_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (org_id, plugin_id) DO UPDATE
		  SET granted_at = NOW(), footprint = EXCLUDED.footprint,
		      uninstall_state = NULL, uninstall_started_at = NULL,
		      uninstall_offset_events = 0, uninstall_offset_data = 0,
		      uninstall_keys_total = NULL, uninstall_keys_done = 0,
		      uninstall_last_error = NULL
		RETURNING platform_token`,
		orgID, pluginID, newToken, fpJSON,
	).Scan(&persisted)
	if err != nil {
		return Result{}, fmt.Errorf("upsert plugin_installs: %w", err)
	}

	return Result{
		OrgID:         orgID,
		PluginID:      pluginID,
		ShortID:       shortID,
		SQLitePath:    sqlitePath,
		PlatformToken: persisted,
	}, nil
}

func randomToken(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Uninstall drops the per-(plugin, org) RW state created by Install:
//
//   - Every view in the footprint snapshot under schema_org_<short>.
//     DROP VIEW IF EXISTS so a partial install (or one whose schema was
//     wiped out of band) doesn't block uninstall.
//   - The per-(plugin, org) LOGIN role. DROP USER cleans up dependent
//     ACL entries; any view drops above release the role's privileges.
//
// The view list comes from the footprint snapshot stored on the
// plugin_installs row at install time, so uninstall does not depend on the
// plugin still being published in the registry.
//
// The shared per-org schema (`schema_org_<short>`) is NOT dropped — it's
// shared with every plugin installed in that org. Re-install of this
// plugin reuses the same schema.
//
// The `plugin_installs` row itself + the per-(plugin, org) state
// directory (/var/lib/plugins/<plugin>/<org_id>/) are the caller's
// problem. This method only handles RW-side teardown so it stays
// reusable from both the operator break-glass path and the
// self-service uninstall worker (ADR-0050).
func (i *Installer) Uninstall(ctx context.Context, orgID uuid.UUID, shortID, pluginID string) error {
	var fpJSON []byte
	err := i.controlPool.QueryRow(ctx,
		`SELECT footprint FROM plugin_installs WHERE org_id = $1 AND plugin_id = $2`,
		orgID, pluginID,
	).Scan(&fpJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownPlugin
		}
		return fmt.Errorf("load footprint snapshot: %w", err)
	}
	var fp Footprint
	if len(fpJSON) > 0 {
		if err := json.Unmarshal(fpJSON, &fp); err != nil {
			return fmt.Errorf("unmarshal footprint snapshot: %w", err)
		}
	}

	role := fmt.Sprintf("%s_org_%s", pluginID, shortID)
	schema := fmt.Sprintf("schema_org_%s", shortID)

	for _, v := range fp.LogicalViews {
		// IF EXISTS so a re-run after a partial drop is still idempotent.
		dropSQL := fmt.Sprintf("DROP VIEW IF EXISTS %s.%s", schema, v.Name)
		if _, err := i.rwPool.Exec(ctx, dropSQL); err != nil {
			return fmt.Errorf("drop view %s.%s: %w", schema, v.Name, err)
		}
	}

	// DROP USER on a missing role would fail; gate on pg_roles.
	var exists bool
	if err := i.rwPool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)`, role,
	).Scan(&exists); err != nil {
		return fmt.Errorf("pg_roles probe %s: %w", role, err)
	}
	if exists {
		if _, err := i.rwPool.Exec(ctx, fmt.Sprintf("DROP USER %s", role)); err != nil {
			return fmt.Errorf("drop user %s: %w", role, err)
		}
	}
	return nil
}
