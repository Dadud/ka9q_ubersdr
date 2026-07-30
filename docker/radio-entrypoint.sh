#!/bin/sh
set -eu

config_path="${1:-/etc/ka9q-radio/radiod@ubersdr.conf}"
if [ ! -f "$config_path" ]; then
    echo "No generated receiver configuration found; using the bundled setup receiver."
    cp /usr/local/share/ka9q-radio/config-templates/radiod@siggen.conf "$config_path"
fi

# UltraSDR writes only declarative driver modules/libraries into this volume.
# A config may opt into one with `library = /opt/ultrasdr-drivers/modules/x.so`.
driver_lib_paths=""
for driver_lib in /opt/ultrasdr-drivers/*/lib; do
    [ -d "$driver_lib" ] || continue
    driver_lib_paths="${driver_lib_paths:+$driver_lib_paths:}$driver_lib"
done
if [ -n "$driver_lib_paths" ]; then
    LD_LIBRARY_PATH="$driver_lib_paths${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
    export LD_LIBRARY_PATH
fi

ip link set lo multicast on 2>/dev/null || true
if ip link show eth0 >/dev/null 2>&1; then
    ip link set eth0 multicast on 2>/dev/null || true
    ip link set eth0 allmulticast on 2>/dev/null || true
fi

(
    while :; do
        if [ -f /var/run/restart-trigger/restart ]; then
            rm -f /var/run/restart-trigger/restart
            kill -TERM 1
            exit 0
        fi
        sleep 1
    done
) &

exec /usr/local/sbin/radiod "$config_path"
