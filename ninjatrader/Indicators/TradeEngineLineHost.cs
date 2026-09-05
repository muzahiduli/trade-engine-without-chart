// TradeEngineLineHost — ZERO-LOGIC line host for trade-engine-without-chart.
//
// This NinjaTrader INDICATOR exists for exactly one purpose: host the three
// horizontal Entry / Stop Loss / Take Profit lines on the chart so the user can
// drag them around. It contains NO trading logic: no sizing, no RR math, no
// order submission, no bracket logic.
//
// Dragging is implemented the same way the RiskRewardTool does it (and is the
// only reliable way in NT8): the lines are drawn ourselves in OnRender with
// RenderTarget.DrawLine (full chart width), and the chart panel's mouse events
// are handled ourselves — hit-test within 30px of a line on mouse-down,
// capture the mouse, convert Y<->price through the cached ChartScale on move,
// and on release push the three prices to the Go engine as UPDATE_PRICES.
// (NT8's Draw.Line/HorizontalLine objects are unreliable to drag on charts
// with other chart objects; custom render + own mouse handlers always work.)
//
// Data flow:
//   - USER drags a line → mouse-move converts Y to price → on release we send
//     UPDATE_PRICES {entryPrice, stopPrice, targetPrice} to the Go engine over
//     the WebSocket (same message the web control panel sends).
//   - The Go engine (authoritative for ALL logic) broadcasts SYNC_STATE; this
//     indicator re-positions the lines to match engine truth so the chart
//     always shows the authoritative state. While the user is dragging we do
//     not fight them (engine updates are ignored until the drag ends).
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
using System.Windows;
using System.Windows.Input;
using System.Windows.Media;
using System.Web.Script.Serialization;
using NinjaTrader.Cbi;
using NinjaTrader.Core;
using NinjaTrader.Data;
using NinjaTrader.Gui;
using NinjaTrader.Gui.Chart;
using NinjaTrader.Gui.Tools;
using NinjaTrader.NinjaScript;
using NinjaTrader.NinjaScript.DrawingTools;
using SharpDX;
using SharpDX.Direct2D1;
#endregion

namespace NinjaTrader.NinjaScript
{
    // Self-contained label placement enum (mirrors the same enum RiskRewardTool
    // defines, so no cross-indicator dependency).
    public enum EngLineLabelPosition
    {
        Right,
        Left,
        Middle
    }
}

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
        private const int SendDebounceMs = 250;

        // ---- Line prices (the three draggable levels) ----
        private double entryPrice;
        private double stopPrice;
        private double targetPrice;
        private bool hasEngineState;      // received SYNC_STATE at least once
        private bool engineInPosition;
        private int currentPositionSize = 1; // from SYNC_STATE position qty
        private int planPositionSize = 1;     // from SYNC_STATE calculatedQty (engine sizing while flat)

        // ---- Line colors / rendering ----
        private System.Windows.Media.Brush entryWpfColor = Brushes.DodgerBlue;
        private System.Windows.Media.Brush stopWpfColor = Brushes.Crimson;
        private System.Windows.Media.Brush targetWpfColor = Brushes.LimeGreen;
        private float lineThickness = 2f;
        private SharpDX.Direct2D1.Brush entryBrush;
        private SharpDX.Direct2D1.Brush stopBrush;
        private SharpDX.Direct2D1.Brush targetBrush;
        private SharpDX.Direct2D1.Brush targetHoverBrush;
        private SharpDX.Direct2D1.RenderTarget cachedRenderTarget;

        // ---- Drag state (RiskRewardTool pattern) ----
        private bool subscribedMouse;
        private bool chartEventsHooked;
        private ChartScale cachedScale;
        // Armed-line model: clicking a line ARMS it for dragging; moving the
        // mouse then moves it WITHOUT holding the button (RiskRewardTool style).
        // Clicking again (anywhere) releases it.
        private int armedLine; // 0=none, 1=entry, 2=stop, 3=target
        private bool isTargetDraggedIndependently; // target grabbed directly vs moved-with-entry
        private const double HitTestPixels = 30.0;
        private bool mouseCaptured;

        private string webSocketUrl = "ws://localhost:8080/ws?type=LINEHOST";

        // ---- Label rendering (RiskRewardTool style boxes on each line) ----
        private SharpDX.DirectWrite.TextFormat textFormat = null;
        private SharpDX.Direct2D1.SolidColorBrush labelBgBrush = null;
        private SharpDX.Direct2D1.SolidColorBrush textBrush = null;
        private const float LabelWidth = 240f;
        private const float LabelHeight = 38f;
        private NinjaTrader.NinjaScript.EngLineLabelPosition labelPosition = NinjaTrader.NinjaScript.EngLineLabelPosition.Right;

        [NinjaScriptProperty]
        [Display(Name = "Label Position", Order = 5, GroupName = "Lines")]
        public NinjaTrader.NinjaScript.EngLineLabelPosition LabelPosition
        {
            get { return labelPosition; }
            set { labelPosition = value; }
        }

        [NinjaScriptProperty]
        [Display(Name = "WebSocket URL", Order = 1, GroupName = "Engine Connection")]
        public string WebSocketUrl
        {
            get { return webSocketUrl; }
            set { webSocketUrl = value; }
        }

        [NinjaScriptProperty]
        [Display(Name = "Entry Line Color", Order = 2, GroupName = "Lines")]
        public System.Windows.Media.Brush EntryColor
        {
            get { return entryWpfColor; }
            set { entryWpfColor = value; }
        }

        [NinjaScriptProperty]
        [Display(Name = "Stop Line Color", Order = 3, GroupName = "Lines")]
        public System.Windows.Media.Brush StopColor
        {
            get { return stopWpfColor; }
            set { stopWpfColor = value; }
        }

        [NinjaScriptProperty]
        [Display(Name = "Target Line Color", Order = 4, GroupName = "Lines")]
        public System.Windows.Media.Brush TargetColor
        {
            get { return targetWpfColor; }
            set { targetWpfColor = value; }
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
            else if (State == State.DataLoaded)
            {
                // RiskRewardTool pattern: hook the chart-panel mouse events as
                // soon as the chart exists (UI thread). Doing this inside
                // OnRender runs on the render thread and breaks the render.
                SubscribeMouse();
            }
            else if (State == State.Realtime)
            {
                // Seed prices from the current market so lines are visible
                // immediately; SYNC_STATE will correct them.
                EnsureInitialPrices();
                StartWebSocket();
                CreateLabelFormat();
                // Re-subscribe in case DataLoaded ran before ChartControl was
                // fully attached (RiskRewardTool re-subscribes the same way).
                SubscribeMouse();
            }
            else if (State == State.Terminated)
            {
                StopWebSocket();
                UnsubscribeMouse();
                DisposeBrushes();
                DisposeLabel();
                UnsubscribeChartEvents();
            }
        }

        // Re-subscribe when the user switches charts (RiskRewardTool's
        // SubscribeChartEvents equivalent): the panel events move with the
        // chart, and stale handlers would silently stop receiving input.
        private void ChartLoadedHandler(object sender, System.Windows.RoutedEventArgs e)
        {
            SubscribeMouse();
        }

        private void SubscribeChartEvents()
        {
            try
            {
                if (chartEventsHooked || ChartControl == null) return;
                ChartControl.Loaded += ChartLoadedHandler;
                chartEventsHooked = true;
            }
            catch { }
        }

        private void UnsubscribeChartEvents()
        {
            try
            {
                if (!chartEventsHooked || ChartControl == null) return;
                ChartControl.Loaded -= ChartLoadedHandler;
                chartEventsHooked = false;
            }
            catch { }
        }

        private void CreateLabelFormat()
        {
            try
            {
                if (textFormat != null) return;
                textFormat = new SharpDX.DirectWrite.TextFormat(
                    NinjaTrader.Core.Globals.DirectWriteFactory,
                    "Consolas",
                    SharpDX.DirectWrite.FontWeight.Bold,
                    SharpDX.DirectWrite.FontStyle.Normal,
                    12f);
                textFormat.TextAlignment = SharpDX.DirectWrite.TextAlignment.Leading;
                textFormat.ParagraphAlignment = SharpDX.DirectWrite.ParagraphAlignment.Center;
            }
            catch { }
        }

        private void DisposeLabel()
        {
            try
            {
                if (labelBgBrush != null) { labelBgBrush.Dispose(); labelBgBrush = null; }
                if (textBrush != null) { textBrush.Dispose(); textBrush = null; }
                if (textFormat != null) { textFormat.Dispose(); textFormat = null; }
            }
            catch { }
        }

        protected override void OnBarUpdate()
        {
            if (State != State.Realtime) return;
            if (!hasEngineState && armedLine == 0)
            {
                // Before the engine first reports state, follow the market so
                // the lines stay visible/attached to streaming bars.
                EnsureInitialPrices();
                if (ChartControl != null) ChartControl.InvalidateVisual();
            }
        }

        // ------------------------------------------------------------------
        // Prices
        // ------------------------------------------------------------------
        private double GetCurrentPrice()
        {
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

        private void EnsureInitialPrices()
        {
            double basePrice = GetCurrentPrice();
            if (basePrice <= 0) return;
            if (entryPrice <= 0) { entryPrice = basePrice; stopPrice = basePrice - 10 * TickSize; targetPrice = basePrice + 20 * TickSize; }
        }

        private double RoundToTickSize(double price)
        {
            double tick = TickSize > 0 ? TickSize : 0.25;
            return Math.Round(price / tick) * tick;
        }

        // ------------------------------------------------------------------
        // Mouse drag handling (RiskRewardTool pattern): events are hooked on
        // ChartPanels[0]; we own the entire gesture.
        // ------------------------------------------------------------------
        private void SubscribeMouse()
        {
            if (subscribedMouse) return;
            if (ChartControl == null || ChartControl.ChartPanels == null || ChartControl.ChartPanels.Count == 0) return;
            ChartPanel panel = ChartControl.ChartPanels[0];
            panel.AddHandler(UIElement.MouseMoveEvent, new MouseEventHandler(OnChartMouseMove), true);
            panel.AddHandler(UIElement.MouseLeftButtonDownEvent, new MouseButtonEventHandler(OnChartMouseLeftButtonDown), true);
            panel.AddHandler(UIElement.MouseLeftButtonUpEvent, new MouseButtonEventHandler(OnChartMouseLeftButtonUp), true);
            subscribedMouse = true;
        }

        private void UnsubscribeMouse()
        {
            if (!subscribedMouse) return;
            if (ChartControl == null || ChartControl.ChartPanels == null || ChartControl.ChartPanels.Count == 0) return;
            ChartPanel panel = ChartControl.ChartPanels[0];
            panel.RemoveHandler(UIElement.MouseMoveEvent, new MouseEventHandler(OnChartMouseMove));
            panel.RemoveHandler(UIElement.MouseLeftButtonDownEvent, new MouseButtonEventHandler(OnChartMouseLeftButtonDown));
            panel.RemoveHandler(UIElement.MouseLeftButtonUpEvent, new MouseButtonEventHandler(OnChartMouseLeftButtonUp));
            subscribedMouse = false;
            armedLine = 0;
        }

        private void OnChartMouseLeftButtonDown(object sender, MouseButtonEventArgs e)
        {
            if (cachedScale == null || ChartControl == null) return;
            System.Windows.Point p = e.GetPosition((IInputElement)sender);

            float entryY = (float)cachedScale.GetYByValueWpf(entryPrice);
            float stopY = (float)cachedScale.GetYByValueWpf(stopPrice);
            float targetY = (float)cachedScale.GetYByValueWpf(targetPrice);

            double dEntry = Math.Abs(p.Y - entryY);
            double dStop = Math.Abs(p.Y - stopY);
            double dTarget = Math.Abs(p.Y - targetY);

            int clickedLine = 0;
            if (dEntry < HitTestPixels) clickedLine = 1;
            else if (dStop < HitTestPixels) clickedLine = 2;
            else if (dTarget < HitTestPixels) clickedLine = 3;

            if (clickedLine > 0)
            {
                // Click-to-arm: if a line is already armed, clicking it again
                // (or clicking another line) releases the old one and arms the
                // new one. A second click on the SAME line while armed releases.
                if (armedLine == clickedLine)
                {
                    armedLine = 0; // release — line is now placed
                    if (mouseCaptured) ReleaseMouseCaptureSafe(sender);
                    e.Handled = true;
                    PushPrices(); // final authoritative push on release
                    return;
                }
                armedLine = clickedLine;
                isTargetDraggedIndependently = (clickedLine == 3);
                e.Handled = true;
                // Keep mouse capture so the arm survives even while the mouse
                // leaves the panel momentarily (RiskRewardTool behavior).
                ((IInputElement)sender).CaptureMouse();
                mouseCaptured = true;
            }
            else if (armedLine > 0)
            {
                // Clicked on empty chart space: release the armed line.
                armedLine = 0;
                isTargetDraggedIndependently = false;
                if (mouseCaptured) ReleaseMouseCaptureSafe(sender);
                e.Handled = true;
                PushPrices();
            }
        }

        private void OnChartMouseMove(object sender, MouseEventArgs e)
        {
            if (armedLine == 0) return;
            if (cachedScale == null || ChartControl == null) return;

            System.Windows.Point p = e.GetPosition((IInputElement)sender);
            double newPrice = cachedScale.GetValueByYWpf(p.Y);
            newPrice = RoundToTickSize(newPrice);
            if (newPrice <= 0) return;

            if (armedLine == 1)
            {
                // Moving the entry line shifts the target by the SAME amount
                // (entry->target gap is preserved — the plan stays parallel).
                double delta = newPrice - entryPrice;
                entryPrice = newPrice;
                if (targetPrice > 0 && !isTargetDraggedIndependently)
                {
                    targetPrice = RoundToTickSize(targetPrice + delta);
                }
            }
            else if (armedLine == 2) stopPrice = newPrice;
            else if (armedLine == 3) targetPrice = newPrice;

            ChartControl.InvalidateVisual();
            PushPricesIfDebounced();
            e.Handled = true;
        }

        private void OnChartMouseLeftButtonUp(object sender, MouseButtonEventArgs e)
        {
            bool wasDragging = armedLine > 0;
            // Do NOT release the arm on button-up: the line stays armed until
            // clicked again (RiskRewardTool click-to-drag). We just drop the
            // capture so clicks elsewhere are received; armedLine persists.
            if (mouseCaptured) ReleaseMouseCaptureSafe(sender);
            mouseCaptured = false;
            if (wasDragging)
            {
                // Final authoritative push on the "click-release" gesture end.
                // (The line is still armed, so a subsequent click will release.)
                PushPrices();
                if (ChartControl != null) ChartControl.InvalidateVisual();
            }
            e.Handled = true;
        }

        private void ReleaseMouseCaptureSafe(object sender)
        {
            try
            {
                IInputElement el = sender as IInputElement;
                if (el != null) el.ReleaseMouseCapture();
            }
            catch { }
        }

        // ------------------------------------------------------------------
        // Pushing prices to the Go engine (UPDATE_PRICES).
        // ------------------------------------------------------------------
        private void PushPricesIfDebounced()
        {
            long now = DateTime.UtcNow.Ticks / TimeSpan.TicksPerMillisecond;
            if (now - lastSendUnix < SendDebounceMs) return;
            lastSendUnix = now;
            PushPrices();
        }

        private void PushPrices()
        {
            if (entryPrice <= 0 || stopPrice <= 0 || targetPrice <= 0) return;
            string payload = string.Format(
                CultureInfo.InvariantCulture,
                "{{\"entryPrice\":{0},\"stopPrice\":{1},\"targetPrice\":{2}}}",
                entryPrice.ToString("R", CultureInfo.InvariantCulture),
                stopPrice.ToString("R", CultureInfo.InvariantCulture),
                targetPrice.ToString("R", CultureInfo.InvariantCulture));
            SendWs("UPDATE_PRICES", payload);
        }

        // ------------------------------------------------------------------
        // Engine -> lines: on SYNC_STATE, reposition lines to the engine's
        // authoritative prices. Ignored mid-drag (we never fight the user).
        // ------------------------------------------------------------------
        private void ApplyEngineState(string payload)
        {
            try
            {
                var serializer = new JavaScriptSerializer();
                Dictionary<string, object> root = serializer.Deserialize<Dictionary<string, object>>(payload);
                if (root == null) return;

                Dictionary<string, object> pos = root.ContainsKey("position") ? root["position"] as Dictionary<string, object> : null;
                string mp = "Flat";
                int posQty = 1;
                if (pos != null && pos.ContainsKey("marketPosition") && pos["marketPosition"] != null)
                    mp = pos["marketPosition"].ToString();
                if (pos != null && pos.ContainsKey("quantity") && pos["quantity"] != null)
                {
                    int q;
                    if (int.TryParse(pos["quantity"].ToString(), out q)) posQty = q;
                }

                double entry = GetDoubleField(root, "entryPrice", 0);
                double stop = GetDoubleField(root, "stopPrice", 0);
                double target = GetDoubleField(root, "targetPrice", 0);

                // Engine's computed position size (plan size while flat) — keeps
                // the indicator's "N contracts" labels in sync with the engine.
                if (root.ContainsKey("calculatedQty") && root["calculatedQty"] != null)
                {
                    int cq;
                    if (int.TryParse(root["calculatedQty"].ToString(), out cq) && cq > 0)
                        planPositionSize = cq;
                }

                bool inPos = !string.Equals(mp, "Flat", StringComparison.OrdinalIgnoreCase);
                if (armedLine != 0) return; // user is moving a line

                if (inPos && entry > 0)
                {
                    engineInPosition = true;
                    currentPositionSize = posQty > 0 ? posQty : 1;
                    entryPrice = entry;
                    stopPrice = stop > 0 ? stop : entry - 10 * TickSize;
                    targetPrice = target > 0 ? target : entry + 20 * TickSize;
                    hasEngineState = true;
                }
                else if (!inPos)
                {
                    // Flat: keep the user's plan levels if present, otherwise
                    // seed near market. hasEngineState stays true once seen so
                    // bars don't yank plan lines around.
                    if (!hasEngineState)
                    {
                        double basePrice = GetCurrentPrice();
                        if (basePrice > 0) { entryPrice = entry > 0 ? entry : basePrice; stopPrice = stop > 0 ? stop : basePrice - 10 * TickSize; targetPrice = target > 0 ? target : basePrice + 20 * TickSize; }
                    }
                    else
                    {
                        if (entry > 0) entryPrice = entry;
                        if (stop > 0) stopPrice = stop;
                        if (target > 0) targetPrice = target;
                    }
                    hasEngineState = true;
                    engineInPosition = false;
                }

                if (ChartControl != null) ChartControl.InvalidateVisual();
            }
            catch { }
        }

        // ------------------------------------------------------------------
        // Rendering: three full-width horizontal lines (RiskRewardTool style).
        // ------------------------------------------------------------------
        protected override void OnRender(ChartControl chartControl, ChartScale chartScale)
        {
            base.OnRender(chartControl, chartScale);
            try
            {
                cachedScale = chartScale;
                // NOTE: mouse handlers are hooked in State.DataLoaded / Realtime
                // (UI thread) — never here, OnRender runs on the render thread and
                // touching WPF UI from it breaks rendering (the "lines vanished" bug).
                if (RenderTarget == null || chartControl == null || chartScale == null) return;
                if (entryPrice <= 0 || stopPrice <= 0 || targetPrice <= 0) return;

            double canvasLeft = chartControl.CanvasLeft;
            double canvasRight = chartControl.CanvasRight;

            float entryY = (float)chartScale.GetYByValue(entryPrice);
            float stopY = (float)chartScale.GetYByValue(stopPrice);
            float targetY = (float)chartScale.GetYByValue(targetPrice);

            if (entryBrush == null || cachedRenderTarget != RenderTarget)
            {
                if (entryBrush != null) entryBrush.Dispose();
                if (stopBrush != null) stopBrush.Dispose();
                if (targetBrush != null) targetBrush.Dispose();
                if (targetHoverBrush != null) targetHoverBrush.Dispose();
                cachedRenderTarget = RenderTarget;
                entryBrush = CreateDxBrush(entryWpfColor);
                stopBrush = CreateDxBrush(stopWpfColor);
                targetBrush = CreateDxBrush(targetWpfColor);
                targetHoverBrush = CreateDxBrush(targetWpfColor, 0.7f);
            }

            // Winner lines only when engine reports an open position; plan mode
            // always shows draggable entry/stop/target lines.
            RenderTarget.DrawLine(new Vector2((float)canvasLeft, entryY), new Vector2((float)canvasRight, entryY), entryBrush, lineThickness);
            RenderTarget.DrawLine(new Vector2((float)canvasLeft, stopY), new Vector2((float)canvasRight, stopY), stopBrush, lineThickness);
            RenderTarget.DrawLine(new Vector2((float)canvasLeft, targetY), new Vector2((float)canvasRight, targetY), targetBrush, lineThickness);

            // RiskRewardTool-style detail boxes on each line.
            EnsureLabelBrushes();
            double pointValue = Instrument.MasterInstrument.PointValue;
            double slDist = Math.Abs(entryPrice - stopPrice);
            double stopDollars = slDist * pointValue;
            double tpDist = Math.Abs(targetPrice - entryPrice);
            double tpDollars = tpDist * pointValue;

            DrawLineLabel(canvasLeft, canvasRight, entryY, entryBrush,
                string.Format(" ENTRY ({0} contracts):\n {1:F2}", PositionQty(), entryPrice)); 
            DrawLineLabel(canvasLeft, canvasRight, stopY, stopBrush,
                string.Format(" STOP ({0} contracts):\n {1:F2} (-{2:F2} pts) (-{3:C2})", PositionQty(), stopPrice, slDist, stopDollars));
            DrawLineLabel(canvasLeft, canvasRight, targetY, targetBrush,
                string.Format(" TARGET ({0} contracts):\n {1:F2} (+{2:F2} pts) (+{3:C2})", PositionQty(), targetPrice, tpDist, tpDollars));
            }
            catch
            {
                // A render exception must never remove the indicator from the
                // chart — swallow it and let the next render repaint.
            }
        }

        // PositionSizeDoc: qty shown on labels. When the engine reports an open
        // position we show its qty; otherwise the plan's presumed size (use the
        // engine's calculated qty for flat if known, else 1).
        private int PositionQty()
        {
            if (engineInPosition) return currentPositionSize;
            // Flat but engine has computed a plan size — mirror it so the
            // indicator labels match the engine's actual sizing/risk numbers.
            if (planPositionSize > 0) return planPositionSize;
            return 1;
        }

        private void EnsureLabelBrushes()
        {
            try
            {
                if (labelBgBrush == null || cachedRenderTarget != RenderTarget)
                {
                    if (labelBgBrush != null) { labelBgBrush.Dispose(); labelBgBrush = null; }
                    if (textBrush != null) { textBrush.Dispose(); textBrush = null; }
                    labelBgBrush = new SharpDX.Direct2D1.SolidColorBrush(RenderTarget, new SharpDX.Color4(0.05f, 0.06f, 0.09f, 0.85f));
                    textBrush = new SharpDX.Direct2D1.SolidColorBrush(RenderTarget, new SharpDX.Color4(0.95f, 0.96f, 0.98f, 1f));
                }
            }
            catch { }
        }

        // Filled label box (bg + colored border + multi-line text) anchored to
        // its line's Y, placed Right/Left/Middle per the LabelPosition property.
        private void DrawLineLabel(double canvasLeft, double canvasRight, float lineY,
            SharpDX.Direct2D1.Brush borderBrush, string text)
        {
            if (RenderTarget == null || textFormat == null || labelBgBrush == null || textBrush == null) return;
            try
            {
                float labelX = (float)canvasRight - LabelWidth - 10f; // Right
                if (labelPosition == NinjaTrader.NinjaScript.EngLineLabelPosition.Left)
                    labelX = (float)canvasLeft + 10f;
                else if (labelPosition == NinjaTrader.NinjaScript.EngLineLabelPosition.Middle)
                    labelX = (float)canvasLeft + (float)(canvasRight - canvasLeft) / 2f - LabelWidth / 2f;

                float labelY = lineY - LabelHeight / 2f;
                SharpDX.RectangleF box = new SharpDX.RectangleF(labelX, labelY, LabelWidth, LabelHeight);
                RenderTarget.FillRectangle(box, labelBgBrush);
                RenderTarget.DrawRectangle(box, borderBrush, 1.5f);
                RenderTarget.DrawText(text, textFormat, new SharpDX.RectangleF(labelX + 5f, labelY, LabelWidth - 10f, LabelHeight), textBrush);
            }
            catch { }
        }

        private SharpDX.Direct2D1.Brush CreateDxBrush(System.Windows.Media.Brush wpf)
        {
            try
            {
                System.Windows.Media.SolidColorBrush scb = wpf as System.Windows.Media.SolidColorBrush;
                if (scb != null)
                {
                    System.Windows.Media.Color cc = scb.Color;
                    return new SharpDX.Direct2D1.SolidColorBrush(RenderTarget, new SharpDX.Color4(cc.R / 255f, cc.G / 255f, cc.B / 255f, cc.A / 255f));
                }
            }
            catch { }
            return new SharpDX.Direct2D1.SolidColorBrush(RenderTarget, new SharpDX.Color4(0.3f, 0.4f, 1.0f, 1.0f));
        }

        private SharpDX.Direct2D1.Brush CreateDxBrush(System.Windows.Media.Brush wpf, float alpha)
        {
            try
            {
                System.Windows.Media.SolidColorBrush scb = wpf as System.Windows.Media.SolidColorBrush;
                if (scb != null)
                {
                    System.Windows.Media.Color cc = scb.Color;
                    return new SharpDX.Direct2D1.SolidColorBrush(RenderTarget, new SharpDX.Color4(cc.R / 255f, cc.G / 255f, cc.B / 255f, Math.Min(1f, alpha)));
                }
            }
            catch { }
            return new SharpDX.Direct2D1.SolidColorBrush(RenderTarget, new SharpDX.Color4(0.3f, 0.4f, 1.0f, 1.0f));
        }

        private void DisposeBrushes()
        {
            try
            {
                if (entryBrush != null) { entryBrush.Dispose(); entryBrush = null; }
                if (stopBrush != null) { stopBrush.Dispose(); stopBrush = null; }
                if (targetBrush != null) { targetBrush.Dispose(); targetBrush = null; }
                if (targetHoverBrush != null) { targetHoverBrush.Dispose(); targetHoverBrush = null; }
            }
            catch { }
            cachedRenderTarget = null;
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

        // ---- JSON helpers (same style as the gateway) ----
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