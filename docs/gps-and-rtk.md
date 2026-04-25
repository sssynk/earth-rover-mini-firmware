# GPS and RTK

## Hardware

The robot has a **Quectel LC29H** multi-band L1/L5 multi-constellation GNSS module. It tracks GPS, GLONASS, Galileo, BeiDou, and QZSS simultaneously — that's why it'll get a single-point fix indoors when an old single-band L1 GPS would not.

- Connected to the rockchip on **`/dev/ttyS2 @ 115200 baud`** (8N1, raw)
- Firmware string observed: `LC29HDANR11A03S_RSA` (March 2024)
- Default NMEA output rate: 1 Hz
- Supports RTCM 3.x corrections written back to the same UART for RTK

## Talking to the chip

The chip emits standard NMEA when active:

```
$GNGGA,041724.000,3720.816722,N,12156.444619,W,1,06,6.67,49.425,M,-25.673,M,,*73
$GNRMC,...
$GPGSV,...   ← GPS satellites in view
$GLGSV,...   ← GLONASS
$GAGSV,...   ← Galileo
$GBGSV,...   ← BeiDou
$GQGSV,...   ← QZSS
```

The "GN" prefix means multi-constellation merged solution.

To start/stop NMEA output, write Quectel proprietary commands:

```
$PQTMGNSSSTART*51\r\n     # begin GNSS output
$PQTMGNSSSTOP*52\r\n      # stop
$PQTMVERNO*58\r\n         # query firmware version
$PQTMCFGMSGRATE,W,GGA,1   # set GGA emission rate to 1 Hz
$PQTMCFGMSGRATE,W,GSV,1
$PQTMCFGMSGRATE,W,RMC,1
$PQTMCFGMSGRATE,W,GSA,0   # disable
$PQTMCFGMSGRATE,W,GLL,0
$PQTMCFGMSGRATE,W,VTG,0
```

(Each `$P*` command needs a 2-hex-digit checksum after `*` — XOR of all chars between `$` and `*`.)

If the chip is silent after you take over from `frodobot.bin`, send `$PQTMGNSSSTART*51\r\n` once and NMEA will begin flowing.

## How RTK works on this robot

The vendor's `frodobot.bin` runs an embedded **NTRIP client** (you can see `libntrip::NtripClient::*` symbols in its `strings`). It:

1. Reads the chip's `$GNGGA` output to know rough position
2. Connects to an NTRIP caster (Geodnet's network), sending `Authorization: Basic <base64(user:pass)>`
3. Sends a periodic GGA upstream (mountpoint "AUTO" routes to nearest base station)
4. Receives RTCM 3.x correction stream
5. Writes those bytes verbatim back to `/dev/ttyS2`
6. The LC29H ingests the corrections and (with sky view) climbs through fix qualities:
   `0` no fix → `1` single → `2` DGPS → `5` RTK float → `4` RTK fixed

The NTRIP caster + credentials are **logged in plaintext** to `/userdata/logs/frodobots.log` at runtime — grep for `RTK token`. They're vendor-shipped credentials shared across all units.

## Doing RTK without `frodobot.bin`

You need:

1. **The caster URL/mount/credentials** — from your own device's `/userdata/logs/frodobots.log`
2. **A working NMEA stream from the LC29H** — send `$PQTMGNSSSTART*51\r\n` if needed
3. **An NTRIP client** that:
   - Authenticates to the caster
   - Sends a periodic GGA (the chip's, not a fake one — the caster's "AUTO" mountpoint picks the base nearest the GGA you submit, so faking your location gives you wrong corrections)
   - Forwards the RTCM byte stream to `/dev/ttyS2`

### Minimal Python NTRIP client

```python
#!/usr/bin/env python3
import socket, base64, sys, threading, time

CASTER, PORT, MOUNT = "rtk.geodnet.com", 2101, "AUTO"
USER, PWD = "...", "..."  # pull from /userdata/logs/frodobots.log on YOUR device
LAT, LON = 37.7749, -122.4194  # replace with your real position!

def nmea_cs(s):
    x = 0
    for c in s.encode(): x ^= c
    return f"{x:02X}"

def gga():
    t = time.gmtime()
    ts = f"{t.tm_hour:02d}{t.tm_min:02d}{t.tm_sec:02d}.00"
    ld, lm = int(abs(LAT)), (abs(LAT)-int(abs(LAT)))*60
    Ld, Lm = int(abs(LON)), (abs(LON)-int(abs(LON)))*60
    body = (f"GPGGA,{ts},{ld:02d}{lm:07.4f},{'N' if LAT>=0 else 'S'},"
            f"{Ld:03d}{Lm:07.4f},{'E' if LON>=0 else 'W'},"
            f"1,12,1.0,10.0,M,0.0,M,,")
    return f"${body}*{nmea_cs(body)}\r\n"

auth = base64.b64encode(f"{USER}:{PWD}".encode()).decode()
s = socket.create_connection((CASTER, PORT), timeout=10)
s.send(f"GET /{MOUNT} HTTP/1.0\r\nNtrip-Version: Ntrip/2.0\r\n"
       f"User-Agent: my-rover/1.0\r\nAuthorization: Basic {auth}\r\n\r\n".encode())

# Read response header
buf = b""
while b"\r\n\r\n" not in buf and b"ICY" not in buf[:8]:
    buf += s.recv(2048)
print(f"caster: {buf[:64].decode(errors='replace')}", file=sys.stderr)

# Send initial GGA + start GGA refresh thread
s.send(gga().encode())
def loop_gga():
    while True:
        time.sleep(10)
        try: s.send(gga().encode())
        except: return
threading.Thread(target=loop_gga, daemon=True).start()

# Forward RTCM to stdout (which you pipe into /dev/ttyS2)
end = buf.find(b"\r\n\r\n")
sys.stdout.buffer.write(buf[end+4:] if end > 0 else buf[buf.find(b"\r\n")+2:])
sys.stdout.buffer.flush()
while True:
    chunk = s.recv(4096)
    if not chunk: break
    sys.stdout.buffer.write(chunk); sys.stdout.buffer.flush()
```

### Piping RTCM into the LC29H

Run the above on your dev machine, pipe to a TCP forwarder (avoid adb shell's PTY mangling binary data):

```bash
# On the robot (via adb): start a TCP→serial bridge
adb shell "stty -F /dev/ttyS2 115200 raw -echo -hupcl; nc -l -p 9999 > /dev/ttyS2"

# On your dev machine: forward + run NTRIP client
adb forward tcp:9999 tcp:9999
python3 ntrip.py | nc -q 1 127.0.0.1 9999
```

You should see the chip's `$GNGGA` `fix_quality` field climb from 1 to 4 once it has corrections AND clear sky view. **Indoors it usually won't** — multipath kills carrier-phase tracking even with good RTCM.

## Indoor reality check

Single-point fix indoors is achievable thanks to L5 + multi-constellation. RTK indoors usually isn't. The pipeline still works — you just won't get a fixed solution until the antenna sees sky.

To validate the pipeline indoors, watch:
- Caster returns `200 OK` (auth accepted)
- RTCM byte counter on stderr grows steadily (~1–10 KB/s typical)
- Robot's NMEA continues to stream during injection (chip didn't choke on the bytes)

If all three are true, you're good — go outside and watch fix_quality climb.
