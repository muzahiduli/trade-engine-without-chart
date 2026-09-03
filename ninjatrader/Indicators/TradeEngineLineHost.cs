// TradeEngineLineHost — ZERO-LOGIC line host for trade-engine-without-chart.
//
// This NinjaTrader INDICATOR exists for exactly one purpose: host the three
// horizontal Entry / Stop Loss / Take Profit lines on the chart so the user can
// drag them around (NinjaTrader handles the dragging natively for lines drawn
// via Draw.Line — the user's mouse moves the whole line). It contains NO
// trading logic: no sizing, no RR math, no order submission, no bracket logic.
//
// Data flow:
//   - The user drags a line -> NT8 updates the drawing object -> OnChartObjectModified
//     fires -> we send UPDATE_PRICES {entryPrice, stopPrice, targetPrice} to the
//     Go engine over WebSocket (same message the web control panel sends).
//   - The Go engine (authoritative for ALL logic) replies/broadcasts SYNC_STATE;
//     this indicator re-positions the lines to match engine truth so the chart
//     always shows the authoritative state (e.g. after a fill or a web-panel edit).
//
// Safety: this indicator never calls SendOrder()/Submit(); it cannot execute
// trades even by accident. All execution happens in the Go engine via the
// separate gateway strategy.
#region Using declarations
using System;
using System.Collections.Generic;
using System.ComponentModel;
using System.Net.WebSockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using System.Windows.Media;
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
        private long lastSendTimeTicks;

        // ---- Drawn lines (held references) ----
        private Line entryLine;
        private Line stopLine;
        private Line targetLine;
        private bool linesCreated;

        // ---- Engine state mirrored onto the lines ----
        private double currentEntry;
        private double currentStop;
        private double currentTarget;
        private bool isInPosition;
        private bool suppressDragPush;

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

        protected override void OnBarUpdate()
        {
            if (State != State.Realtime) return;
            // Keep lines attached to the latest bar as the chart advances.
            // Anchor prices are untouched when the engine state matches.
            EnsureLines(CurrentBar);
        }

        // ------------------------------------------------------------------
        // Line hosting: create / reposition the three draggable horizontal lines.
        // ------------------------------------------------------------------
        private void EnsureLines(int barIndex)
        {
            int idx = Math.Max(0, barIndex);
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

            entryLine ??= Draw.Line(this, "LineHost_Entry", idx, entry, idx, entry, Brushes.DodgerBlue);
            stopLine ??= Draw.Line(this, "LineHost_Stop", idx, stop, idx, stop, Brushes.Crimson);
            targetLine ??= Draw.Line(this, "LineHost_Target", idx, target, idx, target, Brushes.LimeGreen);
            linesCreated = entryLine != null && stopLine != null && targetLine != null;

            // Reposition to engine truth unless the user is actively dragging
            // one of these lines (the drag itself will push prices on release).
            if (suppressDragPush) return;
            Reposition(entryLine, entry);
            Reposition(stopLine, stop);
            Reposition(targetLine, target);
        }

        private static void Reposition(Line ln, double price)
        {
            if (ln == null || ln.StartAnchor == null || ln.EndAnchor == null) return;
            // Only move when the price actually differs; avoid anchor churn.
            if (Math.Abs(ln.StartAnchor.Price - price) < 0.0001) return;
            ln.StartAnchor.Price = price;
            ln.EndAnchor.Price = price;
        }

        // ------------------------------------------------------------------
        // The 'UI changed' signal: the user dragged one of our lines in NT8.
        // Push the resulting prices to the Go engine as UPDATE_PRICES.
        // ------------------------------------------------------------------
        protected override void OnChartObjectModified(ChartObject chartObject, ChartObject.ChartObjectModificationType modificationType)
        {
            if (chartObject == null || !(chartObject is Line)) return;
            if (chartObject.Tag != "LineHost_Entry" && chartObject.Tag != "LineHost_Stop" && chartObject.Tag != "LineHost_Target") return;
            if (suppressDragPush) return; // we repositioned from engine state

            // Debounce: several modified events fire per drag operation; wait
            // until the drag settles before pushing a price update.
            long now = DateTime.UtcNow.Ticks;
            if (now - lastSendTimeTicks < TimeSpan.TicksPerSecond / 4) return;
            lastSendTimeTicks = now;

            if (entryLine == null || stopLine == null || targetLine == null) return;
            double entry = entryLine.StartAnchor?.Price ?? 0;
            double stop = stopLine.StartAnchor?.Price ?? 0;
            double target = targetLine.StartAnchor?.Price ?? 0;
            if (entry <= 0 || stop <= 0 || target <= 0) return;

            string payload = string.Format(
                System.Globalization.CultureInfo.InvariantCulture,
                "{{\"entryPrice\":{0},\"stopPrice\":{1},\"targetPrice\":{2}}}",
                entry.ToString("R", System.Globalization.CultureInfo.InvariantCulture),
                stop.ToString("R", System.Globalization.CultureInfo.InvariantCulture),
                target.ToString("R", System.Globalization.CultureInfo.InvariantCulture));
            SendWs("UPDATE_PRICES", payload);
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
            return Instrument.MarketData.Last;
        }

        // ------------------------------------------------------------------
        // Engine -> lines: reposition anchors to engine's authoritative state.
        // Suppress the drag-push to avoid a feedback loop.
        // ------------------------------------------------------------------
        private void ApplyEngineState(string payload)
        {
            try
            {
                var doc = System.Text.Json.JsonDocument.Parse(payload);
                var root = doc.RootElement;

                double entry = 0, stop = 0, target = 0;
                string mp = "Flat";
                if (root.TryGetProperty("position", out var pos) &&
                    pos.TryGetProperty("marketPosition", out var m) &&
                    m.ValueKind == System.Text.Json.JsonValueKind.String)
                    mp = m.GetString() ?? "Flat";
                if (root.TryGetProperty("entryPrice", out var ep) && ep.ValueKind == System.Text.Json.JsonValueKind.Number) entry = ep.GetDouble();
                if (root.TryGetProperty("stopPrice", out var sp) && sp.ValueKind == System.Text.Json.JsonValueKind.Number) stop = sp.GetDouble();
                if (root.TryGetProperty("targetPrice", out var tp) && tp.ValueKind == System.Text.Json.JsonValueKind.Number) target = tp.GetDouble();

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
            _ = Task.Run(() => ConnectLoopAsync(wsCts.Token));
        }

        private void StopWebSocket()
        {
            try
            {
                wsCts?.Cancel();
                wsClient?.Dispose();
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
                    using (var client = new ClientWebSocket())
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
                var bytes = Encoding.UTF8.GetBytes(msg);
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
                var doc = System.Text.Json.JsonDocument.Parse(json);
                var root = doc.RootElement;
                if (!root.TryGetProperty("type", out var t) || t.ValueKind != System.Text.Json.JsonValueKind.String) return;
                string type = t.GetString();
                if (type == "SYNC_STATE" && root.TryGetProperty("payload", out var payload))
                    ApplyEngineState(payload.GetRawText());
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
    }
}