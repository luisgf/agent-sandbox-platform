#!/bin/sh
# The browser of the desktop variant: Chromium on display :1, the size of the screen, through the
# node's proxy (the kernel command line gives every service HTTPS_PROXY), with the DevTools Protocol
# on the guest's loopback only. The managed policy (/etc/chromium/policies/managed/asp.json) turns off
# QUIC, DNS over HTTPS and the background traffic to Google. None of this is the egress boundary: the
# node drops whatever does not go through its proxy, whatever the browser is told.
proxy="${HTTPS_PROXY:-${https_proxy:-${HTTP_PROXY:-${http_proxy:-}}}}"
w="${ASP_DESKTOP_GEOMETRY%x*}" h="${ASP_DESKTOP_GEOMETRY#*x}"
set -- --no-first-run --no-default-browser-check --password-store=basic --ozone-platform=x11 \
	--user-data-dir="$HOME/.config/chromium" \
	--remote-debugging-address=127.0.0.1 --remote-debugging-port=9222 \
	--window-position=0,0 --window-size="$w,$h" --start-maximized
[ -n "$proxy" ] && set -- "$@" --proxy-server="$proxy"
exec /usr/bin/chromium "$@" ${ASP_DESKTOP_EXTRA_FLAGS:-} about:blank
