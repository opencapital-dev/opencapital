import { useEffect, useRef, useState } from "react";
import { listen } from "@tauri-apps/api/event";
import { css, keyframes } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import { Alert, Button, Icon, Text, useStyles2 } from "@grafana/ui";
import { api, errMsg } from "../api";
import { ProgressButton } from "./ProgressButton";

type Props = {
  // Preferred human email for the Grafana identity (empty -> falls back to the
  // subject id in the backend).
  userEmail: string;
};

// Ordered launch stages. The backend emits a `status` matching one of these
// keys on the `launch-progress` event. We don't surface the stage names — they
// only drive how far the progress bar has filled.
const STAGE_KEYS = [
  "runtime",
  "provision",
  "reconcile",
  "config",
  "spawn",
  "health",
  "ready",
] as const;

export function LaunchView({ userEmail }: Props) {
  const styles = useStyles2(getStyles);
  const [phase, setPhase] = useState<"idle" | "launching" | "live">("idle");
  const [activeKey, setActiveKey] = useState<string | null>(null);
  const [error, setError] = useState("");
  const phaseRef = useRef(phase);
  phaseRef.current = phase;

  useEffect(() => {
    const unsubs: Array<Promise<() => void>> = [];

    unsubs.push(
      listen<{ status: string; detail: string }>("launch-progress", (e) => {
        const { status } = e.payload;
        setActiveKey(status);
        if (status === "ready") setPhase("live");
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

  async function launch() {
    setPhase("launching");
    setActiveKey(STAGE_KEYS[0]);
    setError("");
    try {
      await api.launch(userEmail, userEmail);
    } catch (e) {
      setError(errMsg(e));
      setPhase("idle");
    }
  }

  // Map the current stage to a fill percentage. Keep a small floor so the bar
  // is visibly underway the moment a launch starts.
  const activeIdx = activeKey ? STAGE_KEYS.indexOf(activeKey as (typeof STAGE_KEYS)[number]) : -1;
  const pct = Math.max(8, Math.round(((activeIdx + 1) / STAGE_KEYS.length) * 100));

  return (
    <div className={styles.wrap}>
      <header className={styles.head}>
        <Text element="h1" variant="h3">
          Launch
        </Text>
      </header>

      <div className={styles.stage}>
        {phase === "live" ? (
          <div className={styles.liveCard}>
            <div className={styles.liveIcon}>
              <Icon name="check-circle" size="xxl" />
            </div>
            <Text element="h2" variant="h4">
              OpenCapital is live
            </Text>
            <Text color="secondary">
              Grafana opened in its own window. You can relaunch any time.
            </Text>
            <Button variant="secondary" icon="external-link-alt" onClick={launch}>
              Relaunch
            </Button>
          </div>
        ) : (
          <div className={styles.idleCard}>
            <div className={styles.pulse}>
              <Icon name="rocket" size="xxl" />
            </div>
            {phase === "idle" && (
              <Text color="secondary">
                Plugins and provisioning are reconciled automatically on launch.
              </Text>
            )}
            <ProgressButton
              active={phase === "launching"}
              value={pct}
              idleLabel="Launch"
              activeLabel="Launching…"
              icon="play"
              size="lg"
              onClick={launch}
              className={styles.launchBtn}
            />
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
    boxShadow: theme.shadows.z1,
  }),
  pulse: css({
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    width: 88,
    height: 88,
    borderRadius: theme.shape.radius.circle,
    color: theme.colors.primary.text,
    background: theme.colors.primary.transparent,
    boxShadow: `inset 0 0 0 1px ${theme.colors.primary.borderTransparent}`,
    animation: `${pulse} 3s ease-in-out infinite`,
    "@media (prefers-reduced-motion: reduce)": {
      animation: "none",
    },
  }),
  launchBtn: css({ marginTop: theme.spacing(1), minWidth: 220, justifyContent: "center" }),
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
    boxShadow: theme.shadows.z1,
  }),
  liveIcon: css({
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    width: 88,
    height: 88,
    borderRadius: theme.shape.radius.circle,
    color: theme.colors.success.text,
    background: theme.colors.success.transparent,
    boxShadow: `inset 0 0 0 1px ${theme.colors.success.borderTransparent}`,
  }),
});
