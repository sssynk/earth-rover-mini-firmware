# Building Custom Firmware

This page walks through replacing the vendor's `frodobot.bin` with your own program. The example is a Go binary that exposes motor control, IMU/RPM telemetry, and GPS over an HTTP API. The full source is in this repo at `robot_app/main.go`.

## Why Go?

- Cross-compiles to any target with one env var, no Docker needed
- Statically links by default with `CGO_ENABLED=0`
- Resulting binary doesn't depend on the system's uClibc — works regardless of libc version
- Stdlib has `net/http`, `encoding/json`, file I/O — everything you need for a robot HTTP server

C, Rust, Zig, or anything else also work — Go is just the easiest. If you want C, use `zig cc -target arm-linux-musleabihf -static` for a similarly self-contained binary.

## Target

The robot's SoC is **ARMv7l, hardfloat (VFPv4 + NEON), uClibc 1.0.31**. For a static binary that doesn't care about libc:

```
GOOS=linux
GOARCH=arm
GOARM=7
CGO_ENABLED=0
```

## Building

```bash
mkdir robot_app && cd robot_app
go mod init robot_app
# write main.go
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -trimpath -ldflags="-w -s" -o robot_app .
```

You'll get a fully static ~5 MB ELF. `file` should report:

```
robot_app: ELF 32-bit LSB executable, ARM, EABI5 version 1 (SYSV), statically linked, ... stripped
```

## Pushing and running

```bash
adb -s 192.168.11.1:5555 push robot_app /userdata/robot_app
adb -s 192.168.11.1:5555 shell "chmod +x /userdata/robot_app"

# Stop the vendor stack so we can take over the serial ports + cameras
adb shell "killall frodobot.sh frodobot.bin"

# Run our binary as a daemon
adb shell "/userdata/robot_app -daemon -listen :8080 -allow-motor"
```

## The self-daemonize trick

`adb shell` allocates a PTY and HUPs all children when its session ends — even with `nohup`/`setsid` shell tricks. The reliable fix is to make your binary daemonize itself:

```go
// daemonize re-execs ourselves detached. Parent exits; child continues with
// stdin=/dev/null, stdout/stderr -> log file, and a fresh session id.
func daemonize(logPath string) {
    if os.Getenv("DAEMONIZED") == "1" {
        return // already the child
    }
    logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
    if err != nil { panic(err) }
    null, _ := os.Open(os.DevNull)
    cmd := exec.Command(os.Args[0], os.Args[1:]...)
    cmd.Env = append(os.Environ(), "DAEMONIZED=1")
    cmd.Stdin = null
    cmd.Stdout = logF
    cmd.Stderr = logF
    cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
    cmd.Start()
    fmt.Fprintf(os.Stderr, "daemonized as pid %d\n", cmd.Process.Pid)
    os.Exit(0)
}

func main() {
    daemon := flag.Bool("daemon", false, "fork into background")
    flag.Parse()
    if *daemon { daemonize("/tmp/robot.log") }
    // ... rest of program
}
```

The child runs in a brand new session (`setsid`), with all file descriptors redirected. When the parent (and the adb shell) exits, the child keeps going.

## Architecture of the example

```
                    ┌──────────────────┐
   POST /api/motor──┤  HTTP server     │      ┌──────────────┐
                    │  (net/http)      │──┬──→│ motor writer │──── /dev/ttyS0
                    │                  │  │   │ (10 Hz tick) │     UCP encoder
                    │  GET  /api/...   │  │   └──────────────┘
                    └────────┬─────────┘  │           ▲
                             │            │           │
                             ▼            │     watchdog (state)
                    ┌─────────────────────┴─┐
                    │  Shared state         │←──┐
                    │  • latest telemetry   │   │
                    │  • latest GPS fix     │   │
                    │  • current motor cmd  │   │
                    └───────────────────────┘   │
                             ▲                  │
                             │                  │
              ┌──────────────┴──────┐           │
              │ STM32 reader        │           │
              │ (UCP frame parser)  │←──────────┤  /dev/ttyS0
              └─────────────────────┘           │
                                                │
              ┌─────────────────────┐           │
              │ GPS reader          │           │
              │ (NMEA parser)       │←──────────┘  /dev/ttyS2
              └─────────────────────┘
```

## Motor watchdog

The HTTP API accepts one motor command per POST. A goroutine sends the *current* commanded motor frame to the STM32 at 10 Hz. If no new POST arrives within `motor-watchdog` (default 500 ms), the goroutine sends zero — so a crashed HTTP client can't make the robot run away.

```go
func (s *State) motorTarget(timeout time.Duration) (MotorCmd, bool) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    if time.Since(s.lastMotor) > timeout {
        return MotorCmd{}, true  // zero command if stale
    }
    return s.curMotor, false
}
```

Combined with the `-allow-motor` flag (motors are no-op unless explicitly enabled at startup) and a hard-coded `-max-speed` cap, this gives you three independent safety gates.

## HTTP API

```
GET  /                  Plain-text help
GET  /api/streams       JSON list of RTSP URLs
GET  /api/telemetry     JSON: latest STM32 RPM_REPORT
GET  /api/gps           JSON: latest GPS fix (parsed $GxGGA)
POST /api/motor         Body: {"speed": int, "angular": int}
                        Both clamped to [-max-speed, max-speed]
                        Returns 403 if -allow-motor not set
```

Request example:
```bash
curl -X POST -H 'Content-Type: application/json' \
     -d '{"speed":30,"angular":0}' \
     http://192.168.11.1:8080/api/motor
```

## Replacing the stock supervisor

If you want your binary to start on boot instead of `frodobot.sh`:

```bash
# /etc/init.d/ is on the read-only rootfs, but you can hook /userdata
# Add to /userdata/init.sh (called by some vendor init scripts), or write your own /etc/init.d entry that survives reboot.
# Since /etc is read-only, you need to add a startup hook in /userdata that the existing init scripts pick up — TBD per firmware revision.
# Easiest: leave frodobot.sh in place but kill it from your binary, then keep your binary running.
```

This part is firmware-revision-dependent and worth working out per device. For development, just `adb shell` in and start your binary manually.

## Going further

Things you can layer on:

- Spawn `sample_demo_dual_camera` as a child process so video starts when your binary starts
- Add the NTRIP client (port the Python from [gps-and-rtk.md](gps-and-rtk.md) into your Go binary's goroutine — a few hundred lines)
- WebSocket endpoint for real-time telemetry instead of polling `/api/telemetry`
- Web UI served from the same binary (embed static files with `embed.FS`)
- Auth on `/api/motor` so any device on the AP can't drive the robot

The protocol pieces are all here. The rest is application code.
