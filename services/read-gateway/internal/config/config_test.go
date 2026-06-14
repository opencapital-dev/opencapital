package config

import (
	"os"
	"testing"
)

func TestLoad_RequiresControlPlaneAndRW(t *testing.T) {
	os.Clearenv()
	if _, err := Load(); err == nil {
		t.Fatal("want error when CONTROL_PLANE_URL unset")
	}
	os.Setenv("CONTROL_PLANE_URL", "http://cp:8080")
	if _, err := Load(); err == nil {
		t.Fatal("want error when RISINGWAVE_DSN unset")
	}
	os.Setenv("RISINGWAVE_DSN", "postgres://root:root@rw:4566/dev")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cfg.JWKSURL() != "http://cp:8080/jwt/jwks" {
		t.Fatalf("jwks url: %s", cfg.JWKSURL())
	}
}
