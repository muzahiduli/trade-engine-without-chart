// TradeEngineHotkeyAddOn — forwards global hotkeys to the Go trade engine.
//
// Invisible AddOn (no window, no browser). Its only job: a Win32 low-level
// keyboard hook maps the same combos the web terminal uses to engine HOTKEY
// actions and sends them over WebSocket to the hub.
//
// Key behaviors:
//  - FOCUS-GATED: hotkeys only act while a NinjaTrader window has the
//    foreground focus. When you're in Chrome / any other app the hook lets
//    every key pass through untouched — it never intercepts normal typing.
//  - TOGGLE: pressing plain 'L' (no modifiers) flips forwarding on/off.
//    The engine is told via HOTKEY_STATUS {enabled} so the web panel can show
//    the state.
//  - CONNECTS AS "HOTKEY": the WebSocket reports type=HOTKEY so the hub can
//    distinguish this addon from the browser panel and expose
//    hotkeyForwarding/hotkeyConnected in SYNC_STATE.
//
// The engine executes all trades through the gateway strategy — this addon
// never calls SendOrder()/Submit().
//
// C# 7.x / .NET Framework syntax only (NT8's editor Roslyn).
#region Using declarations
using System;
using System.Collections.Generic;
using System.Net.WebSockets;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using System.Windows;
using NinjaTrader.Gui;
using NinjaTrader.NinjaScript;
#endregion

namespace NinjaTrader.NinjaScript.AddOns
{
    public class TradeEngineHotkeyAddOn : AddOnBase
    {
        // ---------------- WebSocket to the Go engine ----------------
        private ClientWebSocket wsClient;
        private CancellationTokenSource wsCts;
        private readonly Queue<string> outboundQueue = new Queue<string>();
        private readonly SemaphoreSlim queueSignal = new SemaphoreSlim(0);
        private bool isWsConnected;
        private string webSocketUrl = "ws://localhost:8080/ws?type=HOTKEY";

        // ---------------- Keyboard hook ----------------
        private const int WH_KEYBOARD_LL = 13;
        private const int WM_KEYDOWN = 0x0100;
        private const int WM_SYSKEYDOWN = 0x0104;
        private LowLevelKeyboardProc hookProc;
        private IntPtr hookHandle = IntPtr.Zero;
        private readonly Dictionary<string, HotkeyBinding> bindings = new Dictionary<string, HotkeyBinding>();
        private bool hooked;

        // ---------------- State ----------------
        private bool forwardingEnabled = true; // default ON; toggled with plain 'L'
        private uint nt8Pid;                   // our own process id (NinjaTrader)

        private class HotkeyBinding
        {
            public string Action;
            public bool Ctrl;
            public bool Shift;
            public bool Alt;
            public byte Key;
            public HotkeyBinding(string action, bool ctrl, bool shift, bool alt, byte key) { Action = action; Ctrl = ctrl; Shift = shift; Alt = alt; Key = key; }
        }

        // Defaults mirror the web terminal (web/js/hotkeys.js).
        private void BuildDefaultBindings()
        {
            bindings.Clear();
            bindings["INSTANT_ENTRY"]   = new HotkeyBinding("INSTANT_ENTRY",   false, true,  false, VK_F);
            bindings["BREAKEVEN"]       = new HotkeyBinding("BREAKEVEN",       false, false, false, VK_B);
            bindings["CLOSE_25"]        = new HotkeyBinding("CLOSE_25",        true,  false, false, (byte)'1');
            bindings["CLOSE_50"]        = new HotkeyBinding("CLOSE_50",        true,  false, false, (byte)'2');
            bindings["CANCEL_ENTRY"]    = new HotkeyBinding("CANCEL_ENTRY",    false, false, false, VK_C);
            bindings["SCALE_OUT"]       = new HotkeyBinding("SCALE_OUT",       true,  false, false, VK_F);
            bindings["FLATTEN"]         = new HotkeyBinding("FLATTEN",         true,  false, false, VK_SPACE);
            bindings["STOP_AT_5M"]      = new HotkeyBinding("STOP_AT_5M",      false, false, false, VK_R);
            bindings["TOGGLE_RISK"]     = new HotkeyBinding("TOGGLE_RISK",     false, false, false, VK_Q);
            bindings["LOCK_SL"]         = new HotkeyBinding("LOCK_SL",         false, false, false, VK_L);
            bindings["KILL_SWITCH"]     = new HotkeyBinding("KILL_SWITCH",     true,  true,  false, VK_K);
        }

        private const byte VK_F = 0x46, VK_R = 0x52, VK_B = 0x42, VK_C = 0x43, VK_K = 0x4B, VK_L = 0x4C, VK_Q = 0x51, VK_SPACE = 0x20;

        // Diagnostic breadcrumb so failures are visible without UI automation.
        private static string CrumbPath
        {
            get { return System.IO.Path.Combine(System.IO.Path.GetTempPath(), "TradeEngineHotkeyAddOn.log"); }
        }

        private void Crumb(string text)
        {
            try
            {
                System.IO.File.AppendAllText(CrumbPath,
                    DateTime.Now.ToString("HH:mm:ss.fff") + "  " + text + Environment.NewLine);
            }
            catch { }
        }

        protected override void OnStateChange()
        {
            if (State == State.SetDefaults)
            {
                Name = "TradeEngineHotkeyAddOn";
            }
            else if (State == State.Terminated)
            {
                Crumb("Terminated");
                StopWebSocket();
                UnhookKeyboard();
            }
        }

        protected override void OnWindowCreated(Window window)
        {
            ControlCenter cc = window as ControlCenter;
            if (cc == null) { Crumb("OnWindowCreated: window is " + (window == null ? "null" : window.GetType().Name) + " (not ControlCenter)"); return; }

            Crumb("OnWindowCreated: ControlCenter detected");
            nt8Pid = (uint)System.Diagnostics.Process.GetCurrentProcess().Id;
            BuildDefaultBindings();
            StartWebSocket();
            HookKeyboard();
            Crumb("startup: WS=" + (isWsConnected ? "connected" : "connecting") + " hook=" + (hooked ? "installed" : "FAILED"));

            cc.Dispatcher.InvokeAsync(() =>
            {
                try
                {
                    NinjaTrader.Code.Output.Process("TradeEngineHotkeyAddOn: hotkey forwarding active (L = toggle)", PrintTo.OutputTab1);
                }
                catch { }
            });
        }

        protected override void OnWindowDestroyed(Window window)
        {
            // NOTE: do NOT unhook / disconnect here. NT8 calls OnWindowDestroyed
            // for many window lifecycle transitions (Control Center close/reopen,
            // workspace switches) and tearing down here would leave hotkeys dead
            // until a full restart (the "not connecting anymore" bug). Only
            // State == State.Terminated (real AddOn unload) stops everything.
            Crumb("OnWindowDestroyed (ignored, staying alive)");
        }

        // ===================== Keyboard hook =====================
        private delegate IntPtr LowLevelKeyboardProc(int nCode, IntPtr wParam, IntPtr lParam);

        [StructLayout(LayoutKind.Sequential)]
        private struct KBDLLHOOKSTRUCT
        {
            public uint vkCode;
            public uint scanCode;
            public uint flags;
            public uint time;
            public IntPtr dwExtraInfo;
        }

        [DllImport("user32.dll", SetLastError = true)]
        private static extern IntPtr SetWindowsHookEx(int idHook, LowLevelKeyboardProc lpfn, IntPtr hMod, uint dwThreadId);

        [DllImport("user32.dll", SetLastError = true)]
        private static extern bool UnhookWindowsHookEx(IntPtr hhk);

        [DllImport("user32.dll")]
        private static extern IntPtr CallNextHookEx(IntPtr hhk, int nCode, IntPtr wParam, IntPtr lParam);

        [DllImport("user32.dll")]
        private static extern short GetAsyncKeyState(int vKey);

        [DllImport("user32.dll")]
        private static extern IntPtr GetForegroundWindow();

        [DllImport("user32.dll")]
        private static extern uint GetWindowThreadProcessId(IntPtr hWnd, out uint lpdwProcessId);

        [DllImport("kernel32.dll", CharSet = CharSet.Auto)]
        private static extern IntPtr GetModuleHandle(string lpModuleName);

        private static bool IsKeyDown(int vKey) { return (GetAsyncKeyState(vKey) & 0x8000) != 0; }

        private void HookKeyboard()
        {
            if (hooked) { Crumb("HookKeyboard: already hooked"); return; }
            hookProc = KeyboardProc;
            try
            {
                using (var cur = System.Diagnostics.Process.GetCurrentProcess())
                using (var m = cur.MainModule)
                {
                    hookHandle = SetWindowsHookEx(WH_KEYBOARD_LL, hookProc, GetModuleHandle(m.ModuleName), 0);
                }
            }
            catch (Exception ex)
            {
                Crumb("HookKeyboard exception: " + ex.Message);
            }
            hooked = hookHandle != IntPtr.Zero;
            Crumb("HookKeyboard: handle=" + (hookHandle == IntPtr.Zero ? "NULL (FAILED)" : hookHandle.ToString()) + " hooked=" + hooked);
        }

        private void UnhookKeyboard()
        {
            if (!hooked) return;
            if (hookHandle != IntPtr.Zero) UnhookWindowsHookEx(hookHandle);
            hookHandle = IntPtr.Zero;
            hooked = false;
        }

        // Returns true ONLY when the FOREGROUND window is a NinjaTrader CHART window.
        // Hotkeys fire only on the charts — never while typing in Chrome, other
        // apps, NT8 dialogs/editors, or even NT8's own non-chart windows. This
        // is the hard rule that finally stops key-swallowing everywhere else.
        // Fail-closed: if we can't tell, typing always wins over hotkeys.
        private bool IsNinjaTraderFocused()
        {
            try
            {
                IntPtr fg = GetForegroundWindow();
                if (fg == IntPtr.Zero) return false;
                uint pid;
                GetWindowThreadProcessId(fg, out pid);
                if (pid != nt8Pid) return false;

                var title = new System.Text.StringBuilder(256);
                GetWindowText(fg, title, title.Capacity);
                string t = title.ToString();
                if (t.IndexOf("Chart", StringComparison.OrdinalIgnoreCase) >= 0) return true;
                if (t.IndexOf("SuperDOM", StringComparison.OrdinalIgnoreCase) >= 0) return true;
                if (t.IndexOf("Chart Trader", StringComparison.OrdinalIgnoreCase) >= 0) return true;

                // WPF layer: if a chart control has keyboard focus, it's a chart.
                try
                {
                    var fe = System.Windows.Input.Keyboard.FocusedElement;
                    if (fe != null && fe is NinjaTrader.Gui.Chart.ChartControl) return true;
                    if (fe != null && fe.GetType().Name.Contains("Chart")) return true;
                }
                catch { }
                return false;
            }
            catch
            {
                return false; // fail closed — typing always wins
            }
        }

        [DllImport("user32.dll", CharSet = CharSet.Auto)]
        private static extern int GetWindowText(IntPtr hWnd, System.Text.StringBuilder lpString, int nMaxCount);

        [StructLayout(LayoutKind.Sequential)]
        private struct GUITHREADINFO
        {
            public int cbSize;
            public uint flags;
            public IntPtr hwndActive;
            public IntPtr hwndFocus;
            public IntPtr hwndCapture;
            public IntPtr hwndMenuOwner;
            public IntPtr hwndMoveSize;
            public IntPtr hwndCaret;
        }

        [DllImport("user32.dll")]
        private static extern bool GetGUIThreadInfo(uint idThread, ref GUITHREADINFO lpgui);

        [DllImport("user32.dll", CharSet = CharSet.Auto)]
        private static extern int GetClassName(IntPtr hWnd, System.Text.StringBuilder lpClassName, int nMaxCount);

        // True when the foreground NT8 window has focus inside a text editor.
// Two layers: WPF's own Keyboard.FocusedElement (catches TextBox/RichTextBox/
// PasswordBox/ComboBox — the Win32 class-name check misses WPF-hosted
// editors), falling back to the Win32 focused-window class (Edit/RichEdit).
// While typing, ALL hotkeys pass through untouched.
private bool IsTypingInNinjaTrader()
{
    try
    {
        if (!IsNinjaTraderFocused()) return false;
        // WPF layer: the hook callback runs on NT8's UI thread (the hook is
        // installed from OnWindowCreated), so reading Keyboard.FocusedElement
        // is thread-safe here. If it throws (rare), fall through to Win32.
        try
        {
            var fe = System.Windows.Input.Keyboard.FocusedElement;
            if (fe is System.Windows.Controls.TextBox ||
                fe is System.Windows.Controls.PasswordBox ||
                fe is System.Windows.Controls.RichTextBox ||
                fe is System.Windows.Controls.ComboBox)
                return true;
        }
        catch { }
        // Win32 layer: WPF-companion Edit hosts / WinForms edit controls.
        IntPtr fg = GetForegroundWindow();
        uint tid;
        GetWindowThreadProcessId(fg, out tid);
        GUITHREADINFO gi = new GUITHREADINFO();
        gi.cbSize = Marshal.SizeOf(typeof(GUITHREADINFO));
        if (!GetGUIThreadInfo(tid, ref gi) || gi.hwndFocus == IntPtr.Zero) return false;
        var cls = new System.Text.StringBuilder(128);
        GetClassName(gi.hwndFocus, cls, cls.Capacity);
        string name = cls.ToString();
        return name.IndexOf("Edit", StringComparison.OrdinalIgnoreCase) >= 0 ||
               name.IndexOf("RichEdit", StringComparison.OrdinalIgnoreCase) >= 0 ||
               name.IndexOf("TextBox", StringComparison.OrdinalIgnoreCase) >= 0;
    }
    catch
    {
        return false;
    }
}

private IntPtr KeyboardProc(int nCode, IntPtr wParam, IntPtr lParam)
{
    if (nCode >= 0)
    {
        uint msg = (uint)wParam.ToInt64();
        if (msg == WM_KEYDOWN || msg == WM_SYSKEYDOWN)
        {
            KBDLLHOOKSTRUCT k = (KBDLLHOOKSTRUCT)Marshal.PtrToStructure(lParam, typeof(KBDLLHOOKSTRUCT));
            bool ctrl = IsKeyDown(0x11);
            bool shift = IsKeyDown(0x10);
            bool alt = IsKeyDown(0x12);

            // FOCUS GATE FIRST: never intercept keys unless NinjaTrader itself
            // is the foreground app — otherwise we'd hijack Chrome/other apps
            // and typed combos (or even plain 'L') would toggle trades.
            if (!IsNinjaTraderFocused()) return CallNextHookEx(hookHandle, nCode, wParam, lParam);

            // TYPING GATE: while focus is inside an NT8 text field, pass every
            // key through untouched (B/C/Q/R/L must type normally). The charm
            // hotkeys are for chart focus, not for typing.
            if (IsTypingInNinjaTrader()) return CallNextHookEx(hookHandle, nCode, wParam, lParam);

            // NO toggle key: hotkeys on/off is controlled ONLY by the web
            // button (state.enableHotkeys), gated server-side in the hub.
            // 'L' is just the Lock-SL action from the user's hotkey set.

            if (!forwardingEnabled) return CallNextHookEx(hookHandle, nCode, wParam, lParam);

            foreach (var b in bindings.Values)
            {
                if (b.Key != (byte)k.vkCode) continue;
                if (b.Ctrl != ctrl || b.Shift != shift || b.Alt != alt) continue;
                if (!isWsConnected)
                {
                    Crumb("KEY " + b.Action + " ignored: WS not connected");
                    return CallNextHookEx(hookHandle, nCode, wParam, lParam);
                }
                Crumb("KEY forward " + b.Action);
                SendHotkeyAction(b.Action);
                return new IntPtr(1); // swallow the key
            }
        }
    }
    return CallNextHookEx(hookHandle, nCode, wParam, lParam);
}

        private void SendHotkeyAction(string action)
        {
            string payload = string.Format("{{\"action\":\"{0}\"}}", action);
            SendWs("HOTKEY", payload);
        }

        private void SendStatus()
        {
            string payload = string.Format("{{\"enabled\":{0}}}", forwardingEnabled ? "true" : "false");
            SendWs("HOTKEY_STATUS", payload);
        }

        // ===================== WebSocket plumbing =====================
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
            Crumb("StopWebSocket: wsClient cleared");
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
                        await client.ConnectAsync(new Uri(webSocketUrl), ct).ConfigureAwait(false);
                        wsClient = client;
                        isWsConnected = true;
                        Crumb("WS connected to " + webSocketUrl);
                        SendStatus(); // tell the engine our enabled state on (re)connect
                        NinjaTrader.Code.Output.Process("TradeEngineHotkeyAddOn: connected to engine", PrintTo.OutputTab1);
                        await Task.WhenAll(SenderAsync(ct), ReceiverAsync(ct)).ConfigureAwait(false);
                    }
                }
                catch (Exception ex)
                {
                    if (!ct.IsCancellationRequested)
                    {
                        Crumb("WS connect error: " + ex.Message);
                        NinjaTrader.Code.Output.Process("TradeEngineHotkeyAddOn: connect error " + ex.Message, PrintTo.OutputTab1);
                    }
                }
                isWsConnected = false;
                wsClient = null;
                Crumb("WS retry in 2s (connected=" + isWsConnected + ")");
                if (!ct.IsCancellationRequested)
                {
                    try { await Task.Delay(2000, ct).ConfigureAwait(false); }
                    catch { break; }
                }
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
                    // Watchdog: if no frame arrives within 15s, the engine is
                    // gone but the socket didn't error (abrupt kill). Dispose the
                    // client so ConnectLoopAsync sees the break and reconnects.
                    var recvTask = wsClient.ReceiveAsync(new ArraySegment<byte>(buffer), ct);
                    var done = await Task.WhenAny(recvTask, Task.Delay(15000, ct)).ConfigureAwait(false);
                    if (done != recvTask)
                    {
                        Crumb("WS watchdog: no traffic 15s, forcing reconnect");
                        try { wsClient.Dispose(); } catch { }
                        return;
                    }
                    WebSocketReceiveResult result = recvTask.Result;
                    if (result.MessageType == WebSocketMessageType.Close) break;
                    // Inbound: sync our forwarding state from the hub's SINGLE
                    // SOURCE OF TRUTH (enableHotkeys in SYNC_STATE). The web
                    // button and 'L' both just flip enableHotkeys — the AddOn
                    // always mirrors it here, so a web-panel toggle and an L-press
                    // can never disagree.
                    if (result.EndOfMessage)
                    {
                        try
                        {
                            var line = Encoding.UTF8.GetString(buffer, 0, result.Count);
                            if (line != null && line.IndexOf("SYNC_STATE", StringComparison.OrdinalIgnoreCase) >= 0)
                            {
                                var ser = new System.Web.Script.Serialization.JavaScriptSerializer();
                                Dictionary<string, object> root = ser.Deserialize<Dictionary<string, object>>(line);
                                if (root != null && root.ContainsKey("payload"))
                                {
                                    var p = root["payload"] as Dictionary<string, object>;
                                    if (p != null && p.ContainsKey("enableHotkeys"))
                                    {
                                        bool en;
                                        if (bool.TryParse(p["enableHotkeys"].ToString(), out en))
                                        {
                                            bool changed = en != forwardingEnabled;
                                            forwardingEnabled = en;
                                            if (changed) Crumb("ENGINE enableHotkeys -> forwarding " + (forwardingEnabled ? "ON" : "OFF"));
                                        }
                                    }
                                }
                            }
                        }
                        catch { }
                        sb.Clear();
                    }
                    else
                    {
                        sb.Append(Encoding.UTF8.GetString(buffer, 0, result.Count));
                    }
                }
                catch
                {
                    break;
                }
            }
        }

        private void SendWs(string msgType, string payload)
        {
            if (!isWsConnected || wsClient == null || wsClient.State != WebSocketState.Open) return;
            string msg = string.Format("{{\"type\":\"{0}\",\"payload\":{1}}}", msgType, payload);
            lock (outboundQueue)
            {
                if (outboundQueue.Count > 10000) outboundQueue.Dequeue();
                outboundQueue.Enqueue(msg);
            }
            try { queueSignal.Release(); } catch { }
        }
    }
}