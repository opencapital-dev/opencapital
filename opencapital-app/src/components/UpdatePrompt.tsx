import { Button, Modal, Text } from "@grafana/ui";
import type { UpdaterState } from "../updater-core";
import { ProgressButton } from "./ProgressButton";

type Props = {
  state: UpdaterState;
  onInstall: () => void;
  onDismiss: () => void;
};

export function UpdatePrompt({ state, onInstall, onDismiss }: Props) {
  if (
    state.status !== "available" &&
    state.status !== "downloading" &&
    state.status !== "readyToRestart"
  ) {
    return null;
  }

  const downloading = state.status === "downloading";
  const restarting = state.status === "readyToRestart";
  const installActive = downloading || restarting;

  return (
    <Modal title={`Update available — ${state.version}`} isOpen onDismiss={onDismiss}>
      <Text element="p">
        {state.status === "available"
          ? state.notes || "A new version is ready to install."
          : "Hang tight — the update is installing and the app will restart."}
      </Text>
      <Modal.ButtonRow>
        <Button variant="secondary" onClick={onDismiss} disabled={installActive}>
          Later
        </Button>
        <ProgressButton
          active={installActive}
          value={state.status === "downloading" ? state.pct : undefined}
          idleLabel="Install & restart"
          activeLabel={restarting ? "Restarting…" : "Downloading…"}
          onClick={onInstall}
        />
      </Modal.ButtonRow>
    </Modal>
  );
}
