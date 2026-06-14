import { describe, it, expect } from "vitest";
import { classify, progressReducer, type Progress } from "./updater-core";

describe("classify", () => {
  it("null -> upToDate", () => {
    expect(classify(null)).toEqual({ status: "upToDate" });
  });
  it("update -> available with version + notes", () => {
    expect(classify({ version: "0.1.1", body: "Fixes" } as any)).toEqual({
      status: "available",
      version: "0.1.1",
      notes: "Fixes",
    });
  });
  it("missing body -> empty notes", () => {
    expect(classify({ version: "0.1.1" } as any)).toEqual({
      status: "available",
      version: "0.1.1",
      notes: "",
    });
  });
});

describe("progressReducer", () => {
  const zero: Progress = { total: 0, downloaded: 0, pct: 0 };
  it("Started sets total, resets pct", () => {
    expect(
      progressReducer(zero, { event: "Started", data: { contentLength: 200 } } as any),
    ).toEqual({ total: 200, downloaded: 0, pct: 0 });
  });
  it("Progress accumulates and rounds pct", () => {
    const started = progressReducer(zero, { event: "Started", data: { contentLength: 200 } } as any);
    const p1 = progressReducer(started, { event: "Progress", data: { chunkLength: 100 } } as any);
    expect(p1.pct).toBe(50);
  });
  it("Progress clamps at 100", () => {
    const started = progressReducer(zero, { event: "Started", data: { contentLength: 100 } } as any);
    const over = progressReducer(started, { event: "Progress", data: { chunkLength: 250 } } as any);
    expect(over.pct).toBe(100);
  });
  it("Finished -> 100", () => {
    const started = progressReducer(zero, { event: "Started", data: { contentLength: 200 } } as any);
    expect(progressReducer(started, { event: "Finished", data: {} } as any).pct).toBe(100);
  });
});
