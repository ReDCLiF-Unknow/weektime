#!/usr/bin/env python3
"""Regenerate the screenshots in docs/.

    python tools/screenshots/shoot.py

It starts a Weektime server on a spare port with an empty database, fills it
with a plausible week of work, drives headless Chrome over the DevTools
protocol, and writes the PNGs into docs/. Nothing it touches outlives the run:
the database, the browser profile and both processes live in a temporary
directory that is deleted at the end.

Needs Go, Chrome (or set the CHROME environment variable) and the `websockets`
package: python -m pip install websockets

Why DevTools rather than Chrome's own --screenshot flag: that flag ignores
prefers-color-scheme, so the light screenshot comes out as a second copy of
the dark one, and on Windows it clamps the window to about 476px, so a phone
screenshot is silently rendered at the wrong width. Emulation.* controls both
exactly, and Network.setCookie signs us in without a login form.
"""

import asyncio
import base64
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from datetime import date, timedelta
from pathlib import Path

try:
    import websockets
except ImportError:
    sys.exit("this needs the websockets package: python -m pip install websockets")

REPO = Path(__file__).resolve().parents[2]
DOCS = REPO / "docs"

# name, path, width, height, colour scheme, phone. "{share}" is the week's
# read-only link, which only exists once seed() has made it.
SHOTS = [
    ("week-dark.png", "/", 1280, 1160, "dark", False),
    ("week-light.png", "/", 1280, 1160, "light", False),
    ("shared-light.png", "{share}", 1280, 900, "light", False),
    ("mobile-dark.png", "/", 390, 844, "dark", True),
]

CHROMES = [
    os.environ.get("CHROME"),
    r"C:\Program Files\Google\Chrome\Application\chrome.exe",
    r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe",
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "google-chrome",
    "chromium",
]


def find_chrome():
    for c in CHROMES:
        if c and (Path(c).exists() or shutil.which(c)):
            return c
    sys.exit("no Chrome found; set CHROME to its path")


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def kill_tree(proc):
    """Stop a process and its children. `go run` builds and then execs the
    server as a child, so killing only the parent would leave it running."""
    if proc.poll() is not None:
        return
    if os.name == "nt":
        subprocess.run(["taskkill", "/T", "/F", "/PID", str(proc.pid)],
                       capture_output=True)
    else:
        proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


def wait_for(url, what, tries=60):
    for _ in range(tries):
        try:
            urllib.request.urlopen(url, timeout=1).read()
            return
        except (urllib.error.URLError, ConnectionError, OSError):
            time.sleep(1)
    sys.exit(f"{what} never came up at {url}")


class API:
    def __init__(self, base):
        self.base = base

    def form(self, path, token):
        """POST to one of the HTML endpoints, which answer with a redirect
        rather than JSON."""
        req = urllib.request.Request(self.base + path, data=b"", method="POST")
        req.add_header("Authorization", "Bearer " + token)
        urllib.request.urlopen(req).read()

    def __call__(self, path, body=None, token=None, method=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method)
        req.add_header("Content-Type", "application/json")
        if token:
            req.add_header("Authorization", "Bearer " + token)
        with urllib.request.urlopen(req) as r:
            return json.load(r) if r.length != 0 else None


def seed(api):
    """Fill an empty database with a week worth photographing, and return
    Alex's token and the share link's path. Dates are relative to this week,
    so "Today" is always right whenever this is run."""
    today = date.today()
    monday = today - timedelta(days=today.weekday())
    day = lambda n: (monday + timedelta(days=n)).isoformat()

    alex = api("/api/users", {"name": "Alex Morgan"})["token"]
    # Alex has had this timesheet a while, so is past the "bookmark your
    # private link" reminder a brand new one sees.
    api.form("/me/link/saved", alex)

    def log(d, start, end, note=""):
        api("/api/entries", {"date": day(d), "start": start, "end": end, "note": note}, alex)

    # This week: a freelancer's week with a client in it. All of it, whatever
    # day this runs on, so the screenshots look the same on a Monday as on a
    # Friday.
    week = [
        (0, "08:30", "10:00", "Inbox, invoices and planning the week"),
        (0, "10:15", "12:45", "Harbour Books: homepage wireframes"),
        (0, "13:30", "17:00", "Harbour Books: wireframes, second round"),
        (1, "09:00", "11:30", "Client call + proposal draft"),
        (1, "12:30", "16:15", "Proposal: scope and estimate"),
        (2, "09:15", "12:00", "Harbour Books: style guide"),
        (2, "13:00", "15:30", "Design review with Sam"),
        (3, "08:45", "12:15", "Checkout flow prototype"),
        (3, "13:15", "17:45", "Checkout flow prototype"),
        (4, "09:00", "10:30", "Usability test prep"),
        (4, "10:30", "12:00", "Usability test, round 1"),
    ]
    for d, start, end, note in week:
        log(d, start, end, note)

    # Earlier weeks, so the sidebar has some history.
    for back in (1, 2, 3):
        for d in range(5):
            log(d - 7 * back, "09:00", "12:30" if (d + back) % 2 else "12:00", "Harbour Books")
            log(d - 7 * back, "13:30", "17:00" if d != 4 else "15:00")

    share = api("/api/weeks/current/share", {}, alex)
    return alex, "/s/" + share["token"]


def page_target(port):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/json") as r:
        for t in json.load(r):
            if t["type"] == "page":
                return t["webSocketDebuggerUrl"]
    sys.exit("Chrome exposed no page to drive")


class CDP:
    """Just enough of the DevTools protocol: send a command, await its reply."""

    def __init__(self, ws):
        self.ws, self.n = ws, 0

    async def send(self, method, **params):
        self.n += 1
        await self.ws.send(json.dumps({"id": self.n, "method": method, "params": params}))
        while True:
            msg = json.loads(await self.ws.recv())
            if msg.get("id") == self.n:
                if "error" in msg:
                    sys.exit(f"{method}: {msg['error']}")
                return msg.get("result", {})

    async def wait_for(self, event, timeout=30):
        while True:
            msg = json.loads(await asyncio.wait_for(self.ws.recv(), timeout))
            if msg.get("method") == event:
                return msg


async def capture(debug_port, base, token, share):
    async with websockets.connect(page_target(debug_port), max_size=64 * 1024 * 1024) as ws:
        cdp = CDP(ws)
        await cdp.send("Page.enable")
        await cdp.send("Network.enable")
        # Sign in without a login form. The app's cookie is HttpOnly, which
        # only stops page scripts from reading it, not DevTools from setting it.
        await cdp.send("Network.setCookie", name="weektime_token", value=token,
                       domain="localhost", path="/", httpOnly=True)
        for name, path, w, h, scheme, phone in SHOTS:
            await cdp.send("Emulation.setDeviceMetricsOverride", width=w, height=h,
                           deviceScaleFactor=2, mobile=phone, screenWidth=w, screenHeight=h)
            await cdp.send("Emulation.setEmulatedMedia", media="screen",
                           features=[{"name": "prefers-color-scheme", "value": scheme}])
            await cdp.send("Emulation.setTouchEmulationEnabled", enabled=phone, maxTouchPoints=5)
            # The shared week is photographed as the stranger it was sent to
            # sees it: 127.0.0.1 is another site to the browser, so the
            # owner's cookie for localhost does not go with the request.
            origin = base.replace("localhost", "127.0.0.1") if "{share}" in path else base
            await cdp.send("Page.navigate", url=origin + path.format(share=share))
            await cdp.wait_for("Page.loadEventFired")
            await asyncio.sleep(1.5)  # let the fonts settle and the event stream open
            shot = await cdp.send("Page.captureScreenshot", format="png", captureBeyondViewport=False)
            out = DOCS / name
            out.write_bytes(base64.b64decode(shot["data"]))
            print(f"  {name}  {w}x{h} @2x {scheme}{' phone' if phone else ''}")


def main():
    chrome = find_chrome()
    port, debug_port = free_port(), free_port()
    base = f"http://localhost:{port}"
    work = Path(tempfile.mkdtemp(prefix="weektime-shots-"))
    server = browser = None
    try:
        print(f"starting a server on {port}")
        server = subprocess.Popen(
            ["go", "run", "./cmd/server", "-addr", f"localhost:{port}", "-db", str(work / "demo.db")],
            cwd=REPO, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        wait_for(base + "/welcome", "the server")

        print("seeding the demo data")
        token, share = seed(API(base))

        print("starting Chrome")
        browser = subprocess.Popen(
            [chrome, "--headless", "--disable-gpu", "--no-sandbox", "--no-first-run",
             "--hide-scrollbars", f"--remote-debugging-port={debug_port}",
             f"--user-data-dir={work / 'profile'}", "about:blank"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        wait_for(f"http://127.0.0.1:{debug_port}/json/version", "Chrome")

        print(f"writing to {DOCS}")
        asyncio.run(capture(debug_port, base, token, share))
        print("done")
    finally:
        for p in (browser, server):
            if p:
                kill_tree(p)
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    main()
