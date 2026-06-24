import { useMemo, useState } from "react";
import { css, keyframes } from "@emotion/css";
import { GrafanaTheme2, SelectableValue } from "@grafana/data";
import {
  Alert,
  Badge,
  Button,
  ConfirmModal,
  EmptyState,
  FilterInput,
  Icon,
  Select,
  Spinner,
  Stack,
  Switch,
  Text,
  useStyles2,
} from "@grafana/ui";
import { errMsg } from "../api";
import {
  useCatalog,
  useSetPin,
  useToggleSelection,
  useVersions,
} from "../queries";
import type { CatalogEntry } from "../types";

// Versions in the registry are mixed: legacy bare ("0.1.0") and new
// v-prefixed ("v1.0.4"). Display them uniformly as "vX.Y.Z" without
// double-prefixing. API calls + equality checks use the raw value.
const fmtVersion = (v?: string | null) => (v ? `v${v.replace(/^v/, "")}` : v);

// Sentinel for the "follow latest validated" option (a null pin).
const LATEST = "__latest__";

export function PluginsView() {
  const styles = useStyles2(getStyles);
  const { data, isLoading, isFetching, isError, error, refetch } = useCatalog();
  const toggleSel = useToggleSelection();
  const setPin = useSetPin();

  const [search, setSearch] = useState("");
  const [dirty, setDirty] = useState(false);
  // A third-party plugin pending a trust confirm before it's selected.
  const [confirm, setConfirm] = useState<CatalogEntry | null>(null);

  const catalog = data?.catalog ?? [];
  const selection = useMemo(() => new Set(data?.selection ?? []), [data?.selection]);
  const pins = data?.pins ?? {};

  const isSelected = (p: CatalogEntry) => p.required || selection.has(p.plugin_id);

  // Toggling only records intent (local selection); launch installs/uninstalls.
  // Required plugins are locked on and cannot be deselected.
  function toggle(p: CatalogEntry) {
    if (p.required) return;
    const turningOn = !selection.has(p.plugin_id);
    if (turningOn && p.source && !p.source.verified) {
      setConfirm(p); // gate third-party opt-in behind a trust confirm
      return;
    }
    applyToggle(p);
  }

  function applyToggle(p: CatalogEntry) {
    const selected = !selection.has(p.plugin_id);
    setDirty(true);
    toggleSel.mutate({ pluginId: p.plugin_id, selected });
  }

  function onPin(p: CatalogEntry, version: string | null) {
    setDirty(true);
    setPin.mutate({ pluginId: p.plugin_id, version });
  }

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return catalog;
    return catalog.filter(
      (p) =>
        p.display_name.toLowerCase().includes(q) ||
        p.plugin_id.toLowerCase().includes(q) ||
        p.description.toLowerCase().includes(q),
    );
  }, [catalog, search]);

  const selectedCount = catalog.filter((p) => isSelected(p)).length;
  const actionError = toggleSel.error ?? setPin.error;

  return (
    <div className={styles.wrap}>
      <header className={styles.head}>
        <div className={styles.titleBlock}>
          <Stack alignItems="center" gap={1}>
            <Text element="h1" variant="h3">
              Plugins
            </Text>
            {/* Quiet background-revalidation hint — first load uses the skeleton. */}
            {isFetching && !isLoading && <Spinner size="sm" />}
          </Stack>
          <Text color="secondary">
            {selectedCount} selected. Selection applies on next launch.
          </Text>
        </div>
        <Stack alignItems="center" gap={1}>
          <Button
            variant="secondary"
            icon="sync"
            onClick={() => refetch()}
            disabled={isFetching}
            tooltip="Check for new plugins"
          >
            Refresh
          </Button>
          <FilterInput
            value={search}
            onChange={setSearch}
            placeholder="Search plugins…"
            width={28}
          />
        </Stack>
      </header>

      {dirty && (
        <Alert title="Relaunch to apply" severity="info">
          You changed your plugin selection. Relaunch Grafana from the Launch tab — installs and
          uninstalls run on launch.
        </Alert>
      )}
      {(isError || actionError) && (
        <Alert
          title="Plugin operation failed"
          severity="error"
          onRemove={() => {
            toggleSel.reset();
            setPin.reset();
          }}
        >
          {errMsg(actionError ?? error)}
        </Alert>
      )}

      {isLoading ? (
        <Stack direction="column" gap={1}>
          <SkeletonCard />
          <SkeletonCard />
          <SkeletonCard />
        </Stack>
      ) : filtered.length === 0 ? (
        <EmptyState variant="not-found" message="No plugins match your search." />
      ) : (
        <Stack direction="column" gap={1}>
          {filtered.map((p) => (
            <PluginCard
              key={p.plugin_id}
              p={p}
              pin={pins[p.plugin_id] ?? null}
              selected={isSelected(p)}
              onToggle={() => toggle(p)}
              onPin={(v) => onPin(p, v)}
            />
          ))}
        </Stack>
      )}

      {confirm && (
        <ConfirmModal
          isOpen
          title="Install third-party plugin?"
          body={`"${confirm.display_name}" comes from an unverified source (${confirm.source?.publisher || "unknown"}). Installing runs its code on your machine on next launch. Continue?`}
          confirmText="Select"
          onConfirm={() => {
            const p = confirm;
            setConfirm(null);
            applyToggle(p);
          }}
          onDismiss={() => setConfirm(null)}
        />
      )}
    </div>
  );
}

type CardProps = {
  p: CatalogEntry;
  pin: string | null;
  selected: boolean;
  onToggle: () => void;
  onPin: (version: string | null) => void;
};

function PluginCard({ p, pin, selected, onToggle, onPin }: CardProps) {
  const styles = useStyles2(getStyles);
  // Versions load only once the menu is opened, then stay cached.
  const [menuOpened, setMenuOpened] = useState(false);
  const versions = useVersions(p.plugin_id, menuOpened);
  const vsList = versions.data ?? [];

  // "Latest" is the recommended default; specific versions pin (freeze) the
  // plugin. Always include the current pin so the control shows it even before
  // the version list has loaded. No icon on Latest — the control's prefix
  // already renders the arrow (avoids a doubled arrow).
  const versionOptions: Array<SelectableValue<string>> = [
    {
      label: "Latest",
      value: LATEST,
      description: "Auto-updates to the newest validated build",
    },
  ];
  const seen = new Set<string>();
  vsList.forEach((vs) => {
    seen.add(vs.version);
    versionOptions.push({ label: fmtVersion(vs.version)!, value: vs.version });
  });
  if (pin && !seen.has(pin)) {
    versionOptions.push({ label: fmtVersion(pin)!, value: pin });
  }

  const installLabel = p.required ? "Required" : selected ? "Selected" : "Install";

  return (
    <div className={styles.card}>
      <div className={styles.cardHead}>
        <Text element="h2" variant="h5">
          {p.display_name}
        </Text>
        <div className={styles.badges}>
          {p.type === "datasource" && <Badge text="Datasource" color="blue" icon="database" />}
          {p.type === "app" && <Badge text="App" color="purple" />}
          {p.type === "panel" && <Badge text="Panel" color="green" />}
          {p.required && <Badge text="Required" color="purple" />}
          {p.installed && <Badge text="Installed" color="green" icon="check" />}
          {selected && !p.installed && (
            <Badge text="Installs next launch" color="blue" icon="clock-nine" />
          )}
          {!selected && p.installed && (
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
        </div>
      </div>

      <Text color="secondary" variant="bodySmall">
        {p.description}
      </Text>

      <div className={styles.cardFoot}>
        {p.installed && (
          <div className={styles.version}>
            <Text variant="bodySmall" color="secondary">
              Version
            </Text>
            <Select<string>
              width={24}
              value={pin ?? LATEST}
              options={versionOptions}
              isLoading={menuOpened && versions.isLoading}
              placeholder="Latest"
              onChange={(v) => onPin(v.value && v.value !== LATEST ? v.value : null)}
              onOpenMenu={() => setMenuOpened(true)}
              prefix={<Icon name={pin ? "lock" : "arrow-up"} />}
              aria-label={`Version for ${p.display_name}`}
            />
            {pin && (
              <Button
                size="sm"
                variant="secondary"
                fill="text"
                icon="arrow-up"
                onClick={() => onPin(null)}
              >
                Use latest
              </Button>
            )}
          </div>
        )}
        <div className={styles.install}>
          <Text variant="bodySmall" color="secondary">
            {installLabel}
          </Text>
          <Switch
            value={selected}
            disabled={p.required}
            onChange={onToggle}
            aria-label={`Select ${p.display_name} for next launch`}
          />
        </div>
      </div>
    </div>
  );
}

function SkeletonCard() {
  const styles = useStyles2(getStyles);
  return (
    <div className={styles.card} aria-hidden>
      <div className={styles.cardHead}>
        <span className={styles.skel} style={{ width: 160, height: 22 }} />
        <div className={styles.badges}>
          <span className={styles.skel} style={{ width: 56, height: 20, borderRadius: 10 }} />
          <span className={styles.skel} style={{ width: 72, height: 20, borderRadius: 10 }} />
        </div>
      </div>
      <span className={styles.skel} style={{ width: "70%", height: 14 }} />
      <div className={styles.cardFoot}>
        <span className={styles.skel} style={{ width: 200, height: 32 }} />
        <span
          className={styles.skel}
          style={{ width: 96, height: 24, marginLeft: "auto" }}
        />
      </div>
    </div>
  );
}

const shimmer = keyframes({
  "0%": { backgroundPosition: "200% 0" },
  "100%": { backgroundPosition: "-200% 0" },
});

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
  titleBlock: css({
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(0.5),
  }),
  card: css({
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(1),
    padding: theme.spacing(2),
    background: theme.colors.background.secondary,
    border: `1px solid ${theme.colors.border.weak}`,
    borderRadius: theme.shape.radius.default,
    transition: "border-color 0.15s ease",
    "&:hover": {
      borderColor: theme.colors.border.medium,
    },
  }),
  // Title and badges share one row → badges sit at the title's height.
  cardHead: css({
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    gap: theme.spacing(2),
  }),
  badges: css({
    display: "flex",
    alignItems: "center",
    flexWrap: "wrap",
    justifyContent: "flex-end",
    gap: theme.spacing(1),
  }),
  cardFoot: css({
    display: "flex",
    alignItems: "center",
    gap: theme.spacing(2),
    marginTop: theme.spacing(0.5),
  }),
  version: css({
    display: "flex",
    alignItems: "center",
    gap: theme.spacing(1),
  }),
  // Install/Required control, pinned to the bottom-right corner of the card.
  install: css({
    display: "flex",
    alignItems: "center",
    gap: theme.spacing(1),
    marginLeft: "auto",
  }),
  skel: css({
    display: "inline-block",
    borderRadius: theme.shape.radius.default,
    background: `linear-gradient(90deg, ${theme.colors.background.secondary} 25%, ${theme.colors.border.medium} 37%, ${theme.colors.background.secondary} 63%)`,
    backgroundSize: "200% 100%",
    animation: `${shimmer} 1.4s ease-in-out infinite`,
    "@media (prefers-reduced-motion: reduce)": {
      animation: "none",
    },
  }),
});
