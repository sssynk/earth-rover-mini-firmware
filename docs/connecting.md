# Connecting to the Robot

## Get on the network

The robot broadcasts its own Wi-Fi access point.

- **SSID**: `frodobot_<id>` (the suffix matches the device hostname suffix)
- **Password**: `12345678`
- **Robot IP**: `192.168.11.1`
- You'll get `192.168.11.x` via DHCP

The robot is also a station on whatever Wi-Fi you've taught it (the vendor app does this, but you can edit `/userdata/wpa_supplicant.conf` after you're in).

## ADB (port 5555, no auth)

The simplest way in is **adb over TCP** — port 5555 is open and unauthenticated by design (it's how the vendor's mobile app talks to the bot).

```bash
adb connect 192.168.11.1:5555
adb -s 192.168.11.1:5555 shell
```

You're root inside.

## SSH (also open, but no creds)

OpenSSH 9.3 is listening on port 22. The vendor doesn't ship public credentials and `root` does not accept the Wi-Fi password. Practically: **use ADB**.

## Other open ports

```
22    OpenSSH 9.3                      no creds shipped
80    HTTP control panel               (status/UI page, no streaming)
554   not listening by default — only when you run a camera demo binary
5555  ADB                              unauthenticated, root shell
```

## Filesystem layout (what's writable)

```
/                  /dev/root       11.3 MB    100% used  read-only firmware
/oem               ubiblock7_0     21.9 MB    100% used  read-only firmware  ← vendor binaries
/userdata          ubi8_0         117.7 MB   ~7% used   writable             ← put your stuff here
/tmp               tmpfs           92.9 MB                ephemeral RAM
/run, /var/empty   tmpfs                                  ephemeral RAM
```

**Practical implication**: you can't replace `/oem/usr/bin/frodobot.bin` directly because `/oem` is read-only. To swap firmware, drop your binary in `/userdata/` and `chmod +x`, then either run it manually or replace what `init` calls.

## Pushing files

```bash
adb -s 192.168.11.1:5555 push my_binary /userdata/my_binary
adb -s 192.168.11.1:5555 shell "chmod +x /userdata/my_binary"
```

## Backgrounding services on the robot

Be aware: `adb shell "cmd &"` does NOT cleanly detach — when the adb shell session ends, children get HUPed regardless of `nohup` or `setsid` shell tricks. The reliable solution is to **make the binary self-daemonize** via `setsid` syscall (see [building-custom-firmware.md](building-custom-firmware.md) for a working Go example).

If you must use shell-only daemonization, the only thing that consistently works in our testing is launching from an **interactive** adb shell that you keep open in a `screen`/`tmux` session — the binary survives because adb shell is still alive.

## Disabling the stock firmware

The vendor's app stack is supervised by a script that auto-restarts:

```sh
# /usr/bin/frodobot.sh
/usr/bin/frodobot-uart /dev/ttyS2     # one-shot init for STM32
echo "frodobot-uart app exit."
while true; do
  /usr/bin/frodobot.bin
  echo "Exit frodobot. Sleep 5 sec..."
  sleep 5
done
```

To stop it cleanly:

```bash
adb shell "killall frodobot.sh frodobot.bin"
```

Killing only `frodobot.bin` is **not enough** — the supervisor script will respawn it after 5 seconds.

After killing, `quectel-CM` (cellular), `hostapd` (Wi-Fi AP), `wpa_supplicant` (Wi-Fi station), `chisel.sh` (cloud tunnel), and `dnsmasq` (DHCP) keep running — the network stack is independent of `frodobot.bin`.

To restart the stock app:

```bash
adb shell "nohup /usr/bin/frodobot.sh </dev/null >/dev/null 2>&1 &"
```

(Or just reboot — `init` will start it on boot.)
