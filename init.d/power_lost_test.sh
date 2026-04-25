#!/bin/sh
# /data/cfg/rockchip_test/power_lost_test.sh
#
# This is NOT a power-lost-test script — it's a boot hook for our custom firmware.
# We're using it because /etc/init.d/S99_auto_reboot already sources whatever
# script lives at this path, and /data (-> /userdata) is the only writable
# location available pre-rcS-finish on this unit (rootfs and /oem are squashfs).
#
# To revert to stock vendor firmware:
#   rm /data/cfg/rockchip_test/power_lost_test.sh
#   reboot
#
# (Or run /userdata/restore_stock.sh which does the rm + tells you what to do.)

# Idempotent stop: kill the vendor app AND any previous instances of ours
# (so re-running this script by hand doesn't pile up duplicate processes).
killall frodobot.sh frodobot.bin robot_app sample_demo_dual_camera 2>/dev/null
sleep 1

# Camera demo: 4 RTSP streams at rtsp://<robot>:554/live/{0..3}
setsid /oem/usr/bin/sample_demo_dual_camera \
    -s 0 -W 1920 -H 1080 -w 720 -h 576 -f 30 -r 0 \
    -s 1 -W 1920 -H 1080 -w 720 -h 576 -f 30 -r 0 \
    -n 0 -b 1 \
    </dev/null >/tmp/rtsp.log 2>&1 &

# Give cameras a moment to claim the sensors before we start touching ttyS2.
sleep 3

# Source per-device config (NTRIP creds, etc) from /userdata/robot_app.env if present.
# This file is NOT committed to the public repo — credentials stay on-device.
NTRIP_ARGS=""
if [ -f /userdata/robot_app.env ]; then
    . /userdata/robot_app.env
    [ -n "$NTRIP_HOST"  ] && NTRIP_ARGS="$NTRIP_ARGS -ntrip-host $NTRIP_HOST"
    [ -n "$NTRIP_PORT"  ] && NTRIP_ARGS="$NTRIP_ARGS -ntrip-port $NTRIP_PORT"
    [ -n "$NTRIP_MOUNT" ] && NTRIP_ARGS="$NTRIP_ARGS -ntrip-mount $NTRIP_MOUNT"
    [ -n "$NTRIP_USER"  ] && NTRIP_ARGS="$NTRIP_ARGS -ntrip-user $NTRIP_USER"
    [ -n "$NTRIP_PASS"  ] && NTRIP_ARGS="$NTRIP_ARGS -ntrip-pass $NTRIP_PASS"
fi

# Custom firmware: HTTP API, RTK pipeline (if env supplied), motor control.
/userdata/robot_app -daemon -log /tmp/robot.log \
    -listen :8080 \
    -allow-motor -max-speed 100 \
    $NTRIP_ARGS
