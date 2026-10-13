#!/bin/sh
# Waits up to 10 s for the X display :1 to accept connections (its socket appears).
i=0
while [ ! -S /tmp/.X11-unix/X1 ]; do
	i=$((i + 1))
	[ "$i" -le 100 ] || { echo "display :1 is not up" >&2; exit 1; }
	sleep 0.1
done
