import { useEffect, useMemo, useState } from "react";
import { css } from "@emotion/css";
import { GrafanaTheme2, SelectableValue } from "@grafana/data";
import {
  Alert,
  Badge,
  Button,
  Card,
  ConfirmModal,
  EmptyState,
  FilterInput,
  Icon,
  InlineField,
  Select,
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

// Sentinel for the "follow latest validated" option (a null pin).
const LATEST = "__latest__";

type Props = { org: Org };

export function PluginsView({ org }: Props) {
  const styles = useStyles2(getStyles);
  const [catalog, setCatalog] = useState<CatalogEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [dirty, setDirty] = useState(false);
  const [versions, setVersions] = useState<Record<string, VersionStatus[]>>({});
  const [pins, setPins] = useState<Record<string, string | null>>({});
  // Desired OPTIONAL plugins (local selection). Required plugins are always
  // installed at launch and are not stored here. The Switch reflects
  // required || selection; launch reconciles installed to match.
  const [selection, setSelection] = useState<Set<string>>(new Set());
  const [showPreview, setShowPreview] = useState(false);
  // A third-party plugin pending a trust confirm before it's selected.
  const [confirm, setConfirm] = useState<CatalogEntry | null>(null);

  const isSelected = (p: CatalogEntry) => p.required || selection.has(p.plugin_id);

  async function load() {
    setLoading(true);
    setError("");
    try {
      const c = await api.catalog(org.org_id);
      setCatalog(c.plugins);
      // Seed (first view only) from installed optionals so a pre-selection org
      // doesn't show its installed plugins as pending-uninstall.
      const installedOptional = c.plugins
        .filter((p) => p.installed && !p.required)
        .map((p) => p.plugin_id);
      await api.seedPluginSelection(org.org_id, installedOptional);
      const sel = await api.getPluginSelection(org.org_id);
      setSelection(new Set(sel));
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

  // Toggling only records intent (local selection); launch installs/uninstalls.
  // Required plugins are locked on and cannot be deselected.
  async function toggle(p: CatalogEntry) {
    if (p.required) return;
    const turningOn = !selection.has(p.plugin_id);
    if (turningOn && p.source && !p.source.verified) {
      setConfirm(p); // gate third-party opt-in behind a trust confirm
      return;
    }
    await applyToggle(p);
  }

  async function applyToggle(p: CatalogEntry) {
    const next = !selection.has(p.plugin_id);
    setError("");
    try {
      await api.setPluginSelection(org.org_id, p.plugin_id, next);
      setSelection((prev) => {
        const s = new Set(prev);
        if (next) s.add(p.plugin_id);
        else s.delete(p.plugin_id);
        return s;
      });
      setDirty(true);
    } catch (e) {
      setError(errMsg(e));
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

  const selectedCount = catalog.filter((p) => isSelected(p)).length;

  return (
    <div className={styles.wrap}>
      <header className={styles.head}>
        <div>
          <Text element="h1" variant="h3">
            Plugins
          </Text>
          <Text color="secondary">
            {selectedCount} selected in {org.name}. Selection applies on next launch.
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
          You changed your plugin selection. Relaunch Grafana from the Launch tab — installs and
          uninstalls run on launch.
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
            const loaded = versions[p.plugin_id] !== undefined;

            const visibleVersions = vsList.filter(
              (vs) => vs.validated || showPreview || vs.version === pin
            );

            // "Latest" is the recommended default; specific versions pin (freeze)
            // the plugin. Always include the current pin so the control shows it
            // even before the version list has loaded.
            const versionOptions: Array<SelectableValue<string>> = [
              {
                label: "Latest",
                value: LATEST,
                description: "Auto-updates to the newest validated build",
                icon: "arrow-up",
              },
            ];
            const seen = new Set<string>();
            visibleVersions.forEach((vs) => {
              seen.add(vs.version);
              versionOptions.push({
                label: fmtVersion(vs.version)!,
                value: vs.version,
                description: vs.validated ? undefined : "preview",
              });
            });
            if (pin && !seen.has(pin)) {
              versionOptions.push({ label: fmtVersion(pin)!, value: pin });
            }

            return (
              <Card key={p.plugin_id}>
                <Card.Heading>{p.display_name}</Card.Heading>
                <Card.Meta>{p.plugin_id}</Card.Meta>
                <Card.Description>{p.description}</Card.Description>
                <Card.Tags>
                  <Stack alignItems="center" gap={1}>
                    {p.type === "datasource" && (
                      <Badge text="Datasource" color="blue" icon="database" />
                    )}
                    {p.type === "app" && <Badge text="App" color="purple" />}
                    {p.type === "panel" && <Badge text="Panel" color="green" />}
                    {p.required && <Badge text="Required" color="purple" />}
                    {p.installed && <Badge text="Installed" color="green" icon="check" />}
                    {isSelected(p) && !p.installed && (
                      <Badge text="Installs next launch" color="blue" icon="clock-nine" />
                    )}
                    {!isSelected(p) && p.installed && (
                      <Badge text="Uninstalls next launch" color="orange" icon="clock-nine" />
                    )}
                    {p.installed && pin && (
                      <Badge text={`Pinned ${fmtVersion(pin)}`} color="orange" icon="lock" />
                    )}
                    {p.source && !p.source.verified && (
                      <Badge
                        text={`Third-party · ${p.source.publisher || "unknown"}`}
                        color="orange"
                        icon="exclamation-triangle"
                      />
                    )}
                  </Stack>
                </Card.Tags>
                <Card.Actions>
                  {p.installed && (
                    <div className={styles.version}>
                      <Text variant="bodySmall" color="secondary">
                        Version
                      </Text>
                      <Select<string>
                        width={26}
                        value={pin ?? LATEST}
                        options={versionOptions}
                        isLoading={!loaded}
                        placeholder="Latest"
                        onChange={(v) =>
                          onSelect(p, v.value && v.value !== LATEST ? v.value : null)
                        }
                        onOpenMenu={() => loadVersions(p)}
                        prefix={<Icon name={pin ? "lock" : "arrow-up"} />}
                        aria-label={`Version for ${p.display_name}`}
                      />
                      {pin && (
                        <Button
                          size="sm"
                          variant="secondary"
                          fill="text"
                          icon="arrow-up"
                          onClick={() => onSelect(p, null)}
                        >
                          Use latest
                        </Button>
                      )}
                    </div>
                  )}
                  <InlineField
                    label={p.required ? "Required" : isSelected(p) ? "Selected" : "Install"}
                    transparent
                    disabled={p.required}
                  >
                    <Switch
                      value={isSelected(p)}
                      disabled={p.required}
                      onChange={() => toggle(p)}
                      aria-label={`Select ${p.display_name} for next launch`}
                    />
                  </InlineField>
                </Card.Actions>
              </Card>
            );
          })}
        </Stack>
      )}

      {confirm && (
        <ConfirmModal
          isOpen
          title="Install third-party plugin?"
          body={`"${confirm.display_name}" comes from an unverified source (${confirm.source?.publisher || "unknown"}). Installing runs its code on your machine on next launch. Continue?`}
          confirmText="Select"
          onConfirm={async () => {
            const p = confirm;
            setConfirm(null);
            await applyToggle(p);
          }}
          onDismiss={() => setConfirm(null)}
        />
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
  version: css({
    display: "flex",
    alignItems: "center",
    gap: theme.spacing(1),
    marginRight: "auto",
  }),
});
