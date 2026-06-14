package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	ListenAddr      string
	ControlPlaneURL string        // JWKS at $URL/jwt/jwks; portfolios at $URL/portfolios
	JWTIssuer       string
	JWKSRefresh     time.Duration
	RisingwaveDSN   string        // sole RW reader
	OwnershipTTL    time.Duration // portfolios-list cache TTL
	LogLevel        string
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:      envOr("LISTEN_ADDR", ":8095"),
		ControlPlaneURL: os.Getenv("CONTROL_PLANE_URL"),
		JWTIssuer:       envOr("JWT_ISSUER", "control-plane"),
		RisingwaveDSN:   os.Getenv("RISINGWAVE_DSN"),
		LogLevel:        envOr("LOG_LEVEL", "info"),
	}
	d, err := time.ParseDuration(envOr("JWKS_REFRESH", "5m"))
	if err != nil {
		return cfg, fmt.Errorf("JWKS_REFRESH: %w", err)
	}
	cfg.JWKSRefresh = d
	t, err := time.ParseDuration(envOr("OWNERSHIP_TTL", "30s"))
	if err != nil {
		return cfg, fmt.Errorf("OWNERSHIP_TTL: %w", err)
	}
	cfg.OwnershipTTL = t
	if cfg.ControlPlaneURL == "" {
		return cfg, fmt.Errorf("CONTROL_PLANE_URL is required")
	}
	if cfg.RisingwaveDSN == "" {
		return cfg, fmt.Errorf("RISINGWAVE_DSN is required")
	}
	return cfg, nil
}

func (c Config) JWKSURL() string { return trimSlash(c.ControlPlaneURL) + "/jwt/jwks" }

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func trimSlash(s string) string { return strings.TrimRight(s, "/") }
