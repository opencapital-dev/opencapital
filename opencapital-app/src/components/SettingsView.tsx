import { useEffect, useState } from "react";
import { getVersion } from "@tauri-apps/api/app";
import { Button, Spinner, Text } from "@grafana/ui";
import type { UpdaterState } from "../updater-core";

type Props = {
  state: UpdaterState;
  onCheck: () => void;
  onInstall: () => void;
};

export function SettingsView({ state, onCheck, onInstall }: Props) {
  const [version, setVersion] = useState("");
  useEffect(() => {
    getVersion().then(setVersion).catch(() => setVersion("unknown"));
  }, []);

  const busy = state.status === "checking" || state.status === "downloading";

  return (
    <div>
      <Text element="h2" variant="h4">
        Settings
      </Text>
      <div style={{ marginTop: 16 }}>
        <Text element="h3" variant="h5">
          Updates
        </Text>
        <Text element="p" color="secondary">
          Current version: {version || "…"}
        </Text>
        <Button variant="secondary" icon="sync" disabled={busy} onClick={onCheck}>
          Check for updates
        </Button>
        <div style={{ marginTop: 12 }}>
          {state.status === "checking" && (
            <Text>
              <Spinner inline /> Checking…
            </Text>
          )}
          {state.status === "upToDate" && (
            <Text color="secondary">You're up to date.</Text>
          )}
          {state.status === "available" && (
            <>
              <Text element="p">
                Version {state.version} available. {state.notes}
              </Text>
              <Button variant="primary" onClick={onInstall}>
                Install &amp; restart
              </Button>
            </>
          )}
          {state.status === "downloading" && (
            <Text>
              <Spinner inline /> Downloading… {state.pct}%
            </Text>
          )}
          {state.status === "readyToRestart" && (
            <Text>
              <Spinner inline /> Restarting…
            </Text>
          )}
          {state.status === "error" && (
            <Text color="error">Couldn't check: {state.message}. Retry.</Text>
          )}
        </div>
      </div>
    </div>
  );
}
