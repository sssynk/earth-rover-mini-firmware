# Earth Rover Mini — Custom Firmware

A drop-in replacement for the stock `frodobot.bin` on the FrodoBots Earth Rover Mini+. Exposes motor control, IMU/RPM telemetry, GPS, and RTK over a local HTTP API. No cloud relay. No Agora SDK. Everything runs on the robot.

The `docs/` folder is a separate writeup of the reverse-engineering process — pinout, wire protocols, where things live in the filesystem — useful even if you don't want to use this firmware.

## Status

Verified on a single test unit:
- HTTP API live and serving telemetry, GPS, motor commands at ~6 Hz STM32 telemetry rate
- Motor command round-trip tested at speed=8 and speed=25, all four wheels respond evenly
- NTRIP/RTK pipeline auths to caster and would stream RTCM with sky view (verified end-to-end indoors except for the actual fix step)
- 4-stream RTSP from prebuilt vendor demo binary; ~30 fps, H.265, 1920×1080 main + 720×576 sub per camera
- Self-daemonizes (works around `adb shell` PTY HUPs)
- Auto-starts on boot via `init.d/S95frodobots`

## Quick start

```bash
# Build for the robot's ARMv7 hardfloat target
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -trimpath -ldflags="-w -s" -o robot_app .

# Push to robot (assumes you're on its Wi-Fi AP "frodobot_xxx", default pw 12345678)
adb connect 192.168.11.1:5555
adb push robot_app /userdata/robot_app
adb shell "chmod +x /userdata/robot_app"

# Stop the vendor stack
adb shell "killall frodobot.sh frodobot.bin 2>/dev/null"

# Run (read-only mode — motors disabled until you add -allow-motor)
adb shell "/userdata/robot_app -daemon -listen :8080"

# Or with motors enabled and RTK
adb shell "/userdata/robot_app -daemon -listen :8080 \
    -allow-motor -max-speed 50 \
    -ntrip-host rtk.geodnet.com -ntrip-mount AUTO \
    -ntrip-user YOUR_USER -ntrip-pass YOUR_PASS"
```

## API

```
GET  /                                   plain-text help
GET  /api/streams                        4 RTSP URLs (JSON)
GET  /api/telemetry                      latest STM32 frame (volt, rpm, IMU, mag, heading)
GET  /api/gps                            parsed $GxGGA: lat/lon/alt/fix/sats/hdop
GET  /api/rtk/status                     NTRIP pipeline state + byte counter
POST /api/motor                          {"speed":int, "angular":int} (-100..100)
POST /api/imu/calibrate-mag/start
POST /api/imu/calibrate-mag/end
```

## CLI flags

```
-listen          ":8080"          HTTP listen address
-daemon                           fork into background, redirect stdio to -log
-log             "/tmp/robot.log" log file path when -daemon
-stm32           "/dev/ttyS0"     STM32 serial device
-gps             "/dev/ttyS2"     LC29H GNSS serial device
-allow-motor                      accept POST /api/motor (motors stay locked otherwise)
-max-speed       60               clamp |speed| and |angular| to this magnitude
-motor-watchdog  500ms            auto-stop if no motor command in this window
-ntrip-host                       NTRIP caster host (omit to disable RTK)
-ntrip-port      2101             caster port
-ntrip-mount     "AUTO"           mountpoint (AUTO = auto-route by GGA)
-ntrip-user                       NTRIP username
-ntrip-pass                       NTRIP password
```

## Auto-start on boot (zero modifications to read-only filesystems)

The rootfs (`/`) and `/oem` are both **squashfs** — fundamentally read-only. We can't edit `/etc/init.d/` or any vendor binary directly. But the stock `S99_auto_reboot` init script already sources `/data/cfg/rockchip_test/power_lost_test.sh` if it exists, and `/data` is a symlink to the writable `/userdata`. So we get a clean boot hook just by **creating that file** — no system-file edits, no overlay tricks, no risk to the vendor firmware.

```bash
# 1. Per-device credentials (NTRIP). Pull yours from the vendor log first:
adb shell "grep 'RTK token' /userdata/logs/frodobots.log | tail -1"

# Edit init.d/robot_app.env.example with the values, then:
adb push init.d/robot_app.env.example /userdata/robot_app.env

# 2. Install the boot hook + restore helper
adb shell "mkdir -p /data/cfg/rockchip_test"
adb push init.d/power_lost_test.sh /data/cfg/rockchip_test/power_lost_test.sh
adb push init.d/restore_stock.sh   /userdata/restore_stock.sh
adb shell "chmod +x /data/cfg/rockchip_test/power_lost_test.sh /userdata/restore_stock.sh"

# 3. Test the hook now (no reboot needed) — it kills the vendor app and starts ours
adb shell "/data/cfg/rockchip_test/power_lost_test.sh"
```

The hook reads NTRIP credentials from `/userdata/robot_app.env` (per-device, never committed to git). On boot it kills the vendor's `frodobot.sh` / `frodobot.bin`, then launches `sample_demo_dual_camera` and `robot_app`.

## Reverting to stock vendor firmware

One file removal:

```bash
adb shell "/userdata/restore_stock.sh"
adb shell "reboot"
```

That deletes `/data/cfg/rockchip_test/power_lost_test.sh` (the boot hook) and stops the running custom services. Next boot is pure stock vendor firmware. Vendor binaries (`/oem/usr/bin/frodobot.bin` etc) live on a read-only squashfs partition and were never modified, so they're guaranteed intact.

## Documentation

The `docs/` folder is set up to be rendered by GitHub Pages. After pushing to GitHub:

1. Repo Settings → **Pages** → Source: `Deploy from a branch`
2. Branch: `main`, Folder: `/docs`
3. Save. Site lives at `https://<your-username>.github.io/<repo-name>/` after a minute.

Pages content:

- [Hardware](docs/hardware.md) — RV1106 SoC, STM32F407, LC29H GNSS, Quectel modem, sensors, block diagram
- [Connecting](docs/connecting.md) — Wi-Fi AP, ADB, filesystem layout, killing the vendor supervisor
- [UCP wire protocol](docs/ucp-protocol.md) — STM32 framing, Modbus CRC16, motor + RPM_REPORT layouts
- [GPS and RTK](docs/gps-and-rtk.md) — LC29H, NMEA, PQTM commands, NTRIP pipeline
- [Camera streams](docs/camera-streams.md) — RTSP demo binaries, URL table, ffplay/gst recipes
- [Building custom firmware](docs/building-custom-firmware.md) — Go cross-compile + self-daemonize trick

## Disclaimer

Unofficial — derived from a public sample repo and runtime poking on a single test unit. Things will change between firmware revisions. Don't ship serious work without testing on your own unit.

The vendor's NTRIP credentials are not committed here. Pull them from your own device's `/userdata/logs/frodobots.log` (search for `RTK token`) if you want to use the same caster.
