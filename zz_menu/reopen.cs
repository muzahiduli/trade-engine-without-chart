using System;
using System.Windows.Automation;
class ReopenEditor2 {
  static int Main(string[] args) {
    long hwnd = long.Parse(args[0]);
    var root = AutomationElement.FromHandle(new IntPtr(hwnd));
    foreach (AutomationElement el in root.FindAll(TreeScope.Descendants, Condition.TrueCondition)) {
      try {
        string n = (el.Current.Name ?? "").Trim();
        object pat;
        if (string.Equals(n, "Tools", StringComparison.OrdinalIgnoreCase) && el.TryGetCurrentPattern(ExpandCollapsePattern.Pattern, out pat)) {
          ((ExpandCollapsePattern)pat).Expand(); break;
        }
      } catch { }
    }
    System.Threading.Thread.Sleep(500);
    foreach (AutomationElement el in root.FindAll(TreeScope.Descendants, Condition.TrueCondition)) {
      try {
        string n = el.Current.Name ?? "";
        if (n.IndexOf("NinjaScript Editor", StringComparison.OrdinalIgnoreCase) >= 0) {
          object ip;
          if (el.TryGetCurrentPattern(InvokePattern.Pattern, out ip)) { ((InvokePattern)ip).Invoke(); return 0; }
          if (el.TryGetCurrentPattern(SelectionItemPattern.Pattern, out ip)) { ((SelectionItemPattern)ip).Select(); return 0; }
        }
      } catch { }
    }
    return 1;
  }
}
