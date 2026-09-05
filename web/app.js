// Trade Engine Command Center � Main Application Entry Point
// No charts, no overlay: this is the execution/risk control panel only.
// The SL/TP/entry lines live on the NinjaTrader chart (LineHost drawing tool)
// and the engine is authoritative for all logic.
import { onStateChange, getState } from './js/state.js';
import { connectWS, executeTrade, cancelOrders, cancelSpecificOrder, changeSpecificOrder, flattenPosition, sendPriceUpdate, splitTargetOrder } from './js/ws.js';
import { initHotkeys, HOTKEY_ACTIONS, getBinding, setBinding, resetBindings, normalizeKeyEvent, isEditableTarget } from './js/hotkeys.js';
import {
  renderUI,
  setDirection,
  setEntryModel,
  setRiskCash,
  onRiskCashChanged,
  onMaxContractsChanged,
  setRR,
  toggleDynRisk,
  toggleLockSl,
  onQuickSlPadChanged,
  togglePartial,
  toggleShowLines,
  toggleAutoTrack,
  onTrackAnchorChanged,
  onTrackTfChanged,
  onTrackOffsetChanged,
  onAccountChanged,
  openEngineSettingsModal,
  closeChartSettingsModal,
  switchSettingsTab,
  applyEngineSettings,
  toggleHotkeysArmed,
  triggerKillSwitch,
  rearmKillSwitch,
} from './js/ui.js';

// Wire reactive state subscribers
onStateChange(() => {
  renderUI();
});

// Expose handlers to global window for HTML inline event listeners
window.getState = getState;
window.executeTrade = executeTrade;
window.cancelOrders = cancelOrders;
window.cancelSpecificOrder = cancelSpecificOrder;
window.changeSpecificOrder = changeSpecificOrder;
window.flattenPosition = flattenPosition;
window.splitTargetTp = function () {
  const state = getState();
  if (!state) return;
  // Find the working Take Profit order (whole-lot "ActiveTP" or partial
  // "ActiveTP_1") and split it in half; the hub derives the far-leg price.
  const tp = (state.workingOrders || []).find(o =>
    (o.name === 'ActiveTP' || o.name === 'ActiveTP_1') &&
    (o.state === 'Working' || o.state === 'Accepted'));
  if (!tp) {
    console.warn('Split TP: no working Take Profit order found');
    return;
  }
  splitTargetOrder(tp.orderId, tp.qty, tp.price);
};
window.setDirection = setDirection;
window.setEntryModel = setEntryModel;
window.setRiskCash = setRiskCash;
window.onRiskCashChanged = onRiskCashChanged;
window.onMaxContractsChanged = onMaxContractsChanged;
window.setRR = setRR;
window.toggleDynRisk = toggleDynRisk;
window.toggleLockSl = toggleLockSl;
window.onQuickSlPadChanged = onQuickSlPadChanged;
window.togglePartial = togglePartial;
window.toggleShowLines = toggleShowLines;
window.toggleAutoTrack = toggleAutoTrack;
window.onTrackAnchorChanged = onTrackAnchorChanged;
window.onTrackTfChanged = onTrackTfChanged;
window.onTrackOffsetChanged = onTrackOffsetChanged;
window.onAccountChanged = onAccountChanged;
window.openEngineSettingsModal = openEngineSettingsModal;
window.closeChartSettingsModal = closeChartSettingsModal;
window.switchSettingsTab = switchSettingsTab;
window.applyEngineSettings = applyEngineSettings;
window.toggleHotkeysArmed = toggleHotkeysArmed;
window.triggerKillSwitch = triggerKillSwitch;
window.rearmKillSwitch = rearmKillSwitch;

// ---- Hotkey binding capture (configurable from Settings ? Engine ? Hotkeys) ----
let pendingCaptureAction = null;

function labelForKey(descriptor) {
  if (!descriptor) return '';
  return descriptor
    .split('+')
    .map(p => (p === 'ctrl' ? 'Ctrl' : p === 'shift' ? 'Shift' : p === 'alt' ? 'Alt' : p.charAt(0).toUpperCase() + p.slice(1)))
    .join('+');
}

// Refresh all capture buttons to reflect current stored bindings.
function refreshHotkeyButtons() {
  document.querySelectorAll('.hotkey-capture').forEach(btn => {
    const action = btn.getAttribute('data-action');
    const def = HOTKEY_ACTIONS.find(a => a.action === action);
    btn.innerText = labelForKey(getBinding(action)) || (def ? labelForKey(def.defaultKey) : '');
    btn.classList.remove('capturing');
  });
}

// Enter "listening" mode on a button; the next real key combo reassigns it.
window.captureHotkey = function (btn) {
  const action = btn.getAttribute('data-action');
  if (pendingCaptureAction === action) {
    // cancel listening
    pendingCaptureAction = null;
    window.__hotkeyCapturing = false;
    btn.classList.remove('capturing');
    btn.innerText = labelForKey(getBinding(action));
    return;
  }
  // clear any other capture state
  document.querySelectorAll('.hotkey-capture').forEach(b => b.classList.remove('capturing'));
  pendingCaptureAction = action;
  window.__hotkeyCapturing = true;
  btn.classList.add('capturing');
  btn.innerText = 'Press a key�';
};

window.resetHotkeyBindings = function () {
  resetBindings();
  pendingCaptureAction = null;
  window.__hotkeyCapturing = false;
  refreshHotkeyButtons();
};

// Intercept a single keypress while a capture is pending.
window.addEventListener('keydown', (e) => {
  if (!pendingCaptureAction) return;
  if (e.repeat) return; // ignore auto-repeat during rebind capture
  const descriptor = normalizeKeyEvent(e);
  if (!descriptor) return; // pure modifier � keep listening
  e.preventDefault();
  e.stopPropagation();
  setBinding(pendingCaptureAction, descriptor);
  pendingCaptureAction = null;
  window.__hotkeyCapturing = false;
  refreshHotkeyButtons();
}, true);

refreshHotkeyButtons();

// Snap Entry to Live Market Price (engine-side intent; lines live in NT8)
window.snapLinesToMarket = function () {
  const state = getState();
  const cur = state ? state.currentMarketPrice : 0;
  if (!cur || cur <= 0) return;
  const tick = state.tickSize || 0.25;
  const rr = state.selectedRR || 2.0;
  const pin = { entryPrice: cur };
  if (!state.isSlLocked) {
    const slOffset = 20 * tick;
    pin.stopPrice = state.isLong ? cur - slOffset : cur + slOffset;
    pin.targetPrice = state.isLong ? cur + slOffset * rr : cur - slOffset * rr;
  } else {
    const slDist = Math.abs((state.entryPrice || cur) - (state.stopPrice || cur));
    pin.targetPrice = state.isLong ? cur + slDist * rr : cur - slDist * rr;
  }
  sendPriceUpdate({ ...pin, selectedRR: state.selectedRR });
};

// Unified Command Registry for Web Terminal Hotkeys
const COMMANDS = {
  'snap-to-market':   { keys: ['ALT+S'], fn: () => window.snapLinesToMarket() },
  'execute-trade':    { keys: ['ALT+E'], fn: () => executeTrade() },
  'flatten-position': { keys: ['ALT+F'], fn: () => flattenPosition() },
  'toggle-direction': { keys: ['ALT+D'], fn: () => setDirection(!getState()?.isLong) },
  'cancel-all':       { keys: ['ALT+C'], fn: () => cancelOrders() },
};

window.addEventListener('keydown', (e) => {
  if (isEditableTarget(e.target)) return;
  const combo = `${e.altKey ? 'ALT+' : ''}${e.ctrlKey ? 'CTRL+' : ''}${e.shiftKey ? 'SHIFT+' : ''}${(e.key || '').toUpperCase()}`;
  for (const cmd of Object.values(COMMANDS)) {
    if (cmd.keys.includes(combo)) {
      e.preventDefault();
      cmd.fn();
      return;
    }
  }
});

// ---- Collapse / expand the top bar (saves vertical space) ----
window.toggleTopBar = function () {
  const collapsed = !document.body.classList.contains('bar-collapsed');
  document.body.classList.toggle('bar-collapsed', collapsed);
  const chev = document.getElementById('barChevron');
  if (chev) chev.textContent = collapsed ? '?' : '?';
  try { localStorage.setItem('tradeEngine_bar', collapsed ? 'collapsed' : 'expanded'); } catch (e) {}
};

// Clicking the indicator explains how to toggle; the real toggle is plain 'L'
// inside NinjaTrader (the AddOn sends HOTKEY_STATUS which flips the badge).
window.toggleWebHotkeyHint = function () {
  const el = document.getElementById('hotkeyIndicator');
  if (!el) return;
  const body = el.innerHTML;
  el.innerHTML = 'PRESS L IN NINJATRADER';
  setTimeout(() => { el.innerHTML = body; }, 2500);
};

function applySavedBar() {
  let collapsed = false;
  try { collapsed = localStorage.getItem('tradeEngine_bar') === 'collapsed'; } catch (e) {}
  document.body.classList.toggle('bar-collapsed', collapsed);
  const chev = document.getElementById('barChevron');
  if (chev) chev.textContent = collapsed ? '?' : '?';
}

// ---- Panel layout toggle: vertical side column vs horizontal top bar ----
window.toggleLayout = function () {
  const main = document.querySelector('main');
  const btn = document.getElementById('btnLayout');
  if (!main) return;
  const horiz = !main.classList.contains('layout-horizontal');
  main.classList.toggle('layout-horizontal', horiz);
  if (btn) btn.title = horiz ? 'Toggle panel layout: horizontal top bar' : 'Toggle panel layout: vertical side column';
  try { localStorage.setItem('tradeEngine_layout', horiz ? 'horizontal' : 'vertical'); } catch (e) {}
};

function applySavedLayout() {
  const main = document.querySelector('main');
  if (!main) return;
  let horiz = false;
  try { horiz = localStorage.getItem('tradeEngine_layout') === 'horizontal'; } catch (e) {}
  main.classList.toggle('layout-horizontal', horiz);
  const btn = document.getElementById('btnLayout');
  if (btn) btn.title = horiz ? 'Toggle panel layout: horizontal top bar' : 'Toggle panel layout: vertical side column';
}

// Boot terminal
function boot() {
  // connectWS first: the status element defaults to "CONNECTING..." in the HTML.
  connectWS();
  try { initHotkeys(); } catch (e) { console.error('initHotkeys failed:', e); }
  // Load saved engine settings (inputs) into the panel.
  try { applyEngineSettings(); } catch (e) { console.error('applyEngineSettings failed:', e); }
  try { applySavedLayout(); } catch (e) { console.error('applySavedLayout failed:', e); }
  try { applySavedBar(); } catch (e) { console.error('applySavedBar failed:', e); }
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', boot);
} else {
  boot();
}