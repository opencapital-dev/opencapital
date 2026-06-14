import { useEffect, useState } from "react";
import { getVersion } from "@tauri-apps/api/app";
import { css } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import { Icon, Text, useStyles2 } from "@grafana/ui";
import type { UpdaterState } from "../updater-core";
import { ProgressButton } from "./ProgressButton";

type Props = {
  state: UpdaterState;
  onCheck: () => void;
  onInstall: () => void;
};

export function SettingsView({ state, onCheck, onInstall }: Props) {
  const styles = useStyles2(getStyles);
  const [version, setVersion] = useState("");
  useEffect(() => {
    getVersion().then(setVersion).catch(() => setVersion("unknown"));
  }, []);

  const checking = state.status === "checking";
  const downloading = state.status === "downloading";
  const restarting = state.status === "readyToRestart";
  const installActive = downloading || restarting;
  const hasUpdate = state.status === "available" || installActive;

  let updateVersion = "";
  if (state.status === "available" || state.status === "downloading" || state.status === "readyToRestart") {
    updateVersion = state.version;
  }

  return (
    <div className={styles.wrap}>
      <Text element="h1" variant="h3">
        Settings
      </Text>

      <section className={styles.card}>
        <div className={styles.head}>
          <div className={styles.headCopy}>
            <Text element="h2" variant="h5">
              Updates
            </Text>
            <Text color="secondary">Current version {version || "…"}</Text>
          </div>
          <ProgressButton
            active={checking}
            idleLabel="Check for updates"
            activeLabel="Checking…"
            icon="sync"
            variant="secondary"
            onClick={onCheck}
            disabled={installActive}
          />
        </div>

        <div className={styles.status}>
          {state.status === "upToDate" && (
            <div className={styles.row}>
              <Icon name="check-circle" className={styles.ok} />
              <Text color="secondary">You're on the latest version.</Text>
            </div>
          )}

          {hasUpdate && (
            <div className={styles.update}>
              <div className={styles.row}>
                <Icon name="arrow-up" className={styles.accent} />
                <Text element="p">
                  Version <strong>{updateVersion}</strong> is available.
                </Text>
              </div>
              {state.status === "available" && state.notes && (
                <Text color="secondary">{state.notes}</Text>
              )}
              <ProgressButton
                active={installActive}
                value={state.status === "downloading" ? state.pct : undefined}
                idleLabel="Install & restart"
                activeLabel={restarting ? "Restarting…" : "Downloading…"}
                icon="download-alt"
                onClick={onInstall}
                className={styles.installBtn}
              />
            </div>
          )}

          {state.status === "error" && (
            <div className={styles.row}>
              <Icon name="exclamation-triangle" className={styles.err} />
              <Text color="error">Couldn't check for updates: {state.message}</Text>
            </div>
          )}
        </div>
      </section>
    </div>
  );
}

const getStyles = (theme: GrafanaTheme2) => ({
  wrap: css({
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(3),
    width: "100%",
    maxWidth: 640,
    margin: "0 auto",
  }),
  card: css({
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(2.5),
    padding: theme.spacing(3),
    background: theme.colors.background.primary,
    border: `1px solid ${theme.colors.border.weak}`,
    borderRadius: theme.shape.radius.default,
    boxShadow: theme.shadows.z1,
  }),
  head: css({
    display: "flex",
    alignItems: "flex-start",
    justifyContent: "space-between",
    gap: theme.spacing(2),
    flexWrap: "wrap",
  }),
  headCopy: css({
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(0.5),
  }),
  status: css({
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(1),
  }),
  row: css({
    display: "flex",
    alignItems: "center",
    gap: theme.spacing(1),
  }),
  update: css({
    display: "flex",
    flexDirection: "column",
    alignItems: "flex-start",
    gap: theme.spacing(1.5),
    paddingTop: theme.spacing(1),
    borderTop: `1px solid ${theme.colors.border.weak}`,
  }),
  installBtn: css({ minWidth: 200, marginTop: theme.spacing(0.5) }),
  ok: css({ color: theme.colors.success.text }),
  accent: css({ color: theme.colors.primary.text }),
  err: css({ color: theme.colors.error.text }),
});
