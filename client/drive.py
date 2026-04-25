#!/usr/bin/env python3
"""
Local laptop teleop + recorder for the Earth Rover Mini custom firmware.

Run while connected to the frodobot Wi-Fi AP (or via tailnet):
    python3 drive.py

Then open http://localhost:9000/ in any browser.

The browser captures real keydown/keyup events (no jolty terminal auto-repeat),
proxies motor commands to the robot's HTTP API, and toggles a local ffmpeg
subprocess that records the front camera to ~/Downloads/frodobot/sessions/.

Why this lives on the laptop instead of the robot:
  - Browser native multi-key handling beats any terminal hack
  - Recording locally cuts a network hop for the H.265 frames
  - Keeps the robot's API surface minimal — robot stays a clean control plane
  - No Jetson required for data collection

Keys:
  W A S D    drive
  space      stop
  r          toggle recording
  + / -      adjust speed step
"""

import argparse
import json
import subprocess
import threading
import urllib.request
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

DEFAULTS = dict(
    api="http://192.168.11.1:8080",
    rtsp="rtsp://192.168.11.1:554/live/0",
    sessions=str(Path.home() / "Downloads" / "frodobot" / "sessions"),
    port=9000,
    bind="127.0.0.1",
)


# ----- shared state ----------------------------------------------------------

class State:
    def __init__(self):
        self.lock = threading.Lock()
        self.recording = False
        self.ffmpeg: subprocess.Popen | None = None
        self.session_dir: Path | None = None
        self.last_motor = {"speed": 0, "angular": 0}


state = State()


# ----- HTML page (embedded, no static files needed) --------------------------

INDEX_HTML = """\
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Frodobot Drive</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { font-family: -apple-system, BlinkMacSystemFont, "SF Pro", system-ui, sans-serif;
         background: #0c0c0d; color: #eaeaea; margin: 0; padding: 24px; max-width: 880px; }
  h1 { font-size: 22px; margin: 0 0 16px; letter-spacing: 0.05em; }
  .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
  .panel { background: #181819; border: 1px solid #2a2a2c; border-radius: 10px; padding: 18px; }
  .panel h2 { font-size: 11px; text-transform: uppercase; letter-spacing: 0.12em;
              color: #888; margin: 0 0 14px; font-weight: 600; }
  .row { display: flex; align-items: center; gap: 12px; margin-bottom: 12px; }
  .row:last-child { margin-bottom: 0; }
  .label { font-size: 12px; color: #888; width: 64px; }
  .value { font-family: ui-monospace, "SF Mono", monospace; font-size: 14px;
           color: #eaeaea; min-width: 44px; text-align: right; }
  .bar { flex: 1; background: #232325; height: 8px; border-radius: 4px; overflow: hidden; position: relative; }
  .bar::before { content: ""; position: absolute; left: 50%; top: 0; bottom: 0; width: 1px; background: #444; }
  .bar-fg { position: absolute; top: 0; bottom: 0; background: #4ade80; transition: all 80ms; }
  .bar-fg.neg { background: #f87171; }
  .rec-wrap { text-align: center; padding: 8px 0; }
  .rec-btn { font-size: 64px; line-height: 1; color: #555; cursor: pointer;
             user-select: none; transition: color 120ms; }
  .rec-btn:hover { color: #777; }
  .rec-btn.active { color: #ef4444; animation: pulse 1.2s ease-in-out infinite; }
  @keyframes pulse { 50% { opacity: 0.45; } }
  .rec-label { margin-top: 8px; font-size: 13px; letter-spacing: 0.05em; color: #999; }
  .rec-path { margin-top: 4px; font-family: ui-monospace, monospace; font-size: 11px;
              color: #666; word-break: break-all; }
  .keys { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; font-size: 13px; color: #999; }
  kbd { font-family: ui-monospace, monospace; font-size: 11px;
        background: #232325; border: 1px solid #303033; padding: 2px 7px;
        border-radius: 4px; color: #ccc; }
  .pressed { background: #4ade80 !important; color: #0c0c0d !important; border-color: #4ade80 !important; }
  .status { font-size: 11px; color: #555; margin-top: 16px; font-family: ui-monospace, monospace; }
  .status.bad { color: #f87171; }
</style>
</head>
<body>
<h1>FRODOBOT DRIVE</h1>
<div class="grid">
  <div class="panel">
    <h2>motion</h2>
    <div class="row">
      <span class="label">speed</span>
      <div class="bar"><div class="bar-fg" id="speedBar"></div></div>
      <span class="value" id="speedVal">+0</span>
    </div>
    <div class="row">
      <span class="label">angular</span>
      <div class="bar"><div class="bar-fg" id="angBar"></div></div>
      <span class="value" id="angVal">+0</span>
    </div>
    <div class="row" style="margin-top:18px">
      <span class="label">step</span>
      <span class="value" id="stepVal" style="text-align:left;flex:1">25</span>
    </div>
  </div>
  <div class="panel rec-wrap">
    <h2 style="text-align:left">recording</h2>
    <div class="rec-btn" id="recBtn">●</div>
    <div class="rec-label" id="recLabel">idle</div>
    <div class="rec-path" id="recPath"></div>
  </div>
</div>
<div class="panel" style="margin-top:16px">
  <h2>keys</h2>
  <div class="keys">
    <kbd id="kw">W</kbd><kbd id="ka">A</kbd><kbd id="ks">S</kbd><kbd id="kd">D</kbd>
    <span style="color:#555">drive</span>
    <span style="color:#444;margin:0 4px">·</span>
    <kbd>space</kbd> <span style="color:#555">stop</span>
    <span style="color:#444;margin:0 4px">·</span>
    <kbd>+</kbd> / <kbd>-</kbd> <span style="color:#555">step</span>
    <span style="color:#444;margin:0 4px">·</span>
    <kbd>R</kbd> <span style="color:#555">record</span>
  </div>
</div>
<div class="status" id="status">connecting…</div>

<script>
const keys = new Set();
let speedStep = 25, angStep = 30;
const MAX = 100, MIN = 5;
let recording = false;
let lastOk = 0;

const $ = id => document.getElementById(id);
const setBar = (el, val) => {
  const pct = Math.abs(val) / MAX * 50;
  el.classList.toggle('neg', val < 0);
  if (val >= 0) { el.style.left = '50%'; el.style.right = (50 - pct) + '%'; }
  else          { el.style.right = '50%'; el.style.left  = (50 - pct) + '%'; }
};

document.addEventListener('keydown', e => {
  if (e.repeat) return;
  const k = e.key.toLowerCase();
  if (k === 'r')                    { e.preventDefault(); toggleRec(); return; }
  if (k === '+' || k === '=')       { speedStep = Math.min(MAX, speedStep + 5); angStep = Math.min(MAX, angStep + 5); return; }
  if (k === '-')                    { speedStep = Math.max(MIN, speedStep - 5); angStep = Math.max(MIN, angStep - 5); return; }
  if (k === ' ')                    { keys.clear(); return; }
  if ('wasd'.includes(k))           keys.add(k);
});
document.addEventListener('keyup', e => keys.delete(e.key.toLowerCase()));
window.addEventListener('blur', () => keys.clear());

async function toggleRec() {
  try {
    const r = await fetch('/api/record/toggle', { method: 'POST' }).then(r => r.json());
    recording = r.recording;
    $('recBtn').classList.toggle('active', recording);
    $('recLabel').textContent = recording ? '● REC' : 'idle';
    $('recPath').textContent = r.path || '';
  } catch (e) {
    $('status').textContent = 'record toggle error: ' + e;
    $('status').classList.add('bad');
  }
}
$('recBtn').addEventListener('click', toggleRec);

setInterval(async () => {
  let speed = 0, angular = 0;
  if (keys.has('w')) speed = speedStep;
  else if (keys.has('s')) speed = -speedStep;
  if (keys.has('d')) angular = angStep;
  else if (keys.has('a')) angular = -angStep;

  $('speedVal').textContent = (speed >= 0 ? '+' : '') + speed;
  $('angVal').textContent   = (angular >= 0 ? '+' : '') + angular;
  $('stepVal').textContent  = speedStep;
  setBar($('speedBar'), speed);
  setBar($('angBar'), angular);
  ['w','a','s','d'].forEach(k => $('k' + k).classList.toggle('pressed', keys.has(k)));

  try {
    const r = await fetch('/api/motor', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ speed, angular })
    });
    if (r.ok) {
      lastOk = Date.now();
      $('status').textContent = `connected · last ok ${new Date(lastOk).toLocaleTimeString()}`;
      $('status').classList.remove('bad');
    } else {
      $('status').textContent = `robot returned ${r.status}`;
      $('status').classList.add('bad');
    }
  } catch (e) {
    $('status').textContent = 'no route to robot · ' + e;
    $('status').classList.add('bad');
  }
}, 100);

// Init recording state on load
fetch('/api/record/status').then(r => r.json()).then(r => {
  recording = r.recording;
  $('recBtn').classList.toggle('active', recording);
  $('recLabel').textContent = recording ? '● REC' : 'idle';
  $('recPath').textContent = r.path || '';
}).catch(() => {});
</script>
</body>
</html>
"""


# ----- HTTP server -----------------------------------------------------------

def make_handler(args):
    sessions_dir = Path(args.sessions)
    sessions_dir.mkdir(parents=True, exist_ok=True)

    class H(BaseHTTPRequestHandler):
        # Quiet the default access log
        def log_message(self, *a, **k): pass

        def _json(self, obj, status=200):
            body = json.dumps(obj).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path == "/" or self.path == "/index.html":
                body = INDEX_HTML.encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            if self.path == "/api/record/status":
                with state.lock:
                    self._json({
                        "recording": state.recording,
                        "path": str(state.session_dir / "front.mp4") if state.session_dir else None,
                    })
                return
            self.send_response(404); self.end_headers()

        def do_POST(self):
            length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(length) if length else b""

            if self.path == "/api/motor":
                req = urllib.request.Request(
                    args.api + "/api/motor",
                    data=body, method="POST",
                    headers={"Content-Type": "application/json"},
                )
                try:
                    urllib.request.urlopen(req, timeout=0.5).read()
                    state.last_motor = json.loads(body) if body else {}
                    self._json({"ok": True})
                except Exception as e:
                    self._json({"ok": False, "error": str(e)}, status=502)
                return

            if self.path == "/api/record/toggle":
                with state.lock:
                    if not state.recording:
                        ts = datetime.now(timezone.utc).strftime("%Y%m%d_%H%M%SZ")
                        state.session_dir = sessions_dir / ts
                        state.session_dir.mkdir(parents=True, exist_ok=True)
                        out = state.session_dir / "front.mp4"
                        state.ffmpeg = subprocess.Popen(
                            [
                                "ffmpeg", "-loglevel", "error", "-nostdin",
                                "-rtsp_transport", "tcp",
                                "-i", args.rtsp,
                                "-c", "copy", "-map", "0:v:0",
                                str(out),
                            ],
                            stdin=subprocess.DEVNULL,
                            stdout=subprocess.DEVNULL,
                            stderr=subprocess.PIPE,
                        )
                        state.recording = True
                        path = str(out)
                    else:
                        if state.ffmpeg is not None:
                            state.ffmpeg.terminate()
                            try:
                                state.ffmpeg.wait(timeout=4)
                            except subprocess.TimeoutExpired:
                                state.ffmpeg.kill()
                            state.ffmpeg = None
                        state.recording = False
                        path = str(state.session_dir / "front.mp4") if state.session_dir else None
                    self._json({"recording": state.recording, "path": path})
                return

            self.send_response(404); self.end_headers()

    return H


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--api", default=DEFAULTS["api"], help="robot HTTP API base")
    p.add_argument("--rtsp", default=DEFAULTS["rtsp"], help="front camera RTSP URL")
    p.add_argument("--sessions", default=DEFAULTS["sessions"], help="local recording dir")
    p.add_argument("--port", type=int, default=DEFAULTS["port"])
    p.add_argument("--bind", default=DEFAULTS["bind"], help="0.0.0.0 to expose to LAN")
    args = p.parse_args()

    handler = make_handler(args)
    server = ThreadingHTTPServer((args.bind, args.port), handler)
    print(f"robot api : {args.api}")
    print(f"rtsp      : {args.rtsp}")
    print(f"sessions  : {args.sessions}")
    print(f"\n  → http://{args.bind if args.bind != '0.0.0.0' else 'localhost'}:{args.port}/\n")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        with state.lock:
            if state.ffmpeg is not None:
                state.ffmpeg.terminate()


if __name__ == "__main__":
    main()
