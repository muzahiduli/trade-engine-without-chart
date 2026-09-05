// TradeEngineHotkeyAddOn — forwards global hotkeys to the Go trade engine.
//
// NO overlay window, NO browser — it is invisible. Its only job: a Win32
// low-level keyboard hook maps the same combos the web terminal uses
// (Ctrl+1/2/3, Shift+F/R/S/B/C, Ctrl+Space, Ctrl+Shift+K, etc.) to engine
// HOTKEY actions, and sends {"type":"HOTKEY","payload":{"action":...}} over a
// WebSocket to the hub (same endpoint/type as the web panel). Works even while
// the user drags lines / has focus anywhere in NinjaTrader.
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
        private string webSocketUrl = "ws://localhost:8080/ws?type=WEB";

        // ---------------- Keyboard hook ----------------
        private const int WH_KEYBOARD_LL = 13;
        private const int WM_KEYDOWN = 0x0100;
        private const int WM_SYSKEYDOWN = 0x0104;
        private LowLevelKeyboardProc hookProc;
        private IntPtr hookHandle = IntPtr.Zero;
        private readonly Dictionary<string, HotkeyBinding> bindings = new Dictionary<string, HotkeyBinding>();
        private bool hooked;

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
            bindings["BREAKOUT_ENTRY"]  = new HotkeyBinding("BREAKOUT_ENTRY",  false, true,  false, VK_R);
            bindings["TRAIL_STOP"]      = new HotkeyBinding("TRAIL_STOP",      false, true,  false, VK_S);
            bindings["BREAKEVEN"]       = new HotkeyBinding("BREAKEVEN",       false, false, false, VK_B);
            bindings["BREAKEVEN_PLUS"]  = new HotkeyBinding("BREAKEVEN_PLUS",  false, true,  false, VK_B);
            bindings["CLOSE_25"]        = new HotkeyBinding("CLOSE_25",        true,  false, false, (byte)'1');
            bindings["CLOSE_50"]        = new HotkeyBinding("CLOSE_50",        true,  false, false, (byte)'2');
            bindings["CLOSE_RUNNER"]    = new HotkeyBinding("CLOSE_RUNNER",    true,  false, false, (byte)'3');
            bindings["CANCEL_ENTRY"]    = new HotkeyBinding("CANCEL_ENTRY",    false, false, false, VK_C);
            bindings["CANCEL_ORDERS"]   = new HotkeyBinding("CANCEL_ORDERS",   false, true,  false, VK_C);
            bindings["SWAP_DIRECTION"]  = new HotkeyBinding("SWAP_DIRECTION",  false, false, false, VK_S);
            bindings["SCALE_OUT"]       = new HotkeyBinding("SCALE_OUT",       true,  false, false, VK_F);
            bindings["FLATTEN"]         = new HotkeyBinding("FLATTEN",         true,  false, false, VK_SPACE);
            bindings["KILL_SWITCH"]     = new HotkeyBinding("KILL_SWITCH",     true,  true,  false, VK_K);
        }

        private const byte VK_F = 0x46, VK_R = 0x52, VK_S = 0x53, VK_B = 0x42, VK_C = 0x43, VK_K = 0x4B, VK_SPACE = 0x20;

        protected override void OnStateChange()
        {
            if (State == State.SetDefaults)
            {
                Name = "TradeEngineHotkeyAddOn";
            }
            else if (State == State.Terminated)
            {
                StopWebSocket();
                UnhookKeyboard();
            }
        }

        protected override void OnWindowCreated(Window window)
        {
            ControlCenter cc = window as ControlCenter;
            if (cc == null) return;

            BuildDefaultBindings();
            StartWebSocket();
            HookKeyboard();

            cc.Dispatcher.InvokeAsync(() =>
            {
                try
                {
                    NinjaTrader.Code.Output.Process("TradeEngineHotkeyAddOn: hotkey forwarding active", PrintTo.OutputTab1);
                }
                catch { }
            });
        }

        protected override void OnWindowDestroyed(Window window)
        {
            UnhookKeyboard();
            StopWebSocket();
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

        [DllImport("kernel32.dll", CharSet = CharSet.Auto)]
        private static extern IntPtr GetModuleHandle(string lpModuleName);

        private static bool IsKeyDown(int vKey) { return (GetAsyncKeyState(vKey) & 0x8000) != 0; }

        private void HookKeyboard()
        {
            if (hooked) return;
            hookProc = KeyboardProc;
            using (var cur = System.Diagnostics.Process.GetCurrentProcess())
            using (var m = cur.MainModule)
            {
                hookHandle = SetWindowsHookEx(WH_KEYBOARD_LL, hookProc, GetModuleHandle(m.ModuleName), 0);
            }
            hooked = hookHandle != IntPtr.Zero;
        }

        private void UnhookKeyboard()
        {
            if (!hooked) return;
            if (hookHandle != IntPtr.Zero) UnhookWindowsHookEx(hookHandle);
            hookHandle = IntPtr.Zero;
            hooked = false;
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

                    foreach (var b in bindings.Values)
                    {
                        if (b.Key != (byte)k.vkCode) continue;
                        if (b.Ctrl != ctrl || b.Shift != shift || b.Alt != alt) continue;
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
                        NinjaTrader.Code.Output.Process("TradeEngineHotkeyAddOn: connected to engine", PrintTo.OutputTab1);
                        await Task.WhenAll(SenderAsync(ct), ReceiverAsync(ct)).ConfigureAwait(false);
                    }
                }
                catch (Exception ex)
                {
                    if (!ct.IsCancellationRequested)
                        NinjaTrader.Code.Output.Process("TradeEngineHotkeyAddOn: connect error " + ex.Message, PrintTo.OutputTab1);
                }
                isWsConnected = false;
                wsClient = null;
                if (!ct.IsCancellationRequested)
                    await Task.Delay(2000, ct).ConfigureAwait(false);
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
                    if (result.MessageType == WebSocketMessageType.Close) break;
                    // We only SEND hotkeys; inbound is intentionally ignored.
                    if (result.EndOfMessage) sb.Clear();
                    else sb.Append(Encoding.UTF8.GetString(buffer, 0, result.Count));
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