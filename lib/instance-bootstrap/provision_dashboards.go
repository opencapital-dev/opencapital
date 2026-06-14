package bootstrap

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// dashboardProviderDoc matches Grafana's dashboard provisioning provider schema.
// Note: dashboard providers use camelCase `orgId`, unlike the plugins provisioning
// which uses snake_case `org_id`.
type dashboardProviderDoc struct {
	APIVersion int                 `yaml:"apiVersion"`
	Providers  []dashboardProvider `yaml:"providers"`
}

type dashboardProvider struct {
	Name                  string                   `yaml:"name"`
	OrgID                 int                      `yaml:"orgId"`
	Type                  string                   `yaml:"type"`
	DisableDeletion       bool                     `yaml:"disableDeletion"`
	AllowUIUpdates        bool                     `yaml:"allowUiUpdates"`
	UpdateIntervalSeconds int                      `yaml:"updateIntervalSeconds"`
	Options               dashboardProviderOptions `yaml:"options"`
}

type dashboardProviderOptions struct {
	Path                      string `yaml:"path"`
	FoldersFromFilesStructure bool   `yaml:"foldersFromFilesStructure"`
}

// provisionDashboards copies each app plugin's bundled dashboard JSON files into
// a dedicated content subroot, <ProvisioningDir>/dashboards/plugins/<plugin_id>/,
// and writes a single provider YAML pointing at <ProvisioningDir>/dashboards/plugins.
//
// The provider YAML lives at <ProvisioningDir>/dashboards/plugin-dashboards.yaml:
// Grafana scans <ProvisioningDir>/dashboards/*.yaml (non-recursively) for provider
// configs at boot, so the YAML must sit directly under dashboards/, while the
// plugins/ subroot it points at holds only plugin-owned JSON. The subroot isolates
// this read-only provider from any sibling dir the launcher fills (e.g. dashboards/json).
func provisionDashboards(cfg Config, plugins []Plugin) error {
	dashRoot := filepath.Join(cfg.ProvisioningDir, "dashboards")
	contentRoot := filepath.Join(dashRoot, "plugins")
	if err := os.MkdirAll(contentRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", contentRoot, err)
	}

	// Build desired set of app plugins that have dashboards.
	type pluginDash struct {
		pluginID string
		title    string
		srcDir   string
	}
	var toProvision []pluginDash
	desiredIDs := make(map[string]bool, len(plugins))

	for _, p := range plugins {
		if p.Type != "app" || p.GrafanaSlug == "" {
			continue
		}
		srcDir := filepath.Join(cfg.PluginsDir, p.GrafanaSlug, "dashboards")
		if _, err := os.Stat(srcDir); os.IsNotExist(err) {
			// Plugin ships no dashboards/ dir — not an error.
			continue
		}
		title := pluginDisplayName(cfg.PluginsDir, p.GrafanaSlug, p.PluginID)
		desiredIDs[title] = true
		toProvision = append(toProvision, pluginDash{pluginID: p.PluginID, title: title, srcDir: srcDir})
	}

	// Prune dirs for plugins no longer in the desired set.
	if err := pruneDashboardDirs(contentRoot, desiredIDs); err != nil {
		return fmt.Errorf("prune dashboard dirs: %w", err)
	}

	// Copy dashboard JSON files for each app plugin, reconciling exactly: clear the
	// plugin's dest dir first so a dashboard dropped in a new bundle version stops
	// being provisioned (Grafana only unprovisions when the JSON file disappears).
	for _, pd := range toProvision {
		destDir := filepath.Join(contentRoot, pd.title)
		if err := os.RemoveAll(destDir); err != nil {
			return fmt.Errorf("clear dashboard dir for %s: %w", pd.pluginID, err)
		}
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", destDir, err)
		}
		if err := copyDashboardJSON(pd.srcDir, destDir); err != nil {
			return fmt.Errorf("copy dashboards for %s: %w", pd.pluginID, err)
		}
	}

	// Write the single provider YAML (harmless even when toProvision is empty).
	return writeDashboardProviderYAML(cfg, dashRoot, contentRoot)
}

// pruneDashboardDirs removes subdirs under contentRoot whose name is not in
// desiredIDs. It is scoped to the dashboards/plugins subroot, so it can never
// touch a sibling like dashboards/json or the provider YAML under dashboards/.
func pruneDashboardDirs(contentRoot string, desiredIDs map[string]bool) error {
	entries, err := os.ReadDir(contentRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if desiredIDs[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(contentRoot, e.Name())); err != nil {
			return fmt.Errorf("remove stale dashboard dir %s: %w", e.Name(), err)
		}
	}
	return nil
}

// copyDashboardJSON recursively copies every *.json file from srcDir into destDir,
// preserving each file's subpath so nested bundle dirs become nested Grafana folders
// under foldersFromFilesStructure. Non-.json files are skipped.
func copyDashboardJSON(srcDir, destDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(destDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyFile(path, dst)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func writeDashboardProviderYAML(cfg Config, dashRoot, contentRoot string) error {
	doc := dashboardProviderDoc{
		APIVersion: 1,
		Providers: []dashboardProvider{{
			Name:                  "plugin-dashboards",
			OrgID:                 1,
			Type:                  "file",
			DisableDeletion:       true,
			AllowUIUpdates:        false,
			UpdateIntervalSeconds: 30,
			Options: dashboardProviderOptions{
				Path:                      contentRoot,
				FoldersFromFilesStructure: true,
			},
		}},
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal dashboard provider yaml: %w", err)
	}
	path := filepath.Join(dashRoot, "plugin-dashboards.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write dashboard provider yaml: %w", err)
	}
	return nil
}
