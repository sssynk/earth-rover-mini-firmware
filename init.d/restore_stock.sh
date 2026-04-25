#!/bin/sh
# /userdata/restore_stock.sh
#
# Removes the custom-firmware boot hook so the next reboot launches
# only the vendor app stack.

set -e

HOOK=/data/cfg/rockchip_test/power_lost_test.sh
if [ -f "$HOOK" ]; then
    rm -f "$HOOK"
    echo "Removed boot hook: $HOOK"
else
    echo "No boot hook present at $HOOK — nothing to do."
fi

killall robot_app sample_demo_dual_camera 2>/dev/null && \
    echo "Stopped custom services." || \
    echo "Custom services were not running."

echo
echo "Reboot to fully restore stock vendor firmware, or run:"
echo "    /etc/init.d/S95frodobots start"
echo "to start the vendor stack now without rebooting."
