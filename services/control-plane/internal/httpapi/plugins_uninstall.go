package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/portfolio-management/control-plane/internal/store"
)

// Async uninstall worker (ADR-0050). Runs detached from the request
// context. Order:
//   1. Disable the plugin in Grafana for this org so no new traffic
//      reaches it during the deletion.
//   2. Page through every Kafka key the plugin produced (filter on
//      plugin_id + org_id via RW) and POST batches to the gateway's
//      /internal/tombstone endpoint. Gateway publishes null payloads;
//      RW's FORMAT UPSERT applies the deletes via the message key.
//      Checkpoint each page to plugin_installs so a control-plane
//      restart resumes from where we left off.
//   3. Drop the RW per-(plugin, org) role + views via
//      installer.Uninstall().
//   4. Remove the per-(plugin, org) state directory.
//   5. DELETE the plugin_installs row. The catalog UI sees state ==
//      "not_installed" on the next poll.
//
// Failures at step 2 mark the row 'failed' with the error string;
// operator + admin can retry by calling DELETE again (which resets to
// 'in_progress' via MarkUninstallStarted). Failures at steps 3-5 are
// rare (DDL + filesystem against the same compose); they also bubble
// up to 'failed' so an operator sees them.

const (
	tombstonePageSize     = 10_000
	tombstoneHTTPTimeout  = 30 * time.Second
	tombstoneWorkerCtxTTL = 10 * time.Minute
)

// gatewayTombstoneClient wraps POST /internal/tombstone on the gateway.
// Authenticated via a short-lived capability JWT minted per call by
// the Server (ADR-0050). The mintFn closure carries the scope into
// PublishBatch so the worker doesn't need direct access to the
// signing key set.
type gatewayTombstoneClient struct {
	baseURL string // e.g. "http://gateway:8090"
	mintFn  func(orgID, pluginID string, topics []string) (string, error)
	http    *http.Client
}

func newGatewayTombstoneClient(baseURL string, mintFn func(orgID, pluginID string, topics []string) (string, error)) *gatewayTombstoneClient {
	if baseURL == "" || mintFn == nil {
		return nil
	}
	return &gatewayTombstoneClient{
		baseURL: baseURL,
		mintFn:  mintFn,
		http:    &http.Client{Timeout: tombstoneHTTPTimeout},
	}
}

// PublishBatch mints a fresh capability JWT scoped to (orgID,
// pluginID, [topic]) and posts the key batch to gateway. Gateway
// verifies signature + audience + scope before publishing tombstones.
func (c *gatewayTombstoneClient) PublishBatch(ctx context.Context, orgID, pluginID, topic string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	tok, err := c.mintFn(orgID, pluginID, []string{topic})
	if err != nil {
		return fmt.Errorf("mint tombstone jwt: %w", err)
	}
	body, _ := json.Marshal(struct {
		Topic string   `json:"topic"`
		Keys  []string `json:"keys"`
	}{Topic: topic, Keys: keys})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/internal/tombstone", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gateway tombstone POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gateway tombstone POST: %d %s", resp.StatusCode, b)
	}
	return nil
}

// runUninstall is the worker entrypoint. ctx is detached from the
// triggering HTTP request; the caller passes context.Background().
func (s *Server) runUninstall(ctx context.Context, orgID uuid.UUID, pluginID, actor string) {
	ctx, cancel := context.WithTimeout(ctx, tombstoneWorkerCtxTTL)
	defer cancel()

	if err := s.uninstallSteps(ctx, orgID, pluginID); err != nil {
		s.logger.Error("plugins uninstall worker", "err", err, "org", orgID, "plugin", pluginID)
		if markErr := s.store.MarkUninstallFailed(ctx, orgID, pluginID, err.Error()); markErr != nil {
			s.logger.Error("plugins uninstall worker: mark failed", "err", markErr)
		}
		s.audit(ctx, store.AuditEntry{
			Actor:       actor,
			ActorSource: "grafana",
			Action:      "plugins.uninstall.failed",
			Target: map[string]any{
				"org_id":    orgID.String(),
				"plugin_id": pluginID,
				"error":     err.Error(),
			},
			Result: "failed",
		})
		return
	}
	s.audit(ctx, store.AuditEntry{
		Actor:       actor,
		ActorSource: "grafana",
		Action:      "plugins.uninstall.done",
		Target: map[string]any{
			"org_id":    orgID.String(),
			"plugin_id": pluginID,
		},
		Result: "ok",
	})
}

func (s *Server) uninstallSteps(ctx context.Context, orgID uuid.UUID, pluginID string) error {
	if s.installer == nil {
		return errors.New("installer not available")
	}
	if s.gatewayTombstone == nil {
		return errors.New("gateway tombstone client not configured")
	}

	shortID, err := s.store.GetOrgShortID(ctx, orgID)
	if err != nil {
		return fmt.Errorf("short_id lookup: %w", err)
	}

	// v8: the per-instance Grafana picks up the disappearance of the
	// plugin at next restart (instance-bootstrap renders provisioning
	// YAML from plugin_installs; the deleted row means no YAML, so
	// Grafana stops loading the plugin). No admin-API disable here.
	rwPool := s.installer.RWPool()
	if rwPool == nil {
		return errors.New("RW pool nil; installer not initialised")
	}

	// Resume from the persisted checkpoint if a prior attempt got
	// part-way through. Fresh attempts start at offset 0.
	rows, err := s.store.ListPluginInstallsForOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("read install row: %w", err)
	}
	var pInstall *store.PluginInstall
	for i := range rows {
		if rows[i].PluginID == pluginID {
			pInstall = &rows[i]
			break
		}
	}
	if pInstall == nil {
		return errors.New("plugin_installs row gone")
	}
	offEvts := pInstall.UninstallOffEvts
	offData := pInstall.UninstallOffData
	done := pInstall.UninstallDone

	// data_log + portfolio_events_log carry the plugin_id column from
	// the V002 RW migration. The schemas were dropped before plugin_id
	// existed reference NULL plugin_id rows (pre-migration data);
	// these are NOT tombstoned. ADR-0050 acknowledges this.
	type stream struct {
		table  string
		topic  string
		offset *int
	}
	streams := []stream{
		{table: "portfolio_events_log", topic: "portfolio_events.v2", offset: &offEvts},
		{table: "data_log", topic: "data.v2", offset: &offData},
	}

	for _, st := range streams {
		for {
			batch, err := selectKeyPage(ctx, rwPool, st.table, orgID.String(), pluginID, *st.offset, tombstonePageSize)
			if err != nil {
				return fmt.Errorf("select keys from %s: %w", st.table, err)
			}
			if len(batch) == 0 {
				break
			}
			if err := s.gatewayTombstone.PublishBatch(ctx, orgID.String(), pluginID, st.topic, batch); err != nil {
				return fmt.Errorf("publish tombstones %s: %w", st.topic, err)
			}
			*st.offset += len(batch)
			done += len(batch)
			if err := s.store.UpdateUninstallProgress(ctx, orgID, pluginID, offEvts, offData, done, nil); err != nil {
				return fmt.Errorf("checkpoint progress: %w", err)
			}
			if len(batch) < tombstonePageSize {
				break
			}
		}
	}

	// Step 3: drop RW role + views.
	if err := s.installer.Uninstall(ctx, orgID, shortID, pluginID); err != nil {
		return fmt.Errorf("rw uninstall: %w", err)
	}

	// Step 4: remove the per-(plugin, org) state dir. Plugin
	// subprocess may still hold the SQLite FD open; under POSIX the
	// unlink is safe — the inode survives until the FD closes and the
	// next open() returns ENOENT, which the SDK retries through.
	stateDir := filepath.Join(s.installer.PluginsRoot(), pluginID, orgID.String())
	if err := os.RemoveAll(stateDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rm state dir %s: %w", stateDir, err)
	}

	// Step 5: drop the row. Catalog UI now shows "not installed".
	if err := s.store.DeletePluginInstall(ctx, orgID, pluginID); err != nil {
		return fmt.Errorf("delete plugin_installs row: %w", err)
	}
	return nil
}

// selectKeyPage runs the paged enumeration query against RW.
// data_log/portfolio_events_log carry an `rw_key` column from the
// INCLUDE KEY AS rw_key clause in the source DDL; this is the Kafka
// message key the gateway used to publish, which is what we tombstone
// against.
func selectKeyPage(ctx context.Context, pool *pgxpool.Pool, table, orgID, pluginID string, offset, limit int) ([]string, error) {
	rows, err := pool.Query(ctx,
		fmt.Sprintf(`SELECT rw_key FROM %s WHERE org_id = $1 AND plugin_id = $2
		              ORDER BY rw_key LIMIT %d OFFSET $3`, table, limit),
		orgID, pluginID, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ResumeInFlightUninstalls is called once on control-plane startup
// after the Server is ready. Picks up any plugin_installs rows whose
// uninstall_state is 'in_progress' (the prior process crashed
// mid-worker) and re-launches the worker from the persisted offsets.
func (s *Server) ResumeInFlightUninstalls(ctx context.Context) {
	if s.installer == nil || s.gatewayTombstone == nil {
		return
	}
	all, err := s.store.ListAllPluginInstalls(ctx)
	if err != nil {
		s.logger.Error("resume uninstalls: list", "err", err)
		return
	}
	for _, p := range all {
		if p.UninstallState != "in_progress" {
			continue
		}
		s.logger.Info("resuming in-flight uninstall",
			"org", p.OrgID, "plugin", p.PluginID,
			"offset_events", p.UninstallOffEvts,
			"offset_data", p.UninstallOffData)
		go s.runUninstall(context.Background(), p.OrgID, p.PluginID, "bootstrap-resume")
	}
}

