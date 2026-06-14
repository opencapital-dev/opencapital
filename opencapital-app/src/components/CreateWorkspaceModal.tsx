import { useState } from "react";
import { Button, Field, Input, Modal, Stack } from "@grafana/ui";
import { errMsg } from "../api";

type Props = {
  isOpen: boolean;
  onDismiss: () => void;
  onCreate: (name: string, currency: string) => Promise<void>;
};

export function CreateWorkspaceModal({ isOpen, onDismiss, onCreate }: Props) {
  const [name, setName] = useState("");
  const [currency, setCurrency] = useState("USD");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  function close() {
    if (busy) return;
    setName("");
    setCurrency("USD");
    setError("");
    onDismiss();
  }

  async function submit() {
    if (!name.trim()) {
      setError("Give your workspace a name.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      await onCreate(name.trim(), currency || "USD");
      setName("");
      setCurrency("USD");
      onDismiss();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal title="New workspace" isOpen={isOpen} onDismiss={close}>
      <Field label="Name" error={error || undefined} invalid={!!error}>
        <Input
          autoFocus
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
          placeholder="e.g. Personal"
          disabled={busy}
          onKeyDown={(e) => e.key === "Enter" && submit()}
        />
      </Field>
      <Field label="Base currency" description="Three-letter ISO code used for reporting.">
        <Input
          value={currency}
          onChange={(e) => setCurrency(e.currentTarget.value.toUpperCase())}
          placeholder="USD"
          width={14}
          maxLength={3}
          disabled={busy}
        />
      </Field>
      <Modal.ButtonRow>
        <Stack gap={1}>
          <Button variant="secondary" fill="outline" onClick={close} disabled={busy}>
            Cancel
          </Button>
          <Button icon={busy ? undefined : "plus"} onClick={submit} disabled={busy}>
            {busy ? "Creating…" : "Create workspace"}
          </Button>
        </Stack>
      </Modal.ButtonRow>
    </Modal>
  );
}
