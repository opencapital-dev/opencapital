import { Button, Modal, Spinner, Text } from "@grafana/ui";
import type { UpdaterState } from "../updater-core";

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

  return (
    <Modal title={`Update available — ${state.version}`} isOpen onDismiss={onDismiss}>
      {state.status === "available" && (
        <>
          <Text element="p">
            {state.notes || "A new version is ready to install."}
          </Text>
          <Modal.ButtonRow>
            <Button variant="secondary" onClick={onDismiss}>
              Later
            </Button>
            <Button variant="primary" onClick={onInstall}>
              Install &amp; restart
            </Button>
          </Modal.ButtonRow>
        </>
      )}
      {state.status === "downloading" && (
        <Text element="p">
          <Spinner inline /> Downloading… {state.pct}%
        </Text>
      )}
      {state.status === "readyToRestart" && (
        <Text element="p">
          <Spinner inline /> Restarting…
        </Text>
      )}
    </Modal>
  );
}
