#!/usr/bin/env node

import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const appPath = new URL("../internal/dashboard/static/app.js", import.meta.url);
const source = await readFile(appPath, "utf8");

function loadApp({ fetchImpl } = {}) {
  const wanList = {
    innerHTML: "",
    querySelectorAll() { return []; },
  };
  const document = {
    hidden: false,
    documentElement: { dataset: {} },
    addEventListener() {},
    querySelector(selector) {
      if (selector === "#wan-list") return wanList;
      return null;
    },
    querySelectorAll() { return []; },
  };
  const window = {
    confirm: () => true,
    matchMedia: () => ({ matches: false }),
    setTimeout,
    setInterval,
  };
  const context = vm.createContext({
    console,
    document,
    fetch: fetchImpl || (() => { throw new Error("unexpected fetch"); }),
    Intl,
    JSON,
    localStorage: { getItem: () => null, setItem() {} },
    Set,
    window,
    setTimeout,
    clearTimeout,
  });
  vm.runInContext(source, context, { filename: "app.js" });
  return { app: window.ViberConsole, wanList };
}

test("normalizes and sorts the per-slot WAN API response", () => {
  const { app } = loadApp();
  const slots = app.normalizeWANs([
    { index: 2, state: "EMPTY", speed_mbps: 0 },
    { index: 0, state: "active", speed_mbps: 42.5, exit_ip: "203.0.113.1" },
  ]);
  assert.equal(slots.length, 2);
  assert.equal(slots[0].index, 0);
  assert.equal(slots[0].speed_mbps, 42.5);
  assert.equal(slots[0].exit_ip, "203.0.113.1");
  assert.equal(slots[1].state, "empty");
});

test("renders WAN speed, exit IP, state bar, and Drop action", () => {
  const { app, wanList } = loadApp();
  app.renderWANs(app.normalizeWANs([
    { index: 0, state: "active", speed_mbps: 42.5, conns: 3, exit_ip: "203.0.113.1", last_probe: new Date().toISOString() },
    { index: 1, state: "empty", speed_mbps: 0 },
  ]), true);
  assert.match(wanList.innerHTML, /42\.5 Mb\/s/);
  assert.match(wanList.innerHTML, /Exit 203\.0\.113\.1/);
  assert.match(wanList.innerHTML, /class="wan-bar /);
  assert.match(wanList.innerHTML, />Drop<\/button>/);
  assert.match(wanList.innerHTML, /wan-bar empty/);
});

test("API helper sends POST and reports structured upstream errors", async () => {
  const calls = [];
  const { app } = loadApp({
    fetchImpl: async (path, options) => {
      calls.push({ path, options });
      return {
        ok: false,
        status: 502,
        statusText: "Bad Gateway",
        text: async () => JSON.stringify({ message: "replacement failed test" }),
      };
    },
  });
  await assert.rejects(
    app.api("/api/viberoxy/wans/0/drop", { method: "POST" }),
    /replacement failed test/,
  );
  assert.equal(calls[0].path, "/api/viberoxy/wans/0/drop");
  assert.equal(calls[0].options.method, "POST");
});

test("relative timestamps are human-readable", () => {
  const { app } = loadApp();
  const now = Date.parse("2026-09-05T12:00:00Z");
  assert.equal(app.relativeTime("2026-09-05T11:58:00Z", now), "2 minutes ago");
  assert.equal(app.relativeTime("2026-09-05T12:01:00Z", now), "in 1 minute");
});
