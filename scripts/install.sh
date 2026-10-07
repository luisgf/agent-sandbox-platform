#!/bin/sh
# Installs ASP on this host: the CLI, a control plane, or a node.
#
#   curl -fsSL https://github.com/luisgf/agent-sandbox-platform/releases/latest/download/install.sh | sudo sh
#   curl -fsSL .../install.sh | sudo INSTALL_ASP_ROLE=server sh
#   curl -fsSL .../install.sh | sudo INSTALL_ASP_ROLE=agent INSTALL_ASP_SERVER=https://cp.example:8443 \
#       INSTALL_ASP_TOKEN=<token from `asp node enroll-token`> sh
#
# It downloads the packages of a release (.deb or .rpm; a tarball on any other Linux), checks
# them against the release's SHA256SUMS, installs them, writes /etc/asp/*.env from the
# variables below, and starts the service. It never starts a service on a host without
# systemd, and it never overwrites a settings file that is already there.
#
#   INSTALL_ASP_ROLE      cli (default), server (control plane + cli) or agent (node + cli)
#   INSTALL_ASP_VERSION   a release, e.g. 0.1.0 (default: the latest)
#   INSTALL_ASP_URL       where the release's files are, without the trailing slash (a mirror, or
#                         a directory served over HTTP); default: this project's GitHub release
#   INSTALL_ASP_METHOD    deb, rpm or tar (default: the package manager of the host)
#   INSTALL_ASP_NO_START  1 = install and configure, start nothing
#   INSTALL_ASP_FORCE     1 = replace /etc/asp/*.env even if it exists (the old one is kept as .bak)
#
# server:
#   INSTALL_ASP_LISTEN        address to listen on (default 127.0.0.1:8080; nodes on other hosts
#                             need an address they reach, and TLS)
#   INSTALL_ASP_DATABASE_URL  a Postgres (default: none, and the state is lost at a restart)
#   INSTALL_ASP_SELF_SIGNED   1 = make a self-signed TLS certificate (/etc/asp/tls.crt) for
#                             INSTALL_ASP_TLS_SAN (default: this host's name and 127.0.0.1); nodes
#                             trust it as their INSTALL_ASP_CA
#   INSTALL_ASP_TLS_CERT, INSTALL_ASP_TLS_KEY   a certificate you already have
#
# agent (a node, which needs KVM):
#   INSTALL_ASP_SERVER    the URL of the control plane (required)
#   INSTALL_ASP_TOKEN     an enroll token (asp node enroll-token --node-id <id> on the control plane)
#   INSTALL_ASP_NODE_ID   this node's id (default: the host name)
#   INSTALL_ASP_ENDPOINT  how the control plane reaches this node (default https://<id>:9443)
#   INSTALL_ASP_CA        a PEM file with the CA of the control plane's TLS certificate
#   INSTALL_ASP_SKIP_IMAGE  1 = do not pull the guest kernel and image of the release
set -eu

REPO="${INSTALL_ASP_REPO:-luisgf/agent-sandbox-platform}"
ROLE="${INSTALL_ASP_ROLE:-cli}"
VERSION="${INSTALL_ASP_VERSION:-latest}"
BASE_URL="${INSTALL_ASP_URL:-}"
ETC="${INSTALL_ASP_ETC:-/etc/asp}"

say() { printf '%s\n' "$*"; }
warn() { printf 'install-asp: %s\n' "$*" >&2; }
die() { warn "$*"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo sh)"
for r in $(printf '%s' "$ROLE" | tr ',' ' '); do
	case "$r" in cli | server | agent) ;; *) die "INSTALL_ASP_ROLE: $r is not cli, server or agent" ;; esac
done
has_role() { case ",$ROLE," in *",$1,"*) return 0 ;; *) return 1 ;; esac }

OS=$(uname -s)
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "no release for the architecture $(uname -m)" ;;
esac
case "$OS" in
Linux) OSN=linux ;;
Darwin) OSN=darwin ;;
*) die "no release for $OS" ;;
esac
if [ "$OSN" = darwin ] && { has_role server || has_role agent; }; then
	die "a control plane and a node run on Linux; on $OS only INSTALL_ASP_ROLE=cli"
fi

# Download with curl, or wget.
fetch() { # <url> <file>
	if have curl; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif have wget; then
		wget -q -O "$2" "$1"
	else
		die "needs curl or wget"
	fi
}
sha256() {
	if have sha256sum; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# The latest release is where github.com/<repo>/releases/latest redirects to.
if [ "$VERSION" = latest ]; then
	[ -z "$BASE_URL" ] || die "INSTALL_ASP_URL needs INSTALL_ASP_VERSION: a directory has no \"latest\""
	have curl || die "finding the latest release needs curl: name one with INSTALL_ASP_VERSION"
	VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's|.*/tag/||; s|^v||')
	[ -n "$VERSION" ] && [ "${VERSION#http}" = "$VERSION" ] || die "cannot tell which release is the latest (is there one yet?): name it with INSTALL_ASP_VERSION"
fi
VERSION=${VERSION#v}
[ -n "$BASE_URL" ] || BASE_URL="https://github.com/$REPO/releases/download/v$VERSION"

METHOD="${INSTALL_ASP_METHOD:-}"
if [ -z "$METHOD" ]; then
	if [ "$OSN" = linux ] && have dpkg; then
		METHOD=deb
	elif [ "$OSN" = linux ] && have rpm; then
		METHOD=rpm
	else
		METHOD=tar
	fi
fi
case "$METHOD" in deb | rpm | tar) ;; *) die "INSTALL_ASP_METHOD: $METHOD is not deb, rpm or tar" ;; esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

say "==> ASP $VERSION for $OSN/$ARCH ($METHOD), role $ROLE"
fetch "$BASE_URL/SHA256SUMS" "$TMP/SHA256SUMS" || die "cannot download $BASE_URL/SHA256SUMS (is $VERSION a release?)"

# download <file name> [optional]: into $TMP, and checked against SHA256SUMS. A file that cannot be
# fetched is fatal, unless it is optional; one that does not match never is accepted.
download() {
	f=$1
	if ! fetch "$BASE_URL/$f" "$TMP/$f"; then
		[ "${2:-}" = optional ] && return 1
		die "cannot download $BASE_URL/$f"
	fi
	want=$(awk -v f="$f" '{ n=$2; sub(/^\*/, "", n); if (n == f) print $1 }' "$TMP/SHA256SUMS")
	[ -n "$want" ] || die "$f is not in the release's SHA256SUMS"
	got=$(sha256 "$TMP/$f")
	[ "$got" = "$want" ] || die "$f is $got but SHA256SUMS says $want: not installing it"
}

# install_component <asp | asp-control-plane | asp-node-agent>
install_component() {
	name=$1
	case "$METHOD" in
	deb)
		f="${name}_${VERSION}_linux_${ARCH}.deb"
		download "$f"
		dpkg -i "$TMP/$f" >/dev/null
		;;
	rpm)
		f="${name}_${VERSION}_linux_${ARCH}.rpm"
		download "$f"
		rpm -U --quiet "$TMP/$f"
		;;
	tar)
		f="${name}_${VERSION}_${OSN}_${ARCH}.tar.gz"
		download "$f"
		d="$TMP/x-$name"
		mkdir -p "$d"
		tar -xzf "$TMP/$f" -C "$d"
		install -m 0755 "$d/$name" /usr/local/bin/"$name"
		if [ "$name" = asp-control-plane ] && ! getent passwd asp-control-plane >/dev/null 2>&1; then
			useradd --system --user-group --home-dir /var/lib/asp-control-plane --no-create-home \
				--shell /usr/sbin/nologin asp-control-plane || warn "cannot create the user asp-control-plane"
		fi
		if [ -f "$d/$name.service" ]; then
			mkdir -p /etc/systemd/system
			# The packaged units run /usr/bin/<name>; a tarball puts it in /usr/local/bin.
			sed "s|/usr/bin/$name|/usr/local/bin/$name|g" "$d/$name.service" >/etc/systemd/system/"$name.service"
		fi
		for e in "$d"/*.env; do
			[ -f "$e" ] || continue
			mkdir -p "$ETC"
			[ -e "$ETC/$(basename "$e")" ] || install -m 0600 "$e" "$ETC/$(basename "$e")"
		done
		;;
	esac
	say "    installed $name"
}

install_component asp
BIN_ASP=asp
[ "$METHOD" = tar ] && BIN_ASP=/usr/local/bin/asp

# The scripts that take it away again, like k3s's.
if [ "$OSN" = linux ]; then
	for s in uninstall killall; do
		if download "$s.sh" optional 2>/dev/null; then
			install -m 0755 "$TMP/$s.sh" "/usr/local/bin/asp-$s.sh"
		fi
	done
fi

systemd_up() { [ -d /run/systemd/system ] && have systemctl; }
want_start() { [ "${INSTALL_ASP_NO_START:-}" != 1 ] && systemd_up; }

# write_env <name> <content>: a settings file of mode 0600. A file that has settings already is
# left as it is (INSTALL_ASP_FORCE=1 replaces it and keeps the old one as .bak); the commented
# example a package leaves does not count.
write_env() {
	f="$ETC/$1.env"
	mkdir -p "$ETC"
	if [ -e "$f" ] && grep -q '^[A-Za-z_]' "$f" 2>/dev/null; then
		if [ "${INSTALL_ASP_FORCE:-}" != 1 ]; then
			warn "$f has settings already: left as it is (INSTALL_ASP_FORCE=1 replaces it)"
			return 1
		fi
		cp -p "$f" "$f.bak"
	fi
	(umask 077 && printf '%s' "$2" >"$f")
	chmod 0600 "$f"
	return 0
}

random_hex() { head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }

if has_role server; then
	say "==> control plane"
	install_component asp-control-plane
	LISTEN="${INSTALL_ASP_LISTEN:-127.0.0.1:8080}"
	SCHEME=http
	TLS=""
	if [ "${INSTALL_ASP_SELF_SIGNED:-}" = 1 ]; then
		have openssl || die "INSTALL_ASP_SELF_SIGNED needs openssl"
		SAN="${INSTALL_ASP_TLS_SAN:-$(hostname -f 2>/dev/null || hostname),127.0.0.1}"
		alt=""
		for n in $(printf '%s' "$SAN" | tr ',' ' '); do
			case "$n" in
			*[!0-9.]* | "") alt="$alt${alt:+,}DNS:$n" ;;
			*) alt="$alt${alt:+,}IP:$n" ;;
			esac
		done
		if [ ! -e "$ETC/tls.crt" ]; then
			(umask 077 && openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
				-subj "/CN=asp-control-plane" -addext "subjectAltName=$alt" \
				-keyout "$ETC/tls.key" -out "$ETC/tls.crt" 2>/dev/null)
			chmod 0644 "$ETC/tls.crt"
		fi
		INSTALL_ASP_TLS_CERT="$ETC/tls.crt"
		INSTALL_ASP_TLS_KEY="$ETC/tls.key"
	fi
	if [ -n "${INSTALL_ASP_TLS_CERT:-}" ] && [ -n "${INSTALL_ASP_TLS_KEY:-}" ]; then
		SCHEME=https
		TLS="ASP_TLS_CERT=$INSTALL_ASP_TLS_CERT
ASP_TLS_KEY=$INSTALL_ASP_TLS_KEY
"
		# The service's user reads the key; nobody else does.
		if getent group asp-control-plane >/dev/null 2>&1; then
			if chgrp asp-control-plane "$INSTALL_ASP_TLS_KEY" 2>/dev/null; then
				chmod 0640 "$INSTALL_ASP_TLS_KEY"
			else
				warn "make $INSTALL_ASP_TLS_KEY readable by the user asp-control-plane"
			fi
		fi
		# The enrollment CA of the control plane (it makes it at its first start) is what vouches
		# for the nodes' certificates: with it, each route of a node demands that node's certificate.
		TLS="${TLS}ASP_CLIENT_CA=/var/lib/asp-control-plane/ca.crt
"
	fi
	case "$LISTEN" in 127.* | localhost:* | "[::1]:"*) ;; *) [ -n "$TLS" ] || warn "listening on $LISTEN without TLS: authentication is on, but keys and tokens cross the network in the clear" ;; esac
	# The address to give the CLI: the one it listens on, or this host's name when that is every address.
	case "$LISTEN" in
	0.0.0.0:* | "[::]:"* | :*) SHOW="$(hostname -f 2>/dev/null || hostname):${LISTEN##*:}" ;;
	*) SHOW="$LISTEN" ;;
	esac
	if [ -s "$ETC/admin-key" ]; then
		KEY=$(cat "$ETC/admin-key")
	else
		KEY=$(random_hex)
		(umask 077 && printf '%s\n' "$KEY" >"$ETC/admin-key")
	fi
	chmod 0600 "$ETC/admin-key"
	DBLINE=""
	[ -z "${INSTALL_ASP_DATABASE_URL:-}" ] || DBLINE="ASP_DATABASE_URL=$INSTALL_ASP_DATABASE_URL
"
	write_env control-plane "# Written by install.sh. Every variable: control-plane/README.md.
ASP_LISTEN_ADDR=$LISTEN
ASP_BOOTSTRAP_API_KEY=$KEY
${TLS}${DBLINE}" || true
	[ -n "$DBLINE" ] || warn "no database (INSTALL_ASP_DATABASE_URL): the state is lost when the control plane restarts"
	if want_start; then
		systemctl daemon-reload
		systemctl enable --now asp-control-plane >/dev/null 2>&1 || warn "the control plane did not start: journalctl -u asp-control-plane"
		url="$SCHEME://$LISTEN"
		i=0
		while [ "$i" -lt 30 ]; do
			if have curl && curl -fsSk "$url/healthz" >/dev/null 2>&1; then
				say "    the control plane answers at $url"
				break
			fi
			i=$((i + 1))
			sleep 1
		done
	else
		say "    not started (INSTALL_ASP_NO_START, or no systemd): systemctl enable --now asp-control-plane"
	fi
	say ""
	say "The control plane's key is in $ETC/admin-key. To use it:"
	say "  export ASP_CONTROL_PLANE_URL=$SCHEME://$SHOW ASP_API_KEY=\$(cat $ETC/admin-key)"
	say "  asp node enroll-token --node-id <id>      # then, on the node:"
	say "  curl -fsSL $BASE_URL/install.sh | sudo INSTALL_ASP_ROLE=agent INSTALL_ASP_VERSION=$VERSION \\"
	say "      INSTALL_ASP_SERVER=$SCHEME://$SHOW INSTALL_ASP_TOKEN=<token> sh"
fi

if has_role agent; then
	say "==> node"
	[ -n "${INSTALL_ASP_SERVER:-}" ] || die "INSTALL_ASP_ROLE=agent needs INSTALL_ASP_SERVER, the URL of the control plane"
	[ -c /dev/kvm ] || warn "no /dev/kvm: this host cannot run sandboxes (virtualization off, or a VM without nested virtualization)"
	install_component asp-node-agent
	NODE_ID="${INSTALL_ASP_NODE_ID:-$(hostname -s 2>/dev/null || hostname)}"
	ENDPOINT="${INSTALL_ASP_ENDPOINT:-https://$NODE_ID:9443}"
	CA=""
	if [ -n "${INSTALL_ASP_CA:-}" ]; then
		[ -r "$INSTALL_ASP_CA" ] || die "INSTALL_ASP_CA: cannot read $INSTALL_ASP_CA"
		mkdir -p "$ETC"
		[ "$INSTALL_ASP_CA" = "$ETC/cp-ca.pem" ] || cp "$INSTALL_ASP_CA" "$ETC/cp-ca.pem"
		chmod 0644 "$ETC/cp-ca.pem"
		CA="ASP_CONTROL_PLANE_CA=$ETC/cp-ca.pem
"
	fi
	ENROLL=""
	[ -z "${INSTALL_ASP_TOKEN:-}" ] || ENROLL="ASP_ENROLL=1
ASP_NODE_ENROLL_TOKEN=$INSTALL_ASP_TOKEN
"
	write_env node-agent "# Written by install.sh. The enroll lines go away once the node has enrolled.
ASP_CONTROL_PLANE_URL=$INSTALL_ASP_SERVER
ASP_NODE_ID=$NODE_ID
ASP_AGENT_TLS_LISTEN=0.0.0.0:9443
ASP_ENDPOINT=$ENDPOINT
${CA}${ENROLL}" || true
	if [ "${INSTALL_ASP_SKIP_IMAGE:-}" != 1 ]; then
		say "    pulling the guest kernel and image of $VERSION"
		# asp image pull reads INSTALL_ASP_URL's layout: the files of the release.
		"$BIN_ASP" image pull --version "$VERSION" --base-url "$BASE_URL" || warn "no guest image: sudo asp image pull --version $VERSION (or INSTALL_ASP_SKIP_IMAGE=1 to bring your own)"
	fi
	if want_start; then
		systemctl daemon-reload
		systemctl enable --now asp-node-agent >/dev/null 2>&1 || warn "the node-agent did not start: journalctl -u asp-node-agent"
		i=0
		while [ "$i" -lt 60 ]; do
			if journalctl -u asp-node-agent --no-pager 2>/dev/null | grep -qE 'registered with control plane'; then
				say "    the node registered"
				break
			fi
			i=$((i + 1))
			sleep 1
		done
		# The token has done its job, and must not stay in a file.
		if [ -n "$ENROLL" ] && [ -f "$ETC/node-agent.env" ]; then
			grep -v -E '^(ASP_ENROLL|ASP_NODE_ENROLL_TOKEN)=' "$ETC/node-agent.env" >"$ETC/node-agent.env.new" || true
			chmod 0600 "$ETC/node-agent.env.new"
			mv "$ETC/node-agent.env.new" "$ETC/node-agent.env"
			say "    the enroll token is out of $ETC/node-agent.env"
		fi
	else
		say "    not started (INSTALL_ASP_NO_START, or no systemd): systemctl enable --now asp-node-agent"
	fi
	say ""
	say "Check this host: sudo asp doctor"
fi

if [ "$ROLE" = cli ]; then
	say ""
	say "Next: export ASP_CONTROL_PLANE_URL=https://<your control plane> and ASP_API_KEY=<key>, then: asp session start"
fi
say "==> done"
