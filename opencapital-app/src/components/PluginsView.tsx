import { useEffect, useMemo, useState } from "react";
import { css } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import {
  Alert,
  Badge,
  Button,
  Card,
  Dropdown,
  EmptyState,
  FilterInput,
  InlineField,
  Menu,
  Spinner,
  Stack,
  Switch,
  Text,
  useStyles2,
} from "@grafana/ui";
import { api, errMsg } from "../api";
import type { CatalogEntry, Org, VersionStatus } from "../types";

// Versions in the registry are mixed: legacy bare ("0.1.0") and new
// v-prefixed ("v1.0.4"). Display them uniformly as "vX.Y.Z" without
// double-prefixing. API calls + equality checks use the raw value.
const fmtVersion = (v?: string | null) => (v ? `v${v.replace(/^v/, "")}` : v);

type Props = { org: Org };

export function PluginsView({ org }: Props) {
  const styles = useStyles2(getStyles);
  const [catalog, setCatalog] = useState<CatalogEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [working, setWorking] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [dirty, setDirty] = useState(false);
  const [versions, setVersions] = useState<Record<string, VersionStatus[]>>({});
  const [pins, setPins] = useState<Record<string, string | null>>({});
  const [showPreview, setShowPreview] = useState(false);

  async function load() {
    setLoading(true);
    setError("");
    try {
      const c = await api.catalog(org.org_id);
      setCatalog(c.plugins);
      const pinEntries = await Promise.all(
        c.plugins
          .filter((p) => p.installed)
          .map(async (p) => {
            const pin = await api.getPluginPin(org.org_id, p.plugin_id);
            return [p.plugin_id, pin] as [string, string | null];
          })
      );
      setPins(Object.fromEntries(pinEntries));
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    setDirty(false);
    api.getShowPreview().then(setShowPreview).catch(() => {});
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [org.org_id]);

  async function toggle(p: CatalogEntry) {
    setWorking(p.plugin_id);
    setError("");
    try {
      if (p.installed) {
        await api.uninstall(org.org_id, p.plugin_id);
      } else {
        await api.install(org.org_id, p.plugin_id);
      }
      setDirty(true);
      await load();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setWorking(null);
    }
  }

  async function onSelect(p: CatalogEntry, version: string | null) {
    try {
      await api.setPluginPin(org.org_id, p.plugin_id, version);
      setDirty(true);
      setPins((prev) => ({ ...prev, [p.plugin_id]: version }));
    } catch (e) {
      setError(errMsg(e));
    }
  }

  async function loadVersions(p: CatalogEntry) {
    if (versions[p.plugin_id]) return;
    try {
      const vs = await api.pluginVersions(p.plugin_id);
      setVersions((prev) => ({ ...prev, [p.plugin_id]: vs }));
    } catch (e) {
      setError(errMsg(e));
    }
  }

  async function handleShowPreview(v: boolean) {
    setShowPreview(v);
    await api.setShowPreview(v).catch(() => {});
  }

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return catalog;
    return catalog.filter(
      (p) =>
        p.display_name.toLowerCase().includes(q) ||
        p.plugin_id.toLowerCase().includes(q) ||
        p.description.toLowerCase().includes(q)
    );
  }, [catalog, search]);

  const installedCount = catalog.filter((p) => p.installed).length;

  return (
    <div className={styles.wrap}>
      <header className={styles.head}>
        <div>
          <Text element="h1" variant="h3">
            Plugins
          </Text>
          <Text color="secondary">
            {installedCount} installed in {org.name}. Changes apply on next launch.
          </Text>
        </div>
        <Stack alignItems="center" gap={2}>
          <InlineField label="Show preview versions" transparent>
            <Switch value={showPreview} onChange={(e) => handleShowPreview(e.currentTarget.checked)} />
          </InlineField>
          <FilterInput
            value={search}
            onChange={setSearch}
            placeholder="Search plugins…"
            width={32}
          />
        </Stack>
      </header>

      {dirty && (
        <Alert title="Relaunch to apply" severity="info">
          You changed installed plugins. Relaunch Grafana from the Launch tab to load them.
        </Alert>
      )}
      {error && (
        <Alert title="Plugin operation failed" severity="error" onRemove={() => setError("")}>
          {error}
        </Alert>
      )}

      {loading ? (
        <div className={styles.center}>
          <Spinner size="lg" />
        </div>
      ) : filtered.length === 0 ? (
        <EmptyState variant="not-found" message="No plugins match your search." />
      ) : (
        <Stack direction="column" gap={1}>
          {filtered.map((p) => {
            const pin = pins[p.plugin_id] ?? null;
            const vsList = versions[p.plugin_id] ?? [];
            const pinnedStatus = vsList.find((vs) => vs.version === pin);
            const isPinnedPreview = pin !== null && pinnedStatus !== undefined && !pinnedStatus.validated;

            const versionLabel = pin
              ? `${fmtVersion(pin)}${isPinnedPreview ? " (preview)" : ""}`
              : "Latest";

            const visibleVersions = vsList.filter(
              (vs) => vs.validated || showPreview || vs.version === pin
            );

            return (
              <Card key={p.plugin_id}>
                <Card.Heading>{p.display_name}</Card.Heading>
                <Card.Meta>{p.plugin_id}</Card.Meta>
                <Card.Description>{p.description}</Card.Description>
                <Card.Tags>
                  <div className={styles.tags}>
                    <Stack alignItems="center" gap={1}>
                      <Badge text={versionLabel} color="blue" />
                      {p.type === "datasource" && (
                        <Badge text="Datasource" color="blue" icon="database" />
                      )}
                      {p.type === "app" && (
                        <Badge text="App" color="purple" />
                      )}
                      {p.type === "panel" && (
                        <Badge text="Panel" color="green" />
                      )}
                      {p.required && <Badge text="Required" color="purple" />}
                      {p.installed && <Badge text="Installed" color="green" icon="check" />}
                    </Stack>
                    {p.installed && (
                      <Dropdown
                        onVisibleChange={(v) => v && loadVersions(p)}
                        overlay={() => (
                          <Menu>
                            <Menu.Item
                              label="Latest"
                              active={pin === null}
                              onClick={() => onSelect(p, null)}
                            />
                            {visibleVersions.map((vs) => (
                              <Menu.Item
                                key={vs.version}
                                label={vs.validated ? fmtVersion(vs.version)! : `${fmtVersion(vs.version)}  (preview)`}
                                active={vs.version === pin}
                                onClick={() => onSelect(p, vs.version)}
                              />
                            ))}
                          </Menu>
                        )}
                      >
                        <Button
                          size="sm"
                          variant="secondary"
                          fill="text"
                          icon="angle-down"
                          disabled={working === p.plugin_id}
                        >
                          {versionLabel}
                        </Button>
                      </Dropdown>
                    )}
                  </div>
                </Card.Tags>
                <Card.Actions>
                  {pin !== null && (
                    <Button
                      size="sm"
                      variant="secondary"
                      fill="text"
                      onClick={() => onSelect(p, null)}
                    >
                      Use latest validated
                    </Button>
                  )}
                  {p.required ? (
                    <Button variant="secondary" disabled icon="lock">
                      Required
                    </Button>
                  ) : p.installed ? (
                    <Button
                      variant="destructive"
                      fill="outline"
                      icon={working === p.plugin_id ? undefined : "trash-alt"}
                      onClick={() => toggle(p)}
                      disabled={working !== null}
                    >
                      {working === p.plugin_id ? "Removing…" : "Uninstall"}
                    </Button>
                  ) : (
                    <Button
                      icon={working === p.plugin_id ? undefined : "plus"}
                      onClick={() => toggle(p)}
                      disabled={working !== null}
                    >
                      {working === p.plugin_id ? "Installing…" : "Install"}
                    </Button>
                  )}
                </Card.Actions>
              </Card>
            );
          })}
        </Stack>
      )}
    </div>
  );
}

const getStyles = (theme: GrafanaTheme2) => ({
  wrap: css({
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(2),
    width: "100%",
    maxWidth: 820,
    margin: "0 auto",
  }),
  head: css({
    display: "flex",
    alignItems: "flex-end",
    justifyContent: "space-between",
    gap: theme.spacing(2),
    flexWrap: "wrap",
  }),
  center: css({
    display: "flex",
    justifyContent: "center",
    padding: theme.spacing(6),
  }),
  tags: css({
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    gap: theme.spacing(1),
    width: "100%",
  }),
});
