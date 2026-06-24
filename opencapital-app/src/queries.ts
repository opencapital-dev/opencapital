import {
  useMutation,
  useQuery,
  useQueryClient,
  type QueryClient,
} from "@tanstack/react-query";
import { api } from "./api";
import type { CatalogEntry } from "./types";

// One query backs the Plugins view: the catalog plus the desired selection and
// per-plugin version pins. Kept together so the view has a single source of
// truth and one loading state.
export type CatalogData = {
  catalog: CatalogEntry[];
  selection: string[];
  pins: Record<string, string | null>;
};

export const catalogKey = ["catalog"] as const;
export const versionsKey = (id: string) => ["plugin-versions", id] as const;

// Composite read. Seeding is idempotent (no-op once a selection exists), so it's
// safe on every refetch — including the silent background revalidations that
// keep the list current without blocking the UI.
export async function fetchCatalog(): Promise<CatalogData> {
  const c = await api.catalog();
  const installedOptional = c.plugins
    .filter((p) => p.installed && !p.required)
    .map((p) => p.plugin_id);
  await api.seedPluginSelection(installedOptional);
  const selection = await api.getPluginSelection();
  const pinEntries = await Promise.all(
    c.plugins
      .filter((p) => p.installed)
      .map(
        async (p) =>
          [p.plugin_id, await api.getPluginPin(p.plugin_id)] as [
            string,
            string | null,
          ],
      ),
  );
  return {
    catalog: c.plugins,
    selection,
    pins: Object.fromEntries(pinEntries),
  };
}

export const catalogQueryOptions = {
  queryKey: catalogKey,
  queryFn: fetchCatalog,
};

// Warm the cache before the user opens the Plugins page (called on login). The
// list is then instant on first open, and subsequent visits reuse the cache and
// revalidate in the background.
export function prefetchCatalog(qc: QueryClient) {
  return qc.prefetchQuery(catalogQueryOptions);
}

export function useCatalog() {
  return useQuery(catalogQueryOptions);
}

// Versions load lazily — only once a card's version menu opens — so they stay
// out of the initial fetch, but are cached per plugin once seen.
export function useVersions(pluginId: string, enabled: boolean) {
  return useQuery({
    queryKey: versionsKey(pluginId),
    queryFn: () => api.pluginVersions(pluginId),
    enabled,
    staleTime: 5 * 60_000,
  });
}

// Selection/pin writes update the cache optimistically (snappy toggles), roll
// back on error, and revalidate on settle so the cache converges on server truth.
export function useToggleSelection() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ pluginId, selected }: { pluginId: string; selected: boolean }) =>
      api.setPluginSelection(pluginId, selected),
    onMutate: async ({ pluginId, selected }) => {
      await qc.cancelQueries({ queryKey: catalogKey });
      const prev = qc.getQueryData<CatalogData>(catalogKey);
      if (prev) {
        const selection = selected
          ? [...new Set([...prev.selection, pluginId])]
          : prev.selection.filter((id) => id !== pluginId);
        qc.setQueryData<CatalogData>(catalogKey, { ...prev, selection });
      }
      return { prev };
    },
    onError: (_e, _v, ctx) => {
      if (ctx?.prev) qc.setQueryData(catalogKey, ctx.prev);
    },
    onSettled: () => qc.invalidateQueries({ queryKey: catalogKey }),
  });
}

export function useSetPin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ pluginId, version }: { pluginId: string; version: string | null }) =>
      api.setPluginPin(pluginId, version),
    onMutate: async ({ pluginId, version }) => {
      await qc.cancelQueries({ queryKey: catalogKey });
      const prev = qc.getQueryData<CatalogData>(catalogKey);
      if (prev) {
        qc.setQueryData<CatalogData>(catalogKey, {
          ...prev,
          pins: { ...prev.pins, [pluginId]: version },
        });
      }
      return { prev };
    },
    onError: (_e, _v, ctx) => {
      if (ctx?.prev) qc.setQueryData(catalogKey, ctx.prev);
    },
    onSettled: () => qc.invalidateQueries({ queryKey: catalogKey }),
  });
}
