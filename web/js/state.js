// Central Trade State Management for Trade Engine 2

let state = null;
const subscribers = new Set();

export function getState() {
  return state;
}

export function setState(newState) {
  state = newState;
  notifySubscribers();
}

export function patchState(patch) {
  if (!state) state = {};
  Object.assign(state, patch);
  notifySubscribers();
}

export function onStateChange(fn) {
  subscribers.add(fn);
  if (state) fn(state);
  return () => subscribers.delete(fn);
}

function notifySubscribers() {
  subscribers.forEach(fn => {
    try {
      fn(state);
    } catch (e) {
      console.error('Error in state subscriber:', e);
    }
  });
}

// Pure helper queries
export function isInPosition() {
  return Boolean(
    state &&
    state.position &&
    state.position.marketPosition &&
    state.position.marketPosition !== 'Flat' &&
    state.position.quantity > 0
  );
}
