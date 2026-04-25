# Camera Streams

The robot has two **GalaxyCore GC2093** image sensors (front + back), each capable of 1920×1080 at 30 fps. They're hooked into the Rockchip RV1106's ISP, and the SoC has a hardware H.264/H.265 encoder.

## You don't need to write any video code

The vendor's `/oem/usr/bin/` ships with a whole pile of prebuilt sample binaries that wrap Rockchip's "rockit" media framework. The most useful is **`sample_demo_dual_camera`** — it brings up both sensors and exposes 4 RTSP streams.

```bash
# Free the cameras (frodobot.bin holds them by default)
adb shell "killall frodobot.sh frodobot.bin"

# Launch the dual-camera RTSP demo
adb shell "/userdata/start_rtsp.sh"  # see startup script below
```

## Startup script

Because backgrounding through `adb shell` is unreliable, write a script:

```sh
#!/bin/sh
# /userdata/start_rtsp.sh — launch dual-camera RTSP server
# args:
#   -s <id>  sensor id (0=front, 1=back)
#   -W/-H    main stream resolution
#   -w/-h    sub stream resolution
#   -f       fps
#   -r       HDR
#   -n       enable NPU (set 0 to disable analytics)
#   -b       enable buffer sharing (1 = save memory)

setsid /oem/usr/bin/sample_demo_dual_camera \
    -s 0 -W 1920 -H 1080 -w 720 -h 576 -f 30 -r 0 \
    -s 1 -W 1920 -H 1080 -w 720 -h 576 -f 30 -r 0 \
    -n 0 -b 1 \
    </dev/null >/tmp/rtsp.log 2>&1 &
```

Or start it from a self-daemonizing host program — see [building-custom-firmware.md](building-custom-firmware.md).

## RTSP URLs

Once running:

| URL                                      | Sensor    | Stream | Resolution | Codec |
|------------------------------------------|-----------|--------|------------|-------|
| `rtsp://192.168.11.1:554/live/0`         | Front     | main   | 1920×1080  | H.265 |
| `rtsp://192.168.11.1:554/live/1`         | Front     | sub    | 720×576    | H.265 |
| `rtsp://192.168.11.1:554/live/2`         | Back      | main   | 1920×1080  | H.265 |
| `rtsp://192.168.11.1:554/live/3`         | Back      | sub    | 720×576    | H.265 |

## Consuming the streams

### ffplay
```bash
ffplay -rtsp_transport tcp rtsp://192.168.11.1:554/live/0
```

### Save a clip
```bash
ffmpeg -rtsp_transport tcp -i rtsp://192.168.11.1:554/live/0 -t 10 -c copy clip.mp4
```

### GStreamer
```bash
gst-launch-1.0 rtspsrc location=rtsp://192.168.11.1:554/live/0 latency=200 ! \
    rtph265depay ! h265parse ! avdec_h265 ! videoconvert ! autovideosink
```

### Probe stream metadata
```bash
ffprobe -rtsp_transport tcp rtsp://192.168.11.1:554/live/0
```

## Other useful binaries in `/oem/usr/bin/`

| Binary                                          | Purpose                              |
|-------------------------------------------------|--------------------------------------|
| `sample_demo_dual_camera`                       | The one you want — 4 RTSP streams    |
| `sample_demo_dual_camera_wrap`                  | Same with EPTZ "wrap" mode           |
| `sample_demo_multi_camera_eptz`                 | Multi-cam with electronic PTZ        |
| `simple_vi_bind_venc_rtsp`                      | Single sensor, single RTSP at /live/0 |
| `simple_vi_bind_venc_rtsp_three_camera_rv1106`  | 3-camera variant                     |
| `sample_vi`, `sample_venc_stresstest`           | VI/VENC stress tests                 |
| `tegrastats`-equivalents                        | Various Rockchip diagnostics         |

There are ~30 sample binaries total — `ls /oem/usr/bin/ | grep -E 'sample|simple'` to see them all.

## Tips and pitfalls

- **The cameras can only be opened by ONE process at a time.** If you see "failed to open camera:stream_cif_mipi_id0" in the log, something else is holding them — most often `frodobot.bin`. Kill the supervisor first.
- The chip needs warm-up on first start; the demo log will print `m00_f_gc2093` / `m01_b_gc2093` when sensors initialize. If those messages don't appear, sensor init failed.
- `-n 1` enables NPU analytics (object detection on top of the video pipeline). Adds CPU/RAM overhead. Set `-n 0` if you don't need analytics — saves memory and avoids needing the IVA model files in `/usr/lib/`.
- The `rtsp_demo` library is the vendor's own — it doesn't fully implement the RTSP spec, so some clients may stumble. `ffplay -rtsp_transport tcp` always works in our testing.
