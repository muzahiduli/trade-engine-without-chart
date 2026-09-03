// Keyboard-driven trade management for Trade Engine 2.
// Captures hotkeys and dispatches them as HOTKEY commands to the Go hub,
// which performs all sizing/bracket/routing logic (web is a thin client).
//
// Key bindings are user-configurable and stored in localStorage (client-side only —
// the Go hub never sees the raw key, only the resolved ACTION string).
import { getState } from './state.js';
import { sendHotkey } from './ws.js';

let flashTimer = null;

// The canonical list of hotkey actions and their DEFAULT key descriptors.
// descriptor format: "mod+key" e.g. "shift+f", "ctrl+space", or bare "s".
export const HOTKEY_ACTIONS = [
  { action: 'INSTANT_ENTRY', label: 'Instant Entry', defaultKey: 'shift+f', icon: '⚡' },
  { action: 'BREAKOUT_ENTRY', label: 'Breakout Entry', defaultKey: 'shift+r', icon: '⚡' },
  { action: 'TRAIL_STOP', label: 'Trail Stop', defaultKey: 'shift+s', icon: '🔒' },
  { action: 'BREAKEVEN', label: 'Stop to Breakeven', defaultKey: 'b', icon: '🎯' },
  { action: 'BREAKEVEN_PLUS', label: 'Stop to BE + Offset', defaultKey: 'shift+b', icon: '🎯' },
  { action: 'CLOSE_25', label: 'Close 25%', defaultKey: 'ctrl+1', icon: '📊' },
  { action: 'CLOSE_50', label: 'Close 50%', defaultKey: 'ctrl+2', icon: '📊' },
  { action: 'CLOSE_RUNNER', label: 'Close Runner (100%)', defaultKey: 'ctrl+3', icon: '📊' },
  { action: 'CANCEL_ENTRY', label: 'Cancel Entry Order', defaultKey: 'c', icon: '❌' },
  { action: 'CANCEL_ORDERS', label: 'Cancel All Orders', defaultKey: 'shift+c', icon: '❌' },
  { action: 'SWAP_DIRECTION', label: 'Swap Long/Short', defaultKey: 's', icon: '⇄' },
  { action: 'SCALE_OUT', label: 'Scale Out', defaultKey: 'ctrl+f', icon: '📊' },
  { action: 'FLATTEN', label: 'Flatten', defaultKey: 'ctrl+space', icon: '🛑' },
  { action: 'KILL_SWITCH', label: 'Emergency Kill Switch', defaultKey: 'ctrl+shift+k', icon: '🚨' },
  { action: 'SET_RR_1', label: 'Set RR 1:1', defaultKey: 'r', icon: '🎯' },
  { action: 'TOGGLE_RISK', label: 'Toggle Risk Cash', defaultKey: 'q', icon: '💰' },
  { action: 'AUTO_TRACK', label: 'Auto-Track Toggle', defaultKey: 'a', icon: '🎯' },
  { action: 'CYCLE_TRACK_TF', label: 'Cycle Track Timeframe', defaultKey: 't', icon: '🕐' },
  { action: 'LOCK_SL', label: 'Lock SL Toggle', defaultKey: 'l', icon: '🔒' },
];

const TRACK_TFS = ['15s', '1m', '5m', '100t'];

// Client-side hotkey actions (UI/model settings applied locally, then synced
// to the hub via the same sendConfig path the buttons use). Actions NOT listed
// here are forwarded to the Go hub as HOTKEY commands.
const CLIENT_ACTIONS = {
  // Toggle Lock Stop Loss on/off (same as button).
  LOCK_SL: () => {
    if (typeof window.toggleLockSl === 'function') window.toggleLockSl();
  },
  // Set R:R to 1:1 (same as pressing the 1:1 preset).
  SET_RR_1: () => {
    if (typeof window.setRR === 'function') window.setRR(1.0);
  },
  // Toggle Risk Cash between the two preset buttons marked data-risktoggle —
  // default $100 ↔ $200. Configure the pair by editing those <option>-style
  // values on the two buttons in index.html (data-value attributes).
  TOGGLE_RISK: () => {
    const elA = document.querySelector('[data-risktoggle="a"]');
    const elB = document.querySelector('[data-risktoggle="b"]');
    if (!elA || !elB) return;
    const a = parseInt(elA.getAttribute('data-value'), 10);
    const b = parseInt(elB.getAttribute('data-value'), 10);
    if (isNaN(a) || isNaN(b)) return;
    const state = getState();
    const cur = state && state.riskCash;
    const target = (cur === a) ? b : a;
    if (typeof window.setRiskCash === 'function') window.setRiskCash(target);
  },
  // Flip Auto-Track on/off (same as the button).
  AUTO_TRACK: () => {
    if (typeof window.toggleAutoTrack === 'function') window.toggleAutoTrack();
  },
  // Cycle the engine's tracking timeframe: 15s → 1m → 5m → 100t → 15s.
  CYCLE_TRACK_TF: () => {
    const state = getState();
    const cur = (state && state.trackTimeframe) || '15s';
    const idx = TRACK_TFS.indexOf(cur);
    const next = TRACK_TFS[(idx + 1) % TRACK_TFS.length];
    const sel = document.getElementById('trackTfSelect');
    if (sel) sel.value = next;
    if (typeof window.onTrackTfChanged === 'function') window.onTrackTfChanged(next);
  },
};

const STORAGE_KEY = 'tradeEngine_hotkeyBindings';

let bindingMap = null; // { descriptor -> action }, rebuilt from localStorage

function loadBindings() {
  const map = {};
  let saved = null;
  try {
    saved = JSON.parse(localStorage.getItem(STORAGE_KEY) || 'null');
  } catch (e) {
    saved = null;
  }
  for (const def of HOTKEY_ACTIONS) {
    const key = (saved && saved[def.action] !== undefined) ? saved[def.action] : def.defaultKey;
    if (key && key.trim() !== '') {
      map[key] = def.action;
    }
  }
  return map;
}

export function getBinding(action) {
  let saved = null;
  try {
    saved = JSON.parse(localStorage.getItem(STORAGE_KEY) || 'null');
  } catch (e) {
    saved = null;
  }
  const def = HOTKEY_ACTIONS.find(a => a.action === action);
  if (!def) return '';
  return (saved && saved[action]) || def.defaultKey;
}

export function setBinding(action, descriptor) {
  let saved = null;
  try {
    saved = JSON.parse(localStorage.getItem(STORAGE_KEY) || 'null');
  } catch (e) {
    saved = null;
  }
  saved = saved || {};
  saved[action] = descriptor;
  localStorage.setItem(STORAGE_KEY, JSON.stringify(saved));
  bindingMap = null; // force rebuild on next keypress
}

export function resetBindings() {
  localStorage.removeItem(STORAGE_KEY);
  bindingMap = null;
}

// Normalize a DOM keyboard event into a canonical descriptor, or null if it's
// just a modifier (Shift/Control/Alt/Meta) with no real key.
export function normalizeKeyEvent(e) {
  let key = e.key || '';
  if (key === ' ') key = 'Space';
  if (['Shift', 'Control', 'Alt', 'Meta'].includes(key)) return null;

  const mods = [];
  if (e.shiftKey) mods.push('shift');
  if (e.ctrlKey || e.metaKey) mods.push('ctrl');
  if (e.altKey) mods.push('alt');

  const keyLower = key.toLowerCase();
  return mods.length > 0 ? `${mods.join('+')}+${keyLower}` : keyLower;
}

function showHotkeyToast(text, isError = false) {
  let el = document.getElementById('hotkeyStatus');
  if (!el) {
    el = document.createElement('div');
    el.id = 'hotkeyStatus';
    Object.assign(el.style, {
      position: 'fixed',
      bottom: '18px',
      left: '50%',
      transform: 'translateX(-50%)',
      padding: '8px 16px',
      borderRadius: '6px',
      fontSize: '13px',
      fontWeight: '600',
      zIndex: '9999',
      transition: 'opacity 0.25s ease',
      pointerEvents: 'none',
      fontFamily: 'monospace',
    });
    document.body.appendChild(el);
  }
  el.innerText = text;
  el.style.background = isError ? 'rgba(255,80,80,0.15)' : 'rgba(0,192,118,0.15)';
  el.style.color = isError ? '#ff6b6b' : '#00c076';
  el.style.border = `1px solid ${isError ? 'rgba(255,80,80,0.4)' : 'rgba(0,192,118,0.4)'}`;
  el.style.opacity = '1';

  if (flashTimer) clearTimeout(flashTimer);
  flashTimer = setTimeout(() => {
    el.style.opacity = '0';
    setTimeout(() => { if (el.parentNode) el.parentNode.removeChild(el); }, 300);
  }, 2500);
}

export function isEditableTarget(target) {
  if (!target) return false;
  const tag = (target.tagName || '').toLowerCase();
  return (
    tag === 'input' ||
    tag === 'textarea' ||
    tag === 'select' ||
    target.isContentEditable
  );
}

function onKeyDown(e) {
  // Ignore OS key auto-repeat (holding a key fires many keydowns) — this would
  // otherwise spam duplicate HOTKEY commands and stack orders on a single press.
  if (e.repeat) return;

  const state = getState();
  // Hotkeys disabled → do nothing (let browser/NinjaTrader defaults through)
  if (!state || state.enableHotkeys === false) return;

  // While a rebind capture is in progress (Settings), let the capture handler own the key
  if (window.__hotkeyCapturing) return;

  if (isEditableTarget(e.target)) return;

  const descriptor = normalizeKeyEvent(e);
  if (!descriptor) return;

  if (!bindingMap) bindingMap = loadBindings();
  const action = bindingMap[descriptor];
  if (!action) return;

  // Kill Switch guard
  if (state.tradingDisabled && action !== 'KILL_SWITCH') {
    e.preventDefault();
    e.stopPropagation();
    showHotkeyToast('Trading disabled by Kill-Switch', true);
    return;
  }

  // Execution arming guard
  const isExecAction = (action === 'INSTANT_ENTRY' || action === 'BREAKOUT_ENTRY');
  if (isExecAction && state.hotkeysArmed === false) {
    e.preventDefault();
    e.stopPropagation();
    showHotkeyToast('Execution hotkeys disarmed (toggle arm in UI)', true);
    return;
  }

  // Suppress browser default (e.g. Ctrl+Space scroll)
  e.preventDefault();
  e.stopPropagation();

  const def = HOTKEY_ACTIONS.find(a => a.action === action);
  const label = def ? `${def.icon} ${descriptor.toUpperCase()} → ${def.label.toUpperCase()}` : action;
  showHotkeyToast(label);

  // Client-side actions (RR preset, size toggle) never leave the browser.
  const clientFn = CLIENT_ACTIONS[action];
  if (clientFn) {
    clientFn();
    return;
  }
  sendHotkey(action);
}

export function initHotkeys() {
  window.addEventListener('keydown', onKeyDown, true);
  console.info('[Hotkeys] armed — bindings configurable in Settings → Engine → Hotkeys');
  window.__hotkeysArmed = true;
}
