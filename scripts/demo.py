#!/usr/bin/env python3
"""Generate site/demo.svg: the upload, as a terminal recording.

The session below is the source. Everything else, the layout, the typing, the
timeline, is computed, so changing a line of the demo means editing one string
rather than hand-tuning an animation. Run: make docs

The animation is SMIL, which browsers run inside an <img>, which is how GitHub
serves an SVG in a README. The static attributes hold the *finished* frame, so
a renderer that ignores animation still shows the whole session rather than an
empty terminal.
"""

import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parent.parent
OUT = ROOT / "site" / "demo.svg"

# The session. "in" is typed, "cont" continues the previous command without a
# new prompt, "out" arrives all at once, "note" is a comment.
SESSION = [
    ("in", 'curl -X POST -H "Authorization: Bearer $GODROP_TOKEN" \\'),
    ("cont", '     -F "file=@photo.jpg" https://files.example.com/upload'),
    ("out", "{"),
    ("out", '  "files": [{ "url": "https://files.example.com/f/20260815-143022-8f4e2c91…/photo.jpg",'),
    ("out", '              "name": "photo.jpg", "size_bytes": 12345 }]'),
    ("out", "}"),
    ("in", "curl -O https://files.example.com/f/20260815-143022-8f4e2c91…/photo.jpg"),
    ("note", "# no token needed to download, and nobody can guess the URL"),
]

PROMPT = "$ "

# Layout, in pixels. The advance is 0.6em, which every monospace font uses.
FONT = 14
ADVANCE = FONT * 0.6
LINE = 22
PAD_X = 20
PAD_Y = 16
BAR = 34
# The window is as wide as the widest line, never narrower than this. Measuring
# the session rather than fixing a width is what keeps a URL from running off
# the right-hand edge when a line in it gets longer.
MIN_COLUMNS = 78
COLUMNS = max(MIN_COLUMNS, max(len(PROMPT if kind == "in" else "") + len(text)
                               for kind, text in SESSION) + 1)

# Timing, in seconds.
TYPE_PER_CHAR = 0.032
AFTER_TYPING = 0.45
BETWEEN_OUTPUT = 0.09
AFTER_BLOCK = 0.7
HOLD = 2.6
START = 0.5


def runs(kind: str, text: str):
    """Split a line into (text, class) runs, which is all the highlighting."""
    if kind == "note":
        return [(text, "note")]
    if kind in ("in", "cont"):
        return shell_runs(text)
    return json_runs(text)


def shell_runs(text: str):
    """Colour quoted strings and URLs; everything else is plain command text."""
    out, last = [], 0
    for m in re.finditer(r'"[^"]*"|https?://\S+', text):
        if m.start() > last:
            out.append((text[last : m.start()], "cmd"))
        out.append((m.group(), "str" if m.group().startswith('"') else "url"))
        last = m.end()
    if last < len(text):
        out.append((text[last:], "cmd"))
    return out


def json_runs(text: str):
    """Colour keys, string values and numbers; punctuation stays muted."""
    out, last = [], 0
    for m in re.finditer(r'"[^"]*"(\s*:)?|\b\d+\b', text):
        if m.start() > last:
            out.append((text[last : m.start()], "punct"))
        token = m.group()
        if m.group(1):
            out.append((token[: -len(m.group(1))], "key"))
            out.append((m.group(1), "punct"))
        elif token.startswith('"'):
            out.append((token, "val"))
        else:
            out.append((token, "num"))
        last = m.end()
    if last < len(text):
        out.append((text[last:], "punct"))
    return out


def xs(start_col: int, text: str) -> str:
    """One x per character, so the layout does not depend on font metrics."""
    return " ".join(f"{PAD_X + (start_col + i) * ADVANCE:.1f}" for i in range(len(text)))


def text_element(row: int, col: int, parts) -> str:
    y = BAR + PAD_Y + row * LINE + FONT
    spans = []
    for chunk, cls in parts:
        if not chunk:
            continue
        spans.append(f'<tspan class="{cls}" x="{xs(col, chunk)}">{escape(chunk)}</tspan>')
        col += len(chunk)
    return f'<text y="{y:.0f}" xml:space="preserve">{"".join(spans)}</text>'


def escape(text: str) -> str:
    return text.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def keytimes(cycle: float, times) -> str:
    return ";".join(f"{min(t / cycle, 1):.4f}" for t in times)


def build() -> str:
    # First pass: when does each line appear, and how long is the cycle.
    plan, t = [], START
    for kind, text in SESSION:
        col = len(PROMPT) if kind == "in" else 0
        if kind in ("in", "cont"):
            typing = len(text) * TYPE_PER_CHAR
            plan.append({"kind": kind, "text": text, "col": col, "at": t, "typing": typing})
            t += typing + AFTER_TYPING
        else:
            plan.append({"kind": kind, "text": text, "col": col, "at": t, "typing": 0})
            t += BETWEEN_OUTPUT
        if kind == "out" and text == "}":
            t += AFTER_BLOCK
    cycle = t + HOLD

    rows = []
    for row, line in enumerate(plan):
        rows.append(render(row, line, cycle))

    height = BAR + PAD_Y * 2 + len(plan) * LINE
    width = PAD_X * 2 + COLUMNS * ADVANCE
    return TEMPLATE.format(
        width=f"{width:.0f}",
        height=f"{height:.0f}",
        bar=BAR,
        dots=dots(),
        body="\n    ".join(rows),
        title_x=f"{width / 2:.0f}",
        title_y=f"{BAR / 2 + 4:.0f}",
    )


def render(row: int, line: dict, cycle: float) -> str:
    """One line: a prompt, the text, and whatever animation reveals it."""
    kind, text, at = line["kind"], line["text"], line["at"]
    parts = []
    if kind == "in":
        # The prompt arrives with the line, not at the start of the loop.
        parts.append(appears(text_element(row, 0, [(PROMPT, "prompt")]), at, cycle))

    body = text_element(row, line["col"], runs(kind, text))

    if kind in ("in", "cont"):
        # Typed: a clip rectangle grows one character at a time.
        cid = f"type{row}"
        steps = len(text)
        widths = [0.0] + [PAD_X + (line["col"] + i + 1) * ADVANCE for i in range(steps)]
        times = [0.0, at] + [at + (i + 1) * line["typing"] / steps for i in range(steps - 1)]
        full = f"{widths[-1]:.1f}"
        parts.append(
            f'<clipPath id="{cid}"><rect x="0" y="0" height="100%" width="{full}">'
            f'<animate attributeName="width" calcMode="discrete" '
            f'values="{";".join(f"{w:.1f}" for w in widths)}" '
            f'keyTimes="{keytimes(cycle, times)}" dur="{cycle:.2f}s" repeatCount="indefinite"/>'
            f"</rect></clipPath>"
        )
        parts.append(f'<g clip-path="url(#{cid})">{body}</g>')
        parts.append(cursor(row, line, cycle))
    else:
        # Output: it is there, or it is not.
        parts.append(appears(body, at, cycle))
    return "".join(parts)


def appears(markup: str, at: float, cycle: float) -> str:
    """Wrap markup so that it shows up at `at` and stays for the rest of the loop."""
    return (
        f'<g opacity="1"><animate attributeName="opacity" calcMode="discrete" '
        f'values="0;1" keyTimes="{keytimes(cycle, [0, at])}" '
        f'dur="{cycle:.2f}s" repeatCount="indefinite"/>{markup}</g>'
    )


def cursor(row: int, line: dict, cycle: float) -> str:
    """A block cursor that keeps up with the typing and blinks when it stops."""
    at, typing, steps = line["at"], line["typing"], len(line["text"])
    y = BAR + PAD_Y + row * LINE + 4
    positions = [PAD_X + line["col"] * ADVANCE] + [
        PAD_X + (line["col"] + i + 1) * ADVANCE for i in range(steps)
    ]
    times = [0.0, at] + [at + (i + 1) * typing / steps for i in range(steps - 1)]
    end = at + typing + AFTER_TYPING
    return (
        f'<rect class="cursor" y="{y:.0f}" width="{ADVANCE:.1f}" height="{FONT + 3}" '
        f'x="{positions[-1]:.1f}" opacity="0">'
        f'<animate attributeName="x" calcMode="discrete" '
        f'values="{";".join(f"{x:.1f}" for x in positions)}" '
        f'keyTimes="{keytimes(cycle, times)}" dur="{cycle:.2f}s" repeatCount="indefinite"/>'
        f'<animate attributeName="opacity" calcMode="discrete" values="0;1;0" '
        f'keyTimes="{keytimes(cycle, [0, at, end])}" dur="{cycle:.2f}s" repeatCount="indefinite"/>'
        f"</rect>"
    )


def dots() -> str:
    colours = ["#ff5f57", "#febc2e", "#28c840"]
    return "".join(
        f'<circle cx="{18 + i * 18}" cy="{BAR / 2:.0f}" r="6" fill="{c}" opacity="0.85"/>'
        for i, c in enumerate(colours)
    )


TEMPLATE = """<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {width} {height}" \
width="{width}" height="{height}" role="img" \
aria-label="Uploading a file with curl and getting back a hard-to-guess URL">
  <title>godrop: upload a file, get a hard-to-guess URL</title>
  <!-- Generated by scripts/demo.py. Edit the session there, not this file. -->
  <style>
    text {{
      font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas,
                   "DejaVu Sans Mono", monospace;
      font-size: 14px;
    }}
    .prompt {{ fill: #4ade9f; }}
    .cmd    {{ fill: #eceae5; }}
    .str    {{ fill: #7dd3fc; }}
    .url    {{ fill: #7dd3fc; }}
    .key    {{ fill: #7dd3fc; }}
    .val    {{ fill: #4ade9f; }}
    .num    {{ fill: #f0b429; }}
    .punct  {{ fill: #9b9b93; }}
    .note   {{ fill: #6b6b66; }}
    .cursor {{ fill: #4ade9f; opacity: 0.9; }}
    .title  {{ fill: #6b6b66; font-size: 12px; }}
  </style>
  <rect width="{width}" height="{height}" rx="10" fill="#0f1115"/>
  <rect width="{width}" height="{bar}" rx="10" fill="#191c22"/>
  <rect y="{bar}" width="{width}" height="1" fill="#000" opacity="0.35"/>
  {dots}
  <text class="title" x="{title_x}" y="{title_y}" text-anchor="middle">godrop</text>
  <g>
    {body}
  </g>
</svg>
"""


def main() -> None:
    OUT.write_text(build())
    print(f"wrote site/{OUT.name}")


if __name__ == "__main__":
    main()
