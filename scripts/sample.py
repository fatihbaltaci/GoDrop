#!/usr/bin/env python3
"""Render site/logo-lockup.svg into the picture `godrop init` leaves on disk.

Setup writes that picture next to the generated configuration so that the first
example it prints uploads a real file. It is the logo, rendered here rather
than at run time: Go has no SVG renderer, and a browser is the one renderer
every machine already has.

Also written: a stamp of the source SVG, so a test can tell that the logo
changed without this being run again, on a machine with no browser on it.
Run: make docs
"""

import hashlib
import pathlib
import shutil
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
SOURCE = ROOT / "site" / "logo-lockup.svg"
OUT = ROOT / "internal" / "wizard" / "assets" / "sample.png"
STAMP = OUT.with_suffix(".source")

# The lockup's own dimensions, at twice the size: the picture is meant to look
# like a picture, not like a favicon.
WIDTH, HEIGHT, SCALE = 830, 240, 2

CHROMES = [
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "/Applications/Chromium.app/Contents/MacOS/Chromium",
    "google-chrome",
    "chromium",
    "chromium-browser",
]


def chrome() -> str | None:
    for candidate in CHROMES:
        if pathlib.Path(candidate).exists() or shutil.which(candidate):
            return candidate
    return None


def render(browser: str) -> bytes:
    # Chrome screenshots a file it can navigate to; the SVG is copied so that
    # the shot is of the picture alone, without the repository around it.
    with tempfile.TemporaryDirectory() as tmp:
        svg = pathlib.Path(tmp) / "logo.svg"
        png = pathlib.Path(tmp) / "shot.png"
        svg.write_bytes(SOURCE.read_bytes())
        subprocess.run(
            [
                browser,
                "--headless",
                "--disable-gpu",
                f"--screenshot={png}",
                f"--window-size={WIDTH},{HEIGHT}",
                f"--force-device-scale-factor={SCALE}",
                # Transparent, so the picture sits on whatever is behind it.
                "--default-background-color=00000000",
                svg.as_uri(),
            ],
            check=True,
            capture_output=True,
        )
        return png.read_bytes()


def main() -> int:
    browser = chrome()
    if browser is None:
        # Not fatal: the committed picture is still the right one, and the
        # test that compares the stamp says so if it is not.
        print("no chrome or chromium found; leaving sample.png as it is", file=sys.stderr)
        return 0
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_bytes(render(browser))
    STAMP.write_text(hashlib.sha256(SOURCE.read_bytes()).hexdigest() + "\n")
    print(f"wrote {OUT.relative_to(ROOT)} ({OUT.stat().st_size} bytes)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
