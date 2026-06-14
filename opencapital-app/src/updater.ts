import { useCallback, useState } from "react";
import { check, type Update } from "@tauri-apps/plugin-updater";
import { relaunch } from "@tauri-apps/plugin-process";
import { classify, progressReducer, type Progress, type UpdaterState } from "./updater-core";

export type { UpdaterState } from "./updater-core";

export function useUpdater() {
  const [state, setState] = useState<UpdaterState>({ status: "idle" });
  const [pending, setPending] = useState<Update | null>(null);

  const checkForUpdate = useCallback(async () => {
    setState({ status: "checking" });
    try {
      const update = await check();
      setPending(update);
      setState(classify(update));
    } catch (e) {
      setState({ status: "error", message: e instanceof Error ? e.message : String(e) });
    }
  }, []);

  const installAndRelaunch = useCallback(async () => {
    if (!pending) return;
    let prog: Progress = { total: 0, downloaded: 0, pct: 0 };
    setState({ status: "downloading", version: pending.version, pct: 0 });
    try {
      await pending.downloadAndInstall((event) => {
        prog = progressReducer(prog, event);
        setState({ status: "downloading", version: pending.version, pct: prog.pct });
      });
      setState({ status: "readyToRestart", version: pending.version });
      await relaunch();
    } catch (e) {
      setState({ status: "error", message: e instanceof Error ? e.message : String(e) });
    }
  }, [pending]);

  return { state, checkForUpdate, installAndRelaunch };
}
