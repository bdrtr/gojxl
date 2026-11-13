#!/usr/bin/env python3
"""Writes the README charts (docs/*-light.svg, docs/*-dark.svg) from
docs/bench.json. Static SVG: horizontal bars, one row per task, one bar per
tool, value labels on every bar (some palette steps are below 3:1 contrast
on the light background, so the numbers carry the reading)."""
import json, os, html

HERE = os.path.dirname(os.path.abspath(__file__))
THEMES = {
    "light": dict(text="#1f2328", muted="#59636e", grid="#d1d9e0",
                  series=["#2a78d6", "#eb6834", "#1baf7a", "#eda100"]),
    "dark": dict(text="#f0f6fc", muted="#9198a1", grid="#3d444d",
                 series=["#3987e5", "#d95926", "#199e70", "#c98500"]),
}
FONT = "font-family=\"-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif\""


def bar_path(x0, y, w, h, r=4):
    """A bar from x0 to x0+w (w may be negative) with only its data end rounded."""
    if w < 0:
        x0, w, left = x0 + w, -w, True
    else:
        left = False
    r = min(r, w / 2, h / 2)
    if left:  # rounded on the left (negative values)
        return (f"M{x0 + w:.1f},{y}h{-(w - r):.1f}a{r},{r} 0 0 0 {-r},{r}"
                f"v{h - 2 * r}a{r},{r} 0 0 0 {r},{r}h{w - r:.1f}z")
    return (f"M{x0:.1f},{y}h{w - r:.1f}a{r},{r} 0 0 1 {r},{r}"
            f"v{h - 2 * r}a{r},{r} 0 0 1 {-r},{r}h{-(w - r):.1f}z")


def chart(spec, theme):
    t = THEMES[theme]
    series, rows = spec["series"], spec["rows"]
    W, left, right = 760, 230, 70
    bar_h, gap, row_gap = 14, 2, 18
    top = 64
    plot_w = W - left - right
    vals = [v for r in rows for v in r["values"] if v is not None]
    lo, hi = min(0, min(vals)), max(0, max(vals))
    scale = plot_w / (hi - lo)
    x_of = lambda v: left + (v - lo) * scale
    row_h = len(series) * (bar_h + gap) - gap
    H = top + len(rows) * (row_h + row_gap) + 26
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-label="{html.escape(spec["title"])}">',
           f'<g {FONT}>',
           f'<text x="0" y="20" font-size="16" font-weight="600" fill="{t["text"]}">{html.escape(spec["title"])}</text>',
           f'<text x="0" y="40" font-size="12" fill="{t["muted"]}">{html.escape(spec["subtitle"])}</text>']
    # legend (always, >= 2 series)
    lx = left
    for i, s in enumerate(series):
        out.append(f'<rect x="{lx}" y="48" width="10" height="10" rx="2" fill="{t["series"][i]}"/>')
        out.append(f'<text x="{lx + 14}" y="57" font-size="12" fill="{t["text"]}">{html.escape(s)}</text>')
        lx += 22 + 7.2 * len(s)
    # zero line
    out.append(f'<line x1="{x_of(0):.1f}" y1="{top - 4}" x2="{x_of(0):.1f}" y2="{H - 22}" stroke="{t["grid"]}" stroke-width="1"/>')
    y = top
    for r in rows:
        out.append(f'<text x="{left - 10}" y="{y + row_h / 2 + 4:.1f}" font-size="13" text-anchor="end" fill="{t["text"]}">{html.escape(r["label"])}</text>')
        for i, v in enumerate(r["values"]):
            by = y + i * (bar_h + gap)
            if v is None:
                out.append(f'<text x="{x_of(0) + 6:.1f}" y="{by + 11}" font-size="11" fill="{t["muted"]}">n/a</text>')
                continue
            w = (v - 0) * scale
            out.append(f'<path d="{bar_path(x_of(0), by, w, bar_h)}" fill="{t["series"][i]}"><title>{html.escape(series[i])}: {spec["fmt"].format(v)}</title></path>')
            lab = spec["fmt"].format(v)
            if v >= 0:
                out.append(f'<text x="{x_of(v) + 5:.1f}" y="{by + 11}" font-size="11" fill="{t["muted"]}">{lab}</text>')
            else:
                out.append(f'<text x="{x_of(v) - 5:.1f}" y="{by + 11}" font-size="11" text-anchor="end" fill="{t["muted"]}">{lab}</text>')
        y += row_h + row_gap
    out.append(f'<text x="{left}" y="{H - 6}" font-size="11" fill="{t["muted"]}">{html.escape(spec["note"])}</text>')
    out.append('</g></svg>')
    return "\n".join(out)


def main():
    specs = json.load(open(os.path.join(HERE, "bench.json")))
    for name, spec in specs.items():
        for theme in THEMES:
            with open(os.path.join(HERE, f"{name}-{theme}.svg"), "w") as f:
                f.write(chart(spec, theme) + "\n")


if __name__ == "__main__":
    main()
