// WebSocket Network Engine for Trade Engine 2
import { getState, setState } from './state.js';

let socket = null;
let reconnectInterval = null;

const listeners = {
  onSyncBars: [],
  onBarUpdate: [],
  onOpen: [],
};

export function onOpen(fn) {
  listeners.onOpen.push(fn);
}

export function onSyncBarsReceived(fn) {
  listeners.onSyncBars.push(fn);
}

export function onBarUpdateReceived(fn) {
  listeners.onBarUpdate.push(fn);
}

export function connectWS() {
  const host = window.location.host || 'localhost:8080';
  const wsUrl = `ws://${host}/ws?type=WEB`;


  updateStatus(false, 'CONNECTING...');
  socket = new WebSocket(wsUrl);

  socket.onopen = () => {
    updateStatus(true, 'ONLINE');
    if (reconnectInterval) {
      clearInterval(reconnectInterval);
      reconnectInterval = null;
    }
    // Always request a fresh state snapshot on (re)connect — otherwise after
    // an engine restart the UI keeps showing stale values (e.g. the hotkey
    // badge stuck on OFFLINE) until the next broadcast happens to fire.
    try {
      sendWS('GET_STATE', {});
    } catch (e) {}
    // Notify on-open listeners (subscribers do their own (re)subscribe).
    listeners.onOpen.forEach(fn => {
      try { fn(); } catch (e) { console.error('onOpen listener failed:', e); }
    });
  };

  socket.onmessage = (event) => {
    const raw = typeof event.data === 'string' ? event.data.trim() : '';
    if (!raw) return;

    const chunks = raw.includes('\n') ? raw.split('\n') : [raw];
    chunks.forEach(chunk => {
      const line = chunk.trim();
      if (!line) return;
      try {
        const msg = JSON.parse(line);
        if (msg.type === 'SYNC_STATE') {
          setState(msg.payload);
        } else if (msg.type === 'SYNC_BARS') {
          listeners.onSyncBars.forEach(fn => fn(msg.payload));
        } else if (msg.type === 'BAR_UPDATE') {
          listeners.onBarUpdate.forEach(fn => fn(msg.payload));
        } else if (msg.type === 'MARKET_DATA') {
          const s = getState();
          if (s) {
            if (msg.payload && msg.payload.bid > 0) s.currentBid = msg.payload.bid;
            if (msg.payload && msg.payload.ask > 0) s.currentAsk = msg.payload.ask;
            if (msg.payload && msg.payload.last > 0) {
              s.lastPrice = msg.payload.last;
              s.currentMarketPrice = msg.payload.last;
            }
          }
        }
      } catch (e) {
        console.error('Failed to parse WebSocket message:', e);
      }
    });
  };

  socket.onclose = () => {
    updateStatus(false, 'OFFLINE');
    if (!reconnectInterval) {
      reconnectInterval = setInterval(connectWS, 1500);
    }
  };

  socket.onerror = (err) => {
    console.error('WebSocket error:', err);
  };
}

export function updateStatus(isOnline, text) {
  const dot = document.getElementById('statusDot');
  const txt = document.getElementById('statusText');
  if (dot) {
    if (isOnline) dot.classList.add('online');
    else dot.classList.remove('online');
  }
  if (txt) txt.innerText = text;
}

export function sendWS(type, payload) {
  if (socket && socket.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify({ type, payload }));
  }
}

export function sendConfig(patch) {
  sendWS('SET_CONFIG', patch);
}

export function sendPriceUpdate(prices) {
  sendWS('UPDATE_PRICES', prices);
}

export function requestHistoricalBars(count = 500, timeframe = '') {
  const payload = { count };
  if (timeframe) payload.timeframe = timeframe;
  sendWS('GET_BARS', payload);
}

// Subscribe to a specific bar-series timeframe. timeframe is the label
// ("15s" | "1m" | "5m" | "100t") the NT8 gateway tags its streams with.
export function subscribeTimeframe(timeframe, timeframeType, timeframeValue, count = 500) {
  sendWS('SUBSCRIBE', { timeframe, timeframeType, timeframeValue, count });
}

export function executeTrade() {
  const state = getState();
  if (!state) return;

  const cmd = {
    action: 'EXECUTE',
    direction: state.isLong ? 'LONG' : 'SHORT',
    orderType: state.entryModel,
    entryPrice: state.entryPrice,
    stopPrice: state.stopPrice,
    targetPrice: state.targetPrice,
    qty: state.calculatedQty,
    targetExits: state.targetExits,
    isAutoTrack: state.isAutoTrackEnabled,
  };

  sendWS('EXECUTE_ORDER', cmd);

  const btn = document.getElementById('btnExecute');
  if (btn) {
    const orig = btn.innerText;
    btn.innerText = '⚡ SUBMITTING TO NT8...';
    btn.style.opacity = '0.7';
    setTimeout(() => {
      if (btn) {
        btn.innerText = orig;
        btn.style.opacity = '1';
      }
    }, 1200);
  }
}

export function cancelOrders() {
  sendWS('CANCEL_ORDER', { action: 'CANCEL_ALL' });
}

export function cancelSpecificOrder(orderId) {
  sendWS('CANCEL_ORDER', { action: 'CANCEL', orderId });
}

export function changeSpecificOrder(orderId, price) {
  sendWS('CHANGE_ORDER', { action: 'CHANGE', orderId, price });
}

// Change only the QUANTITY of a working order (price untouched). Sending qty-only
// (no price) lets the gateway change the size even when the stop is momentarily
// priced through the market — the price guard only applies to real price changes.
export function changeOrderQty(orderId, qty) {
  sendWS('CHANGE_ORDER', { action: 'CHANGE', orderId, qty });
}

// Split a working TakeProfit order in half: half stays at the current level, the
// other half is placed one R further out (computed in the hub).
export function splitTargetOrder(orderId, qty, price) {
  sendWS('SPLIT_TARGET', { action: 'SPLIT', orderId, qty, price });
}

export function flattenPosition() {
  const state = getState();
  sendWS('FLATTEN_POSITION', {
    action: 'FLATTEN',
    accountName: state ? state.selectedAccount || state.accountName || '' : '',
  });
}

export function sendHotkey(action) {
  sendWS('HOTKEY', { action });
}
