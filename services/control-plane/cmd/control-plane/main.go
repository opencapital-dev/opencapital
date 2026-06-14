package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/portfolio-management/control-plane/internal/config"
	"github.com/portfolio-management/control-plane/internal/grafana"
	"github.com/portfolio-management/control-plane/internal/httpapi"
	"github.com/portfolio-management/control-plane/internal/install"
	"github.com/portfolio-management/control-plane/internal/janitor"
	"github.com/portfolio-management/control-plane/internal/jwks"
	"github.com/portfolio-management/control-plane/internal/manifest"
	"github.com/portfolio-management/control-plane/internal/migrate"
	"github.com/portfolio-management/control-plane/internal/registry"
	"github.com/portfolio-management/control-plane/internal/signing"
	"github.com/portfolio-management/control-plane/internal/sources"
	"github.com/portfolio-management/control-plane/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Run migrations against control_db. Uses its own short-lived sql.DB.
	migrateCtx, migrateCancel := context.WithTimeout(ctx, 60*time.Second)
	if err := migrate.Run(migrateCtx, cfg.ControlDBDSN); err != nil {
		migrateCancel()
		logger.Error("migrate", "err", err)
		os.Exit(1)
	}
	migrateCancel()

	pool, err := pgxpool.New(ctx, cfg.ControlDBDSN)
	if err != nil {
		logger.Error("pgxpool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		logger.Error("pgx ping", "err", err)
		os.Exit(1)
	}

	keys := signing.NewStore(pool)
	if err := keys.EnsureBootstrap(ctx); err != nil {
		logger.Error("signing bootstrap", "err", err)
		os.Exit(1)
	}
	logger.Info("signing key ready", "kid", keys.Active().Kid)

	var grafanaJWKS *jwks.Client
	if cfg.GrafanaJWKSURL != "" {
		grafanaJWKS = jwks.New(cfg.GrafanaJWKSURL)
		warmCtx, warmCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := grafanaJWKS.Refresh(warmCtx); err != nil {
			// Don't fail boot — Grafana may come up later. Log + continue;
			// KeyFunc will refresh on first verify.
			logger.Warn("grafana JWKS warm-up failed", "err", err, "url", cfg.GrafanaJWKSURL)
		} else {
			logger.Info("grafana JWKS warm", "url", cfg.GrafanaJWKSURL)
		}
		warmCancel()
		go grafanaJWKS.Run(ctx, func(err error) {
			logger.Warn("grafana JWKS refresh", "err", err)
		})
	} else {
		logger.Info("grafana JWKS not configured; /jwt/mint accepts static-IdP only")
	}

	var kindeJWKS *jwks.Client
	if cfg.KindeJWKSURL != "" {
		kindeJWKS = jwks.New(cfg.KindeJWKSURL)
		warmCtx, warmCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := kindeJWKS.Refresh(warmCtx); err != nil {
			logger.Warn("kinde JWKS warm-up failed", "err", err, "url", cfg.KindeJWKSURL)
		} else {
			logger.Info("kinde JWKS warm", "url", cfg.KindeJWKSURL)
		}
		warmCancel()
		go kindeJWKS.Run(ctx, func(err error) {
			logger.Warn("kinde JWKS refresh", "err", err)
		})
	} else {
		logger.Info("kinde JWKS not configured; /v1/* shell endpoints disabled")
	}

	st := store.New(pool)

	var installer *install.Installer
	if cfg.RisingWaveDSN != "" {
		rwPool, err := pgxpool.New(ctx, cfg.RisingWaveDSN)
		if err != nil {
			logger.Error("rw pgxpool", "err", err)
			os.Exit(1)
		}
		defer rwPool.Close()
		if err := rwPool.Ping(ctx); err != nil {
			logger.Warn("rw ping failed at boot", "err", err)
		}
		installer = install.New(pool, rwPool, cfg.PluginsRoot)
		logger.Info("installer ready", "plugins_root", cfg.PluginsRoot)
	} else {
		logger.Info("RISINGWAVE_DSN unset; install endpoint disabled")
	}

	var grafanaClient *grafana.Client
	if cfg.GrafanaBaseURL != "" {
		grafanaClient = grafana.New(cfg.GrafanaBaseURL, cfg.GrafanaAdminUser, cfg.GrafanaAdminPassword)
		logger.Info("grafana admin client ready",
			"base_url", cfg.GrafanaBaseURL,
			"admin_user", cfg.GrafanaAdminUser,
			"admin_password_configured", cfg.GrafanaAdminPassword != "")
	} else {
		logger.Info("GRAFANA_BASE_URL unset; /api/onboarding/* routes disabled")
	}

	if cfg.PluginsManifestURL == "" {
		logger.Error("PLUGINS_MANIFEST_URL is required (curated marketplace list)")
		os.Exit(1)
	}
	// Federated catalog: official list ∪ user-added DB rows → per-plugin
	// manifests. One cached PluginClient per URL behind a small adapter.
	listClient := manifest.NewListClient(cfg.PluginsManifestURL, nil, manifest.DefaultTTL, logger)
	pf := &pluginFetcher{ttl: manifest.DefaultTTL, log: logger, clients: map[string]*manifest.PluginClient{}}
	provider := sources.New(st, listClient, pf)
	reg := registry.NewCatalog(provider, registry.DefaultRequired)
	logger.Info("federated plugin catalog ready", "marketplace_list", cfg.PluginsManifestURL)

	// Staging janitor client (publish/prune path; its own coords + creds).
	staging := registry.NewStaging(cfg.RegistryInternalURL, cfg.RegistryNamespace,
		cfg.RegistryStagingNamespace, os.Getenv("REGISTRY_USERNAME"), os.Getenv("REGISTRY_PASSWORD"))
	if ghToken := os.Getenv("REGISTRY_PASSWORD"); ghToken != "" && cfg.RegistryOwner != "" {
		staging = staging.WithEnumerator(registry.NewGHCREnumerator(ghToken)).WithGHCRDelete(ghToken)
		logger.Info("staging janitor REST enumeration", "owner", cfg.RegistryOwner)
	}

	server := httpapi.New(cfg, keys, st, grafanaJWKS, kindeJWKS, installer, reg, grafanaClient, logger)
	// Install the tombstone client closure NOW that Server exists. The
	// client captures Server.signTombstoneScope so the uninstall worker
	// can mint capability JWTs without dragging the signing key around.
	server.WithGatewayTombstone()

	// v8 — the per-grafana-org AppPluginConfig push that lived here
	// in v6 is gone; instance-bootstrap inside each Grafana process
	// now renders provisioning YAML from plugin_installs at boot.
	// What stays: resume any uninstalls that were in_progress when
	// the process last died.
	if installer != nil {
		go func() {
			bootCtx, bootCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer bootCancel()
			server.ResumeInFlightUninstalls(bootCtx)
		}()
	}

	// Staging janitor. Periodically prunes UNSIGNED artifacts from
	// plugins-staging/*; NEVER touches the trusted namespace. Signed staging
	// tags are retained forever (promotion now happens in CI). Delete via GHCR
	// REST API when REGISTRY_PASSWORD is set; logs decisions but skips deletes
	// otherwise. Bound to ctx; runs one sweep a bit after boot.
	jan := janitor.New(staging, logger)
	logger.Info("staging janitor ready", "delete_enabled", staging.CanPruneStaging())
	go jan.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("control-plane listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "err", err)
	}
}

// pluginFetcher adapts per-URL manifest.PluginClient instances to
// sources.PluginFetcher, reusing one cached client per URL.
type pluginFetcher struct {
	ttl     time.Duration
	log     *slog.Logger
	mu      sync.Mutex
	clients map[string]*manifest.PluginClient
}

func (f *pluginFetcher) Fetch(ctx context.Context, url string) (*manifest.PluginManifest, error) {
	f.mu.Lock()
	c, ok := f.clients[url]
	if !ok {
		c = manifest.NewPluginClient(url, nil, f.ttl, f.log)
		f.clients[url] = c
	}
	f.mu.Unlock()
	return c.Fetch(ctx)
}
