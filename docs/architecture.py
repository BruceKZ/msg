"""Generate docs/architecture-{light,dark}.svg for the README. Usage: python3 docs/architecture.py docs"""
import sys
from pathlib import Path

THEMES = {
    "light": dict(bg="#f5f5f7", box="#ffffff", stroke="#d2d2d7", text="#1d1d1f", sub="#6e6e73",
                  arrow="#a1a1a6", accent="#007aff", accent_text="#ffffff", accent_sub="#d6e8ff",
                  lane="#86868b", tint="#e8f1fe"),
    "dark": dict(bg="#1c1c1e", box="#2c2c2e", stroke="#3a3a3c", text="#f5f5f7", sub="#98989d",
                 arrow="#636366", accent="#0a84ff", accent_text="#ffffff", accent_sub="#cfe3ff",
                 lane="#8e8e93", tint="#1f2a3a"),
}

FONT = "-apple-system, BlinkMacSystemFont, 'SF Pro Text', 'Segoe UI', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', 'Noto Sans CJK SC', sans-serif"
MONO = "ui-monospace, 'SF Mono', SFMono-Regular, Menlo, Consolas, monospace"

W, H = 1000, 452


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def build(t):
    o = []
    a = o.append
    a(f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" '
      f'role="img" aria-label="msg 架构：来源经 HTTP API 和 Kind 生成 Envelope，存入 SQLite，再由调度器投递到各渠道">')
    a(f'''<defs>
  <marker id="arr" viewBox="0 0 10 10" refX="8.5" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
    <path d="M1 1.5 L8.5 5 L1 8.5" fill="none" stroke="{t['arrow']}" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>
  </marker>
  <marker id="arrA" viewBox="0 0 10 10" refX="8.5" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
    <path d="M1 1.5 L8.5 5 L1 8.5" fill="none" stroke="{t['accent']}" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>
  </marker>
</defs>''')
    a(f'<rect width="{W}" height="{H}" rx="18" fill="{t["bg"]}"/>')
    a(f'<g font-family="{FONT}">')

    def lane(x, y, label):
        a(f'<text x="{x}" y="{y}" font-size="12" font-weight="600" letter-spacing=".6" fill="{t["lane"]}">{esc(label)}</text>')

    def box(x, y, w, h, title, subs=(), accent=False, mono_first=False):
        fill = t["accent"] if accent else t["box"]
        stroke = "none" if accent else t["stroke"]
        a(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="12" fill="{fill}" stroke="{stroke}" stroke-width="1"/>')
        lines = 1 + len(subs)
        top = y + h / 2 - (lines - 1) * 9.5 + 5
        a(f'<text x="{x + 16}" y="{top}" font-size="15" font-weight="600" fill="{t["accent_text"] if accent else t["text"]}">{esc(title)}</text>')
        for i, s in enumerate(subs):
            fam = f' font-family="{MONO}"' if (mono_first and i == 0) else ""
            col = t["accent_sub"] if accent else t["sub"]
            a(f'<text x="{x + 16}" y="{top + 20 * (i + 1)}" font-size="12.5"{fam} fill="{col}">{esc(s)}</text>')

    def pill(x, y, w, label):
        a(f'<rect x="{x}" y="{y}" width="{w}" height="34" rx="10" fill="{t["box"]}" stroke="{t["stroke"]}" stroke-width="1"/>')
        a(f'<text x="{x + 14}" y="{y + 22}" font-size="13.5" fill="{t["text"]}">{esc(label)}</text>')

    def line(d, accent=False, dashed=False):
        col = t["accent"] if accent else t["arrow"]
        dash = ' stroke-dasharray="4 4"' if dashed else ""
        mk = "arrA" if accent else "arr"
        a(f'<path d="{d}" fill="none" stroke="{col}" stroke-width="1.6"{dash} stroke-linecap="round" marker-end="url(#{mk})"/>')

    def label(x, y, s, anchor="start"):
        a(f'<text x="{x}" y="{y}" font-size="12" text-anchor="{anchor}" fill="{t["sub"]}">{esc(s)}</text>')

    # lanes
    lane(24, 44, "来源")
    lane(224, 44, "接入")
    lane(434, 44, "入口 ADAPTER")
    lane(644, 44, "核心")
    lane(834, 44, "出口 ADAPTER")

    # sources
    for i, s in enumerate(["iPhone 快捷指令", "训练脚本 · 定时任务", "任意 HTTP 客户端"]):
        pill(24, 62 + i * 46, 160, s)
    # converge into API (center y=133)
    for i in range(3):
        y = 79 + i * 46
        line(f"M184 {y} C 204 {y}, 204 133, 222 133")

    box(224, 96, 176, 74, "HTTP API", ["POST /api/v1/messages", "token · Cloudflare Access"])
    line("M400 133 L 432 133")
    box(434, 96, 176, 74, "Kind", ["校验 data", "Compose → Envelope"])
    line("M610 133 L 642 133", accent=True)
    box(644, 96, 166, 74, "Envelope", ["标题 · 正文 · 字段", "时间 · 优先级"], accent=True)

    # down to SQLite
    line("M727 170 L 727 246", accent=True)
    label(737, 212, "存储 · 去重")
    box(644, 248, 166, 92, "SQLite", ["messages · deliveries", "senders · routes", "tokens"])

    # dispatcher + senders
    line("M810 294 L 834 294")
    box(836, 262, 140, 64, "调度器", ["投递 · 退避重试"])
    for i, s in enumerate(["Telegram", "Discord", "更多渠道…"]):
        pill(836, 62 + i * 46, 140, s)
    line("M906 262 L 906 200")

    # readers
    line("M644 294 L 612 294", dashed=True)
    box(224, 262, 386, 64, "读取", ["网页消息中心  ·  GET /api/v1/messages  ·  Telegram 命令"])

    # footnote
    a(f'<line x1="24" y1="372" x2="{W - 24}" y2="372" stroke="{t["stroke"]}" stroke-width="1"/>')
    a(f'<text x="24" y="402" font-size="13" fill="{t["text"]}"><tspan font-weight="600">Kind</tspan>'
      f'<tspan fill="{t["sub"]}">：每种消息一个，校验自己的字段并拼装成 Envelope。</tspan></text>')
    a(f'<text x="24" y="426" font-size="13" fill="{t["text"]}"><tspan font-weight="600">Sender</tspan>'
      f'<tspan fill="{t["sub"]}">：每个渠道一个，只认识 Envelope。核心只存储和投递 Envelope，新增类型或渠道都不用改它。</tspan></text>')

    a('</g></svg>')
    return "\n".join(o) + "\n"


out = Path(sys.argv[1])
for name, theme in THEMES.items():
    (out / f"architecture-{name}.svg").write_text(build(theme), encoding="utf-8")
    print("wrote", out / f"architecture-{name}.svg")
