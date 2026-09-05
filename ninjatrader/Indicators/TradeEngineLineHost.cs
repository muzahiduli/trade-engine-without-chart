// TradeEngineLineHost — ZERO-LOGIC line host for trade-engine-without-chart.
//
// This NinjaTrader INDICATOR exists for exactly one purpose: host the three
// horizontal Entry / Stop Loss / Take Profit lines on the chart so the user can
// drag them around (NinjaTrader handles the dragging natively for lines drawn
// via Draw.Line — the user's mouse moves the whole line). It contains NO
// trading logic: no sizing, no RR math, no order submission, no bracket logic.
//
// Data flow:
//   - The user drags a line -> ChartAnchor.Price updates live -> a background
//     poll loop (250 ms) detects the change and sends UPDATE_PRICES
//     {entryPrice, stopPrice, targetPrice} to the Go engine over WebSocket
//     (same message the web control panel sends).
//   - The Go engine (authoritative for ALL logic) broadcasts SYNC_STATE; this
//     indicator re-positions the lines to match engine truth so the chart
//     always shows the authoritative state (e.g. after a fill or a web-panel
//     edit).
//
// Safety: this indicator never calls SendOrder()/Submit(); it cannot execute
// trades even by accident. All execution happens in the Go engine via the
// separate gateway strategy.
//
// Note: written to compile with the classic NinjaTrader 8 Roslyn compiler
// (C# 7.x / .NET Framework): no string interpolation, no ??.=, no out var,
// no discards, and no System.Text.Json (parse via JavaScriptSerializer, the
// same approach the gateway strategy uses).
#region Using declarations
using System;
using System.Collections.Generic;
using System.ComponentModel;
using System.ComponentModel.DataAnnotations;
using System.Globalization;
using System.Net.WebSockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using System.Windows.Media;
using System.Web.Script.Serialization;
using NinjaTrader.Cbi;
using NinjaTrader.Core;
using NinjaTrader.Data;
using NinjaTrader.Gui;
using NinjaTrader.Gui.Chart;
using NinjaTrader.NinjaScript;
using NinjaTrader.NinjaScript.DrawingTools;
#endregion

namespace NinjaTrader.NinjaScript.Indicators
{
    public class TradeEngineLineHost : Indicator
    {
        // ---- WebSocket to the Go engine ----
        private ClientWebSocket wsClient;
        private CancellationTokenSource wsCts;
        private readonly Queue<string> outboundQueue = new Queue<string>();
        private readonly SemaphoreSlim queueSignal = new SemaphoreSlim(0);
        private bool isWsConnected;
        private long lastSendUnix;
        private const int SendDebounceMs = 300;
        private bool pollLoopStarted;

        // ---- Drawn lines (held references) ----
        // HorizontalLine spans the full chart width and is natively draggable
        // vertically in NT8: the user's drag updates StartAnchor.Price, which
        // the poll loop reads and pushes as UPDATE_PRICES. (Drawing these as
        // bars-ago Line objects would anchor them far back in history -- the
        // reason the lines were invisible on the live chart.)
        private NinjaTrader.NinjaScript.DrawingTools.HorizontalLine entryLine;
        private NinjaTrader.NinjaScript.DrawingTools.HorizontalLine stopLine;
        private NinjaTrader.NinjaScript.DrawingTools.HorizontalLine targetLine;

        // ---- Engine state mirrored onto the lines ----
        private double currentEntry;
        private double currentStop;
        private double currentTarget;
        private bool isInPosition;
        private volatile bool suppressDragPush;
        private double lastPushedEntry;
        private double lastPushedStop;
        private double lastPushedTarget;

        private string webSocketUrl = "ws://localhost:8080/ws?type=LINEHOST";

        [NinjaScriptProperty]
        [Display(Name = "WebSocket URL", Order = 1, GroupName = "Engine Connection")]
        public string WebSocketUrl
        {
            get { return webSocketUrl; }
            set { webSocketUrl = value; }
        }

        protected override void OnStateChange()
        {
            if (State == State.SetDefaults)
            {
                Description = @"Zero-logic line host: draggable Entry/SL/TP lines. All logic lives in the Go trade engine.";
                Name = "TradeEngineLineHost";
                Calculate = Calculate.OnBarClose;
                IsOverlay = true;
                DisplayInDataBox = false;
                PaintPriceMarkers = false;
                DrawOnPricePanel = true;
            }
            else if (State == State.Realtime)
            {
                EnsureLines(CurrentBar);
                StartWebSocket();
                if (!pollLoopStarted)
                {
                    pollLoopStarted = true;
                    PreparePollLoop();
                }
            }
            else if (State == State.Terminated)
            {
                StopWebSocket();
                try
                {
                    if (entryLine != null) RemoveDrawObject("LineHost_Entry");
                    if (stopLine != null) RemoveDrawObject("LineHost_Stop");
                    if (targetLine != null) RemoveDrawObject("LineHost_Target");
                }
                catch { }
            }
        }

        // Start the drag-poll loop on a background task (kept separate from
        // OnStateChange so the async worker cannot block NT8's UI thread).
        private void PreparePollLoop()
        {
            CancellationTokenSource cts = wsCts;
            Task.Run(() => DragPollLoopAsync(cts != null ? cts.Token : CancellationToken.None));
        }

        protected override void OnBarUpdate()
        {
            if (State != State.Realtime) return;
            // Keep lines attached to the latest bar as the chart advances.
            EnsureLines(CurrentBar);
        }

        // ------------------------------------------------------------------
        // Line hosting: create / reposition the three draggable horizontal lines.
        // ------------------------------------------------------------------
        private void EnsureLines(int barIndex)
        {
            double basePrice = GetCurrentPrice();
            if (basePrice <= 0) return;

            double entry, stop, target;
            if (isInPosition && currentEntry > 0)
            {
                entry = currentEntry;
                stop = currentStop > 0 ? currentStop : currentEntry - 10 * TickSize;
                target = currentTarget > 0 ? currentTarget : currentEntry + 20 * TickSize;
            }
            else
            {
                entry = basePrice;
                stop = basePrice - 10 * TickSize;
                target = basePrice + 20 * TickSize;
            }

            if (entryLine == null)
                entryLine = Draw.HorizontalLine(this, "LineHost_Entry", entry, Brushes.DodgerBlue);
            if (stopLine == null)
                stopLine = Draw.HorizontalLine(this, "LineHost_Stop", stop, Brushes.Crimson);
            if (targetLine == null)
                targetLine = Draw.HorizontalLine(this, "LineHost_Target", target, Brushes.LimeGreen);

            if (!suppressDragPush)
            {
                Reposition(entryLine, entry);
                Reposition(stopLine, stop);
                Reposition(targetLine, target);
            }
        }

        private static void Reposition(NinjaTrader.NinjaScript.DrawingTools.HorizontalLine ln, double price)
        {
            if (ln == null || ln.StartAnchor == null) return;
            if (Math.Abs(ln.StartAnchor.Price - price) < 0.0001) return;
            ln.StartAnchor.Price = price;
        }

        // ------------------------------------------------------------------
        // Drag detection: this NT8 build has no OnChartObjectModified hook, so
        // we poll the line anchors at 250 ms. ChartAnchor.Price updates live as
        // the user drags; when any price moved, push UPDATE_PRICES (debounced).
        // ------------------------------------------------------------------
        private async Task DragPollLoopAsync(CancellationToken ct)
        {
            while (!ct.IsCancellationRequested)
            {
                try
                {
                    await Task.Delay(250, ct).ConfigureAwait(false);
                }
                catch (OperationCanceledException)
                {
                    break;
                }
                if (!isWsConnected || suppressDragPush) continue;
                if (entryLine == null || stopLine == null || targetLine == null) continue;
                try
                {
                    double entry = entryLine.StartAnchor != null ? entryLine.StartAnchor.Price : 0;
                    double stop = stopLine.StartAnchor != null ? stopLine.StartAnchor.Price : 0;
                    double target = targetLine.StartAnchor != null ? targetLine.StartAnchor.Price : 0;
                    if (entry <= 0 || stop <= 0 || target <= 0) continue;

                    bool moved =
                        Math.Abs(entry - lastPushedEntry) > 0.0001 ||
                        Math.Abs(stop - lastPushedStop) > 0.0001 ||
                        Math.Abs(target - lastPushedTarget) > 0.0001;
                    if (!moved) continue;

                    long now = DateTime.UtcNow.Ticks / TimeSpan.TicksPerMillisecond;
                    if (now - lastSendUnix < SendDebounceMs) continue;
                    lastSendUnix = now;

                    lastPushedEntry = entry;
                    lastPushedStop = stop;
                    lastPushedTarget = target;

                    string payload = string.Format(
                        CultureInfo.InvariantCulture,
                        "{{\"entryPrice\":{0},\"stopPrice\":{1},\"targetPrice\":{2}}}",
                        entry.ToString("R", CultureInfo.InvariantCulture),
                        stop.ToString("R", CultureInfo.InvariantCulture),
                        target.ToString("R", CultureInfo.InvariantCulture));
                    SendWs("UPDATE_PRICES", payload);
                }
                catch { }
            }
        }

        private double GetCurrentPrice()
        {
            if (CurrentBar < 0) return 0;
            try
            {
                double c = Close[0];
                if (!double.IsNaN(c) && c > 0) return c;
            }
            catch { }
            try
            {
                MarketDataEventArgs mde = Instrument.MarketData.Last;
                if (mde != null) return mde.Price;
            }
            catch { }
            return 0;
        }

        // ------------------------------------------------------------------
        // Engine -> lines: reposition anchors to engine's authoritative state
        // (SYNC_STATE broadcast). Suppresses the drag-push to avoid a loop.
        // ------------------------------------------------------------------
        private void ApplyEngineState(string payload)
        {
            try
            {
                var serializer = new JavaScriptSerializer();
                Dictionary<string, object> root = serializer.Deserialize<Dictionary<string, object>>(payload);
                if (root == null) return;

                Dictionary<string, object> pos = root.ContainsKey("position") ? root["position"] as Dictionary<string, object> : null;
                string mp = GetField(root, "marketPosition", "Flat");
                if (pos != null && pos.ContainsKey("marketPosition") && pos["marketPosition"] != null)
                    mp = pos["marketPosition"].ToString();

                double entry = GetDoubleField(root, "entryPrice", 0);
                double stop = GetDoubleField(root, "stopPrice", 0);
                double target = GetDoubleField(root, "targetPrice", 0);

                bool inPos = !string.Equals(mp, "Flat", StringComparison.OrdinalIgnoreCase);
                if (!inPos || entry <= 0) return; // keep draggable placeholder lines

                suppressDragPush = true;
                try
                {
                    isInPosition = true;
                    currentEntry = entry;
                    currentStop = stop > 0 ? stop : entry - 10 * TickSize;
                    currentTarget = target > 0 ? target : entry + 20 * TickSize;
                    EnsureLines(CurrentBar);
                }
                finally
                {
                    suppressDragPush = false;
                }
            }
            catch { }
        }

        // ------------------------------------------------------------------
        // WebSocket plumbing (mirrors the gateway strategy). This component only
        // SENDS UPDATE_PRICES and RECEIVES SYNC_STATE / MARKET_DATA.
        // ------------------------------------------------------------------
        private void StartWebSocket()
        {
            if (wsClient != null) return;
            wsCts = new CancellationTokenSource();
            Task.Run(() => ConnectLoopAsync(wsCts.Token));
        }

        private void StopWebSocket()
        {
            try
            {
                if (wsCts != null) wsCts.Cancel();
                if (wsClient != null) wsClient.Dispose();
            }
            catch { }
            wsClient = null;
            isWsConnected = false;
        }

        private async Task ConnectLoopAsync(CancellationToken ct)
        {
            while (!ct.IsCancellationRequested)
            {
                try
                {
                    var client = new ClientWebSocket();
                    using (client)
                    {
                        await client.ConnectAsync(new Uri(WebSocketUrl), ct).ConfigureAwait(false);
                        wsClient = client;
                        isWsConnected = true;
                        Print("TradeEngineLineHost: connected to engine at " + WebSocketUrl);
                        await Task.WhenAll(SenderAsync(ct), ReceiverAsync(ct)).ConfigureAwait(false);
                    }
                }
                catch (Exception ex)
                {
                    if (!ct.IsCancellationRequested)
                        Print("TradeEngineLineHost: connection error - " + ex.Message);
                }
                isWsConnected = false;
                wsClient = null;
                if (!ct.IsCancellationRequested)
                    await Task.Delay(2000, ct).ConfigureAwait(false); // reconnect
            }
        }

        private async Task SenderAsync(CancellationToken ct)
        {
            while (!ct.IsCancellationRequested && wsClient != null && wsClient.State == WebSocketState.Open)
            {
                await queueSignal.WaitAsync(ct).ConfigureAwait(false);
                string msg;
                lock (outboundQueue)
                {
                    if (outboundQueue.Count == 0) continue;
                    msg = outboundQueue.Dequeue();
                }
                byte[] bytes = Encoding.UTF8.GetBytes(msg);
                await wsClient.SendAsync(new ArraySegment<byte>(bytes), WebSocketMessageType.Text, true, ct).ConfigureAwait(false);
            }
        }

        private async Task ReceiverAsync(CancellationToken ct)
        {
            var buffer = new byte[65536];
            var sb = new StringBuilder();
            while (!ct.IsCancellationRequested && wsClient != null && wsClient.State == WebSocketState.Open)
            {
                try
                {
                    WebSocketReceiveResult result = await wsClient.ReceiveAsync(new ArraySegment<byte>(buffer), ct).ConfigureAwait(false);
                    if (result.MessageType == WebSocketMessageType.Close)
                        break;
                    sb.Append(Encoding.UTF8.GetString(buffer, 0, result.Count));
                    if (result.EndOfMessage)
                    {
                        string line = sb.ToString();
                        sb.Clear();
                        foreach (string chunk in line.Split('\n'))
                        {
                            string trimmed = chunk.Trim();
                            if (trimmed.Length == 0) continue;
                            HandleIncoming(trimmed);
                        }
                    }
                }
                catch (Exception ex)
                {
                    if (!ct.IsCancellationRequested)
                        Print("TradeEngineLineHost: receive error - " + ex.Message);
                    break;
                }
            }
        }

        private void HandleIncoming(string json)
        {
            try
            {
                var serializer = new JavaScriptSerializer();
                Dictionary<string, object> root = serializer.Deserialize<Dictionary<string, object>>(json);
                if (root == null) return;
                string type = GetField(root, "type", "");
                string payload = GetField(root, "payload", "");
                if (type == "SYNC_STATE" && payload.Length > 0)
                    ApplyEngineState(payload);
                // MARKET_DATA, HEARTBEAT etc. intentionally ignored.
            }
            catch { }
        }

        private void SendWs(string msgType, string payload)
        {
            if (!isWsConnected || wsClient == null || wsClient.State != WebSocketState.Open) return;
            string msg = string.Format("{{\"type\":\"{0}\",\"payload\":{1}}}", msgType, payload);
            lock (outboundQueue)
            {
                if (outboundQueue.Count > 10000)
                    outboundQueue.Dequeue();
                outboundQueue.Enqueue(msg);
            }
            try { queueSignal.Release(); } catch { }
        }

        // ---- JSON helpers (window of convenience; same style as the gateway) ----
        private static string GetField(object obj, string key, string fallback)
        {
            var dict = obj as Dictionary<string, object>;
            if (dict != null && dict.ContainsKey(key) && dict[key] != null)
                return dict[key].ToString();
            return fallback;
        }

        private static double GetDoubleField(object obj, string key, double fallback)
        {
            var dict = obj as Dictionary<string, object>;
            if (dict != null && dict.ContainsKey(key) && dict[key] != null)
            {
                double val;
                if (double.TryParse(dict[key].ToString(), NumberStyles.Any, CultureInfo.InvariantCulture, out val))
                    return val;
            }
            return fallback;
        }
    }
}