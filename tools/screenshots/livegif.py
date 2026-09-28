#!/usr/bin/env python3
"""Record docs/live.gif: a shared week updating while its owner logs time.

    python tools/screenshots/livegif.py

Alex logs time on a laptop; Sam, the client Alex shared the week with, has
the read-only link open on a phone. Each is a headless Chrome of its own (one
Chrome would share its cookies between them, and Sam is meant to be nobody in
particular). Alex logs an entry and it turns up on Sam's screen with the
totals; Alex logs the next one, which starts where the last one ended; Alex
revokes the link and Sam's page says so.

It is a storyboard rather than a screen recording: each frame is taken when
the page has reached the state it shows, and held for as long as the story
needs. So the GIF is the same on a fast machine and a slow one, and never
catches a page half drawn. The changes that reach Sam really do arrive
through the app's live updates; nothing is reloaded by the script.

Needs what shoot.py needs, plus Pillow: python -m pip install pillow
"""

import asyncio
import base64
import io
import json
import shutil
import subprocess
import sys
import tempfile
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from shoot import API, CDP, DOCS, REPO, find_chrome, free_port, kill_tree, page_target, seed, wait_for  # noqa: E402

try:
    import websockets
    from PIL import Image, ImageDraw, ImageFont
except ImportError:
    sys.exit("this needs websockets and Pillow: python -m pip install websockets pillow")

OUT = DOCS / "live.gif"
LAPTOP = (1024, 800)
PHONE = (390, 800)
SCALE = 0.8   # of the finished frame, to keep the file small
GAP = 28      # around and between the two screens
LABEL = 34    # the names above them
CAPTION = 58  # what is happening, underneath
BG = (233, 236, 242)
INK = (29, 39, 59)

# Every click shows where it landed, as a finger or a mouse would.
TAPS = """
addEventListener('DOMContentLoaded', () => {
  const s = document.createElement('style');
  s.textContent = '.__tap{position:fixed;width:38px;height:38px;margin:-19px 0 0 -19px;border-radius:50%;' +
    'background:rgba(32,107,196,.28);border:2px solid rgba(32,107,196,.8);pointer-events:none;z-index:99999}';
  document.head.appendChild(s);
});
addEventListener('pointerdown', e => {
  const d = document.createElement('div');
  d.className = '__tap';
  d.style.left = e.clientX + 'px';
  d.style.top = e.clientY + 'px';
  document.documentElement.appendChild(d);
  setTimeout(() => d.remove(), 700);
}, true);
"""

# __centre(sel): where to click on something, after bringing it into view.
# __row(text): the entry (on the owner's page) or table row (on a shared
# week) whose note contains text.
HELPERS = """
window.__row = text => [...document.querySelectorAll('.entry-row, tbody tr')].find(r => r.textContent.includes(text));
// Scroll only when something is not comfortably on screen (clear of the
// phone's tab bar), and instantly: positions must be read after the page
// has stopped moving.
window.__show = (el, block) => {
  const r = el.getBoundingClientRect();
  if (r.top < 70 || r.bottom > innerHeight - 90) el.scrollIntoView({block: block || 'center', behavior: 'instant'});
};
window.__centre = sel => {
  const el = document.querySelector(sel);
  if (!el) return null;
  __show(el);
  const r = el.getBoundingClientRect();
  return [r.left + r.width / 2, r.top + r.height / 2];
};
// A time field cannot be typed into the same way everywhere, so it is set
// the way the page's own script hears about it: a value and an input event.
window.__set = (sel, value) => {
  const el = document.querySelector(sel);
  el.value = value;
  el.dispatchEvent(new Event('input', {bubbles: true}));
};
"""


def font(size, bold=False):
    names = ["segoeuib.ttf" if bold else "segoeui.ttf", "DejaVuSans-Bold.ttf" if bold else "DejaVuSans.ttf",
             "Arial Bold.ttf" if bold else "Arial.ttf"]
    for name in names:
        try:
            return ImageFont.truetype(name, size)
        except OSError:
            pass
    return ImageFont.load_default(size=size)


class Screen:
    """One person's browser."""

    def __init__(self, name, device, cdp, size, phone):
        self.name, self.device, self.cdp, self.size, self.phone = name, device, cdp, size, phone
        self.last = None

    async def open(self, base, token, path):
        c = self.cdp
        await c.send("Page.enable")
        await c.send("Network.enable")
        if token:
            await c.send("Network.setCookie", name="weektime_token", value=token, domain="localhost", path="/", httpOnly=True)
        w, h = self.size
        await c.send("Emulation.setDeviceMetricsOverride", width=w, height=h, deviceScaleFactor=1,
                     mobile=self.phone, screenWidth=w, screenHeight=h)
        await c.send("Emulation.setEmulatedMedia", media="screen",
                     features=[{"name": "prefers-color-scheme", "value": "light"}])
        await c.send("Page.addScriptToEvaluateOnNewDocument", source=TAPS + HELPERS)
        await c.send("Page.navigate", url=base + path)
        await c.wait_for("Page.loadEventFired")
        await asyncio.sleep(1.5)  # fonts, and the event stream opening

    async def js(self, expr):
        r = await self.cdp.send("Runtime.evaluate", expression=expr, awaitPromise=True, returnByValue=True)
        if "exceptionDetails" in r:
            sys.exit(f"{self.name}: {expr[:60]}...: {r['exceptionDetails']}")
        return r.get("result", {}).get("value")

    async def until(self, expr, what, timeout=8):
        for _ in range(int(timeout / 0.05)):
            if await self.js(expr):
                return
            await asyncio.sleep(0.05)
        state = await self.js("JSON.stringify({url: location.href, title: document.title, "
                              "text: document.body.innerText.replace(/\\s+/g, ' ').slice(0, 400)})")
        sys.exit(f"{self.name}'s screen never showed {what}; it shows {state}")

    async def click(self, sel):
        at = await self.js(f"__centre({json.dumps(sel)})")
        if not at:
            sys.exit(f"{self.name}: nothing to click at {sel!r}")
        await asyncio.sleep(0.15)  # let a scroll settle
        x, y = at
        for kind in ("mousePressed", "mouseReleased"):
            await self.cdp.send("Input.dispatchMouseEvent", type=kind, x=x, y=y, button="left", clickCount=1)
        # Then out of the way, so whatever ends up under it is not left
        # looking hovered.
        await self.cdp.send("Input.dispatchMouseEvent", type="mouseMoved", x=0, y=0)

    async def reveal(self, text):
        """Bring the row with text in it into view, if it is not already."""
        await self.js(f"__show(__row({json.dumps(text)}))")

    async def type(self, text, per_frame=3, film=None):
        for i in range(0, len(text), per_frame):
            await self.cdp.send("Input.insertText", text=text[i:i + per_frame])
            if film:
                await film.frame(90)

    async def scroll_to(self, sel):
        await self.js(f"document.querySelector({json.dumps(sel)}).scrollIntoView({{block: 'start', behavior: 'instant'}}); "
                      "window.scrollBy({top: -12, behavior: 'instant'})")

    async def shoot(self):
        shot = await self.cdp.send("Page.captureScreenshot", format="png")
        self.last = Image.open(io.BytesIO(base64.b64decode(shot["data"]))).convert("RGB")
        return self.last


class Film:
    """Frames of the two screens side by side, with a caption."""

    def __init__(self, left, right):
        self.left, self.right = left, right
        self.caption = ""
        self.frames, self.durations = [], []
        self.label_font, self.caption_font = font(19, bold=True), font(23)

    async def frame(self, ms):
        if ms >= 1000:
            # A tap's ring belongs to the moment of the tap, not to the long
            # look at what it did.
            for screen in (self.left, self.right):
                await screen.js("document.querySelectorAll('.__tap').forEach(t => t.remove())")
        a, b = await self.left.shoot(), await self.right.shoot()
        w = GAP * 3 + a.width + b.width
        h = LABEL + GAP + max(a.height, b.height) + CAPTION
        img = Image.new("RGB", (w, h), BG)
        d = ImageDraw.Draw(img)
        x = GAP
        for screen, shot in ((self.left, a), (self.right, b)):
            d.text((x, LABEL - 10), f"{screen.name} · {screen.device}", font=self.label_font, fill=INK, anchor="ls")
            img.paste(shot, (x, LABEL))
            d.rectangle([x - 1, LABEL - 1, x + shot.width, LABEL + shot.height], outline=(200, 205, 214))
            x += shot.width + GAP
        d.text((w / 2, h - CAPTION / 2 + 2), self.caption, font=self.caption_font, fill=INK, anchor="mm")
        self.frames.append(img.resize((round(w * SCALE), round(h * SCALE)), Image.LANCZOS))
        self.durations.append(ms)

    def save(self, path):
        # One palette for the whole film, so colours do not flicker between frames.
        palette = self.frames[0].quantize(colors=255, method=Image.Quantize.MEDIANCUT)
        frames = [f.quantize(palette=palette, dither=Image.Dither.NONE) for f in self.frames]
        frames[0].save(path, save_all=True, append_images=frames[1:], duration=self.durations,
                       loop=0, optimize=True, disposal=1)


async def record(ports, base, token, share):
    async with websockets.connect(page_target(ports[0]), max_size=64 << 20) as ws_a, \
            websockets.connect(page_target(ports[1]), max_size=64 << 20) as ws_s:
        alex = Screen("Alex", "laptop", CDP(ws_a), LAPTOP, False)
        sam = Screen("Sam, the client", "phone", CDP(ws_s), PHONE, True)
        await alex.open(base, token, "/")
        await sam.open(base, None, share)
        await alex.scroll_to("#add-entry-form")
        film = Film(alex, sam)

        film.caption = "Alex shared this week with a client, Sam, who has the link open on a phone."
        await film.frame(3000)

        # Alex logs the morning; it turns up on Sam's screen by itself.
        film.caption = "Alex logs the morning…"
        await alex.click("#add-entry-form input[name=start]")
        await alex.js("__set('#add-entry-form input[name=start]', '09:00')")
        await film.frame(500)
        await alex.js("__set('#add-entry-form input[name=end]', '11:30')")
        await film.frame(700)  # the button now says "Log 2h 30m"
        await alex.click("#add-entry-form input[name=note]")
        await alex.type("Harbour Books: checkout flow", film=film)
        await alex.click("#add-entry-form button[type=submit]")
        await film.frame(250)
        await alex.until("!!__row('checkout flow')", "the new entry")
        await sam.until("!!__row('checkout flow')", "Alex's new entry")
        await sam.reveal("checkout flow")
        film.caption = "…and it is on Sam's screen at once, with the totals. Nobody reloads anything."
        await film.frame(3200)

        # The next entry starts where the last one ended.
        film.caption = "The next entry starts where the last one ended, so Alex only picks the end."
        await film.frame(2200)
        await alex.js("__set('#add-entry-form input[name=end]', '12:45')")
        await film.frame(700)
        await alex.click("#add-entry-form input[name=note]")
        await alex.type("Call with Sam", film=film)
        await alex.click("#add-entry-form button[type=submit]")
        await film.frame(250)
        await sam.until("!!__row('Call with Sam')", "the second entry")
        await sam.reveal("Call with Sam")
        film.caption = "Sam sees each entry, its duration, the day's total and the week's."
        await film.frame(3000)

        # Alex turns the link off; Sam's page says so.
        film.caption = "Alex can turn the link off at any time…"
        await alex.js("window.scrollTo({top: 0, behavior: 'instant'}); window.confirm = () => true")
        await alex.click('[data-bs-target="#share-modal"]')
        await alex.until("document.getElementById('share-modal').classList.contains('show')", "the share dialog")
        await asyncio.sleep(0.4)  # the dialog fades in
        await film.frame(1600)
        await alex.click("#share-modal button.btn-outline-danger")
        # Sam's page finds out and reloads itself. A page cannot be
        # photographed halfway through loading, so the next frame waits
        # until it has finished.
        await sam.cdp.wait_for("Page.loadEventFired")
        await sam.until("document.title.startsWith('Link turned off')", "the link turned off")
        await asyncio.sleep(0.8)  # fonts, on the page it reloaded to
        film.caption = "…and it stops working that moment, for anyone who has it."
        await film.frame(3200)

        film.caption = "Weektime: a weekly timesheet you can share with a link, on your own server."
        await film.frame(3000)
        return film


def main():
    chrome = find_chrome()
    port = free_port()
    ports = (free_port(), free_port())
    base = f"http://localhost:{port}"
    work = Path(tempfile.mkdtemp(prefix="weektime-gif-"))
    procs = []
    try:
        print(f"starting a server on {port}")
        procs.append(subprocess.Popen(
            ["go", "run", "./cmd/server", "-addr", f"localhost:{port}", "-db", str(work / "demo.db")],
            cwd=REPO, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        wait_for(base + "/welcome", "the server")

        print("seeding the demo data")
        api = API(base)
        token, share = seed(api)
        # Alex logs today in the film, so today starts empty whatever day of
        # the week this is run on.
        today = date.today().isoformat()
        for day in api("/api/weeks/current", token=token)["days"]:
            if day["date"] == today:
                for e in day["entries"]:
                    api(f"/api/entries/{e['id']}", token=token, method="DELETE")

        print("starting two Chromes")
        for i, dp in enumerate(ports):
            procs.append(subprocess.Popen(
                [chrome, "--headless", "--disable-gpu", "--no-sandbox", "--no-first-run",
                 "--hide-scrollbars", f"--remote-debugging-port={dp}",
                 f"--user-data-dir={work / f'profile{i}'}", "about:blank"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
            wait_for(f"http://127.0.0.1:{dp}/json/version", "Chrome")

        print("recording")
        film = asyncio.run(record(ports, base, token, share))
        film.save(OUT)
        seconds = sum(film.durations) / 1000
        print(f"wrote {OUT}: {len(film.frames)} frames, {seconds:.0f}s, "
              f"{film.frames[0].width}x{film.frames[0].height}, {OUT.stat().st_size // 1024} KB")
    finally:
        for p in reversed(procs):
            kill_tree(p)
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    main()
