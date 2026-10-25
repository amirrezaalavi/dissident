/* viber-console dashboard — vanilla JS, no dependencies */
"use strict";

const POLL_MS = 10000;
const STALE_AFTER_MS = 30000;
const SLIDER_KEYS = new Set([
  "DAEMON_PARALLEL", "WAN_COUNT", "MAX_TEST_PER_CYCLE",
  "WAN_FAIL_THRESHOLD", "STABILITY_PROBES",
]);

const state = {
  overview: null,
  wans: [],
  services: [],
  candidates: [],
  schema: null,
  values: null,
  group: "viberayd",
  loadedAt: null,
  loading: false,
  advancedLoaded: false,
  dropping: new Set(),
  restarting: new Set(),
};

const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => document.querySelectorAll(selector);

/* ---------- helpers ---------- */

async function api(path, options = {}) {
  const opts = { ...options };
  opts.headers = { Accept: "application/json", ...(options.headers || {}) };
  const response = await fetch(path, opts);
  const text = await response.text();
  let body = null;
  if (text) {
    try { body = JSON.parse(text); } catch (_) { body = null; }
  }
  if (!response.ok) {
    const message = body?.error || body?.message || text || response.statusText || `HTTP ${response.status}`;
    throw new Error(message);
  }
  return body;
}

function esc(value) {
  return String(value ?? "").replace(/[&<>"']/g, (char) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[char]));
}

function finiteNumber(value, fallback = 0) {
  const number = Number(value);
  return Number.isFinite(number) ? number : fallback;
}

function fmt(value, digits = 0) {
  const number = Number(value);
  if (!Number.isFinite(number)) return "–";
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: digits }).format(number);
}

function relativeTime(value, now = Date.now()) {
  if (!value) return "never";
  const time = new Date(value).getTime();
  if (!Number.isFinite(time)) return "unknown";
  const seconds = Math.round((time - now) / 1000);
  const absolute = Math.abs(seconds);
  if (absolute < 5) return "just now";
  const units = [
    [31536000, "year"], [2592000, "month"], [86400, "day"],
    [3600, "hour"], [60, "minute"], [1, "second"],
  ];
  for (const [size, label] of units) {
    if (absolute >= size) {
      const count = Math.round(absolute / size);
      return seconds < 0
        ? `${count} ${label}${count === 1 ? "" : "s"} ago`
        : `in ${count} ${label}${count === 1 ? "" : "s"}`;
    }
  }
  return "just now";
}

function isStale(value, threshold = STALE_AFTER_MS) {
  const time = new Date(value).getTime();
  return Number.isFinite(time) && Date.now() - time > threshold;
}

let toastTimer = null;
function toast(message, kind = "") {
  const element = $("#toast");
  if (!element) return;
  element.textContent = message;
  element.className = `toast ${kind}`;
  element.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { element.hidden = true; }, 4000);
}

function setBusy(button, busy, label) {
  if (!button) return;
  if (busy) {
    button.dataset.previousLabel = button.textContent;
    button.textContent = label;
    button.disabled = true;
    button.setAttribute("aria-busy", "true");
  } else {
    button.textContent = button.dataset.previousLabel || button.textContent;
    button.disabled = false;
    button.removeAttribute("aria-busy");
  }
}

/* ---------- theme ---------- */

function applyTheme(theme) {
  document.documentElement.dataset.theme = theme;
  const button = $("#theme-toggle");
  if (button) button.textContent = theme === "dark" ? "🌙" : "☀️";
}

function initTheme() {
  const saved = localStorage.getItem("viber-theme");
  const preferred = saved || (window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark");
  applyTheme(preferred);
  $("#theme-toggle")?.addEventListener("click", () => {
    const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
    localStorage.setItem("viber-theme", next);
    applyTheme(next);
  });
}

/* ---------- primary dashboard data ---------- */

function renderInitialLoading() {
  $("#status-label").textContent = "Loading proxy status…";
  $("#summary-text").textContent = "Fetching WANs and services…";
  $("#wan-list").innerHTML = '<div class="empty-state" aria-busy="true">Loading WANs…</div>';
  $("#service-list").innerHTML = '<div class="empty-state" aria-busy="true">Loading services…</div>';
  $("#subscription-text").textContent = "Loading subscription…";
}

async function loadDashboard({ quiet = false } = {}) {
  if (state.loading) return;
  state.loading = true;
  const refresh = $("#refresh");
  if (!quiet) setBusy(refresh, true, "…");

  const results = await Promise.allSettled([
    api("/api/overview"),
    api("/api/viberoxy/wans"),
    api("/api/processes"),
    api("/api/viberoxy/candidates"),
  ]);

  const [overviewResult, wansResult, processesResult, candidatesResult] = results;
  if (overviewResult.status === "fulfilled") state.overview = overviewResult.value;
  if (wansResult.status === "fulfilled") state.wans = normalizeWANs(wansResult.value);
  if (processesResult.status === "fulfilled") state.services = normalizeServices(processesResult.value);
  if (candidatesResult.status === "fulfilled") state.candidates = normalizeCandidates(candidatesResult.value);

  if (overviewResult.status === "fulfilled") state.loadedAt = new Date();
  renderDashboard();

  const failures = results.filter((result) => result.status === "rejected");
  if (failures.length && !quiet) {
    const first = failures[0].reason?.message || "request failed";
    toast(`${failures.length} dashboard request${failures.length === 1 ? "" : "s"} failed: ${first}`, "err");
  }
  state.loading = false;
  if (!quiet) setBusy(refresh, false);
}

function normalizeWANs(payload) {
  const list = Array.isArray(payload) ? payload : (payload?.wans || payload?.slots || []);
  return list.map((slot, position) => ({
    index: Number.isInteger(Number(slot.index)) ? Number(slot.index) : position,
    state: String(slot.state || (finiteNumber(slot.speed_mbps) > 0 ? "active" : "empty")).toLowerCase(),
    speed_mbps: finiteNumber(slot.speed_mbps),
    conns: finiteNumber(slot.conns),
    exit_ip: slot.exit_ip || "",
    last_probe: slot.last_probe || "",
    consecutive_fails: finiteNumber(slot.consecutive_fails),
  })).sort((a, b) => a.index - b.index);
}

function normalizeServices(payload) {
  return Array.isArray(payload) ? payload : (payload?.services || []);
}

function normalizeCandidates(payload) {
  return Array.isArray(payload) ? payload : (payload?.candidates || []);
}

function renderDashboard() {
  const overview = state.overview || {};
  const viberayd = overview.viberayd || {};
  const viberoxy = overview.viberoxy || {};
  const activeWANs = state.wans.filter((wan) => wan.state === "active");
  const totalSpeed = activeWANs.reduce((total, wan) => total + wan.speed_mbps, 0);
  const working = finiteNumber(viberayd.stats?.working);
  const proxyOK = Boolean(viberoxy.reachable) && activeWANs.length > 0;
  const proxyDegraded = Boolean(viberoxy.reachable) && !proxyOK;

  const statusDot = $("#status-dot");
  statusDot.className = `status-dot ${proxyOK ? "" : (proxyDegraded ? "warn" : "err")}`.trim();
  $("#status-label").textContent = proxyOK ? "Proxy is working" : (proxyDegraded ? "Proxy is degraded" : "Proxy is unavailable");
  $("#summary-text").textContent = `${fmt(totalSpeed, 1)} Mb/s · ${activeWANs.length} WAN${activeWANs.length === 1 ? "" : "s"} · ${fmt(working)} working configs`;

  renderWANs(state.wans, viberoxy.reachable);
  renderServices(state.services, { viberayd, viberoxy });
  renderSubscription(viberayd.stats || {});
  updateRelativeTimestamps();
}

/* ---------- WANs ---------- */

function wanVisualState(wan) {
  if (state.dropping.has(wan.index) || ["testing", "replacing", "draining"].includes(wan.state)) return "warn";
  if (wan.state === "empty") return "empty";
  if (wan.state !== "active" || wan.consecutive_fails > 0) return wan.consecutive_fails > 1 ? "err" : "warn";
  return "ok";
}

function renderWANs(wans, reachable = true) {
  const element = $("#wan-list");
  if (!wans.length) {
    element.innerHTML = `<div class="empty-state">${reachable ? "No WAN slots are configured." : "WAN data unavailable — viberoxy is unreachable."}</div>`;
    return;
  }
  const maxSpeed = Math.max(...wans.map((wan) => wan.speed_mbps), 1);
  element.innerHTML = wans.map((wan) => {
    const visual = wanVisualState(wan);
    const dropping = state.dropping.has(wan.index);
    const canDrop = ["active", "draining"].includes(wan.state) && !dropping;
    const width = wan.speed_mbps > 0 ? Math.max(4, Math.round((wan.speed_mbps / maxSpeed) * 100)) : 0;
    const probe = wan.last_probe ? relativeTime(wan.last_probe) : "not probed";
    const stale = wan.last_probe && isStale(wan.last_probe, Math.max(STALE_AFTER_MS, POLL_MS * 6));
    const details = [
      wan.exit_ip ? `Exit ${esc(wan.exit_ip)}` : "Exit IP unknown",
      `${fmt(wan.conns)} connection${wan.conns === 1 ? "" : "s"}`,
      `${stale ? "⚠ stale · " : ""}probed ${esc(probe)}`,
    ];
    if (wan.consecutive_fails) details.push(`${fmt(wan.consecutive_fails)} failed probe${wan.consecutive_fails === 1 ? "" : "s"}`);
    return `
      <article class="wan-row" data-wan-index="${wan.index}">
        <div class="wan-header">
          <span class="wan-name">WAN ${wan.index} · ${esc(dropping ? "replacing" : wan.state)}</span>
          <span class="wan-speed">${wan.speed_mbps > 0 ? `${fmt(wan.speed_mbps, 1)} Mb/s` : "empty"}</span>
        </div>
        <div class="wan-bar-container" role="meter" aria-label="WAN ${wan.index} speed" aria-valuemin="0" aria-valuemax="${maxSpeed}" aria-valuenow="${wan.speed_mbps}">
          <div class="wan-bar ${visual === "ok" ? "" : visual}" style="width:${visual === "empty" ? 100 : width}%"></div>
        </div>
        <div class="wan-header">
          <span class="wan-speed">${details.join(" · ")}</span>
          <button class="btn danger small wan-drop" data-index="${wan.index}" ${canDrop ? "" : "disabled"}>${dropping ? "Replacing…" : "Drop"}</button>
        </div>
      </article>`;
  }).join("");

  element.querySelectorAll?.(".wan-drop").forEach((button) => {
    button.addEventListener("click", () => dropWAN(Number(button.dataset.index)));
  });
}

async function dropWAN(index) {
  const wan = state.wans.find((slot) => slot.index === index);
  if (!wan || state.dropping.has(index)) return;
  const detail = wan.exit_ip ? ` (${wan.exit_ip})` : "";
  if (!window.confirm(`Drop WAN ${index}${detail} and replace its proxy configuration? Active connections on this WAN may be interrupted.`)) return;

  state.dropping.add(index);
  renderWANs(state.wans, state.overview?.viberoxy?.reachable);
  try {
    const response = await api(`/api/viberoxy/wans/${index}/drop`, { method: "POST" });
    if (response?.wan) {
      const replacement = normalizeWANs([response.wan])[0];
      state.wans = state.wans.map((slot) => slot.index === index ? replacement : slot);
    } else if (response?.status === "replacing") {
      state.wans = state.wans.map((slot) => slot.index === index ? { ...slot, state: "replacing", speed_mbps: 0, exit_ip: "" } : slot);
    }
    toast(response?.message || (response?.status === "replaced" ? `WAN ${index} replaced` : `WAN ${index} replacement started`), "ok");
  } catch (error) {
    toast(`WAN ${index} drop failed: ${error.message}`, "err");
  } finally {
    state.dropping.delete(index);
    renderDashboard();
    window.setTimeout(() => loadDashboard({ quiet: true }), 1500);
  }
}

/* ---------- services and subscription ---------- */

function renderServices(services, fallback) {
  const byName = new Map(services.map((service) => [service.name, service]));
  const rows = ["viberoxy", "viberayd"].map((name) => {
    const service = byName.get(name);
    const reachable = Boolean(fallback[name]?.reachable);
    return service || { name, running: reachable, externally_managed: true };
  });
  $("#service-list").innerHTML = rows.map((service) => {
    const busy = state.restarting.has(service.name);
    const running = Boolean(service.running);
    const status = busy ? "Restarting…" : (running ? "Running" : "Stopped");
    const extra = running && service.uptime_sec ? ` · up ${formatDuration(service.uptime_sec)}` : "";
    return `
      <div class="service-row">
        <div class="service-info">
          <span class="service-dot ${busy ? "starting" : (running ? "running" : "stopped")}"></span>
          <span><span class="service-name">${esc(service.name)}</span><br><span class="service-status">${status}${extra}</span></span>
        </div>
        <button class="btn ghost small service-restart" data-service="${esc(service.name)}" ${busy ? "disabled" : ""}>${busy ? "Restarting…" : "Restart"}</button>
      </div>`;
  }).join("");
  $("#service-list").querySelectorAll?.(".service-restart").forEach((button) => {
    button.addEventListener("click", () => restartService(button.dataset.service));
  });
}

function formatDuration(seconds) {
  const value = Math.max(0, finiteNumber(seconds));
  if (value < 60) return `${Math.floor(value)}s`;
  if (value < 3600) return `${Math.floor(value / 60)}m`;
  if (value < 86400) return `${Math.floor(value / 3600)}h`;
  return `${Math.floor(value / 86400)}d`;
}

async function restartService(service) {
  if (state.restarting.has(service)) return;
  if (!window.confirm(`Restart ${service}? Connections may be interrupted briefly.`)) return;
  state.restarting.add(service);
  renderDashboard();
  try {
    const response = await api("/api/control/restart", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ service }),
    });
    const restarted = response?.restarted || [];
    if (!restarted.includes(service)) throw new Error(`${service} is not managed by viber-console`);
    toast(`${service} restarted`, "ok");
  } catch (error) {
    toast(`Restart failed: ${error.message}`, "err");
  } finally {
    state.restarting.delete(service);
    renderDashboard();
    window.setTimeout(() => loadDashboard({ quiet: true }), 1500);
  }
}

function renderSubscription(stats) {
  const total = finiteNumber(stats.total);
  const working = finiteNumber(stats.working);
  const failed = finiteNumber(stats.failed);
  const unreachable = finiteNumber(stats.unreachable);
  const failedTotal = failed + unreachable;
  const candidates = state.candidates.length;
  $("#subscription-text").textContent = `${fmt(total)} URLs · ${fmt(working)} working · ${fmt(failedTotal)} failed · ${fmt(candidates)} replacement candidates`;
}

function updateRelativeTimestamps() {
  const timestamp = state.overview?.generated_at || state.loadedAt;
  const footer = $("#last-updated");
  if (!timestamp) {
    footer.textContent = "not updated";
    return;
  }
  const stale = isStale(timestamp);
  footer.textContent = `${stale ? "⚠ stale · " : ""}updated ${relativeTime(timestamp)}`;
  footer.title = new Date(timestamp).toLocaleString();
}

/* ---------- advanced ---------- */

async function loadAdvanced() {
  const configs = $("#config-list");
  const settings = $("#settings-fields");
  configs.innerHTML = '<div class="empty-state" aria-busy="true">Loading configs…</div>';
  settings.innerHTML = '<div class="empty-state" aria-busy="true">Loading settings…</div>';
  await Promise.all([loadConfigs(), loadSettings()]);
  state.advancedLoaded = true;
}

async function loadConfigs() {
  const filter = $("#state-filter")?.value || "";
  const query = $("#search")?.value.trim().toLowerCase() || "";
  try {
    const page = await api(`/api/viberayd/configs?page=1&per_page=100${filter ? `&state=${encodeURIComponent(filter)}` : ""}`);
    let list = page?.configs || [];
    if (query) list = list.filter((config) => `${config.host || ""}:${config.port || ""}`.toLowerCase().includes(query));
    renderConfigs(list);
  } catch (error) {
    $("#config-list").innerHTML = `<div class="empty-state">Configs unavailable: ${esc(error.message)}</div>`;
  }
}

function renderConfigs(configs) {
  if (!configs.length) {
    $("#config-list").innerHTML = '<div class="empty-state">No configs match.</div>';
    return;
  }
  $("#config-list").innerHTML = configs.map((config) => `
    <div class="config-row">
      <span class="host" title="${esc(config.raw || "")}">${esc(config.host || "?")}:${esc(config.port || "")}</span>
      <span class="proto muted">${esc(config.protocol || "")}</span>
      <span class="state muted">${esc(config.state || "unknown")}</span>
      <span class="muted">${config.latency_ms ? `${fmt(config.latency_ms)}ms` : "–"}</span>
    </div>`).join("");
}

async function loadSettings() {
  try {
    const [schema, values] = await Promise.all([api("/api/config/schema"), api("/api/config/values")]);
    state.schema = schema;
    state.values = values;
    renderSettingsForm();
    await loadURLs();
  } catch (error) {
    $("#settings-fields").innerHTML = `<div class="empty-state">Settings unavailable: ${esc(error.message)}</div>`;
  }
}

function renderSettingsForm() {
  const fields = state.schema?.groups?.[state.group] || [];
  const values = state.values?.[state.group] || {};
  $("#settings-fields").innerHTML = `<div class="form-grid">${fields.map((field) => fieldHTML(field, values[field.key] ?? field.default ?? "")).join("")}</div>`;
  wireProfessionalControls();
}

function fieldHTML(field, rawValue) {
  const id = `f-${field.key}`;
  const required = field.required ? '<span class="req">*</span>' : "";
  const description = field.description ? `<div class="desc">${esc(field.description)}</div>` : "";
  let input = "";

  if (field.type === "bool") {
    const checked = String(rawValue).toLowerCase() === "true";
    input = `<label class="checkbox-label"><input id="${id}" type="checkbox" data-key="${esc(field.key)}" role="switch" ${checked ? "checked" : ""}> ${checked ? "Enabled" : "Disabled"}</label>`;
  } else if (field.type === "enum") {
    input = `<div>${(field.enum || []).map((choice) => `<label class="checkbox-label"><input type="radio" name="${id}" data-key="${esc(field.key)}" value="${esc(choice)}" ${String(rawValue) === choice ? "checked" : ""}> ${esc(choice)}</label>`).join("")}</div>`;
  } else if (field.type === "int" && SLIDER_KEYS.has(field.key) && field.min != null && field.max != null) {
    input = `<div><input id="${id}" class="input" type="range" data-key="${esc(field.key)}" value="${esc(rawValue)}" min="${field.min}" max="${field.max}" aria-describedby="${id}-value"><output id="${id}-value">${esc(rawValue)}</output></div>`;
  } else {
    const type = field.type === "int" ? "number" : (field.secret ? "password" : "text");
    input = `<input id="${id}" class="input" type="${type}" data-key="${esc(field.key)}" value="${esc(rawValue)}" placeholder="${esc(field.placeholder || "")}" ${field.min != null ? `min="${field.min}"` : ""} ${field.max != null ? `max="${field.max}"` : ""}>`;
  }

  return `<div class="field"><label for="${id}">${esc(field.label)} ${required}</label>${description}${input}<div class="err" id="e-${field.key}"></div></div>`;
}

function wireProfessionalControls() {
  $$('#settings-fields input[type="range"]').forEach((input) => {
    input.addEventListener("input", () => { $(`#${input.id}-value`).textContent = input.value; });
  });
  $$('#settings-fields input[type="checkbox"][role="switch"]').forEach((input) => {
    input.addEventListener("change", () => { input.parentElement.lastChild.textContent = input.checked ? " Enabled" : " Disabled"; });
  });
}

function collectFormValues() {
  const values = {};
  $$('#settings-fields [data-key]').forEach((input) => {
    if (input.type === "radio" && !input.checked) return;
    values[input.dataset.key] = input.type === "checkbox" ? String(input.checked) : input.value;
  });
  return values;
}

async function saveSettings(event) {
  event.preventDefault();
  const button = $('#settings-form button[type="submit"]');
  setBusy(button, true, "Saving…");
  $("#form-msg").className = "form-msg";
  $("#form-msg").textContent = "Saving configuration…";
  try {
    const response = await api("/api/config/values", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ group: state.group, values: collectFormValues(), restart: $("#restart-check").checked }),
    });
    const restarted = response?.restarted?.length ? ` · restarted ${response.restarted.join(", ")}` : "";
    $("#form-msg").textContent = `Saved${restarted}`;
    toast("Configuration saved", "ok");
    await loadSettings();
    window.setTimeout(() => loadDashboard({ quiet: true }), 1500);
  } catch (error) {
    $("#form-msg").className = "form-msg err";
    $("#form-msg").textContent = `Error: ${error.message}`;
    toast(`Save failed: ${error.message}`, "err");
  } finally {
    setBusy(button, false);
  }
}

async function loadURLs() {
  if (state.group !== "viberayd") {
    $("#urls-card").hidden = true;
    return;
  }
  try {
    const response = await api("/api/viberayd/urls");
    $("#urls-card").hidden = false;
    $("#urls-textarea").value = (response?.urls || []).join("\n");
    $("#urls-msg").textContent = "";
  } catch (_) {
    $("#urls-card").hidden = true;
  }
}

async function applyURLs() {
  const button = $("#urls-apply");
  setBusy(button, true, "Applying…");
  try {
    const response = await api("/api/viberayd/urls", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ urls: $("#urls-textarea").value.split("\n") }),
    });
    $("#urls-textarea").value = (response?.urls || []).join("\n");
    $("#urls-msg").className = "form-msg";
    $("#urls-msg").textContent = `${response?.urls?.length || 0} URLs applied`;
    toast("Subscription URLs updated", "ok");
  } catch (error) {
    $("#urls-msg").className = "form-msg err";
    $("#urls-msg").textContent = `Error: ${error.message}`;
    toast(`URL update failed: ${error.message}`, "err");
  } finally {
    setBusy(button, false);
  }
}

/* ---------- boot ---------- */

function debounce(fn, delay) {
  let timer;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), delay);
  };
}

function init() {
  initTheme();
  renderInitialLoading();

  $("#refresh").addEventListener("click", () => loadDashboard());
  $("#advanced-toggle").addEventListener("click", async () => {
    const content = $("#advanced-content");
    const expanded = $("#advanced-toggle").getAttribute("aria-expanded") === "true";
    $("#advanced-toggle").setAttribute("aria-expanded", String(!expanded));
    $("#advanced-chevron").classList.toggle("open", !expanded);
    content.hidden = expanded;
    if (!expanded && !state.advancedLoaded) await loadAdvanced();
  });
  $("#state-filter").addEventListener("change", loadConfigs);
  $("#search").addEventListener("input", debounce(loadConfigs, 250));
  $("#settings-form").addEventListener("submit", saveSettings);
  $("#urls-apply").addEventListener("click", applyURLs);

  $$(".tab").forEach((tab) => tab.addEventListener("click", () => {
    $$(".tab").forEach((item) => item.classList.remove("active"));
    tab.classList.add("active");
    state.group = tab.dataset.group;
    renderSettingsForm();
    loadURLs();
  }));

  loadDashboard();
  window.setInterval(() => {
    updateRelativeTimestamps();
    if (!document.hidden) loadDashboard({ quiet: true });
  }, POLL_MS);
}

// A small public surface makes the dependency-free smoke test possible without
// changing runtime behavior.
if (typeof window !== "undefined") {
  window.ViberConsole = { api, relativeTime, normalizeWANs, wanVisualState, renderWANs };
}

document.addEventListener("DOMContentLoaded", init);
