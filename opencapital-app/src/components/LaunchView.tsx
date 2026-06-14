import { useEffect, useRef, useState } from "react";
import { listen } from "@tauri-apps/api/event";
import { css, keyframes } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import { Alert, Button, Icon, Spinner, Text, useStyles2 } from "@grafana/ui";
import { api, errMsg } from "../api";
import type { MeOrgs, Org } from "../types";

type Props = {
  me: MeOrgs;
  org: Org;
  // Preferred human email for the Grafana identity (empty -> falls back to the
  // subject id in the backend).
  userEmail: string;
};

// Ordered launch stages. The backend emits a `status` matching one of these
// keys on the `launch-progress` event; we light each row as it arrives.
const STAGES = [
  { key: "runtime", label: "Checking runtime" },
  { key: "provision", label: "Writing provisioning" },
  { key: "reconcile", label: "Reconciling plugins" },
  { key: "config", label: "Rendering configuration" },
  { key: "spawn", label: "Starting Grafana" },
  { key: "health", label: "Waiting for Grafana" },
  { key: "ready", label: "Ready" },
] as const;

type StageState = "pending" | "active" | "done";

export function LaunchView({ me, org, userEmail }: Props) {
  const styles = useStyles2(getStyles);
  const [phase, setPhase] = useState<"idle" | "launching" | "live">("idle");
  const [activeKey, setActiveKey] = useState<string | null>(null);
  const [detail, setDetail] = useState("");
  const [error, setError] = useState("");
  const phaseRef = useRef(phase);
  phaseRef.current = phase;

  useEffect(() => {
    const unsubs: Array<Promise<() => void>> = [];

    unsubs.push(
      listen<{ status: string; detail: string }>("launch-progress", (e) => {
        const { status, detail } = e.payload;
        setActiveKey(status);
        setDetail(detail || "");
        if (status === "ready") setPhase("live");
      })
    );

    unsubs.push(
      listen<string>("reconcile-progress", (e) => {
        // Backend sends either a JSON plugin event or a plain status line.
        // Turn it into one friendly sub-line; never show raw JSON.
        const friendly = humanizeReconcile(e.payload);
        if (friendly) setDetail(friendly);
      })
    );

    unsubs.push(
      listen<string>("grafana-crashed", (e) => {
        setError(`Grafana exited: ${e.payload}`);
        setPhase("idle");
      })
    );

    // On mount (incl. remount after navigating to Plugins and back), ask the
    // backend whether grafana is already up so we show Relaunch, not Launch.
    api
      .grafanaRunning()
      .then((running) => {
        if (running) {
          setPhase("live");
        }
      })
      .catch(() => {});

    return () => unsubs.forEach((p) => p.then((un) => un()));
  }, []);

  // Reset transient launch UI when the selected workspace changes.
  useEffect(() => {
    setPhase("idle");
    setActiveKey(null);
    setDetail("");
    setError("");
  }, [org.org_id]);

  async function launch() {
    setPhase("launching");
    setActiveKey(STAGES[0].key);
    setDetail("");
    setError("");
    try {
      await api.launch(org.org_id, userEmail, userEmail || me.user_id);
    } catch (e) {
      setError(errMsg(e));
      setPhase("idle");
    }
  }

  const activeIdx = activeKey ? STAGES.findIndex((s) => s.key === activeKey) : -1;

  return (
    <div className={styles.wrap}>
      <header className={styles.head}>
        <div>
          <Text element="h1" variant="h3">
            Launch
          </Text>
          <Text color="secondary">
            Boot the Grafana instance for {org.name} in a dedicated window.
          </Text>
        </div>
      </header>

      <div className={styles.stage}>
        {phase === "live" ? (
          <div className={styles.liveCard}>
            <div className={styles.liveIcon}>
              <Icon name="check-circle" size="xxl" />
            </div>
            <Text element="h2" variant="h4">
              {org.name} is live
            </Text>
            <Text color="secondary">
              Grafana opened in its own window. You can relaunch any time.
            </Text>
            <Button variant="secondary" icon="external-link-alt" onClick={launch}>
              Relaunch
            </Button>
          </div>
        ) : phase === "launching" ? (
          <div className={styles.progressCard}>
            <ol className={styles.steps}>
              {STAGES.map((s, i) => {
                const state: StageState =
                  i < activeIdx ? "done" : i === activeIdx ? "active" : "pending";
                return (
                  <li key={s.key} className={styles.step} data-state={state}>
                    <span className={styles.dot} data-state={state}>
                      {state === "done" ? (
                        <Icon name="check" size="sm" />
                      ) : state === "active" ? (
                        <Spinner inline size="sm" />
                      ) : (
                        <span className={styles.pendingDot} />
                      )}
                    </span>
                    <span className={styles.stepLabel}>{s.label}</span>
                  </li>
                );
              })}
            </ol>
            {detail && (
              <div className={styles.detail}>
                <Text variant="bodySmall" color="secondary">
                  {detail}
                </Text>
              </div>
            )}
          </div>
        ) : (
          <div className={styles.idleCard}>
            <div className={styles.pulse}>
              <Icon name="rocket" size="xxl" />
            </div>
            <Text color="secondary">
              Plugins and provisioning are reconciled automatically on launch.
            </Text>
            <Button size="lg" icon="play" onClick={launch} className={styles.launchBtn}>
              Launch Grafana
            </Button>
          </div>
        )}
      </div>

      {error && (
        <Alert title="Launch failed" severity="error" onRemove={() => setError("")}>
          {error}
        </Alert>
      )}
    </div>
  );
}

function humanizeReconcile(raw: string): string {
  try {
    const o = JSON.parse(raw);
    if (o && o.event === "plugin" && o.plugin) {
      const verb: Record<string, string> = {
        downloading: "Downloading",
        verifying: "Verifying",
        extracting: "Extracting",
        linking: "Linking",
        done: "Installed",
        cached: "Cached",
      };
      const v = verb[o.status] || o.status;
      return `${v} ${prettyPlugin(o.plugin)}`;
    }
  } catch {
    // not JSON — fall through
  }
  if (raw.includes("provisioned")) {
    const m = raw.match(/provisioned (\d+)/);
    return m ? `Provisioned ${m[1]} plugins` : "";
  }
  return "";
}

function prettyPlugin(id: string): string {
  return id
    .replace(/[_-]+/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase())
    .trim();
}

const pulse = keyframes({
  "0%, 100%": { transform: "scale(1)", opacity: 0.9 },
  "50%": { transform: "scale(1.06)", opacity: 1 },
});

const getStyles = (theme: GrafanaTheme2) => ({
  wrap: css({
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(3),
    width: "100%",
    maxWidth: 640,
    margin: "0 auto",
  }),
  head: css({ textAlign: "center" }),
  stage: css({
    display: "flex",
    justifyContent: "center",
  }),
  idleCard: css({
    width: "100%",
    display: "flex",
    flexDirection: "column",
    alignItems: "center",
    gap: theme.spacing(2),
    padding: theme.spacing(6),
    textAlign: "center",
    background: theme.colors.background.primary,
    border: `1px solid ${theme.colors.border.weak}`,
    borderRadius: theme.shape.radius.default,
  }),
  pulse: css({
    color: theme.colors.primary.text,
    animation: `${pulse} 3s ease-in-out infinite`,
  }),
  launchBtn: css({ marginTop: theme.spacing(1), minWidth: 220, justifyContent: "center" }),
  progressCard: css({
    width: "100%",
    padding: theme.spacing(4),
    background: theme.colors.background.primary,
    border: `1px solid ${theme.colors.border.weak}`,
    borderRadius: theme.shape.radius.default,
  }),
  steps: css({
    listStyle: "none",
    margin: 0,
    padding: 0,
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(1.5),
  }),
  step: css({
    display: "flex",
    alignItems: "center",
    gap: theme.spacing(2),
    transition: "opacity 0.2s ease",
    '&[data-state="pending"]': { opacity: 0.45 },
  }),
  dot: css({
    width: 22,
    height: 22,
    flexShrink: 0,
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    borderRadius: theme.shape.radius.circle,
    color: theme.colors.primary.contrastText,
    '&[data-state="done"]': { background: theme.colors.success.main },
    '&[data-state="active"]': { background: theme.colors.primary.main },
    '&[data-state="pending"]': { background: "transparent" },
  }),
  pendingDot: css({
    width: 8,
    height: 8,
    borderRadius: theme.shape.radius.circle,
    border: `2px solid ${theme.colors.border.medium}`,
  }),
  stepLabel: css({
    fontSize: theme.typography.body.fontSize,
    color: theme.colors.text.primary,
  }),
  detail: css({
    marginTop: theme.spacing(2.5),
    paddingTop: theme.spacing(2),
    borderTop: `1px solid ${theme.colors.border.weak}`,
    minHeight: 20,
  }),
  liveCard: css({
    width: "100%",
    display: "flex",
    flexDirection: "column",
    alignItems: "center",
    gap: theme.spacing(2),
    padding: theme.spacing(6),
    textAlign: "center",
    background: theme.colors.background.primary,
    border: `1px solid ${theme.colors.success.borderTransparent}`,
    borderRadius: theme.shape.radius.default,
  }),
  liveIcon: css({ color: theme.colors.success.text }),
});
