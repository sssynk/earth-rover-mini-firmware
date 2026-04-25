#!/usr/bin/env python3
"""
Local laptop teleop + data collection for the Earth Rover Mini.

Run on your laptop while connected to the frodobot Wi-Fi AP (or via tailnet):
    python3 drive.py
Then open http://localhost:9000/ in any browser.

Browser captures real keydown/keyup events (no jolty multi-key issue),
proxies motor commands to the robot's HTTP API at 10 Hz, periodically
polls telemetry/GPS/RTK status, probes the RTSP video feed for liveness,
and toggles a local ffmpeg subprocess that records the front camera
to ~/Downloads/frodobot/sessions/<UTC>/front.mp4 (H.265 copy mode).

The robot stays a clean control plane. The Jetson is unused for data
collection — it's reserved for future on-device inference.
"""

import argparse
import json
import re
import socket
import subprocess
import threading
import time
import urllib.request
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

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
        self.session_started: float | None = None
        self.last_motor_ok_ts: float | None = None
        self.last_motor_lat_ms: float | None = None

        # Video feed health (updated by background prober)
        self.feed_reachable: bool = False
        self.feed_last_ok_ts: float | None = None
        self.feed_codec: str | None = None
        self.feed_width: int | None = None
        self.feed_height: int | None = None
        self.feed_fps: float | None = None

        # Robot telemetry caches (updated by background poller)
        self.tel_raw: dict | None = None
        self.tel_last_ok_ts: float | None = None
        self.gps_raw: dict | None = None
        self.gps_last_ok_ts: float | None = None
        self.rtk_raw: dict | None = None
        self.rtk_last_ok_ts: float | None = None

        # Recording stats from ffmpeg progress
        self.rec_bytes: int = 0
        self.rec_frame: int = 0
        self.rec_fps: float = 0.0


state = State()


# ----- background pollers ----------------------------------------------------

def poll_robot(api_base: str):
    """Pull telemetry / GPS / RTK status from the robot at 1 Hz."""
    ses = urllib.request.build_opener()
    while True:
        for path, attr_raw, attr_ts in [
            ("/api/telemetry", "tel_raw", "tel_last_ok_ts"),
            ("/api/gps", "gps_raw", "gps_last_ok_ts"),
            ("/api/rtk/status", "rtk_raw", "rtk_last_ok_ts"),
        ]:
            try:
                with ses.open(api_base + path, timeout=1.5) as r:
                    data = json.loads(r.read())
                with state.lock:
                    setattr(state, attr_raw, data)
                    setattr(state, attr_ts, time.time())
            except Exception:
                pass
        time.sleep(1.0)


def probe_feed(rtsp_url: str):
    """Quick liveness probe of the RTSP port + occasional full ffprobe."""
    parsed = urlparse(rtsp_url)
    host = parsed.hostname
    port = parsed.port or 554
    last_full_probe = 0.0
    while True:
        # Cheap TCP probe every iteration
        ok = False
        try:
            with socket.create_connection((host, port), timeout=1.5):
                ok = True
        except Exception:
            ok = False
        with state.lock:
            state.feed_reachable = ok
            if ok:
                state.feed_last_ok_ts = time.time()

        # Full ffprobe periodically (slower, gives us codec/res/fps)
        if ok and (time.time() - last_full_probe) > 30:
            try:
                out = subprocess.run(
                    [
                        "ffprobe", "-loglevel", "error", "-rtsp_transport", "tcp",
                        "-show_entries", "stream=codec_name,width,height,r_frame_rate",
                        "-of", "json", rtsp_url,
                    ],
                    capture_output=True, text=True, timeout=10,
                )
                if out.returncode == 0:
                    info = json.loads(out.stdout)
                    streams = info.get("streams") or []
                    if streams:
                        s = streams[0]
                        with state.lock:
                            state.feed_codec = s.get("codec_name")
                            state.feed_width = s.get("width")
                            state.feed_height = s.get("height")
                            num, _, den = (s.get("r_frame_rate") or "0/1").partition("/")
                            try:
                                state.feed_fps = float(num) / float(den) if float(den) else 0.0
                            except ValueError:
                                state.feed_fps = 0.0
            except Exception:
                pass
            last_full_probe = time.time()

        time.sleep(3.0)


# ----- HTML page -------------------------------------------------------------

INDEX_HTML = r"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Frodobot · Teleop</title>
<style>
  :root {
    --bg: #07080a;
    --bg-1: #0e1014;
    --bg-2: #14171c;
    --line: #1f242c;
    --line-2: #2a313b;
    --fg: #d8dde6;
    --fg-2: #8d97a6;
    --fg-3: #5b6573;
    --accent: #67e8f9;        /* data */
    --good: #4ade80;          /* online */
    --warn: #fbbf24;          /* degraded */
    --bad: #f87171;           /* offline / error / recording */
    --grid: rgba(255,255,255,0.03);
  }
  * { box-sizing: border-box; }
  html, body { background: var(--bg); color: var(--fg); margin: 0; }
  body {
    font: 13px/1.45 -apple-system, BlinkMacSystemFont, "SF Pro Text", "Inter", system-ui, sans-serif;
    min-height: 100vh; padding: 18px 20px;
    background-image:
      linear-gradient(var(--grid) 1px, transparent 1px),
      linear-gradient(90deg, var(--grid) 1px, transparent 1px);
    background-size: 40px 40px;
  }
  .mono { font-family: ui-monospace, "SF Mono", "Menlo", "Cascadia Mono", monospace; font-variant-numeric: tabular-nums; }

  /* Header */
  header {
    display: flex; align-items: center; justify-content: space-between;
    padding-bottom: 14px; border-bottom: 1px solid var(--line); margin-bottom: 14px;
  }
  .brand { display: flex; align-items: baseline; gap: 14px; }
  .brand h1 {
    font: 600 13px/1 ui-monospace, monospace; letter-spacing: 0.18em;
    color: var(--fg); text-transform: uppercase; margin: 0;
  }
  .brand .sub { font: 11px/1 ui-monospace, monospace; color: var(--fg-3); letter-spacing: 0.1em; }
  .leds { display: flex; gap: 14px; align-items: center; font: 11px ui-monospace, monospace;
          color: var(--fg-2); letter-spacing: 0.05em; }
  .led { display: inline-flex; align-items: center; gap: 6px; }
  .led .dot {
    width: 8px; height: 8px; border-radius: 50%; background: var(--fg-3);
    box-shadow: 0 0 0 0 transparent;
  }
  .led.good .dot { background: var(--good); box-shadow: 0 0 8px rgba(74,222,128,0.5); }
  .led.warn .dot { background: var(--warn); box-shadow: 0 0 8px rgba(251,191,36,0.5); }
  .led.bad  .dot { background: var(--bad);  box-shadow: 0 0 8px rgba(248,113,113,0.5); }
  .clock { font: 11px ui-monospace, monospace; color: var(--fg-3); letter-spacing: 0.06em; }

  /* Layout */
  .grid {
    display: grid;
    grid-template-columns: 360px 1fr 1fr;
    grid-template-rows: auto auto;
    gap: 12px;
  }
  @media (max-width: 1100px) { .grid { grid-template-columns: 1fr 1fr; } }
  @media (max-width: 720px)  { .grid { grid-template-columns: 1fr; } }
  .panel {
    background: var(--bg-1); border: 1px solid var(--line); border-radius: 6px;
    padding: 14px 16px; min-height: 100%;
  }
  .panel h2 {
    font: 600 10px/1 ui-monospace, monospace;
    text-transform: uppercase; letter-spacing: 0.18em;
    color: var(--fg-3); margin: 0 0 12px;
  }
  .panel.dense h2 { margin-bottom: 8px; }

  /* Field rows */
  .kv { display: grid; grid-template-columns: 90px 1fr; gap: 4px 12px; align-items: baseline; }
  .kv dt { color: var(--fg-3); font: 11px ui-monospace, monospace; letter-spacing: 0.05em;
           text-transform: uppercase; }
  .kv dd { margin: 0; color: var(--fg); }
  .kv dd.mono { color: var(--accent); }
  .kv dd.dim  { color: var(--fg-2); }
  .kv dd.err  { color: var(--bad); }
  .kv dd.warn { color: var(--warn); }

  /* Joystick / vector */
  .joy-wrap { display: flex; flex-direction: column; align-items: center; gap: 10px; }
  .joy { width: 220px; height: 220px; position: relative; }
  .joy svg { width: 100%; height: 100%; display: block; }
  .joy-tag { font: 10px ui-monospace, monospace; color: var(--fg-3); letter-spacing: 0.08em; }
  .joy-readouts { display: flex; gap: 18px; font: 12px ui-monospace, monospace; }
  .joy-readouts .r { color: var(--fg-2); }
  .joy-readouts .v { color: var(--accent); margin-left: 6px; }

  /* Recording */
  .rec { display: grid; grid-template-columns: auto 1fr; gap: 14px; align-items: center; }
  .rec-button {
    width: 56px; height: 56px; border-radius: 50%;
    background: var(--bg-2); border: 1px solid var(--line-2);
    display: flex; align-items: center; justify-content: center;
    cursor: pointer; transition: all 120ms;
    user-select: none;
  }
  .rec-button:hover { border-color: var(--fg-3); }
  .rec-button .core {
    width: 22px; height: 22px; border-radius: 50%; background: var(--fg-3);
    transition: all 200ms;
  }
  .rec-button.active { border-color: var(--bad); }
  .rec-button.active .core { background: var(--bad); animation: rec-pulse 1.6s ease-in-out infinite; }
  @keyframes rec-pulse {
    0%, 100% { box-shadow: 0 0 0 0 rgba(248,113,113,0.55); }
    50% { box-shadow: 0 0 0 10px rgba(248,113,113,0); }
  }
  .rec-meta { display: flex; flex-direction: column; gap: 4px; }
  .rec-state { font: 11px ui-monospace, monospace; color: var(--fg-2); letter-spacing: 0.1em; }
  .rec-state.active { color: var(--bad); }
  .rec-time { font: 14px ui-monospace, monospace; color: var(--fg); }
  .rec-path { font: 11px ui-monospace, monospace; color: var(--fg-3); word-break: break-all; }

  /* Keyboard hints */
  kbd {
    display: inline-block;
    font: 10px/1 ui-monospace, monospace; letter-spacing: 0.04em;
    background: var(--bg-2); border: 1px solid var(--line-2); color: var(--fg-2);
    padding: 4px 7px; border-radius: 3px;
    transition: all 80ms;
  }
  kbd.hot { background: var(--good); color: #06120b; border-color: var(--good); }

  .hints { display: flex; gap: 10px; flex-wrap: wrap; align-items: center;
           color: var(--fg-3); font: 11px ui-monospace, monospace; letter-spacing: 0.04em; }
  .hints kbd { margin-right: 2px; }
  .hints span.s { margin: 0 6px; color: var(--line-2); }

  /* Bars */
  .bar-wrap { display: grid; grid-template-columns: 70px 1fr 56px; gap: 10px; align-items: center; }
  .bar {
    height: 6px; background: var(--bg-2); border: 1px solid var(--line); border-radius: 2px;
    position: relative; overflow: hidden;
  }
  .bar::before { content: ""; position: absolute; left: 50%; top: 0; bottom: 0; width: 1px; background: var(--line-2); }
  .bar-fg { position: absolute; top: 0; bottom: 0; background: var(--accent); transition: all 80ms; }
  .bar-label { color: var(--fg-3); font: 11px ui-monospace, monospace; letter-spacing: 0.05em; text-transform: uppercase; }
  .bar-val { color: var(--accent); font: 12px ui-monospace, monospace; text-align: right; }

  /* Footer status line */
  .tape {
    margin-top: 12px; padding: 8px 12px;
    background: var(--bg-1); border: 1px solid var(--line);
    font: 11px ui-monospace, monospace; color: var(--fg-3); letter-spacing: 0.05em;
    border-radius: 4px; display: flex; gap: 20px; flex-wrap: wrap;
  }
  .tape b { color: var(--fg); font-weight: normal; }
</style>
</head>
<body>

<header>
  <div class="brand">
    <h1>Frodobot · Teleop</h1>
    <span class="sub">earthrover-mini</span>
  </div>
  <div class="leds">
    <span class="led" id="ledApi"><span class="dot"></span>API</span>
    <span class="led" id="ledFeed"><span class="dot"></span>VIDEO</span>
    <span class="led" id="ledRtk"><span class="dot"></span>RTK</span>
    <span class="led" id="ledRec"><span class="dot"></span>REC</span>
  </div>
  <div class="clock mono" id="clock">— UTC</div>
</header>

<div class="grid">
  <!-- Joystick / control -->
  <div class="panel" style="grid-row: span 2">
    <h2>control vector</h2>
    <div class="joy-wrap">
      <div class="joy">
        <svg viewBox="-100 -100 200 200" preserveAspectRatio="xMidYMid meet">
          <defs>
            <pattern id="g" width="20" height="20" patternUnits="userSpaceOnUse">
              <path d="M 20 0 L 0 0 0 20" fill="none" stroke="#1a1f27" stroke-width="0.5"/>
            </pattern>
          </defs>
          <circle cx="0" cy="0" r="92" fill="url(#g)" stroke="#2a313b" stroke-width="1"/>
          <circle cx="0" cy="0" r="46" fill="none" stroke="#1f242c" stroke-width="0.5"/>
          <line x1="-92" y1="0" x2="92" y2="0" stroke="#2a313b" stroke-width="0.5"/>
          <line x1="0" y1="-92" x2="0" y2="92" stroke="#2a313b" stroke-width="0.5"/>
          <text x="0" y="-95" fill="#5b6573" font-family="ui-monospace, monospace"
                font-size="7" text-anchor="middle">FWD</text>
          <text x="0" y="100" fill="#5b6573" font-family="ui-monospace, monospace"
                font-size="7" text-anchor="middle">REV</text>
          <text x="-95" y="3" fill="#5b6573" font-family="ui-monospace, monospace"
                font-size="7" text-anchor="end">L</text>
          <text x="95" y="3" fill="#5b6573" font-family="ui-monospace, monospace"
                font-size="7" text-anchor="start">R</text>
          <line id="vecLine" x1="0" y1="0" x2="0" y2="0" stroke="#67e8f9" stroke-width="1.5" />
          <circle id="vecDot" cx="0" cy="0" r="5" fill="#67e8f9"/>
        </svg>
      </div>
      <div class="joy-readouts mono">
        <span class="r">SPD<span class="v" id="spdNum">+0</span></span>
        <span class="r">ANG<span class="v" id="angNum">+0</span></span>
        <span class="r">STEP<span class="v" id="stepNum">25</span></span>
      </div>
    </div>

    <h2 style="margin-top:18px">input</h2>
    <div class="hints">
      <kbd id="kw">W</kbd><kbd id="ka">A</kbd><kbd id="ks">S</kbd><kbd id="kd">D</kbd>
      <span class="s">·</span>
      <kbd>SPACE</kbd> stop
      <span class="s">·</span>
      <kbd>+</kbd>/<kbd>-</kbd> step
      <span class="s">·</span>
      <kbd>R</kbd> record
    </div>

    <h2 style="margin-top:18px">recording</h2>
    <div class="rec">
      <div id="recBtn" class="rec-button" title="toggle recording (R)"><div class="core"></div></div>
      <div class="rec-meta">
        <div class="rec-state" id="recState">IDLE</div>
        <div class="rec-time mono" id="recTime">00:00:00</div>
        <div class="rec-path mono" id="recPath"></div>
      </div>
    </div>
  </div>

  <!-- Power -->
  <div class="panel dense">
    <h2>power · battery</h2>
    <div class="bar-wrap">
      <span class="bar-label">battery</span>
      <div class="bar"><div class="bar-fg" id="battBar" style="left:0;width:0%"></div></div>
      <span class="bar-val" id="battVal">— %</span>
    </div>
    <dl class="kv" style="margin-top:10px">
      <dt>voltage</dt><dd class="mono" id="voltVal">—</dd>
      <dt>current</dt><dd class="mono" id="currVal">—</dd>
      <dt>power</dt><dd class="mono" id="pwrVal">—</dd>
      <dt>heading</dt><dd class="mono" id="hdgVal">—</dd>
      <dt>rpm</dt><dd class="mono" id="rpmVal">—</dd>
    </dl>
  </div>

  <!-- GPS / RTK -->
  <div class="panel dense">
    <h2>gnss · rtk</h2>
    <dl class="kv">
      <dt>lat</dt><dd class="mono" id="latVal">—</dd>
      <dt>lon</dt><dd class="mono" id="lonVal">—</dd>
      <dt>alt</dt><dd class="mono" id="altVal">—</dd>
      <dt>fix</dt><dd class="mono" id="fixVal">—</dd>
      <dt>sats</dt><dd class="mono" id="satVal">—</dd>
      <dt>hdop</dt><dd class="mono" id="hdopVal">—</dd>
    </dl>
    <hr style="border:0;border-top:1px solid var(--line);margin:12px 0">
    <dl class="kv">
      <dt>caster</dt><dd class="mono dim" id="rtkHost">—</dd>
      <dt>state</dt><dd class="mono" id="rtkState">—</dd>
      <dt>rtcm in</dt><dd class="mono" id="rtkBytes">—</dd>
    </dl>
  </div>

  <!-- Video feed -->
  <div class="panel dense">
    <h2>video feed · rtsp</h2>
    <dl class="kv">
      <dt>url</dt><dd class="mono dim" id="feedUrl">—</dd>
      <dt>state</dt><dd class="mono" id="feedState">—</dd>
      <dt>codec</dt><dd class="mono" id="feedCodec">—</dd>
      <dt>resolution</dt><dd class="mono" id="feedRes">—</dd>
      <dt>fps</dt><dd class="mono" id="feedFps">—</dd>
      <dt>last ok</dt><dd class="mono dim" id="feedLast">—</dd>
    </dl>
  </div>

  <!-- System -->
  <div class="panel dense">
    <h2>system</h2>
    <dl class="kv">
      <dt>soc temp</dt><dd class="mono" id="sysTemp">—</dd>
      <dt>fan</dt><dd class="mono" id="sysFan">—</dd>
      <dt>stop sw.</dt><dd class="mono" id="sysStop">—</dd>
      <dt>fw ver</dt><dd class="mono" id="sysVer">—</dd>
    </dl>
    <hr style="border:0;border-top:1px solid var(--line);margin:12px 0">
    <dl class="kv">
      <dt>api ping</dt><dd class="mono" id="sysPing">—</dd>
      <dt>last ok</dt><dd class="mono dim" id="sysLast">—</dd>
    </dl>
  </div>
</div>

<div class="tape mono">
  <span><b id="tapeApi">api ?</b></span>
  <span><b id="tapeFeed">feed ?</b></span>
  <span><b id="tapeRtk">rtk ?</b></span>
  <span><b id="tapeRec">rec idle</b></span>
  <span style="margin-left:auto;color:var(--fg-3)" id="tapeBuild">build · drive.py</span>
</div>

<script>
"use strict";

const $ = id => document.getElementById(id);
const KEYS = new Set();
const MAX = 100, MIN = 5;
let speedStep = 25, angStep = 30;
let recording = false;
let recStartedAt = 0;

// ----- input handling --------------------------------------------------------
document.addEventListener('keydown', e => {
  if (e.repeat) return;
  const k = e.key.toLowerCase();
  if (k === 'r')              { e.preventDefault(); toggleRec(); return; }
  if (k === '+' || k === '=') { speedStep = Math.min(MAX, speedStep + 5); angStep = Math.min(MAX, angStep + 5); return; }
  if (k === '-')              { speedStep = Math.max(MIN, speedStep - 5); angStep = Math.max(MIN, angStep - 5); return; }
  if (k === ' ')              { KEYS.clear(); return; }
  if ('wasd'.includes(k))     KEYS.add(k);
});
document.addEventListener('keyup', e => KEYS.delete(e.key.toLowerCase()));
window.addEventListener('blur', () => KEYS.clear());

// ----- recording -------------------------------------------------------------
$('recBtn').addEventListener('click', toggleRec);
async function toggleRec() {
  try {
    const r = await fetch('/api/record/toggle', { method: 'POST' }).then(r => r.json());
    applyRec(r);
  } catch (e) { /* noop */ }
}
function applyRec(r) {
  recording = !!r.recording;
  recStartedAt = r.started_at_ms || 0;
  $('recBtn').classList.toggle('active', recording);
  $('recState').classList.toggle('active', recording);
  $('recState').textContent = recording ? '● RECORDING' : 'IDLE';
  $('recPath').textContent = r.path || '';
  $('ledRec').className = 'led ' + (recording ? 'bad' : '');
  $('tapeRec').textContent = recording ? 'rec ●' : 'rec idle';
}

// ----- motor loop ------------------------------------------------------------
let lastApiOk = 0;
let pingMs = null;
setInterval(async () => {
  let speed = 0, angular = 0;
  if (KEYS.has('w')) speed = speedStep;
  else if (KEYS.has('s')) speed = -speedStep;
  if (KEYS.has('d')) angular = angStep;
  else if (KEYS.has('a')) angular = -angStep;

  // Visualize
  $('spdNum').textContent = (speed >= 0 ? '+' : '') + speed;
  $('angNum').textContent = (angular >= 0 ? '+' : '') + angular;
  $('stepNum').textContent = speedStep;
  // Map [-MAX..MAX] → [-92..92] for SVG; flip Y so + is up.
  const vx = (angular / MAX) * 92;
  const vy = -(speed / MAX) * 92;
  $('vecDot').setAttribute('cx', vx);
  $('vecDot').setAttribute('cy', vy);
  $('vecLine').setAttribute('x2', vx);
  $('vecLine').setAttribute('y2', vy);
  ['w','a','s','d'].forEach(k => $('k' + k).classList.toggle('hot', KEYS.has(k)));

  const t0 = performance.now();
  try {
    const r = await fetch('/api/motor', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ speed, angular })
    });
    if (r.ok) {
      pingMs = performance.now() - t0;
      lastApiOk = Date.now();
    }
  } catch (e) { /* stays bad */ }
}, 100);

// ----- telemetry / status poll ----------------------------------------------
setInterval(async () => {
  try {
    const s = await fetch('/api/all').then(r => r.json());
    applyStatus(s);
  } catch (e) {
    setLed('ledApi', 'bad');
  }
}, 1000);

// ----- recording timer (separate so it ticks at 4 Hz) ------------------------
setInterval(() => {
  if (recording && recStartedAt) {
    const elapsed = Math.floor((Date.now() - recStartedAt) / 1000);
    const h = String(Math.floor(elapsed / 3600)).padStart(2, '0');
    const m = String(Math.floor((elapsed % 3600) / 60)).padStart(2, '0');
    const s = String(elapsed % 60).padStart(2, '0');
    $('recTime').textContent = `${h}:${m}:${s}`;
  } else if (!recording) {
    $('recTime').textContent = '00:00:00';
  }

  // UTC clock
  const now = new Date();
  const t = now.toISOString().slice(11, 19);
  $('clock').textContent = `${t} UTC`;
}, 250);

// ----- helpers ---------------------------------------------------------------
function setLed(id, state) {
  const el = $(id);
  el.className = 'led ' + state;
}
function fmtAge(ts_ms) {
  if (!ts_ms) return '—';
  const dt = (Date.now() - ts_ms) / 1000;
  if (dt < 1) return 'now';
  if (dt < 60) return Math.floor(dt) + 's';
  if (dt < 3600) return Math.floor(dt / 60) + 'm';
  return Math.floor(dt / 3600) + 'h';
}
function fmtBytes(n) {
  if (n == null) return '—';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
}

function applyStatus(s) {
  // Header LEDs + footer tape
  setLed('ledApi',  s.api_ok  ? 'good' : 'bad');
  setLed('ledFeed', s.feed?.reachable ? 'good' : 'bad');
  setLed('ledRtk',  s.rtk?.bytes_in > 0 ? 'good' : (s.rtk?.connected ? 'warn' : 'bad'));
  $('tapeApi').textContent  = `api ${s.api_ok ? 'ok' : 'down'}` + (s.api_ping_ms != null ? ' · ' + s.api_ping_ms.toFixed(0) + 'ms' : '');
  $('tapeFeed').textContent = `feed ${s.feed?.reachable ? 'live' : 'down'}`;
  $('tapeRtk').textContent  = `rtk ${s.rtk?.bytes_in > 0 ? 'rx' : (s.rtk?.connected ? 'idle' : 'down')}`;

  // Power / battery (re-mapped from the misnamed UCP fields)
  if (s.tel) {
    const t = s.tel;
    const battPct = t.battery_pct;
    const pct = (battPct == null) ? 0 : Math.max(0, Math.min(100, battPct));
    $('battBar').style.left = '0';
    $('battBar').style.width = pct + '%';
    $('battBar').style.background = battPct == null ? 'var(--fg-3)'
                                  : battPct > 60 ? 'var(--good)'
                                  : battPct > 25 ? 'var(--warn)'
                                  : 'var(--bad)';
    $('battVal').textContent = battPct == null ? '— %' : battPct.toFixed(0) + ' %';
    $('voltVal').textContent = t.voltage_v == null ? '—' : t.voltage_v.toFixed(1) + ' V';
    $('currVal').textContent = t.current_a == null ? '—' : t.current_a.toFixed(2) + ' A';
    $('pwrVal').textContent  = t.power_w   == null ? '—' : t.power_w.toFixed(1) + ' W';
    $('hdgVal').textContent  = t.heading == null ? '—' : t.heading + '°';
    $('rpmVal').textContent  = t.rpm ? '[' + t.rpm.join(', ') + ']' : '—';
    $('sysStop').textContent = t.stop_switch == null ? '—' : t.stop_switch;
    $('sysVer').textContent  = t.version == null ? '—' : t.version;
  }

  // GPS
  if (s.gps) {
    const g = s.gps;
    const valid = !!g.valid;
    $('latVal').textContent = (g.lat == null) ? '—' : g.lat.toFixed(6) + '°';
    $('lonVal').textContent = (g.lon == null) ? '—' : g.lon.toFixed(6) + '°';
    $('altVal').textContent = (g.altitude_m == null) ? '—' : g.altitude_m.toFixed(1) + ' m';
    const fixNames = ['no fix','single','dgps','pps','rtk-fix','rtk-float'];
    $('fixVal').textContent = (g.fix_quality == null) ? '—'
        : `${g.fix_quality} (${fixNames[g.fix_quality] || '?'})`;
    $('fixVal').className = 'mono ' + (valid ? '' : 'dim');
    $('satVal').textContent = g.num_sats == null ? '—' : g.num_sats;
    $('hdopVal').textContent = g.hdop == null ? '—' : g.hdop.toFixed(2);
  }

  // RTK
  if (s.rtk) {
    const r = s.rtk;
    $('rtkHost').textContent = r.host ? `${r.host}:${r.port||2101}/${r.mount||''}` : '—';
    $('rtkState').textContent = r.connected
        ? (r.bytes_in > 0 ? 'streaming' : 'connected (silent)')
        : 'disconnected';
    $('rtkState').className = 'mono ' + (r.connected && r.bytes_in > 0 ? '' : (r.connected ? 'warn' : 'err'));
    $('rtkBytes').textContent = fmtBytes(r.bytes_in);
  }

  // Video feed
  if (s.feed) {
    const f = s.feed;
    $('feedUrl').textContent = f.url || '—';
    $('feedState').textContent = f.reachable ? 'reachable' : 'unreachable';
    $('feedState').className = 'mono ' + (f.reachable ? '' : 'err');
    $('feedCodec').textContent = (f.codec || '—').toUpperCase();
    $('feedRes').textContent = (f.width && f.height) ? `${f.width} × ${f.height}` : '—';
    $('feedFps').textContent = f.fps ? f.fps.toFixed(0) + ' fps' : '—';
    $('feedLast').textContent = f.last_ok_ts ? fmtAge(f.last_ok_ts * 1000) + ' ago' : '—';
  }

  // Fan + temp from telemetry endpoint may be combined; fall back if present
  if (s.fan) {
    $('sysTemp').textContent = (s.fan.soc_temp_c != null) ? s.fan.soc_temp_c + ' °C' : '—';
    $('sysFan').textContent = s.fan.fan_on ? 'ON' : 'off';
    $('sysFan').className = 'mono ' + (s.fan.fan_on ? 'warn' : 'dim');
  }

  // Ping
  $('sysPing').textContent = s.api_ping_ms == null ? '—' : s.api_ping_ms.toFixed(0) + ' ms';
  $('sysLast').textContent = fmtAge(s.api_last_ok_ts * 1000);
}

// Pull initial recording state on load
fetch('/api/record/status').then(r => r.json()).then(applyRec).catch(() => {});
</script>

</body>
</html>"""


# ----- HTTP server -----------------------------------------------------------

def make_handler(args):
    sessions_dir = Path(args.sessions)
    sessions_dir.mkdir(parents=True, exist_ok=True)

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a, **k): pass

        def _json(self, obj, status=200):
            body = json.dumps(obj, default=str).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        # --- GET ------------------------------------------------------------
        def do_GET(self):
            if self.path in ("/", "/index.html"):
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
                        "started_at_ms": int(state.session_started * 1000) if state.session_started else 0,
                    })
                return

            if self.path == "/api/all":
                self._json(_aggregate_status(args))
                return

            self.send_response(404); self.end_headers()

        # --- POST -----------------------------------------------------------
        def do_POST(self):
            length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(length) if length else b""

            if self.path == "/api/motor":
                t0 = time.perf_counter()
                req = urllib.request.Request(
                    args.api + "/api/motor",
                    data=body, method="POST",
                    headers={"Content-Type": "application/json"},
                )
                try:
                    urllib.request.urlopen(req, timeout=0.5).read()
                    dt = (time.perf_counter() - t0) * 1000
                    with state.lock:
                        state.last_motor_ok_ts = time.time()
                        state.last_motor_lat_ms = dt
                    self._json({"ok": True, "lat_ms": round(dt, 1)})
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
                                "ffmpeg", "-loglevel", "warning", "-nostdin",
                                "-rtsp_transport", "tcp",
                                "-i", args.rtsp,
                                "-c", "copy", "-map", "0:v:0",
                                "-progress", "-",
                                str(out),
                            ],
                            stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE,
                            stderr=subprocess.DEVNULL,
                        )
                        state.recording = True
                        state.session_started = time.time()
                        state.rec_bytes = 0
                        state.rec_frame = 0
                        state.rec_fps = 0.0
                        # Background thread to parse ffmpeg progress
                        threading.Thread(
                            target=_consume_ffmpeg_progress, args=(state.ffmpeg,), daemon=True
                        ).start()
                        path = str(out)
                    else:
                        if state.ffmpeg is not None:
                            state.ffmpeg.terminate()
                            try: state.ffmpeg.wait(timeout=4)
                            except subprocess.TimeoutExpired: state.ffmpeg.kill()
                            state.ffmpeg = None
                        state.recording = False
                        path = str(state.session_dir / "front.mp4") if state.session_dir else None
                    self._json({
                        "recording": state.recording,
                        "path": path,
                        "started_at_ms": int(state.session_started * 1000) if state.session_started else 0,
                    })
                return

            self.send_response(404); self.end_headers()

    return H


def _consume_ffmpeg_progress(proc):
    """Parse ffmpeg's -progress key=value stream (frame=, total_size=, fps=, ...)."""
    if proc.stdout is None:
        return
    for raw in proc.stdout:
        try:
            line = raw.decode(errors="replace").strip()
        except Exception:
            continue
        if "=" not in line:
            continue
        k, _, v = line.partition("=")
        with state.lock:
            if k == "frame":
                try: state.rec_frame = int(v)
                except ValueError: pass
            elif k == "total_size":
                try: state.rec_bytes = int(v)
                except ValueError: pass
            elif k == "fps":
                try: state.rec_fps = float(v)
                except ValueError: pass


def _aggregate_status(args):
    """Returns the unified status payload the browser polls at 1 Hz."""
    with state.lock:
        # Re-map the misnamed UCP fields to honest names
        tel_raw = state.tel_raw or {}
        tel = None
        if tel_raw:
            tel = {
                "battery_pct": tel_raw.get("Voltage"),         # UCP "voltage" = battery%
                "voltage_v":   (tel_raw.get("ErrorCode") or 0) / 10.0,  # UCP "error_code" = volt × 10
                "current_a":   (tel_raw.get("Reserve")   or 0) / 100.0, # UCP "reserve" = current × 100
                "power_w":     tel_raw.get("StopSwitch"),       # UCP "stop_switch" = power (W)
                "heading":     tel_raw.get("Heading"),
                "rpm":         tel_raw.get("Rpm"),
                "stop_switch": tel_raw.get("StopSwitch"),       # raw, also exposed
                "version":     tel_raw.get("Version"),
                "updated_unix":tel_raw.get("updated_unix"),
            }

        # Try fan endpoint too (best-effort cache via gps timestamp piggyback isn't ideal;
        # we'll fetch lazily here)
        fan = None
        try:
            with urllib.request.urlopen(args.api + "/api/fan", timeout=0.5) as r:
                fan = json.loads(r.read())
        except Exception:
            pass

        return {
            "api_ok":         state.last_motor_ok_ts is not None and (time.time() - state.last_motor_ok_ts) < 5,
            "api_ping_ms":    state.last_motor_lat_ms,
            "api_last_ok_ts": state.last_motor_ok_ts,
            "tel":            tel,
            "gps":            state.gps_raw,
            "rtk":            state.rtk_raw,
            "fan":            fan,
            "feed": {
                "url":        args.rtsp,
                "reachable":  state.feed_reachable,
                "codec":      state.feed_codec,
                "width":      state.feed_width,
                "height":     state.feed_height,
                "fps":        state.feed_fps,
                "last_ok_ts": state.feed_last_ok_ts,
            },
            "rec": {
                "recording":  state.recording,
                "bytes":      state.rec_bytes,
                "frame":      state.rec_frame,
                "fps":        state.rec_fps,
            },
        }


def main():
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--api", default=DEFAULTS["api"], help="robot HTTP API base URL")
    p.add_argument("--rtsp", default=DEFAULTS["rtsp"], help="front camera RTSP URL")
    p.add_argument("--sessions", default=DEFAULTS["sessions"], help="local recording dir")
    p.add_argument("--port", type=int, default=DEFAULTS["port"])
    p.add_argument("--bind", default=DEFAULTS["bind"], help="0.0.0.0 to expose to LAN")
    args = p.parse_args()

    # Spin up background pollers
    threading.Thread(target=poll_robot, args=(args.api,), daemon=True).start()
    threading.Thread(target=probe_feed, args=(args.rtsp,), daemon=True).start()

    handler = make_handler(args)
    server = ThreadingHTTPServer((args.bind, args.port), handler)
    print(f"robot api : {args.api}")
    print(f"rtsp      : {args.rtsp}")
    print(f"sessions  : {args.sessions}")
    host_show = "localhost" if args.bind in ("127.0.0.1", "localhost") else args.bind
    print(f"\n  → http://{host_show}:{args.port}/\n")
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
