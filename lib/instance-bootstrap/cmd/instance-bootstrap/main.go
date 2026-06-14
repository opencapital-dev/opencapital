// instance-bootstrap reconciles one instance's Grafana plugin set against
// the control plane: it fetches the org's installed plugins, downloads +
// verifies + extracts each host-platform binary from the artifact host,
// symlinks them into the plugins dir, prunes plugins no longer installed,
// and renders provisioning YAML. Designed to run as the first step of the
// Grafana process/container entrypoint, before grafana-server itself.
//
// Subcommands (first positional arg):
//
//	reconcile (default, also when absent) — full reconcile: fetch, install, render.
//	library-panels                        — post-start: upsert each plugin's
//	                                        library panels via the Grafana HTTP API.
//
// Env vars for reconcile (all required unless noted):
//
//	ORG_ID
//	CONTROL_PLANE_URL
//	BOOTSTRAP_TOKEN
//	GRAFANA_PROVISIONING_DIR        (e.g. /etc/grafana/provisioning)
//	GRAFANA_PLUGINS_DIR             (where plugin binaries are symlinked)
//	PLUGIN_CACHE_DIR                (machine-wide extracted-binary cache)
//	PLUGIN_CONTROL_PLANE_URL        (URL plugins use; often == CONTROL_PLANE_URL)
//	PLUGIN_GATEWAY_URL
//	PLUGIN_OTLP_ENDPOINT            (optional)
//	INSTANCE_TOKEN_URL             (loopback URL serving the instance token)
//	PLUGIN_RISINGWAVE_HOST         (RW host plugins connect to; e.g. localhost)
//	PLUGIN_RISINGWAVE_PORT         (RW pg-wire port; e.g. 4566)
//	PLATFORM                        (optional; defaults to host GOOS-GOARCH)
//
// Additional env vars for library-panels (all reconcile vars plus):
//
//	GRAFANA_URL                    (base URL of the local Grafana; e.g. http://localhost:3000)
//	GRAFANA_WEBAUTH_USER           (desktop: header user for auth.proxy; takes priority over Basic)
//	GRAFANA_BASIC_USER             (cloud: admin username for HTTP Basic)
//	GRAFANA_BASIC_PASSWORD         (cloud: admin password for HTTP Basic)
//
// Exit codes:
//
//	0  on success
//	1  on validation/fetch/install/render/upsert failure
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	bootstrap "github.com/portfolio-management/instance-bootstrap"
)

func main() {
	cfg := bootstrap.Config{
		OrgID:                 os.Getenv("ORG_ID"),
		ControlPlaneURL:       os.Getenv("CONTROL_PLANE_URL"),
		BootstrapToken:        os.Getenv("BOOTSTRAP_TOKEN"),
		ProvisioningDir:       os.Getenv("GRAFANA_PROVISIONING_DIR"),
		PluginsDir:            os.Getenv("GRAFANA_PLUGINS_DIR"),
		PluginCacheDir:        os.Getenv("PLUGIN_CACHE_DIR"),
		PluginControlPlaneURL: os.Getenv("PLUGIN_CONTROL_PLANE_URL"),
		PluginGatewayURL:      os.Getenv("PLUGIN_GATEWAY_URL"),
		PluginReadGatewayURL:  os.Getenv("PLUGIN_READ_GATEWAY_URL"),
		PluginComputeURL:      os.Getenv("PLUGIN_COMPUTE_URL"),
		PluginOTLPEndpoint:    os.Getenv("PLUGIN_OTLP_ENDPOINT"),
		InstanceTokenURL:      os.Getenv("INSTANCE_TOKEN_URL"),
		PluginRisingWaveHost:  os.Getenv("PLUGIN_RISINGWAVE_HOST"),
		PluginRisingWavePort:  os.Getenv("PLUGIN_RISINGWAVE_PORT"),
		PluginStateDir:        os.Getenv("PLUGIN_STATE_DIR"),
		Platform:              os.Getenv("PLATFORM"),
		Pins:                  bootstrap.ParsePins(os.Getenv("PLUGIN_PINS")),
		GrafanaURL:            os.Getenv("GRAFANA_URL"),
		GrafanaWebAuthUser:    os.Getenv("GRAFANA_WEBAUTH_USER"),
		GrafanaBasicUser:      os.Getenv("GRAFANA_BASIC_USER"),
		GrafanaBasicPassword:  os.Getenv("GRAFANA_BASIC_PASSWORD"),
		// HTTPTimeout left zero: fetch defaults to 10s, downloads to 5m.
	}

	sub := ""
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}

	switch sub {
	case "library-panels":
		// Post-start: Grafana must already be running. Budget a couple of
		// minutes to cover the health-wait plus the upsert loop.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := bootstrap.ProvisionLibraryPanels(ctx, cfg); err != nil {
			log.Fatalf("instance-bootstrap library-panels: %v", err)
		}
		fmt.Println("instance-bootstrap: library panels provisioned")

	default: // absent or "reconcile"
		// Generous overall budget: several plugin binaries may download on a
		// cold cache.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		n, err := bootstrap.Run(ctx, cfg)
		if err != nil {
			log.Fatalf("instance-bootstrap: %v", err)
		}
		fmt.Printf("instance-bootstrap: provisioned %d plugins\n", n)
	}
}
