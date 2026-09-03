#region Using declarations
using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Net.WebSockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using NinjaTrader.Cbi;
using NinjaTrader.Core;
using NinjaTrader.Data;
using NinjaTrader.NinjaScript;
using System.Web.Script.Serialization;
#endregion

namespace NinjaTrader.NinjaScript.Strategies
{
    /// <summary>
    /// 100% Dumb Remote Adapter for Go Trade Engine.
    /// Performs zero calculation, zero bracket logic, and zero state management.
    /// Strictly translates RPC commands (Submit, Change, Cancel, Flatten) and streams market/account data.
    /// 
    /// =========================================================================
    /// FROZEN GATEWAY - DO NOT MODIFY. ALL BUSINESS LOGIC LIVES IN GO.
    /// =========================================================================
    /// </summary>
    public class TradeEngineStrategy : Strategy
    {
        #region State
        private ClientWebSocket wsClient;
        private CancellationTokenSource cts;
        private CancellationTokenSource connCts;
        private readonly ConcurrentQueue<string> outboundQueue = new ConcurrentQueue<string>();
        private readonly SemaphoreSlim queueSignal = new SemaphoreSlim(0);
        private bool isWsConnected = false;
        private bool hasSentMarketInfo = false;
        private readonly Dictionary<string, DateTime> processedExecutions = new Dictionary<string, DateTime>();
        private readonly Dictionary<string, DateTime> processedRequests = new Dictionary<string, DateTime>();

        // Multi-series bar streaming: the strategy streams the web terminal's
        // selectable timeframes (15s/1m/5m/100t) as secondary data series, each
        // BAR_UPDATE/SYNC_BARS tagged with its timeframe label. Index 0 is the
        // primary (chart) series with label "" unless it matches a web
        // timeframe exactly. NinjaTrader cannot add data series at runtime, so
        // all secondary series are declared up front in State.Configure.
        private readonly List<string> seriesLabels = new List<string>();
        // Index-aligned with seriesLabels; describes each series as
        // "label=TypeValue" (e.g. "100t=Volume100", "15s=Second15",
        // "primary=Second30"). Reported in UPDATE_PRICES as "barSeries" so the
        // Go hub log instantly shows WHICH build/configuration the connected
        // strategy actually runs.
        private readonly List<string> seriesDesc = new List<string>();
        // Index-aligned with seriesLabels; the bars period type of each series
        // (used to decide per-update vs on-close streaming in OnBarUpdate).
        private readonly List<BarsPeriodType> seriesTypes = new List<BarsPeriodType>();
        private readonly HashSet<int> syncedSeries = new HashSet<int>();
        #endregion

        private string webSocketUrl = "ws://localhost:8080/ws?type=NT8";

        // Live market data cache (streamed to Go engine for hotkeys + mark-to-market)
        private double lastBid = 0;
        private double lastAsk = 0;
        private double lastTrade = 0;

        #region Properties
        [NinjaScriptProperty]
        public string WebSocketUrl
        {
            get { return webSocketUrl; }
            set { webSocketUrl = value; }
        }
        #endregion

        protected override void OnStateChange()
        {
            if (State == State.SetDefaults)
            {
                Description = "Dumb Remote Adapter bridge for Go Trade Engine.";
                Name = "TradeEngineStrategy";
                Calculate = Calculate.OnPriceChange;
                EntriesPerDirection = 1;
                EntryHandling = EntryHandling.AllEntries;
                IsExitOnSessionCloseStrategy = false;
                IsFillLimitOnTouch = true;
            }
            else if (State == State.Configure)
            {
                // Declare the four secondary bar series the web terminal can
                // select per pane. A series identical to the primary chart
                // series is skipped — the primary stream already covers it.
                ConfigureSecondarySeries();
            }
            else if (State == State.Realtime || State == State.Historical)
            {
                foreach (var a in Account.All)
                {
                    if (a != null)
                    {
                        a.PositionUpdate -= OnAccountPositionUpdate;
                        a.ExecutionUpdate -= OnAccountExecutionUpdate;
                        a.OrderUpdate -= OnAccountOrderUpdate;
                        a.PositionUpdate += OnAccountPositionUpdate;
                        a.ExecutionUpdate += OnAccountExecutionUpdate;
                        a.OrderUpdate += OnAccountOrderUpdate;
                    }
                }
                StartWebSocket();
            }
            else if (State == State.Terminated)
            {
                foreach (var a in Account.All)
                {
                    if (a != null)
                    {
                        a.PositionUpdate -= OnAccountPositionUpdate;
                        a.ExecutionUpdate -= OnAccountExecutionUpdate;
                        a.OrderUpdate -= OnAccountOrderUpdate;
                    }
                }
                StopWebSocket();
            }
        }

        protected override void OnBarUpdate()
        {
            if (!isWsConnected) return;
            if (BarsInProgress < 0 || BarsInProgress >= BarsArray.Length) return;
            if (CurrentBar < 1) return;

            EnsureSeriesTrackers();
            int si = BarsInProgress;

            // Tag every bar with its series' timeframe label so the Go hub can
            // route it to the correct web chart pane. Empty = primary chart
            // series (legacy behaviour → engine tracking stream).
            string label = (si < seriesLabels.Count) ? seriesLabels[si] : "";

            bool timeBased = (si < seriesTypes.Count) ? IsTimeBased(seriesTypes[si]) : true;
            if (timeBased)
            {
                // Time-based bars (15s/1m/5m/...): Time[0] is stable at the bar
                // boundary, so stream every update — the web renders the
                // forming candle in place.
                long unixSec = new DateTimeOffset(Time[0].ToUniversalTime()).ToUnixTimeSeconds();
                SendWs("BAR_UPDATE", BarJson(unixSec, Open[0], High[0], Low[0], Close[0], (long)Volume[0], label));
            }
            else
            {
                // Tick/volume/range bars: NEVER reconstruct bars from per-event
                // OHLC/volume. NT8 fast-replay batches many ticks (sometimes
                // several whole bars) into a single event, so heuristics kept
                // emitting phantom or repeated "closed" bars at arbitrary
                // volumes — the candle appears/disappears/duplicates symptom.
                // Instead, read each ACTUALLY closed bar straight from the
                // series' own bar collection — the exact bars NT8's chart draws
                // (same GetTime/GetOpen/... as SendHistoricalBars) — and emit
                // each once per close. This makes live == SYNC == the NT8 chart.
                Bars bars = BarsArray[si];
                int count = bars.Count;
                if (count < 1) return;
                int formingIdx = count - 1;

                if (lastBarCount[si] > 0 && count > lastBarCount[si])
                {
                    // Newly closed bars since the last event: indices
                    // [lastBarCount-1 .. count-2] (count-1 is the forming bar).
                    int from = lastBarCount[si] - 1;
                    int to = Math.Max(from, count - 2);
                    for (int idx = from; idx <= to; idx++)
                    {
                        long t = new DateTimeOffset(bars.GetTime(idx).ToUniversalTime()).ToUnixTimeSeconds();
                        SendWs("BAR_UPDATE", BarJson(
                            t, bars.GetOpen(idx), bars.GetHigh(idx), bars.GetLow(idx), bars.GetClose(idx),
                            (long)bars.GetVolume(idx), label, idx));
                    }
                }
                if (lastBarCount[si] == 0)
                {
                    // First event: baseline without re-emitting history (the
                    // one-time SYNC covers historical bars).
                    lastBarCount[si] = count;
                }
                else if (count > lastBarCount[si])
                {
                    lastBarCount[si] = count;
                }

                // ALSO stream the currently FORMING candle on every event, with
                // its series barIndex. Consumers upsert by barIndex, so this
                // REPLACES the forming candle in place — the web's 100t pane
                // builds tick-by-tick exactly like NT8's own chart — while the
                // closed bars above stay frozen (distinct indices are distinct
                // candles). Without barIndex identity this re-emission looked
                // like a new same-time candle on every tick (the old "37
                // candles in one second" phantom); with it, it's one live candle.
                long formingT = new DateTimeOffset(bars.GetTime(formingIdx).ToUniversalTime()).ToUnixTimeSeconds();
                SendWs("BAR_UPDATE", BarJson(
                    formingT, bars.GetOpen(formingIdx), bars.GetHigh(formingIdx), bars.GetLow(formingIdx), bars.GetClose(formingIdx),
                    (long)bars.GetVolume(formingIdx), label, formingIdx));
            }

            // One historical sync per series (primary + each secondary timeframe).
            if (!syncedSeries.Contains(si))
            {
                syncedSeries.Add(si);
                SendHistoricalBars(500, si);
            }

            if (!hasSentMarketInfo)
            {
                hasSentMarketInfo = true;
                SendMarketInfo();
            }
        }

        private static string BarJson(long t, double o, double h, double l, double c, long v, string label, int barIndex = -1)
        {
            string indexField = barIndex >= 0 ? string.Format(",\"barIndex\":{0}", barIndex) : "";
            return string.Format(
                System.Globalization.CultureInfo.InvariantCulture,
                "{{\"time\":{0},\"open\":{1},\"high\":{2},\"low\":{3},\"close\":{4},\"volume\":{5},\"timeframe\":\"{6}\"{7}}}",
                t, o, h, l, c, v, label, indexField
            );
        }

        // Bars whose Time[0] is stable at the bar boundary stream every update;
        // bars whose Time[0] advances intra-bar (tick/volume/range/etc.) are
        // emitted only on close (see OnBarUpdate).
        private static bool IsTimeBased(BarsPeriodType t)
        {
            return t == BarsPeriodType.Second || t == BarsPeriodType.Minute
                || t == BarsPeriodType.Day || t == BarsPeriodType.Week || t == BarsPeriodType.Month
                || t == BarsPeriodType.Year;
        }

        // Per-series state for closed-bar emission (non-time-based series).
        private int[] lastBarCount = null;

        private void EnsureSeriesTrackers()
        {
            if (lastBarCount == null || lastBarCount.Length < BarsArray.Length)
            {
                lastBarCount = new int[BarsArray.Length];
            }
        }

        protected override void OnMarketData(MarketDataEventArgs marketDataUpdate)
        {
            if (marketDataUpdate == null) return;

            if (marketDataUpdate.MarketDataType == MarketDataType.Ask)
                lastAsk = marketDataUpdate.Price;
            else if (marketDataUpdate.MarketDataType == MarketDataType.Bid)
                lastBid = marketDataUpdate.Price;
            else if (marketDataUpdate.MarketDataType == MarketDataType.Last)
                lastTrade = marketDataUpdate.Price;
            else
                return;

            if (!isWsConnected) return;

            // NO outbound throttle: stream every market-data event to the Go
            // hub exactly as NinjaTrader delivers it, so the engine's market
            // reference (bid/ask/last) is always as fresh as the feed. The
            // hub ingests every tick and passes the small price message to the
            // web at full rate; only the expensive full-state re-broadcast is
            // spaced out there (BroadcastStateThrottled) to keep the UI smooth.
            string payload = string.Format(
                System.Globalization.CultureInfo.InvariantCulture,
                "{{\"bid\":{0},\"ask\":{1},\"last\":{2}}}",
                lastBid, lastAsk, lastTrade
            );
            SendWs("MARKET_DATA", payload);
        }
 
        #region Account Helper & Event Handlers
        private Account GetBoundAccount(string explicitName = null)
        {
            if (!string.IsNullOrEmpty(explicitName))
            {
                Account explicitAcc = Account.All.FirstOrDefault(a => a != null && a.Name.Equals(explicitName, StringComparison.OrdinalIgnoreCase));
                if (explicitAcc != null) return explicitAcc;
            }
            return Account ?? Account.All.FirstOrDefault();
        }

        private void OnAccountPositionUpdate(object sender, PositionEventArgs e)
        {
            if (e == null || e.Position == null) return;
            Account bound = GetBoundAccount();
            if (e.Position.Account != null && bound != null && !e.Position.Account.Name.Equals(bound.Name, StringComparison.OrdinalIgnoreCase)) return;
            if (Instrument != null && e.Position.Instrument != null && e.Position.Instrument.FullName != Instrument.FullName) return;
            SendPositionUpdate();
        }

        private void OnAccountOrderUpdate(object sender, OrderEventArgs e)
        {
            if (e == null || e.Order == null) return;
            Account bound = GetBoundAccount();
            if (e.Order.Account != null && bound != null && !e.Order.Account.Name.Equals(bound.Name, StringComparison.OrdinalIgnoreCase)) return;
            if (Instrument != null && e.Order.Instrument != null && e.Order.Instrument.FullName != Instrument.FullName) return;

            // Relay NT8 order rejections to the Go hub (as a LOG_MESSAGE) so the
            // engine log names the exact order NT8 refused — pure event
            // forwarding like ORDERS_UPDATE, no decision-making here.
            if (e.Order.OrderState == OrderState.Rejected)
            {
                try
                {
                    string oid = !string.IsNullOrEmpty(e.Order.OrderId) ? e.Order.OrderId : e.Order.Id.ToString();
                    string text = string.Format(
                        System.Globalization.CultureInfo.InvariantCulture,
                        "NT8 REJECTED order {0} ({1} {2} {3} stop={4} lmt={5})",
                        oid, e.Order.Name ?? "", e.Order.OrderAction, e.Order.OrderType,
                        e.Order.StopPrice, e.Order.LimitPrice
                    );
                    string payload = string.Format(
                        System.Globalization.CultureInfo.InvariantCulture,
                        "{{\"text\":\"{0}\"}}", text.Replace("\"", "'")
                    );
                    SendWs("LOG_MESSAGE", payload);
                }
                catch (Exception ex)
                {
                    Print("TradeEngine RejectionRelay Error: " + ex.Message);
                }
            }
            SendOrdersUpdate();
        }

        private void OnAccountExecutionUpdate(object sender, ExecutionEventArgs e)
        {
            try
            {
                if (e.Execution == null || e.Execution.Order == null) return;
                Account bound = GetBoundAccount();
                if (e.Execution.Order.Account != null && bound != null && !e.Execution.Order.Account.Name.Equals(bound.Name, StringComparison.OrdinalIgnoreCase)) return;
                if (Instrument != null && e.Execution.Order.Instrument != null && e.Execution.Order.Instrument.FullName != Instrument.FullName) return;

                if (processedExecutions.ContainsKey(e.Execution.ExecutionId)) return;
                processedExecutions[e.Execution.ExecutionId] = DateTime.UtcNow;

                // Periodic trimming to prevent unbounded memory growth over multi-week sessions
                if (processedExecutions.Count > 1000)
                {
                    DateTime cutoff = DateTime.UtcNow.AddHours(-1);
                    var stale = processedExecutions.Where(kvp => kvp.Value < cutoff).Select(kvp => kvp.Key).ToList();
                    foreach (var k in stale) processedExecutions.Remove(k);
                }

                var o = e.Execution.Order;
                string accName = o.Account != null ? o.Account.Name : (bound != null ? bound.Name : "");
                string orderId = !string.IsNullOrEmpty(o.OrderId) ? o.OrderId : o.Id.ToString();
                string orderName = o.Name ?? "";
                string actionStr = o.OrderAction.ToString();
                string stateStr = o.OrderState.ToString();

                string payload = string.Format(
                    System.Globalization.CultureInfo.InvariantCulture,
                    "{{\"executionId\":\"{0}\",\"orderId\":\"{1}\",\"name\":\"{2}\",\"action\":\"{3}\",\"fillPrice\":{4},\"fillQty\":{5},\"orderState\":\"{6}\",\"accountName\":\"{7}\",\"instrumentName\":\"{8}\"}}",
                    e.Execution.ExecutionId,
                    orderId,
                    orderName,
                    actionStr,
                    e.Execution.Price,
                    e.Execution.Quantity,
                    stateStr,
                    accName,
                    Instrument != null ? Instrument.FullName : ""
                );

                SendWs("EXECUTION_UPDATE", payload);
                SendPositionUpdate(accName);
                SendOrdersUpdate(accName);
            }
            catch (Exception ex)
            {
                Print("TradeEngine ExecutionUpdate Error: " + ex.Message);
            }
        }
        #endregion

        #region Outbound Streaming Data
        private void SendAck(string command, string orderId, bool success, string message)
        {
            try
            {
                string payload = string.Format(
                    System.Globalization.CultureInfo.InvariantCulture,
                    "{{\"command\":\"{0}\",\"orderId\":\"{1}\",\"success\":{2},\"message\":\"{3}\"}}",
                    command ?? "", (orderId ?? "").Replace("\"", "'"), success ? "true" : "false", (message ?? "").Replace("\"", "'")
                );
                SendWs("COMMAND_ACK", payload);
            }
            catch (Exception ex)
            {
                Print("TradeEngine SendAck Error: " + ex.Message);
            }
        }

        private void SendMarketInfo()
        {
            try
            {
                if (Instrument == null) return;
                double tick = TickSize > 0 ? TickSize : 0.25;
                double pointVal = (Instrument.MasterInstrument != null) ? Instrument.MasterInstrument.PointValue : 20.0;
                string inst = (Instrument.MasterInstrument != null) ? Instrument.MasterInstrument.Name : "NQ";
                Account acc = Account ?? Account.All.FirstOrDefault();
                string accName = (acc != null) ? acc.Name : "Playback101";
                double balance = 0.0;
                bool isEstimated = true;

                if (acc != null)
                {
                    try
                    {
                        balance = acc.Get(AccountItem.CashValue, Currency.UsDollar);
                        if (balance > 0) isEstimated = false;
                    }
                    catch (Exception ex)
                    {
                        Print(string.Format("TradeEngine: Could not read CashValue for {0}: {1}", acc.Name, ex.Message));
                    }

                    if (isEstimated)
                    {
                        try
                        {
                            balance = acc.Get(AccountItem.NetLiquidation, Currency.UsDollar);
                            if (balance > 0) isEstimated = false;
                        }
                        catch { }
                    }

                    if (isEstimated)
                    {
                        try
                        {
                            balance = acc.Get(AccountItem.BuyingPower, Currency.UsDollar);
                            if (balance > 0) isEstimated = false;
                        }
                        catch { }
                    }

                    if (isEstimated)
                    {
                        Print(string.Format("TradeEngine Warning: Real balance unavailable for {0}; reporting fallback 0 (isBalanceEstimated=true)", acc.Name));
                    }
                }
                else
                {
                    Print("TradeEngine Warning: No account associated; reporting balance 0 (isBalanceEstimated=true)");
                }

                List<string> accList = new List<string>();
                try
                {
                    foreach (var a in Account.All)
                    {
                        if (a != null && !string.IsNullOrEmpty(a.Name)) accList.Add(string.Format("\"{0}\"", a.Name));
                    }
                }
                catch (Exception ex)
                {
                    Print("TradeEngine AvailableAccounts Error: " + ex.Message);
                }
                string accsJson = "[" + string.Join(",", accList) + "]";

                double curPrice = (CurrentBar >= 0) ? Close[0] : 0;
                string barSeries = string.Join(";", seriesDesc);
                string payload = string.Format(
                    System.Globalization.CultureInfo.InvariantCulture,
                    "{{\"instrumentName\":\"{0}\",\"accountName\":\"{1}\",\"availableAccounts\":{2},\"tickSize\":{3},\"pointValue\":{4},\"accountBalance\":{5},\"isBalanceEstimated\":{6},\"currentMarketPrice\":{7},\"barSeries\":\"{8}\"}}",
                    inst, accName, accsJson, tick, pointVal, balance, isEstimated ? "true" : "false", curPrice, barSeries
                );
                SendWs("UPDATE_PRICES", payload);
            }
            catch (Exception ex)
            {
                Print("TradeEngine SendMarketInfo Error: " + ex.Message);
            }
        }

        private void SendPositionUpdate(string targetAcc = null)
        {
            try
            {
                if (Instrument == null) return;
                Account acc = GetBoundAccount(targetAcc);
                if (acc == null) return;

                string mktPos = "Flat";
                int qty = 0;
                double avgPrice = 0;
                double unPnL = 0;
                double unPoints = 0;

                Position pos = acc.Positions.FirstOrDefault(p => p != null && p.Instrument != null && 
                    (p.Instrument.FullName == Instrument.FullName || (Instrument.MasterInstrument != null && p.Instrument.MasterInstrument != null && p.Instrument.MasterInstrument.Name == Instrument.MasterInstrument.Name)) && 
                    p.MarketPosition != MarketPosition.Flat && p.Quantity > 0);

                if (pos != null)
                {
                    mktPos = pos.MarketPosition.ToString();
                    qty = pos.Quantity;
                    avgPrice = pos.AveragePrice;
                    try
                    {
                        unPnL = pos.GetUnrealizedProfitLoss(PerformanceUnit.Currency);
                        unPoints = pos.GetUnrealizedProfitLoss(PerformanceUnit.Points);
                    }
                    catch { }
                }

                string payload = string.Format(
                    System.Globalization.CultureInfo.InvariantCulture,
                    "{{\"marketPosition\":\"{0}\",\"quantity\":{1},\"averagePrice\":{2},\"unrealizedPnL\":{3},\"unrealizedPoints\":{4},\"accountName\":\"{5}\",\"instrumentName\":\"{6}\"}}",
                    mktPos, qty, avgPrice, unPnL, unPoints, acc.Name, Instrument.FullName
                );
                SendWs("POSITION_UPDATE", payload);
            }
            catch (Exception ex)
            {
                Print("TradeEngine SendPositionUpdate Error: " + ex.Message);
            }
        }

        private void SendHistoricalBars(int count, int seriesIdx)
        {
            try
            {
                if (BarsArray == null || seriesIdx < 0 || seriesIdx >= BarsArray.Length) return;
                Bars bars = BarsArray[seriesIdx];
                if (bars == null || bars.Count < 1) return;

                string label = (seriesIdx < seriesLabels.Count) ? seriesLabels[seriesIdx] : "";
                int lastIdx = bars.Count - 1;
                int start = Math.Max(0, lastIdx - count + 1);
                StringBuilder sb = new StringBuilder("[");
                bool first = true;
                for (int i = start; i <= lastIdx; i++)
                {
                    if (!first) sb.Append(",");
                    first = false;
                    long t = new DateTimeOffset(bars.GetTime(i).ToUniversalTime()).ToUnixTimeSeconds();
                    sb.AppendFormat(
                        System.Globalization.CultureInfo.InvariantCulture,
                        "{{\"time\":{0},\"open\":{1},\"high\":{2},\"low\":{3},\"close\":{4},\"volume\":{5},\"timeframe\":\"{6}\",\"barIndex\":{7}}}",
                        t, bars.GetOpen(i), bars.GetHigh(i), bars.GetLow(i), bars.GetClose(i), (long)bars.GetVolume(i), label, i
                    );
                }
                sb.Append("]");
                SendWs("SYNC_BARS", sb.ToString());
            }
            catch (Exception ex)
            {
                Print("TradeEngine SendHistoricalBars Error: " + ex.Message);
            }
        }

        private void SendOrdersUpdate(string targetAcc = null)
        {
            try
            {
                Account acc = GetBoundAccount(targetAcc);
                if (acc == null || Instrument == null) return;

                StringBuilder sb = new StringBuilder("[");
                bool first = true;

                var workingOrders = acc.Orders.Where(o => o != null && o.Instrument != null && 
                    o.Instrument.FullName == Instrument.FullName && 
                    (o.OrderState == OrderState.Working || o.OrderState == OrderState.Accepted)).ToList();

                foreach (var o in workingOrders)
                {
                    if (!first) sb.Append(",");
                    first = false;
                    double p = (o.OrderType == OrderType.Limit || o.OrderType == OrderType.StopLimit) ? o.LimitPrice : o.StopPrice;
                    if (p <= 0) p = o.StopPrice > 0 ? o.StopPrice : o.LimitPrice;
                    sb.AppendFormat(
                        System.Globalization.CultureInfo.InvariantCulture,
                        "{{\"orderId\":\"{0}\",\"name\":\"{1}\",\"action\":\"{2}\",\"orderType\":\"{3}\",\"price\":{4},\"qty\":{5},\"state\":\"{6}\",\"accountName\":\"{7}\",\"instrumentName\":\"{8}\"}}",
                        o.OrderId ?? o.Id.ToString(), o.Name ?? "", o.OrderAction.ToString(), o.OrderType.ToString(), p, o.Quantity, o.OrderState.ToString(), acc.Name, Instrument.FullName
                    );
                }
                sb.Append("]");
                SendWs("ORDERS_UPDATE", sb.ToString());
            }
            catch (Exception ex)
            {
                Print("TradeEngine SendOrdersUpdate Error: " + ex.Message);
            }
        }
        #endregion

        #region Execution & RPC Order Commands
        private void SubmitOrders(object token)
        {
            try
            {
                List<Dictionary<string, object>> orderTokens = new List<Dictionary<string, object>>();
                var obj = token as Dictionary<string, object>;
                if (obj != null)
                {
                    if (obj.ContainsKey("orders") && obj["orders"] is System.Collections.IEnumerable && !(obj["orders"] is string))
                    {
                        foreach (var item in (System.Collections.IEnumerable)obj["orders"])
                        {
                            var o = item as Dictionary<string, object>;
                            if (o != null) orderTokens.Add(o);
                        }
                    }
                    else
                    {
                        orderTokens.Add(obj);
                    }
                }
                else if (token is System.Collections.IEnumerable && !(token is string))
                {
                    foreach (var item in (System.Collections.IEnumerable)token)
                    {
                        var o = item as Dictionary<string, object>;
                        if (o != null) orderTokens.Add(o);
                    }
                }

                if (orderTokens.Count == 0)
                {
                    Print("TradeEngine: SUBMIT_ORDER skipped - no valid order objects in payload");
                    SendAck("SUBMIT_ORDER", "", false, "No valid order objects in payload");
                    return;
                }

                List<Order> ordersToSubmit = new List<Order>();
                Account submitAcc = null;

                foreach (var oJson in orderTokens)
                {
                    string targetAcc = GetField(oJson, "accountName", "");
                    Account acc = (!string.IsNullOrEmpty(targetAcc)) 
                        ? Account.All.FirstOrDefault(a => a.Name.Equals(targetAcc, StringComparison.OrdinalIgnoreCase)) 
                        : (Account ?? Account.All.FirstOrDefault());
                    if (acc == null || Instrument == null)
                    {
                        Print(string.Format("TradeEngine: SUBMIT_ORDER skipped - account '{0}' or Instrument not found", targetAcc));
                        SendAck("SUBMIT_ORDER", "", false, "Account or Instrument not found");
                        continue;
                    }
                    submitAcc = acc;

                    string actionStr = GetField(oJson, "action", "").Trim().ToUpperInvariant();
                    OrderAction action;
                    if (actionStr == "BUY")
                    {
                        action = OrderAction.Buy;
                    }
                    else if (actionStr == "SELL")
                    {
                        action = OrderAction.Sell;
                    }
                    else if (actionStr == "SELL_SHORT" || actionStr == "SELLSHORT")
                    {
                        action = OrderAction.SellShort;
                    }
                    else if (actionStr == "BUY_TO_COVER" || actionStr == "BUYTOCOVER")
                    {
                        action = OrderAction.BuyToCover;
                    }
                    else
                    {
                        string err = string.Format("TradeEngine: REJECTED order - invalid/unrecognized action '{0}'", actionStr);
                        Print(err);
                        SendWs("LOG_MESSAGE", string.Format("{{\"text\":\"{0}\"}}", err.Replace("\"", "'")));
                        SendAck("SUBMIT_ORDER", "", false, "Invalid action: " + actionStr);
                        continue;
                    }

                    string orderTypeStr = GetField(oJson, "orderType", "Limit");
                    int qty = Math.Max(1, GetIntField(oJson, "qty", 1));
                    double limitPrice = GetDoubleField(oJson, "limitPrice", 0);
                    double stopPrice = GetDoubleField(oJson, "stopPrice", 0);
                    string ocoId = GetField(oJson, "ocoId", "");
                    string signalName = GetField(oJson, "name", GetField(oJson, "signalName", "GoOrder"));

                    OrderType orderType = OrderType.Limit;
                    if (orderTypeStr.Equals("Market", StringComparison.OrdinalIgnoreCase))
                    {
                        orderType = OrderType.Market;
                        limitPrice = 0;
                        stopPrice = 0;
                    }
                    else if (orderTypeStr.Equals("StopMarket", StringComparison.OrdinalIgnoreCase))
                    {
                        orderType = OrderType.StopMarket;
                        limitPrice = 0;
                    }
                    else if (orderTypeStr.Equals("StopLimit", StringComparison.OrdinalIgnoreCase))
                    {
                        orderType = OrderType.StopLimit;
                        if (stopPrice <= 0) stopPrice = limitPrice;
                        if (limitPrice <= 0) limitPrice = stopPrice;
                    }
                    else
                    {
                        orderType = OrderType.Limit;
                        stopPrice = 0;
                    }

                    Order o = acc.CreateOrder(Instrument, action, orderType, OrderEntry.Automated, TimeInForce.Gtc, qty, limitPrice, stopPrice, ocoId, signalName, DateTime.MaxValue, null);
                    ordersToSubmit.Add(o);
                    Print(string.Format("TradeEngine: Created order {0} {1} {2} (Limit={3}, Stop={4}, Oco={5}, Name={6})", 
                        action, qty, orderType, limitPrice, stopPrice, ocoId, signalName));
                }

                if (submitAcc != null && ordersToSubmit.Count > 0)
                {
                    submitAcc.Submit(ordersToSubmit);
                    Print(string.Format("TradeEngine: Submitted {0} order(s) to account {1}", ordersToSubmit.Count, submitAcc.Name));
                    string firstSignal = ordersToSubmit[0].Name ?? "";
                    SendAck("SUBMIT_ORDER", firstSignal, true, string.Format("Submitted {0} order(s) to account {1}", ordersToSubmit.Count, submitAcc.Name));
                    SendOrdersUpdate();
                }
            }
            catch (Exception ex)
            {
                Print("TradeEngine SubmitOrders Error: " + ex.Message);
                SendAck("SUBMIT_ORDER", "", false, "Exception: " + ex.Message);
            }
        }

        private void CancelAllOrders(string targetAcc = null)
        {
            try
            {
                Account acc = GetBoundAccount(targetAcc);
                if (acc == null || Instrument == null) return;

                var active = acc.Orders.Where(o => o != null && o.Instrument != null && 
                    o.Instrument.FullName == Instrument.FullName && 
                    (o.OrderState == OrderState.Working || o.OrderState == OrderState.Accepted)).ToList();
                int totalCancelled = active.Count;
                if (active.Count > 0)
                {
                    acc.Cancel(active);
                    Print(string.Format("TradeEngine: Cancelled {0} orders on {1} for {2}", active.Count, acc.Name, Instrument.FullName));
                }
                SendAck("CANCEL_ALL", "", true, string.Format("Cancelled {0} order(s) on {1}", totalCancelled, acc.Name));
                SendOrdersUpdate(acc.Name);
            }
            catch (Exception ex)
            {
                Print("TradeEngine CancelAllOrders Error: " + ex.Message);
                SendAck("CANCEL_ALL", "", false, "Exception: " + ex.Message);
            }
        }

        private void CancelSpecificOrder(string orderId, string targetAcc = null)
        {
            try
            {
                if (string.IsNullOrEmpty(orderId))
                {
                    SendAck("CANCEL_ORDER", "", false, "orderId is empty");
                    return;
                }
                Account acc = GetBoundAccount(targetAcc);
                if (acc == null || Instrument == null) return;

                var order = acc.Orders.FirstOrDefault(o => o != null && o.Instrument != null && o.Instrument.FullName == Instrument.FullName &&
                    (o.OrderId == orderId || o.Id.ToString() == orderId) && 
                    (o.OrderState == OrderState.Working || o.OrderState == OrderState.Accepted));

                if (order != null)
                {
                    acc.Cancel(new[] { order });
                    Print(string.Format("TradeEngine: Cancelled order {0} on {1}", orderId, acc.Name));
                    SendAck("CANCEL_ORDER", orderId, true, "Cancelled order on " + acc.Name);
                }
                else
                {
                    Print(string.Format("TradeEngine: Order {0} not found on {1} in working/accepted state to cancel", orderId, acc.Name));
                    SendAck("CANCEL_ORDER", orderId, false, "Order not found in working/accepted state on " + acc.Name);
                }

                SendOrdersUpdate(acc.Name);
            }
            catch (Exception ex)
            {
                Print("TradeEngine CancelSpecificOrder Error: " + ex.Message);
                SendAck("CANCEL_ORDER", orderId, false, "Exception: " + ex.Message);
            }
        }

        private void ChangeSpecificOrder(object token)
        {
            try
            {
                string orderId = GetField(token, "orderId", "");
                string targetAcc = GetField(token, "accountName", "");
                double newPrice = GetDoubleField(token, "price", 0);
                int newQty = GetIntField(token, "qty", 0);

                if (string.IsNullOrEmpty(orderId) || (newPrice <= 0 && newQty <= 0))
                {
                    SendAck("CHANGE_ORDER", orderId, false, "Invalid orderId or no price/qty to change");
                    return;
                }

                Account acc = GetBoundAccount(targetAcc);
                if (acc == null || Instrument == null) return;

                var order = acc.Orders.FirstOrDefault(o => o != null && o.Instrument != null && o.Instrument.FullName == Instrument.FullName &&
                    (o.OrderId == orderId || o.Id.ToString() == orderId) && 
                    (o.OrderState == OrderState.Working || o.OrderState == OrderState.Accepted));

                if (order != null)
                {
                    // ---- Stop-price safety guard ----
                    // NinjaTrader rejects changing a stop order whose resulting stop
                    // price is "priced through" the current market (a sell stop at or
                    // above the market, or a buy stop at or below the market). This
                    // happens when price has traded through the stop while it was
                    // working. Skip ONLY actual PRICE changes in that state — a
                    // qty-only edit must always go through.
                    bool isStopOrder = (order.OrderType == OrderType.StopMarket || order.OrderType == OrderType.StopLimit);
                    bool isSellSide = (order.OrderAction == OrderAction.Sell || order.OrderAction == OrderAction.SellShort);
                    bool priceDiffers = newPrice > 0 && order.StopPrice > 0 && Math.Abs(newPrice - order.StopPrice) >= 0.005;
                    if (isStopOrder && priceDiffers)
                    {
                        double effStop = newPrice > 0 ? newPrice : order.StopPrice;
                        double marketRef = 0;
                        if (isSellSide && GetCurrentBid() > 0) marketRef = GetCurrentBid();
                        else if (!isSellSide && GetCurrentAsk() > 0) marketRef = GetCurrentAsk();
                        else if (CurrentBar >= 0) marketRef = Close[0];
                        else marketRef = effStop; // cannot validate → let NT8 decide

                        bool invalid = isSellSide ? (effStop >= marketRef) : (effStop <= marketRef);
                        if (marketRef > 0 && invalid)
                        {
                            string guardMsg = string.Format("Skipped invalid {0} price change for order {1} (stop {2} priced through market {3})",
                                order.OrderAction, order.OrderId ?? order.Id.ToString(), effStop, marketRef);
                            Print("TradeEngine: " + guardMsg);
                            SendAck("CHANGE_ORDER", orderId, false, guardMsg);
                            return;
                        }
                    }

                    if (newQty > 0 && newQty != order.Quantity)
                    {
                        order.QuantityChanged = newQty;
                    }
                    if (newPrice > 0)
                    {
                        if (order.OrderType == OrderType.Limit)
                        {
                            order.LimitPriceChanged = newPrice;
                        }
                        else if (order.OrderType == OrderType.StopMarket)
                        {
                            order.StopPriceChanged = newPrice;
                        }
                        else if (order.OrderType == OrderType.StopLimit)
                        {
                            order.StopPriceChanged = newPrice;
                            order.LimitPriceChanged = newPrice;
                        }
                    }
                    acc.Change(new[] { order });
                    Print(string.Format("TradeEngine: Changed order {0} (Price={1}, Qty={2}) on {3}", orderId, newPrice, newQty, acc.Name));
                    SendAck("CHANGE_ORDER", orderId, true, string.Format("Changed order on {0}", acc.Name));
                }
                else
                {
                    Print(string.Format("TradeEngine: Order {0} not found on {1} in working/accepted state to change", orderId, acc.Name));
                    SendAck("CHANGE_ORDER", orderId, false, "Order not found in working/accepted state on " + acc.Name);
                }

                SendOrdersUpdate(acc.Name);
            }
            catch (Exception ex)
            {
                Print("TradeEngine ChangeSpecificOrder Error: " + ex.Message);
                SendAck("CHANGE_ORDER", "", false, "Exception: " + ex.Message);
            }
        }

        private void Flatten(string targetAcc = null)
        {
            try
            {
                Account acc = GetBoundAccount(targetAcc);
                if (acc == null || Instrument == null) return;

                var active = acc.Orders.Where(o => o != null && o.Instrument != null && 
                    o.Instrument.FullName == Instrument.FullName && 
                    (o.OrderState == OrderState.Working || o.OrderState == OrderState.Accepted)).ToList();
                if (active.Count > 0)
                {
                    acc.Cancel(active);
                    Print(string.Format("TradeEngine: Cancelled {0} working orders on {1} for {2}", active.Count, acc.Name, Instrument.FullName));
                }

                bool flattened = false;
                Position pos = acc.Positions.FirstOrDefault(p => p != null && p.Instrument != null && p.Instrument.FullName == Instrument.FullName);
                if (pos != null && pos.MarketPosition != MarketPosition.Flat && pos.Quantity > 0)
                {
                    OrderAction act = (pos.MarketPosition == MarketPosition.Long) ? OrderAction.Sell : OrderAction.BuyToCover;
                    Order mkt = acc.CreateOrder(Instrument, act, OrderType.Market, OrderEntry.Automated, TimeInForce.Day, pos.Quantity, 0, 0, "", "Flatten", DateTime.MaxValue, null);
                    acc.Submit(new[] { mkt });
                    Print(string.Format("TradeEngine: Flattened {0} contracts ({1}) on {2} for {3}", pos.Quantity, pos.MarketPosition, acc.Name, Instrument.FullName));
                    flattened = true;
                }

                SendAck("FLATTEN_POSITION", "", true, flattened ? "Position flattened on " + acc.Name : "Already flat on " + acc.Name);
                SendPositionUpdate(acc.Name);
                SendOrdersUpdate(acc.Name);
            }
            catch (Exception ex)
            {
                Print("TradeEngine Flatten Error: " + ex.Message);
                SendAck("FLATTEN_POSITION", "", false, "Exception: " + ex.Message);
            }
        }
        #endregion

        #region WebSocket Client
        private void StartWebSocket()
        {
            StopWebSocket();
            cts = new CancellationTokenSource();
            Task.Run(() => ConnectAndListenLoop(cts.Token));
        }

        private void StopWebSocket()
        {
            try { if (cts != null) cts.Cancel(); } catch { }
            try { if (connCts != null) connCts.Cancel(); } catch { }
            try { if (wsClient != null) wsClient.Dispose(); } catch { }
            wsClient = null;
            isWsConnected = false;
            string discarded;
            while (outboundQueue.TryDequeue(out discarded)) { }
        }

        private async Task ConnectAndListenLoop(CancellationToken rootCt)
        {
            while (!rootCt.IsCancellationRequested)
            {
                connCts = CancellationTokenSource.CreateLinkedTokenSource(rootCt);
                var ct = connCts.Token;

                try
                {
                    wsClient = new ClientWebSocket();
                    // Issue 5: WebSocket keep-alive ping interval
                    wsClient.Options.KeepAliveInterval = TimeSpan.FromSeconds(5);

                    await wsClient.ConnectAsync(new Uri(WebSocketUrl), ct);
                    isWsConnected = true;
                    Print("TradeEngine: Connected to Go engine at " + WebSocketUrl);

                    // Clear queue from previous session
                    string discarded;
                    while (outboundQueue.TryDequeue(out discarded)) { }

                    // Start dedicated single-writer queue loop (Issue 1 & 4)
                    var sendWriterTask = Task.Run(() => OutboundSendLoop(wsClient, ct));

                    // Start application-level heartbeat loop (Issue 5)
                    var heartbeatTask = Task.Run(() => HeartbeatLoop(ct));

                    // Issue 8: Proactively push historical bars for all series on every reconnect!
                    syncedSeries.Clear();
                    if (BarsArray != null)
                    {
                        for (int i = 0; i < BarsArray.Length; i++)
                        {
                            if (BarsArray[i] != null && BarsArray[i].Count > 0)
                            {
                                syncedSeries.Add(i);
                                SendHistoricalBars(500, i);
                            }
                        }
                    }

                    SendMarketInfo();
                    SendPositionUpdate();
                    SendOrdersUpdate();

                    byte[] buffer = new byte[65536];
                    while (wsClient.State == WebSocketState.Open && !ct.IsCancellationRequested)
                    {
                        var res = await wsClient.ReceiveAsync(new ArraySegment<byte>(buffer), ct);
                        if (res.MessageType == WebSocketMessageType.Close)
                        {
                            Print("TradeEngine: Server closed WebSocket cleanly");
                            break;
                        }

                        if (res.Count > 0)
                        {
                            string msg = Encoding.UTF8.GetString(buffer, 0, res.Count);
                            ProcessMessage(msg);
                        }
                    }
                }
                catch (OperationCanceledException) { }
                catch (Exception ex)
                {
                    Print("TradeEngine WebSocket Error: " + ex.Message);
                }
                finally
                {
                    // Issue 6: Reconnect path leaves stale state on graceful close - FIXED
                    isWsConnected = false;
                    try { if (connCts != null) connCts.Cancel(); } catch { }
                    try { if (wsClient != null) wsClient.Dispose(); } catch { }
                    wsClient = null;
                    string discarded;
                    while (outboundQueue.TryDequeue(out discarded)) { }
                }

                try
                {
                    await Task.Delay(2000, rootCt);
                }
                catch (OperationCanceledException) { break; }
            }
        }

        private async Task OutboundSendLoop(ClientWebSocket ws, CancellationToken ct)
        {
            try
            {
                while (!ct.IsCancellationRequested && ws.State == WebSocketState.Open)
                {
                    await queueSignal.WaitAsync(ct);
                    string msg;
                    while (outboundQueue.TryDequeue(out msg))
                    {
                        if (ws.State != WebSocketState.Open || ct.IsCancellationRequested) break;
                        byte[] bytes = Encoding.UTF8.GetBytes(msg);
                        await ws.SendAsync(new ArraySegment<byte>(bytes), WebSocketMessageType.Text, true, ct);
                    }
                }
            }
            catch (OperationCanceledException) { }
            catch (Exception ex)
            {
                Print("TradeEngine SendWriter Error: " + ex.Message);
                isWsConnected = false;
                try { ws.Abort(); } catch { }
                try { if (connCts != null) connCts.Cancel(); } catch { }
            }
        }

        private async Task HeartbeatLoop(CancellationToken ct)
        {
            try
            {
                while (!ct.IsCancellationRequested && isWsConnected)
                {
                    await Task.Delay(5000, ct);
                    if (!isWsConnected) break;
                    long ts = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
                    SendWs("HEARTBEAT", string.Format(System.Globalization.CultureInfo.InvariantCulture, "{{\"timestamp\":{0}}}", ts));
                }
            }
            catch (OperationCanceledException) { }
        }

        private void ProcessMessage(string json)
        {
            try
            {
                var serializer = new JavaScriptSerializer();
                var root = serializer.Deserialize<Dictionary<string, object>>(json);
                if (root == null) return;

                string msgType = GetField(root, "type", "");
                string reqId = GetField(root, "reqId", "");

                // Check sequence/request ID to prevent duplicate execution on reconnects
                if (!string.IsNullOrEmpty(reqId) && (msgType == "SUBMIT_ORDER" || msgType == "EXECUTE_ORDER" || msgType == "CHANGE_ORDER" || msgType == "CANCEL_ORDER"))
                {
                    if (processedRequests.ContainsKey(reqId))
                    {
                        Print("TradeEngine: Ignoring duplicate reqId " + reqId + " for " + msgType);
                        return; // already processed
                    }
                    processedRequests[reqId] = DateTime.UtcNow;

                    if (processedRequests.Count > 1000)
                    {
                        DateTime cutoff = DateTime.UtcNow.AddHours(-1);
                        var stale = processedRequests.Where(kvp => kvp.Value < cutoff).Select(kvp => kvp.Key).ToList();
                        foreach (var k in stale) processedRequests.Remove(k);
                    }
                }

                object payload = root.ContainsKey("payload") && root["payload"] != null ? root["payload"] : root;

                switch (msgType.ToUpperInvariant())
                {
                    case "SUBMIT_ORDER":
                    case "EXECUTE_ORDER":
                        SubmitOrders(payload);
                        break;

                    case "CANCEL_ORDER":
                        {
                            string orderId = GetField(payload, "orderId", GetField(root, "orderId", ""));
                            string accName = GetField(payload, "accountName", GetField(root, "accountName", ""));
                            if (!string.IsNullOrEmpty(orderId)) CancelSpecificOrder(orderId, accName);
                            else CancelAllOrders(accName);
                        }
                        break;

                    case "CHANGE_ORDER":
                        ChangeSpecificOrder(payload);
                        break;

                    case "FLATTEN_POSITION":
                        {
                            string accName = GetField(payload, "accountName", GetField(root, "accountName", ""));
                            Flatten(accName);
                        }
                        break;

                    case "GET_BARS":
                    case "SUBSCRIBE":
                        {
                            int count = GetIntField(payload, "count", GetIntField(root, "count", 500));
                            string tf = GetField(payload, "timeframe", GetField(root, "timeframe", ""));
                            int idx = SeriesIndexForTimeframe(tf);
                            SendHistoricalBars(count, idx);
                            SendMarketInfo();
                        }
                        break;

                    default:
                        break;
                }
            }
            catch (Exception ex)
            {
                Print("TradeEngine ProcessMessage Error: " + ex.Message);
            }
        }

        private void SendWs(string msgType, string payload)
        {
            if (!isWsConnected || wsClient == null || wsClient.State != WebSocketState.Open) return;
            string msg = string.Format("{{\"type\":\"{0}\",\"payload\":{1}}}", msgType, payload);

            // Bounded safety: if queue exceeds 10000 during network stall, drop oldest
            if (outboundQueue.Count > 10000)
            {
                string dropped;
                outboundQueue.TryDequeue(out dropped);
            }
            outboundQueue.Enqueue(msg);
            queueSignal.Release();
        }
        #endregion

        #region Multi-Series Bar Streaming
        // Declare the secondary bar series matching the web terminal's selectable
        // timeframes. Called from State.Configure, before any bars are calculated.
        private void ConfigureSecondarySeries()
        {
            seriesLabels.Clear();
            seriesDesc.Clear();
            seriesTypes.Clear();
            // Index 0 = primary (chart) series. Its label is "" (legacy) unless
            // the chart's own bars period is exactly one of the four web
            // timeframes — then the primary stream IS that timeframe.
            seriesLabels.Add(PrimarySeriesLabel());
            seriesDesc.Add(PrimarySeriesDesc());
            seriesTypes.Add(BarsPeriod != null ? BarsPeriod.BarsPeriodType : BarsPeriodType.Second);

            AddSecondary(BarsPeriodType.Second, 15, "15s");
            AddSecondary(BarsPeriodType.Minute, 1, "1m");
            AddSecondary(BarsPeriodType.Minute, 5, "5m");
            // "100t" is built as VOLUME(100) bars, not Tick(100): Market
            // Replay / Playback cannot feed real tick data to a secondary tick
            // series when the primary is time-based (NT8 synthesizes degenerate
            // bars — pinned opens, bogus tick counts), while volume bars
            // construct exactly from any recorded data. For NQ futures traded
            // volume ≈ contracts ≈ ticks, so the pane behaves like a true
            // ~100-trade chart everywhere. When the primary chart series IS a
            // tick/volume 100 series, AddSecondary skips this duplicate and
            // the primary stream (real ticks) carries the "100t" label instead.
            AddSecondary(BarsPeriodType.Volume, 100, "100t");
        }

        private string PrimarySeriesLabel()
        {
            if (BarsPeriod == null) return "primary";
            if (BarsPeriod.BarsPeriodType == BarsPeriodType.Second && BarsPeriod.Value == 15) return "15s";
            if (BarsPeriod.BarsPeriodType == BarsPeriodType.Minute && BarsPeriod.Value == 1) return "1m";
            if (BarsPeriod.BarsPeriodType == BarsPeriodType.Minute && BarsPeriod.Value == 5) return "5m";
            // A chart primary of 100-Tick OR 100-Volume is the "100t" stream
            // (real ticks when tick-based — the user's own chart view).
            if (BarsPeriod.Value == 100 &&
                (BarsPeriod.BarsPeriodType == BarsPeriodType.Tick || BarsPeriod.BarsPeriodType == BarsPeriodType.Volume)) {
                return "100t";
            }
            // Primary chart series does not match any web timeframe: tag it as
            // the distinct "primary" stream (engine tracking only).
            return "primary";
        }

        private string PrimarySeriesDesc()
        {
            if (BarsPeriod == null) return PrimarySeriesLabel() + "=";
            return PrimarySeriesLabel() + "=" + BarsPeriod.BarsPeriodType + BarsPeriod.Value;
        }

        // Adds a secondary series unless the primary chart series already is
        // exactly that timeframe (duplicates throw / are wasteful).
        private void AddSecondary(BarsPeriodType type, int value, string label)
        {
            if (seriesLabels.Count > 0 && seriesLabels[0] == label) return;
            AddDataSeries(type, value);
            seriesLabels.Add(label);
            seriesDesc.Add(label + "=" + type + value);
            seriesTypes.Add(type);
        }

        // Maps a timeframe label from the web terminal to an index in
        // BarsArray. Unknown/empty labels fall back to the primary series.
        private int SeriesIndexForTimeframe(string tf)
        {
            if (string.IsNullOrEmpty(tf)) return 0;
            for (int i = 0; i < seriesLabels.Count; i++)
            {
                if (string.Equals(seriesLabels[i], tf, StringComparison.OrdinalIgnoreCase)) return i;
            }
            return 0;
        }
        #endregion

        #region JSON Helpers
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
                if (double.TryParse(dict[key].ToString(), System.Globalization.NumberStyles.Any, System.Globalization.CultureInfo.InvariantCulture, out val))
                    return val;
            }
            return fallback;
        }

        private static int GetIntField(object obj, string key, int fallback)
        {
            var dict = obj as Dictionary<string, object>;
            if (dict != null && dict.ContainsKey(key) && dict[key] != null)
            {
                int val;
                if (int.TryParse(dict[key].ToString(), out val))
                    return val;
            }
            return fallback;
        }
        #endregion
    }
}
