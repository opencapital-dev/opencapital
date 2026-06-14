import { invoke } from "@tauri-apps/api/core";
import type { Catalog, KindeProfile, MeOrgs, VersionStatus } from "./types";

export const api = {
  kindeLogin: () => invoke<void>("kinde_login"),
  meOrgs: () => invoke<MeOrgs>("me_orgs"),
  meProfile: () => invoke<KindeProfile>("me_profile"),
  logout: () => invoke<void>("logout"),
  createOrg: (name: string, baseCurrency: string) =>
    invoke<void>("create_org", { name, baseCurrency }),
  catalog: (orgId: string) => invoke<Catalog>("marketplace_catalog", { orgId }),
  install: (orgId: string, pluginId: string) =>
    invoke<void>("install_plugin", { orgId, pluginId }),
  pluginVersions: (pluginId: string) =>
    invoke<VersionStatus[]>("plugin_versions", { pluginId }),
  uninstall: (orgId: string, pluginId: string) =>
    invoke<void>("uninstall_plugin", { orgId, pluginId }),
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
