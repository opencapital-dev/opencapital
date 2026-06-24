import { invoke } from "@tauri-apps/api/core";
import type { Catalog, KindeProfile, PluginSource } from "./types";

export const api = {
  kindeLogin: () => invoke<void>("kinde_login"),
  meProfile: () => invoke<KindeProfile>("me_profile"),
  logout: () => invoke<void>("logout"),
  catalog: () => invoke<Catalog>("marketplace_catalog"),
  pluginVersions: (pluginId: string) =>
    invoke<string[]>("plugin_versions", { pluginId }),
  listSources: () => invoke<PluginSource[]>("list_sources"),
  addSource: (manifestUrl: string) =>
    invoke<PluginSource>("add_source", { manifestUrl }),
  removeSource: (manifestUrl: string) =>
    invoke<void>("remove_source", { manifestUrl }),
  // Plugin selection = desired-state. The plugins view edits this; launch
  // reconciles installed == required ∪ selection. No install/uninstall here.
  getPluginSelection: () =>
    invoke<string[]>("get_plugin_selection"),
  setPluginSelection: (pluginId: string, selected: boolean) =>
    invoke<void>("set_plugin_selection", { pluginId, selected }),
  // Seed selection from already-installed plugins on first view (no-op once a
  // selection exists), so migrated instances don't show installed plugins as pending-uninstall.
  seedPluginSelection: (installed: string[]) =>
    invoke<void>("seed_plugin_selection", { installed }),
  getPluginPin: (pluginId: string) =>
    invoke<string | null>("get_plugin_pin", { pluginId }),
  setPluginPin: (pluginId: string, version: string | null) =>
    invoke<void>("set_plugin_pin", { pluginId, version }),
  launch: (userEmail: string, userName: string) =>
    invoke<void>("launch_grafana", { userEmail, userName }),
  grafanaRunning: () => invoke<boolean>("grafana_running"),
};

export function errMsg(e: unknown): string {
  if (typeof e === "string") return e;
  if (e instanceof Error) return e.message;
  return String(e);
}
