# Hardware

## SoC / Main Computer

**Rockchip RV1106** — single-board computer SoC.
- ARMv7l, ARM Cortex-A7-class, NEON + VFPv4 hardfloat
- Runs Linux 5.10.160 with **uClibc-NG 1.0.31** (not glibc — relevant for cross-compile)
- 21.9 MB read-only `/oem` partition (vendor binaries), 117.7 MB writable `/userdata`, plus a small read-only rootfs
- Hostname: `frodobot_p7aw77` (the suffix appears unique per-unit)

The RV1106 is a vision SoC originally designed for IP cameras — it has a hardware H.264/H.265 encoder, dual ISP, and an NPU. The vendor uses Rockchip's "rockit" media framework and has prebuilt camera/RTSP demos in `/oem/usr/bin/` (see [camera-streams.md](camera-streams.md)).

## Microcontroller

**STM32F407VE** — handles motors, IMU, magnetometer, ToF, LEDs, current sensor.
- Runs RT-Thread RTOS (firmware source published in `Software/STM32/`)
- Wired to the rockchip via UART, 115200 baud, on the rockchip side at **`/dev/ttyS0`**
- Speaks the **UCP** wire protocol — see [ucp-protocol.md](ucp-protocol.md)

Peripherals on the STM32:
| Sensor    | Bus  | Driver in repo         |
|-----------|------|------------------------|
| MPU6050   | I²C  | `applications/MPU6050.c` (IMU: accel + gyro, with DMP) |
| QMC5883L/P | I²C | `applications/QMC5883L.c`, `QMC5883P.c` (magnetometer) |
| INA226    | I²C  | `applications/INA226.c` (battery current/voltage)      |
| VL53L0X   | I²C  | `applications/vl53l0x.c` (ToF distance)                |
| WS2812    | PWM  | `applications/WS2812/`  (RGB status LEDs)              |
| Motors    | PWM  | `applications/motor.c` (4 wheels, with encoders)        |

Telemetry (RPM_REPORT, ID `0x05`) streams unsolicited at ~6 Hz and contains all sensor readings in one frame.

## GNSS

**Quectel LC29H** — multi-band L1/L5 multi-constellation RTK module.
- On the rockchip side at **`/dev/ttyS2 @ 115200`**
- Standard NMEA output (GGA, RMC, GSV for GPS/GLONASS/Galileo/BeiDou/QZSS) + Quectel `$PQTM*` proprietary commands
- Firmware string observed: `LC29HDANR11A03S_RSA` (2024/03/19)
- Accepts **RTCM 3.x corrections** written back to the same UART for RTK
- Emits "GN" prefixed sentences when in multi-constellation mode

See [gps-and-rtk.md](gps-and-rtk.md) for protocol detail and how to do RTK without `frodobot.bin`.

## Cellular Modem

**Quectel** cellular module (LTE Cat-1 family, exact P/N TBD per unit).
- Managed by `/usr/bin/quectel-CM -s simbase` on boot
- Exposes `wwan0` interface as the data plane (got `10.x.x.x/29` on test)
- Multiple `/dev/ttyUSB*` channels for AT/NMEA/diagnostic; `/dev/ttyUSB2` is referenced by `frodobot.bin` (control / AT)
- **GPS in the cell modem is NOT what RTK uses** — RTK is on the LC29H. The modem's GPS is just a fallback `frodobot.bin` knows about.

Killing `frodobot.bin` does **not** affect cellular connectivity. `quectel-CM` keeps `wwan0` up.

## Wi-Fi

The robot is its own access point, broadcasting an SSID like `frodobot_<id>` (ID matches hostname suffix). Default password: `12345678`.
- AP managed by `hostapd /tmp/hostapd.conf -B`
- DHCP/DNS for clients via `dnsmasq -C /tmp/dnsmasq.conf --interface=p2p0`
- Robot is at **`192.168.11.1`** on its own AP. Clients get `192.168.11.x`.
- The robot also has a `wlan0` station mode (`wpa_supplicant`, config in `/userdata/wpa_supplicant.conf`) — that's how it gets internet.

## Cameras

Two **GalaxyCore GC2093** sensors, 1080p each, on MIPI CSI:
- Front: `m00_f_gc2093` on I²C-4 addr 0x37
- Back: `m01_b_gc2093` on I²C-3 addr 0x37
- Both go through Rockchip ISP3X → CIF MIPI-LVDS → V4L2 nodes `/dev/video0..3` for the front and another set for the back

Per camera you typically get a "main" stream (1920×1080) and a "sub" stream (downscaled). The vendor's `/oem/usr/bin/sample_demo_dual_camera` exposes all 4 as RTSP — see [camera-streams.md](camera-streams.md).

## Block Diagram

```
                      ┌─────────────────────────────────┐
                      │  Quectel cell modem (LTE)       │
                      │  /dev/ttyUSB0..3, wwan0          │
                      └────────────┬────────────────────┘
                                   │ USB
              ┌────────────────────┴──────────────────────────┐
              │  Rockchip RV1106 (Linux, uClibc)              │
              │  • frodobot.bin    main app                   │
              │  • RTSP demos      /oem/usr/bin/              │
              │  • adbd            port 5555                  │
              │  • hostapd+dnsmasq AP "frodobot_xxx"          │
              └─┬────────┬─────────┬──────────┬───────────┬───┘
                │ ttyS0  │ ttyS2   │ MIPI CSI │ MIPI CSI  │ Wi-Fi
              UART     UART      front cam   back cam     2.4GHz
                │        │            │           │
        ┌───────┴──┐  ┌──┴───────┐  ┌─┴────┐  ┌───┴──┐
        │ STM32    │  │ LC29H    │  │ GC2093│  │ GC2093│
        │ motors,  │  │ GNSS,    │  │       │  │       │
        │ IMU, mag │  │ NMEA+RTK │  │       │  │       │
        └──────────┘  └──────────┘  └───────┘  └───────┘
```
