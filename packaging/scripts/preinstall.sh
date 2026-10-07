#!/bin/sh
# Before the files are unpacked, in the asp-control-plane package only. The control plane runs
# as a system user of its own, with no login, and its settings files belong to that user's
# group: the group has to exist when the package puts them in place, so this cannot wait for
# postinstall.sh.
set -e
if ! getent group asp-control-plane >/dev/null 2>&1 || ! getent passwd asp-control-plane >/dev/null 2>&1; then
	if command -v useradd >/dev/null 2>&1; then
		useradd --system --user-group --home-dir /var/lib/asp-control-plane --no-create-home \
			--shell /usr/sbin/nologin asp-control-plane || true
	elif command -v adduser >/dev/null 2>&1; then
		adduser --system --group --home /var/lib/asp-control-plane --no-create-home asp-control-plane || true
	fi
fi
exit 0
