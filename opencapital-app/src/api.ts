import { invoke } from "@tauri-apps/api/core";
import type { Catalog, KindeProfile, MeOrgs, PluginSource, VersionStatus } from "./types";

export const api = {
  kindeLogin: () => invoke<void>("kinde_login"),
  meOrgs: () => invoke<MeOrgs>("me_orgs"),
  meProfile: () => invoke<KindeProfile>("me_profile"),
  logout: () => invoke<void>("logout"),
  createOrg: (name: string, baseCurrency: string) =>
    invoke<void>("create_org", { name, baseCurrency }),
  catalog: (orgId: string) => invoke<Catalog>("marketplace_catalog", { orgId }),
  pluginVersions: (pluginId: string) =>
    invoke<VersionStatus[]>("plugin_versions", { pluginId }),
  listSources: () => invoke<PluginSource[]>("list_sources"),
  addSource: (manifestUrl: string) =>
    invoke<PluginSource>("add_source", { manifestUrl }),
  removeSource: (manifestUrl: string) =>
    invoke<void>("remove_source", { manifestUrl }),
  // Plugin selection = desired-state. The plugins view edits this; launch
  // reconciles installed == required ∪ selection. No install/uninstall here.
  getPluginSelection: (orgId: string) =>
    invoke<string[]>("get_plugin_selection", { orgId }),
  setPluginSelection: (orgId: string, pluginId: string, selected: boolean) =>
    invoke<void>("set_plugin_selection", { orgId, pluginId, selected }),
  // Seed selection from already-installed plugins on first view (no-op once a
  // selection exists), so migrated orgs don't show installed plugins as pending-uninstall.
  seedPluginSelection: (orgId: string, installed: string[]) =>
    invoke<void>("seed_plugin_selection", { orgId, installed }),
  getShowPreview: () => invoke<boolean>("get_show_preview"),
  setShowPreview: (on: boolean) => invoke<void>("set_show_preview", { on }),
  getPluginPin: (orgId: string, pluginId: string) =>
    invoke<string | null>("get_plugin_pin", { orgId, pluginId }),
  setPluginPin: (orgId: string, pluginId: string, version: string | null) =>
    invoke<void>("set_plugin_pin", { orgId, pluginId, version }),
  launch: (orgId: string, userEmail: string, userName: string) =>
    invoke<void>("launch_grafana", { orgId, userEmail, userName }),
  grafanaRunning: () => invoke<boolean>("grafana_running"),
};

export function errMsg(e: unknown): string {
  if (typeof e === "string") return e;
  if (e instanceof Error) return e.message;
  return String(e);
}
