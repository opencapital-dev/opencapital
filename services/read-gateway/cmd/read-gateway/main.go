// Command read-gateway is the read-path query service.
//
// It verifies session JWTs against control-plane JWKS, resolves portfolio
// ownership, and executes DSL queries against RisingWave.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	jwks "github.com/portfolio-management/jwks"
	"github.com/portfolio-management/read-gateway/internal/auth"
	"github.com/portfolio-management/read-gateway/internal/compile"
	"github.com/portfolio-management/read-gateway/internal/config"
	"github.com/portfolio-management/read-gateway/internal/httpapi"
	"github.com/portfolio-management/read-gateway/internal/rw"
	"github.com/portfolio-management/read-gateway/internal/surface"
	"github.com/portfolio-management/read-gateway/internal/viewschema"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// JWKS — warm once at boot, then run a background refresh. Boot does not
	// fail on a JWKS hiccup; KeyFunc retries on first verify.
	jwksClient := jwks.New(cfg.JWKSURL(), jwks.WithRefreshInterval(cfg.JWKSRefresh))
	warmCtx, warmCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := jwksClient.Refresh(warmCtx); err != nil {
		logger.Warn("JWKS warm-up failed", "err", err, "url", cfg.JWKSURL())
	} else {
		logger.Info("JWKS warm", "url", cfg.JWKSURL())
	}
	warmCancel()
	go jwksClient.Run(ctx, func(err error) { logger.Warn("JWKS refresh", "err", err) })

	verifier := auth.NewVerifier(jwksClient, cfg.JWTIssuer)
	ownership := auth.NewOwnership(cfg.ControlPlaneURL, cfg.OwnershipTTL)

	reader, err := rw.New(ctx, cfg.RisingwaveDSN)
	if err != nil {
		logger.Error("risingwave", "err", err)
		os.Exit(1)
	}
	defer reader.Close()

	schema := viewschema.New(reader)
	bootCtx, bootCancel := context.WithTimeout(ctx, 30*time.Second)
	viewNames := surface.Names()
	if err := schema.Load(bootCtx, viewNames...); err != nil {
		logger.Error("viewschema boot load", "err", err)
		os.Exit(1)
	}
	bootCancel()

	srv := &httpapi.Server{
		Verifier:  verifier,
		Ownership: ownership,
		Reader:    reader,
		Schema:    schema,
		Compiler:  compile.NewCompiler(schema),
	}

	logger.Info("read-gateway listening", "addr", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, srv.Routes()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("listen", "err", err)
		os.Exit(1)
	}
}
