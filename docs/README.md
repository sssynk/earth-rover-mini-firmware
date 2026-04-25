# Earth Rover Mini — Reverse Engineering Notes

Practical notes on the FrodoBots Earth Rover Mini+ for anyone who wants to write their own firmware, integrate with the robot from outside, or just understand how it's put together. None of this is from a leaked SDK — everything here was figured out by poking at a stock unit and reading the public [`frodobots-org/earth-rover-mini`](https://github.com/frodobots-org/earth-rover-mini) repo.

## Pages

- [Hardware overview](hardware.md) — chips, sensors, who's wired to what
- [Connecting to the robot](connecting.md) — Wi-Fi, ADB, filesystem layout
- [UCP — the STM32 wire protocol](ucp-protocol.md) — motor commands and telemetry
- [GPS and RTK](gps-and-rtk.md) — LC29H GNSS, NTRIP injection, what `frodobot.bin` does for you
- [Camera streams](camera-streams.md) — the prebuilt RTSP demos you can launch in seconds
- [Building custom firmware](building-custom-firmware.md) — Go cross-compile + self-daemonize + HTTP server

## TL;DR for the impatient

The robot's main "brain" is a Rockchip RV1106 (32-bit ARMv7l, uClibc). It runs a vendor binary `/usr/bin/frodobot.bin` that handles motor control, GPS+RTK, and cloud video relay. Out of the box you can SSH-equivalent in via ADB on port 5555 (no password) once you've joined its Wi-Fi access point. There's also an STM32F407 microcontroller on `/dev/ttyS0` that owns the motors/IMU/compass, and a Quectel LC29H multi-band GNSS chip on `/dev/ttyS2`.

To replace `frodobot.bin` with your own program: kill the supervisor (`killall frodobot.sh frodobot.bin`), drop a statically-linked ARMv7 binary into `/userdata/`, push via `adb push`, and run. See [building-custom-firmware.md](building-custom-firmware.md) for a working example that exposes motor control, IMU/RPM telemetry, and GPS over an HTTP API.

## Disclaimer

This is unofficial. Things will change between firmware revisions. Don't ship anything serious without testing on your own unit. Don't post the vendor's NTRIP credentials publicly — pull them from your own device's runtime log if you need them.
