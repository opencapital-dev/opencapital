import type { Update, DownloadEvent } from "@tauri-apps/plugin-updater";

export type UpdaterState =
  | { status: "idle" }
  | { status: "checking" }
  | { status: "upToDate" }
  | { status: "available"; version: string; notes: string }
  | { status: "downloading"; version: string; pct: number }
  | { status: "readyToRestart"; version: string }
  | { status: "error"; message: string };

/** Map a `check()` result to the next state. */
export function classify(update: Update | null): UpdaterState {
  if (!update) return { status: "upToDate" };
  return { status: "available", version: update.version, notes: update.body ?? "" };
}

export type Progress = { total: number; downloaded: number; pct: number };

/** Fold one download event into running totals + a clamped, rounded percentage. */
export function progressReducer(p: Progress, event: DownloadEvent): Progress {
  switch (event.event) {
    case "Started":
      return { total: event.data.contentLength ?? 0, downloaded: 0, pct: 0 };
    case "Progress": {
      const downloaded = p.downloaded + event.data.chunkLength;
      const pct = p.total > 0 ? Math.min(100, Math.round((downloaded / p.total) * 100)) : 0;
      return { total: p.total, downloaded, pct };
    }
    case "Finished":
      return { total: p.total, downloaded: p.total, pct: 100 };
  }
}
