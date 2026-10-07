#!/bin/sh
# Before the files are unpacked, in the asp-server package only. The administration key of a
# single host belongs to a group, and the asp command reads it as one of its members: the group
# is made here, empty. Add a user with `usermod -aG asp <user>`.
set -e
if ! getent group asp >/dev/null 2>&1; then
	if command -v groupadd >/dev/null 2>&1; then
		groupadd --system asp || true
	elif command -v addgroup >/dev/null 2>&1; then
		addgroup --system asp || true
	fi
fi
exit 0
