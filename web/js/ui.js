// UI Controls, Panels & Modal Settings for Trade Engine 2
import { getState, isInPosition, patchState } from './state.js';
import { sendConfig } from './ws.js';
import { DEFAULT_SETTINGS } from './constants.js';

export function renderUI() {
  const state = getState();
  if (!state) return;

  const inPos = isInPosition();
  // Go owns ALL derived trading math (see risk.RecalculateState): qty, risk,
  // reward and R:R are broadcast in SYNC_STATE and rendered verbatim — the UI
  // never recomputes them (client recomputation drifted from the engine and
  // showed wrong HUD numbers / execute sizes).
  const qty = inPos ? (state.position?.quantity || 0) : (state.calculatedQty || 1);
  const actualRisk = state.actualRiskAmount || 0;
  const actualReward = state.actualRewardAmount || 0;
  const rr = state.calculatedRR || 2;

  // 1. Top Header Display
  const instDisplay = document.getElementById('instDisplay');
  if (instDisplay) {
    instDisplay.innerText = `${state.instrumentName || 'NQ'} (${state.tickSize || 0.25})`;
  }

  const accSelect = document.getElementById('accSelect');
  if (accSelect) {
    const curVal = state.selectedAccount || state.accountName;
    const curOpts = Array.from(accSelect.options).map(o => o.value);
    const newOpts = (state.availableAccounts && state.availableAccounts.length > 0)
      ? state.availableAccounts
      : (curVal ? [curVal] : []);

    if (newOpts.length > 0 && curOpts.join(',') !== newOpts.join(',')) {
      accSelect.innerHTML = '';
      newOpts.forEach(acc => {
        const opt = document.createElement('option');
        opt.value = acc;
        opt.innerText = acc;
        accSelect.appendChild(opt);
      });
    }
    if (curVal && accSelect.value !== curVal) {
      accSelect.value = curVal;
    }
  }

  // 2. Position Badge in Header
  const posBadge = document.getElementById('posBadge');
  if (posBadge) {
    if (inPos) {
      posBadge.style.display = 'inline-block';
      const mktPos = state.position.marketPosition.toUpperCase();
      const pnl = state.position.unrealizedPnL || 0;
      posBadge.innerText = `${mktPos} ${state.position.quantity}c ($${pnl.toFixed(2)})`;
      posBadge.className = mktPos === 'LONG' ? 'badge badge-green' : 'badge badge-red';
    } else {
      posBadge.style.display = 'none';
    }
  }

  // 2a. Protection Alert and Command State Badges
  const protBadge = document.getElementById('protectionAlertBadge');
  if (protBadge) {
    if (state.isUnprotectedPosition) {
      protBadge.style.display = 'inline-block';
      protBadge.innerText = '⚠️ NO STOP LOSS!';
      protBadge.title = state.protectionAlert || 'Position is open with no working stop loss!';
    } else {
      protBadge.style.display = 'none';
    }
  }

  const cmdBadge = document.getElementById('commandStateBadge');
  if (cmdBadge) {
    const cs = state.commandState || 'IDLE';
    if (cs !== 'IDLE' && cs !== 'CONFIRMED') {
      cmdBadge.style.display = 'inline-block';
      cmdBadge.innerText = cs;
      if (cs === 'PENDING_FLATTEN' || cs === 'PENDING_KILL_SWITCH') {
        cmdBadge.style.background = 'rgba(255, 209, 102, 0.2)';
        cmdBadge.style.color = '#ffd166';
      } else if (cs === 'REJECTED') {
        cmdBadge.style.background = 'rgba(255, 71, 87, 0.2)';
        cmdBadge.style.color = '#ff4757';
      }
    } else {
      cmdBadge.style.display = 'none';
    }
  }

  const btnArm = document.getElementById('btnHotkeysArm');
  if (btnArm) {
    const armed = state.hotkeysArmed !== false;
    btnArm.innerText = armed ? '🛡️ ARMED' : '⚠️ DISARMED';
    btnArm.style.color = armed ? '#00c076' : '#ffd166';
    btnArm.style.borderColor = armed ? 'rgba(0, 192, 118, 0.4)' : 'rgba(255, 209, 102, 0.4)';
  }

  // Hotkey forwarding indicator (NT8 AddOn): connected + enabled = green ON,
  // connected + disabled = amber, not connected = red OFFLINE.
  const hkInd = document.getElementById('hotkeyIndicator');
  if (hkInd) {
    const connected = state.hotkeyAddonConnected === true;
    const enabled = state.hotkeyForwardingEnabled !== false;
    if (!connected) {
      hkInd.textContent = '⌨ HOTKEYS: OFFLINE';
      hkInd.style.color = '#ff4757';
      hkInd.style.background = 'rgba(255, 71, 87, 0.15)';
      hkInd.style.border = '1px solid rgba(255, 71, 87, 0.4)';
      hkInd.style.display = 'inline-block';
    } else if (enabled) {
      hkInd.textContent = '⌨ HOTKEYS: ON';
      hkInd.style.color = '#00c076';
      hkInd.style.background = 'rgba(0, 192, 118, 0.15)';
      hkInd.style.border = '1px solid rgba(0, 192, 118, 0.4)';
      hkInd.style.display = 'inline-block';
    } else {
      hkInd.textContent = '⌨ HOTKEYS: OFF';
      hkInd.style.color = '#ffd166';
      hkInd.style.background = 'rgba(255, 209, 102, 0.15)';
      hkInd.style.border = '1px solid rgba(255, 209, 102, 0.4)';
      hkInd.style.display = 'inline-block';
    }
  }

  const killBanner = document.getElementById('killSwitchBanner');
  if (killBanner) {
    killBanner.style.display = state.tradingDisabled ? 'block' : 'none';
  }

  // 2b. NT8 Live Open Position Card in HUD
  const liveCard = document.getElementById('livePositionCard');
  const liveBadge = document.getElementById('livePosBadge');
  const liveDot = document.getElementById('livePosDot');
  const liveAvg = document.getElementById('livePosAvgPrice');
  const liveQty = document.getElementById('livePosQty');
  const livePnL = document.getElementById('livePosPnL');

  if (liveCard && liveBadge) {
    if (inPos) {
      const isLong = (state.position.marketPosition || '').toUpperCase() === 'LONG';
      const pnl = state.position.unrealizedPnL || 0;
      const pts = state.position.unrealizedPoints || 0;
      const qty = state.position.quantity || 0;
      const avgPrice = state.position.averagePrice || 0;

      liveCard.style.borderColor = isLong ? 'rgba(0, 192, 118, 0.4)' : 'rgba(255, 59, 87, 0.4)';
      liveCard.style.background = isLong ? 'rgba(0, 192, 118, 0.05)' : 'rgba(255, 59, 87, 0.05)';

      if (liveDot) liveDot.style.background = isLong ? 'var(--accent-green)' : 'var(--accent-red)';
      liveBadge.innerText = isLong ? '▲ LONG' : '▼ SHORT';
      liveBadge.style.background = isLong ? 'rgba(0, 192, 118, 0.2)' : 'rgba(255, 59, 87, 0.2)';
      liveBadge.style.color = isLong ? 'var(--accent-green)' : 'var(--accent-red)';

      if (liveAvg) liveAvg.innerText = avgPrice > 0 ? avgPrice.toFixed(2) : '---';
      if (liveQty) liveQty.innerText = `${qty} CON`;
      if (livePnL) {
        const sign = pnl >= 0 ? '+' : '';
        livePnL.innerText = `${sign}$${pnl.toFixed(2)} (${sign}${pts.toFixed(2)} pts)`;
        livePnL.style.color = pnl >= 0 ? 'var(--accent-green)' : 'var(--accent-red)';
      }
    } else {
      liveCard.style.borderColor = 'var(--border-color)';
      liveCard.style.background = 'rgba(22, 25, 34, 0.8)';
      if (liveDot) liveDot.style.background = 'var(--text-muted)';
      liveBadge.innerText = 'FLAT';
      liveBadge.style.background = 'rgba(98, 107, 130, 0.2)';
      liveBadge.style.color = 'var(--text-muted)';
      if (liveAvg) liveAvg.innerText = '---';
      if (liveQty) liveQty.innerText = '0';
      if (livePnL) {
        livePnL.innerText = '$0.00 (0.00 pts)';
        livePnL.style.color = 'var(--text-muted)';
      }
    }
  }

  // 3. Direction Buttons
  const btnLong = document.getElementById('btnLong');
  const btnShort = document.getElementById('btnShort');
  if (btnLong && btnShort) {
    btnLong.className = state.isLong ? 'btn active-long' : 'btn';
    btnShort.className = !state.isLong ? 'btn active-short' : 'btn';
  }

  // 4. Auto Order Type Resolution Display (decision computed by Go's
  // BuildExecutionPlan / RecalculateState — the UI just renders it)
  const autoBadge = document.getElementById('autoOrderTypeBadge');
  if (autoBadge) {
    const isBreakout = state.isBreakout === true;
    const model = state.effectiveEntryModel || 'LIMIT';
    if (isBreakout) {
      autoBadge.innerText = `AUTO: ${model} (BREAKOUT)`;
      autoBadge.style.color = '#ffd166';
      autoBadge.style.borderColor = 'rgba(255, 209, 102, 0.4)';
      autoBadge.style.background = 'rgba(255, 209, 102, 0.1)';
    } else {
      autoBadge.innerText = `AUTO: ${model} (PULLBACK)`;
      autoBadge.style.color = '#00c076';
      autoBadge.style.borderColor = 'rgba(0, 192, 118, 0.4)';
      autoBadge.style.background = 'rgba(0, 192, 118, 0.1)';
    }
  }

  // 5. Risk & Qty Inputs
  const inRisk = document.getElementById('inputRiskCash');
  if (inRisk && document.activeElement !== inRisk) {
    inRisk.value = (state.riskCash || 100).toFixed(0);
  }

  const inMaxCon = document.getElementById('inputMaxContracts');
  if (inMaxCon && document.activeElement !== inMaxCon) {
    inMaxCon.value = state.maxContracts || 10;
  }

  // 6. Dynamic Risk Section
  const dynPanel = document.getElementById('dynRiskPanel');
  const btnDyn = document.getElementById('btnDynRisk');
  if (btnDyn) {
    btnDyn.className = state.enableDynRisk ? 'btn active' : 'btn';
    btnDyn.innerText = `🛡️ Dynamic Risk: ${state.enableDynRisk ? 'ON' : 'OFF'}`;
  }
  if (dynPanel) {
    dynPanel.style.display = state.enableDynRisk ? 'block' : 'none';
  }
  const dynState = document.getElementById('dynRiskState');
  if (dynState) dynState.innerText = state.dynRiskState || 'NORMAL';
  const dynBuffer = document.getElementById('dynRiskBuffer');
  if (dynBuffer) dynBuffer.innerText = `$${(state.dynRiskBuffer || 0).toFixed(2)}`;

  const btnLockSl = document.getElementById('btnLockSl');
  if (btnLockSl) {
    btnLockSl.className = state.isSlLocked ? 'btn active' : 'btn';
    btnLockSl.innerText = state.isSlLocked ? '🔒 Lock SL: ON' : '🔓 Lock SL: OFF';
  }

  const btnAutoTrack = document.getElementById('btnAutoTrack');
  if (btnAutoTrack) {
    btnAutoTrack.className = state.isAutoTrackEnabled ? 'btn active' : 'btn';
    btnAutoTrack.innerText = state.isAutoTrackEnabled ? '🎯 Auto-Track: ON' : '🎯 Auto-Track: OFF';
  }

  // Sync tracking controls (anchor / track timeframe / offset) from hub state.
  const anchorSel = document.getElementById('trackAnchorSelect');
  if (anchorSel && state.trackAnchor && document.activeElement !== anchorSel) {
    anchorSel.value = state.trackAnchor;
  }
  const tfSel = document.getElementById('trackTfSelect');
  if (tfSel && state.trackTimeframe && document.activeElement !== tfSel) {
    tfSel.value = state.trackTimeframe;
  }
  const offsetInput = document.getElementById('trackOffsetInput');
  if (offsetInput && document.activeElement !== offsetInput && state.trackOffsetTicks !== undefined) {
    offsetInput.value = String(state.trackOffsetTicks);
  }

  const quickSlPad = document.getElementById('quickSlPad');
  if (quickSlPad && document.activeElement !== quickSlPad) {
    quickSlPad.value = (state.slippagePadTicks || 0).toFixed(1);
  }

  // Sync Engine Modal Inputs when not focused
  const elPad = document.getElementById('settingSlippagePad');
  if (elPad && document.activeElement !== elPad) elPad.value = (state.slippagePadTicks || 0).toFixed(1);
  const elCap = document.getElementById('settingSlippageCap');
  if (elCap && document.activeElement !== elCap) elCap.value = (state.slippageCapTicks || 0).toFixed(1);
  const elSync = document.getElementById('settingSlippageSync');
  if (elSync && document.activeElement !== elSync) elSync.checked = !!state.isSlippageSync;

  const elFirstExit = document.getElementById('settingFirstExitFraction');
  if (elFirstExit && document.activeElement !== elFirstExit) elFirstExit.value = Math.round((state.firstExitFraction || 0.3333) * 100);
  const elSubExit = document.getElementById('settingSubsequentExitFraction');
  if (elSubExit && document.activeElement !== elSubExit) elSubExit.value = Math.round((state.subsequentExitFraction || 0.50) * 100);

  const elMLL = document.getElementById('settingMLL');
  if (elMLL && document.activeElement !== elMLL) elMLL.value = (state.mll || 50000).toFixed(0);
  const elPThresh = document.getElementById('settingPenaltyThreshold');
  if (elPThresh && document.activeElement !== elPThresh) elPThresh.value = (state.penaltyThreshold || 1000).toFixed(0);
  const elPRisk = document.getElementById('settingPenaltyRisk');
  if (elPRisk && document.activeElement !== elPRisk) elPRisk.value = (state.penaltyRisk || 100).toFixed(0);
  const elBRisk = document.getElementById('settingBaseRisk');
  if (elBRisk && document.activeElement !== elBRisk) elBRisk.value = (state.baseRisk || 200).toFixed(0);
  const elSFactor = document.getElementById('settingScaleFactor');
  if (elSFactor && document.activeElement !== elSFactor) elSFactor.value = (state.scaleFactor || 0.25).toFixed(2);
  const elMCap = document.getElementById('settingMaxCap');
  if (elMCap && document.activeElement !== elMCap) elMCap.value = (state.maxCap || 400).toFixed(0);

  const elLockSl = document.getElementById('settingLockSl');
  if (elLockSl && document.activeElement !== elLockSl) elLockSl.checked = !!state.isSlLocked;
  const elLockTp = document.getElementById('settingLockTp');
  if (elLockTp && document.activeElement !== elLockTp) elLockTp.checked = !!state.isTpLocked;
  const elShowTicks = document.getElementById('settingShowProfitInTicks');
  if (elShowTicks && document.activeElement !== elShowTicks) elShowTicks.checked = !!state.showProfitInTicks;

  // Hotkey settings
  const elEnableHotkeys = document.getElementById('settingEnableHotkeys');
  if (elEnableHotkeys && document.activeElement !== elEnableHotkeys) elEnableHotkeys.checked = state.enableHotkeys !== false;
  const elInstantOffset = document.getElementById('settingInstantEntryOffsetTicks');
  if (elInstantOffset && document.activeElement !== elInstantOffset) elInstantOffset.value = state.instantEntryOffsetTicks ?? 2;
  const elBreakoutOffset = document.getElementById('settingBreakoutEntryOffsetTicks');
  if (elBreakoutOffset && document.activeElement !== elBreakoutOffset) elBreakoutOffset.value = state.breakoutEntryOffsetTicks ?? 1;
  const elTrailOffset = document.getElementById('settingTrailStopOffsetTicks');
  if (elTrailOffset && document.activeElement !== elTrailOffset) elTrailOffset.value = state.trailStopOffsetTicks ?? 1;
  const elScalePct = document.getElementById('settingScaleOutPercent');
  if (elScalePct && document.activeElement !== elScalePct) elScalePct.value = state.scaleOutPercent ?? 50;
  const elScaleTimeout = document.getElementById('settingScaleOutTimeoutSeconds');
  if (elScaleTimeout && document.activeElement !== elScaleTimeout) elScaleTimeout.value = state.scaleOutTimeoutSeconds ?? 3;
  const elScalePriceMode = document.getElementById('settingScaleOutPriceMode');
  if (elScalePriceMode && document.activeElement !== elScalePriceMode) elScalePriceMode.value = state.scaleOutPriceMode || 'BarHighLow';

  const elComm = document.getElementById('settingCommissionPerContract');
  if (elComm && document.activeElement !== elComm) elComm.value = (state.commissionPerContract || 0).toFixed(2);
  const elMaxSlip = document.getElementById('settingMaxEntrySlippageTicks');
  if (elMaxSlip && document.activeElement !== elMaxSlip) elMaxSlip.value = state.maxEntrySlippageTicks ?? 4;
  const elBrkExp = document.getElementById('settingBreakoutExpirySeconds');
  if (elBrkExp && document.activeElement !== elBrkExp) elBrkExp.value = state.breakoutExpirySeconds ?? 0;
  const elAutoBE = document.getElementById('settingAutoBEOnTP1');
  if (elAutoBE && document.activeElement !== elAutoBE) elAutoBE.checked = state.autoBEOnTP1 !== false;
  const elAutoBEOffset = document.getElementById('settingAutoBEOffsetTicks');
  if (elAutoBEOffset && document.activeElement !== elAutoBEOffset) elAutoBEOffset.value = state.autoBEOffsetTicks ?? 1;
  const elArmed = document.getElementById('settingHotkeysArmed');
  if (elArmed && document.activeElement !== elArmed) elArmed.checked = state.hotkeysArmed !== false;

  // 7. RR Preset Buttons (Max 1:2)
  [
    { id: 'btnRR1', val: 1.0 },
    { id: 'btnRR15', val: 1.5 },
    { id: 'btnRR2', val: 2.0 },
  ].forEach(p => {
    const btn = document.getElementById(p.id);
    if (btn) {
      btn.className = Math.abs((state.selectedRR || 2.0) - p.val) < 0.05 ? 'btn active' : 'btn';
    }
  });

  // 8. Toggles
  const btnPartial = document.getElementById('btnPartial');
  if (btnPartial) {
    btnPartial.className = state.isPartialProfit ? 'btn active' : 'btn';
    btnPartial.innerText = `⚖️ Partial Profit: ${state.isPartialProfit ? 'ON' : 'OFF'}`;
  }

  const btnLines = document.getElementById('btnShowLines');
  if (btnLines) {
    btnLines.className = state.showLines ? 'btn active' : 'btn';
  }

  // 9. Slippage Grid (Go-computed exposure, rendered verbatim)
  const s0 = document.getElementById('slip0');
  const s4 = document.getElementById('slip4');
  const s8 = document.getElementById('slip8');
  if (s0) s0.innerText = `$${(state.slippage0Risk || 0).toFixed(2)}`;
  if (s4) s4.innerText = `$${(state.slippage4Risk || 0).toFixed(2)}`;
  if (s8) s8.innerText = `$${(state.slippage8Risk || 0).toFixed(2)}`;

  // 10. HUD Metrics
  const hudQty = document.getElementById('hudQty');
  if (hudQty) hudQty.innerText = `${inPos ? state.position.quantity : qty} CON`;

  const hudRisk = document.getElementById('hudRisk');
  if (hudRisk) hudRisk.innerText = `$${actualRisk.toFixed(2)}`;

  const hudReward = document.getElementById('hudReward');
  if (hudReward) hudReward.innerText = `$${actualReward.toFixed(2)}`;

  const hudRR = document.getElementById('hudRR');
  if (hudRR) hudRR.innerText = `1 : ${rr.toFixed(2)}`;

  // 11. Chips
  const chipE = document.getElementById('chipEntry');
  const chipS = document.getElementById('chipStop');
  const chipT = document.getElementById('chipTarget');
  if (chipE && state.entryPrice > 0) chipE.innerText = state.entryPrice.toFixed(2);
  if (chipS && state.stopPrice > 0) chipS.innerText = state.stopPrice.toFixed(2);
  if (chipT && state.targetPrice > 0) chipT.innerText = state.targetPrice.toFixed(2);

  // 12. Execute Button (order type & qty come from Go's RecalculateState)
  const btnExec = document.getElementById('btnExecute');
  if (btnExec) {
    const dirText = state.isLong ? 'LONG' : 'SHORT';
    btnExec.innerText = `EXECUTE ${dirText} ${state.effectiveEntryModel || 'LIMIT'} (${qty})`;
    btnExec.className = state.isLong ? 'btn-execute long' : 'btn-execute short';
  }
}

// User Actions
export function setDirection(isLong) {
  sendConfig({ isLong });
}

export function setEntryModel(model) {
  sendConfig({ entryModel: model });
}

export function setRiskCash(cash) {
  sendConfig({ riskCash: parseFloat(cash) || 100 });
}

export function onRiskCashChanged(val) {
  const c = parseFloat(val);
  if (!isNaN(c) && c > 0) setRiskCash(c);
}

export function onMaxContractsChanged(val) {
  const m = parseInt(val, 10);
  if (!isNaN(m) && m > 0) sendConfig({ maxContracts: m });
}

export function setRR(rr) {
  sendConfig({ selectedRR: parseFloat(rr) || 2.0 });
}

export function toggleDynRisk() {
  const state = getState();
  if (state) sendConfig({ enableDynRisk: !state.enableDynRisk });
}

export function toggleLockSl() {
  const state = getState();
  const nextVal = state ? !state.isSlLocked : true;
  patchState({ isSlLocked: nextVal });
  const btnLockSl = document.getElementById('btnLockSl');
  if (btnLockSl) {
    btnLockSl.className = nextVal ? 'btn active' : 'btn';
    btnLockSl.innerText = nextVal ? '🔒 Lock SL: ON' : '🔓 Lock SL: OFF';
  }
  sendConfig({ isSlLocked: nextVal });
}

export function onQuickSlPadChanged(val) {
  const pad = parseFloat(val) || 0;
  sendConfig({ slippagePadTicks: pad });
}

export function togglePartial() {
  const state = getState();
  if (state) sendConfig({ isPartialProfit: !state.isPartialProfit });
}

export function toggleShowLines() {
  const state = getState();
  if (state) sendConfig({ showLines: !state.showLines });
}

export function toggleAutoTrack() {
  const state = getState();
  if (!state) return;
  const nextVal = !state.isAutoTrackEnabled;
  // Optimistic local update so the button flips immediately (SYNC_STATE
  // round-trip re-syncs it anyway a moment later).
  patchState({ isAutoTrackEnabled: nextVal });
  const btn = document.getElementById('btnAutoTrack');
  if (btn) {
    btn.className = nextVal ? 'btn active' : 'btn';
    btn.innerText = nextVal ? '🎯 Auto-Track: ON' : '🎯 Auto-Track: OFF';
  }
  sendConfig({ isAutoTrackEnabled: nextVal });
}

// Which bar the AutoTrack anchor uses: prior (closed) bar vs current bar vs 20-EMA.
export function onTrackAnchorChanged(val) {
  if (!val) return;
  sendConfig({ trackAnchor: val });
}

// Which bar SERIES AutoTrack anchors on (15s/1m/5m/100t), independent of the
// timeframes the chart panes display.
export function onTrackTfChanged(val) {
  if (!val) return;
  sendConfig({ trackTimeframe: val });
}

export function onTrackOffsetChanged(val) {
  const ticks = parseInt(val, 10);
  if (isNaN(ticks)) return;
  sendConfig({ trackOffsetTicks: ticks });
}

export function onAccountChanged(accName) {
  if (!accName) return;
  try { localStorage.setItem('tradeEngine_defaultAccount', accName); } catch (e) {}
  sendConfig({ selectedAccount: accName });
}

// Chart Settings Modal
export function openChartSettingsModal() {
  const m = document.getElementById('chartSettingsModal');
  if (m) m.style.display = 'flex';
}

export function openEngineSettingsModal() {
  const m = document.getElementById('chartSettingsModal');
  if (m) {
    m.style.display = 'flex';
    switchSettingsTab('engine');
    populateEngineSettingsInputs();
  }
}

export function populateEngineSettingsInputs() {
  const state = getState();
  if (!state) return;
  const setVal = (id, val) => {
    const el = document.getElementById(id);
    if (el && val !== undefined && val !== null) el.value = val;
  };
  const setCheck = (id, val) => {
    const el = document.getElementById(id);
    if (el && val !== undefined) el.checked = !!val;
  };

  setVal('settingSlippagePad', state.slippagePadTicks);
  setVal('settingSlippageCap', state.slippageCapTicks);
  setCheck('settingSlippageSync', state.isSlippageSync);
  setVal('settingCommissionPerContract', state.commissionPerContract);
  setVal('settingMaxEntrySlippageTicks', state.maxEntrySlippageTicks);
  setVal('settingBreakoutExpirySeconds', state.breakoutExpirySeconds);
  setCheck('settingAutoBEOnTP1', state.autoBEOnTP1);
  setVal('settingAutoBEOffsetTicks', state.autoBEOffsetTicks);
  setCheck('settingHotkeysArmed', state.hotkeysArmed);
  setVal('settingInstantEntryOffsetTicks', state.instantEntryOffsetTicks);
  setVal('settingBreakoutEntryOffsetTicks', state.breakoutEntryOffsetTicks);
  setVal('settingTrailStopOffsetTicks', state.trailStopOffsetTicks);
  setVal('settingScaleOutPercent', state.scaleOutPercent);
  setVal('settingScaleOutTimeoutSeconds', state.scaleOutTimeoutSeconds);
  setVal('settingScaleOutPriceMode', state.scaleOutPriceMode);
  setCheck('settingEnableHotkeys', state.enableHotkeys);
}

export function toggleHotkeysArmed() {
  const state = getState();
  const next = !(state && state.hotkeysArmed !== false);
  sendConfig({ hotkeysArmed: next });
}

export function triggerKillSwitch() {
  if (confirm('🚨 EMERGENCY KILL SWITCH: Flatten open position, cancel all orders, and disable trading?')) {
    import('./hotkeys.js').then(m => {
      import('./ws.js').then(w => w.sendHotkey('KILL_SWITCH'));
    });
  }
}

export function rearmKillSwitch() {
  sendConfig({ tradingDisabled: false, hotkeysArmed: true });
}

export function closeChartSettingsModal() {
  const m = document.getElementById('chartSettingsModal');
  if (m) m.style.display = 'none';
}

export function switchSettingsTab(tabName) {
  // Single-panel settings: only the Engine & Risk tab exists in this build.
  const t = 'engine';
  const btn = document.getElementById('tvTabBtnEngine');
  const content = document.getElementById('tvTabContentEngine');
  if (btn) btn.classList.add('active');
  if (content) content.style.display = 'flex';
  void tabName;
}

export function applyEngineSettings() {
  const cfg = {};
  const slippagePad = document.getElementById('settingSlippagePad');
  if (slippagePad) cfg.slippagePadTicks = parseFloat(slippagePad.value) || 0;
  const slippageCap = document.getElementById('settingSlippageCap');
  if (slippageCap) cfg.slippageCapTicks = parseFloat(slippageCap.value) || 0;
  const slippageSync = document.getElementById('settingSlippageSync');
  if (slippageSync) cfg.isSlippageSync = slippageSync.checked;

  const firstExit = document.getElementById('settingFirstExitFraction');
  if (firstExit) cfg.firstExitFraction = (parseFloat(firstExit.value) || 33.3) / 100.0;
  const subExit = document.getElementById('settingSubsequentExitFraction');
  if (subExit) cfg.subsequentExitFraction = (parseFloat(subExit.value) || 50.0) / 100.0;

  const mll = document.getElementById('settingMLL');
  if (mll) cfg.mll = parseFloat(mll.value) || 50000;
  const pThresh = document.getElementById('settingPenaltyThreshold');
  if (pThresh) cfg.penaltyThreshold = parseFloat(pThresh.value) || 1000;
  const pRisk = document.getElementById('settingPenaltyRisk');
  if (pRisk) cfg.penaltyRisk = parseFloat(pRisk.value) || 100;
  const bRisk = document.getElementById('settingBaseRisk');
  if (bRisk) cfg.baseRisk = parseFloat(bRisk.value) || 200;
  const sFactor = document.getElementById('settingScaleFactor');
  if (sFactor) cfg.scaleFactor = parseFloat(sFactor.value) || 0.25;
  const mCap = document.getElementById('settingMaxCap');
  if (mCap) cfg.maxCap = parseFloat(mCap.value) || 400;

  const lockSl = document.getElementById('settingLockSl');
  if (lockSl) cfg.isSlLocked = lockSl.checked;
  const lockTp = document.getElementById('settingLockTp');
  if (lockTp) cfg.isTpLocked = lockTp.checked;
  const showTicks = document.getElementById('settingShowProfitInTicks');
  if (showTicks) cfg.showProfitInTicks = showTicks.checked;

  const commission = document.getElementById('settingCommissionPerContract');
  if (commission) cfg.commissionPerContract = parseFloat(commission.value) || 0;
  const maxEntrySlip = document.getElementById('settingMaxEntrySlippageTicks');
  if (maxEntrySlip) cfg.maxEntrySlippageTicks = parseInt(maxEntrySlip.value, 10) || 0;
  const brkExpiry = document.getElementById('settingBreakoutExpirySeconds');
  if (brkExpiry) cfg.breakoutExpirySeconds = parseFloat(brkExpiry.value) || 0;
  const autoBE = document.getElementById('settingAutoBEOnTP1');
  if (autoBE) cfg.autoBEOnTP1 = autoBE.checked;
  const autoBEOffset = document.getElementById('settingAutoBEOffsetTicks');
  if (autoBEOffset) cfg.autoBEOffsetTicks = parseInt(autoBEOffset.value, 10) || 0;
  const armed = document.getElementById('settingHotkeysArmed');
  if (armed) cfg.hotkeysArmed = armed.checked;

  const enableHotkeys = document.getElementById('settingEnableHotkeys');
  if (enableHotkeys) cfg.enableHotkeys = enableHotkeys.checked;
  const instantOffset = document.getElementById('settingInstantEntryOffsetTicks');
  if (instantOffset) cfg.instantEntryOffsetTicks = parseInt(instantOffset.value, 10) || 0;
  const breakoutOffset = document.getElementById('settingBreakoutEntryOffsetTicks');
  if (breakoutOffset) cfg.breakoutEntryOffsetTicks = parseInt(breakoutOffset.value, 10) || 0;
  const trailOffset = document.getElementById('settingTrailStopOffsetTicks');
  if (trailOffset) cfg.trailStopOffsetTicks = parseInt(trailOffset.value, 10) || 0;
  const scalePct = document.getElementById('settingScaleOutPercent');
  if (scalePct) cfg.scaleOutPercent = parseFloat(scalePct.value) || 50;
  const scaleTimeout = document.getElementById('settingScaleOutTimeoutSeconds');
  if (scaleTimeout) cfg.scaleOutTimeoutSeconds = parseFloat(scaleTimeout.value) || 3;
  const scalePriceMode = document.getElementById('settingScaleOutPriceMode');
  if (scalePriceMode) cfg.scaleOutPriceMode = scalePriceMode.value;

  sendConfig(cfg);
}

