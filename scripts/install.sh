#!/bin/sh
# Installs ASP on this host: the CLI, a control plane, a node, or all of it on one host.
#
#   curl -fsSL https://github.com/luisgf/agent-sandbox-platform/releases/latest/download/install.sh | sudo sh
#   curl -fsSL .../install.sh | sudo INSTALL_ASP_ROLE=standalone sh      # then: asp session start
#   curl -fsSL .../install.sh | sudo INSTALL_ASP_ROLE=server sh
#   curl -fsSL .../install.sh | sudo INSTALL_ASP_ROLE=agent INSTALL_ASP_SERVER=https://cp.example:8443 \
#       INSTALL_ASP_TOKEN=<token from `asp node enroll-token`> sh
#
# It downloads the packages of a release (.deb or .rpm; a tarball on any other Linux), checks
# them against the release's SHA256SUMS, installs them, writes the settings the variables below
# give into a drop-in (/etc/asp/server.yaml.d/10-install.yaml, /etc/asp/agent.yaml.d/10-install.yaml)
# and starts the service. It never starts a service on a host without systemd, and it never
# overwrites a drop-in that is already there.
#
#   INSTALL_ASP_ROLE      cli (default), server (control plane + cli), agent (node + cli) or standalone
#                         (a control plane and a node on this host, made and run by asp-server)
#   INSTALL_ASP_VERSION   a release, e.g. 0.1.0 (default: the latest)
#   INSTALL_ASP_URL       where the release's files are, without the trailing slash (a mirror, or
#                         a directory served over HTTP); default: this project's GitHub release
#   INSTALL_ASP_METHOD    deb, rpm or tar (default: the package manager of the host)
#   INSTALL_ASP_NO_START  1 = install and configure, start nothing
#   INSTALL_ASP_FORCE     1 = replace a 10-install.yaml that exists (the old one is kept as .bak)
#   INSTALL_ASP_SKIP_VMM  1 = on a node (agent, standalone), do not install Cloud Hypervisor and virtiofsd
#   INSTALL_ASP_CH_URL    where the Cloud Hypervisor release files are, without the trailing slash (a mirror;
#                         default: the project's release of the version this script pins, whose SHA-256 it checks)
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
# standalone (a node needs KVM; INSTALL_ASP_PROFILE=lab runs one without VMs):
#   INSTALL_ASP_LISTEN    where the control plane listens (default 127.0.0.1:8443, the loopback;
#                         0.0.0.0:8443 lets other hosts join)
#   INSTALL_ASP_TLS_SAN   more names or addresses its certificate is valid for (comma separated)
#   INSTALL_ASP_NODE_ID   the id of the node on this host (default: the host name and -node)
#   INSTALL_ASP_PROFILE   default, or lab
#   INSTALL_ASP_SKIP_IMAGE  1 = do not pull the guest kernel and image of the release
#
# agent (a node, which needs KVM):
#   INSTALL_ASP_SERVER    the URL of the control plane (required)
#   INSTALL_ASP_TOKEN     an enroll token (asp node enroll-token --node-id <id> on the control plane)
#   INSTALL_ASP_NODE_ID   this node's id (default: the host name)
#   INSTALL_ASP_ENDPOINT  how the control plane reaches this node (default https://<id>:9443)
#   INSTALL_ASP_CA        a PEM file with the CA of the control plane's TLS certificate
#   INSTALL_ASP_CA_SHA256 instead of the file: the SHA-256 of the certificate the control plane shows
#                         (asp node enroll-token prints it). The script reads that certificate from
#                         INSTALL_ASP_SERVER, refuses it if the fingerprint differs, and trusts it.
#                         For a certificate that is its own trust anchor (a self-signed one); needs openssl
#   INSTALL_ASP_SKIP_IMAGE  1 = do not pull the guest kernel and image of the release
set -eu

REPO="${INSTALL_ASP_REPO:-luisgf/agent-sandbox-platform}"
ROLE="${INSTALL_ASP_ROLE:-cli}"
VERSION="${INSTALL_ASP_VERSION:-latest}"
BASE_URL="${INSTALL_ASP_URL:-}"
ETC="${INSTALL_ASP_ETC:-/etc/asp}"

# The Cloud Hypervisor a node runs: the version ASP is tested with (the node-agent's doctor warns about
# another major version), from the project's own release. It is refused unless it has the SHA-256 written
# here (GitHub's digests of the release assets). Raising it: docs/how-to/release.md.
CH_VERSION=v53.0
CH_SHA256_AMD64=448af3d4e59b22c2987f7df94c213ad40fb53a10d437e42b5ee6c4fce7c29ecc
CH_SHA256_ARM64=f192b510eea1c710cbc439d716bb0573c223fc463dbe3e6523788a2b7ef62850

say() { printf '%s\n' "$*"; }
warn() { printf 'install-asp: %s\n' "$*" >&2; }
die() { warn "$*"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo sh)"
for r in $(printf '%s' "$ROLE" | tr ',' ' '); do
	case "$r" in cli | server | agent | standalone) ;; *) die "INSTALL_ASP_ROLE: $r is not cli, server, agent or standalone" ;; esac
done
case ",$ROLE," in *,standalone,*)
	case ",$ROLE," in *,server,* | *,agent,*) die "INSTALL_ASP_ROLE: standalone is the control plane and the node together: do not add server or agent" ;; esac ;;
esac
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
if [ "$OSN" = darwin ] && { has_role server || has_role agent || has_role standalone; }; then
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
# Only release files go in it, and the package manager reads them as a user of its own.
chmod 0755 "$TMP"

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

# The packages go in through the package manager when there is one, so that what they depend on (nftables
# and iproute2, for a node) comes with them; dpkg or rpm alone installs what is given and fails on a missing
# dependency.
APT_UPDATED=""
apt_update() {
	[ -n "$APT_UPDATED" ] || { apt-get update -qq >/dev/null 2>&1 || warn "apt-get update failed: installing from the package lists this host has"; }
	APT_UPDATED=1
}
# pkg_add <name>: a package of the distribution, by whatever manager there is. Fails when there is none.
pkg_add() {
	if have apt-get; then
		apt_update
		NEEDRESTART_SUSPEND=1 DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$1" >/dev/null 2>&1
	elif have dnf; then
		dnf install -y -q "$1" >/dev/null 2>&1
	elif have yum; then
		yum install -y -q "$1" >/dev/null 2>&1
	elif have zypper; then
		zypper --non-interactive -q install "$1" >/dev/null 2>&1
	else
		return 1
	fi
}
# pkg_file <deb | rpm> <file>: a package file, with its dependencies from the repositories.
pkg_file() {
	if [ "$1" = deb ]; then
		if have apt-get; then
			apt_update
			NEEDRESTART_SUSPEND=1 DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$2" >/dev/null && return 0
			warn "apt-get could not install $2 with its dependencies: trying dpkg (what it depends on is then yours)"
		fi
		dpkg -i "$2" >/dev/null
	else
		if have dnf; then
			dnf install -y -q "$2" >/dev/null && return 0
		elif have yum; then
			yum install -y -q "$2" >/dev/null && return 0
		elif have zypper; then
			zypper --non-interactive -q install --allow-unsigned-rpm "$2" >/dev/null && return 0
		fi
		warn "the package manager could not install $2 with its dependencies: trying rpm (what it depends on is then yours)"
		rpm -U --quiet "$2"
	fi
}

# install_component <asp | asp-control-plane | asp-node-agent>
install_component() {
	name=$1
	case "$METHOD" in
	deb)
		f="${name}_${VERSION}_linux_${ARCH}.deb"
		download "$f"
		pkg_file deb "$TMP/$f"
		;;
	rpm)
		f="${name}_${VERSION}_linux_${ARCH}.rpm"
		download "$f"
		pkg_file rpm "$TMP/$f"
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
		if [ "$name" = asp-server ] && ! getent group asp >/dev/null 2>&1; then
			groupadd --system asp 2>/dev/null || warn "cannot create the group asp"
		fi
		if [ -f "$d/$name.service" ]; then
			mkdir -p /etc/systemd/system
			# The packaged units run /usr/bin/<name>; a tarball puts it in /usr/local/bin.
			sed "s|/usr/bin/$name|/usr/local/bin/$name|g" "$d/$name.service" >/etc/systemd/system/"$name.service"
		fi
		# The settings file the packages leave in /etc/asp, and its drop-in directory. The control
		# plane reads its own as its own user; the node-agent runs as root.
		for e in "$d"/*.yaml; do
			[ -f "$e" ] || continue
			b=$(basename "$e")
			mkdir -p "$ETC" "$ETC/$b.d"
			if [ "$name" = asp-control-plane ] && getent group asp-control-plane >/dev/null 2>&1; then
				[ -e "$ETC/$b" ] || install -m 0640 -g asp-control-plane "$e" "$ETC/$b"
				chgrp asp-control-plane "$ETC/$b.d" && chmod 0750 "$ETC/$b.d"
			else
				[ -e "$ETC/$b" ] || install -m 0600 "$e" "$ETC/$b"
				chmod 0700 "$ETC/$b.d"
			fi
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

# write_dropin <server|agent> <file name> <content>: a settings drop-in of mode 0600 (the control
# plane's belongs to its user, which reads it itself). A file that has settings already is left as
# it is (INSTALL_ASP_FORCE=1 replaces it and keeps the old one as .bak).
write_dropin() {
	dir="$ETC/$1.yaml.d"
	f="$dir/$2"
	mkdir -p "$dir"
	if [ "$1" = server ] && getent group asp-control-plane >/dev/null 2>&1; then
		chgrp asp-control-plane "$dir" && chmod 0750 "$dir"
	else
		chmod 0700 "$dir"
	fi
	if [ -s "$f" ]; then
		if [ "${INSTALL_ASP_FORCE:-}" != 1 ]; then
			warn "$f exists already: left as it is (INSTALL_ASP_FORCE=1 replaces it)"
			return 1
		fi
		cp -p "$f" "$f.bak"
	fi
	(umask 077 && printf '%s' "$3" >"$f")
	if [ "$1" = server ] && getent group asp-control-plane >/dev/null 2>&1; then
		chgrp asp-control-plane "$f" && chmod 0640 "$f"
	else
		chmod 0600 "$f"
	fi
	return 0
}

# yaml_str <text>: a YAML string, quoted. The only characters that matter in a double-quoted
# string are the backslash and the quote.
yaml_str() { printf '"%s"' "$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')"; }

random_hex() { head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# install_vmm: what a node runs the sandboxes with. Cloud Hypervisor is in no distribution's repositories
# in a version ASP is tested with, so it comes from the project's release, checked against the SHA-256 this
# script pins. virtiofsd (the Rust one, for sandboxes with a workspace) comes from the distribution where it
# has one (Ubuntu 24.04, Debian 13); elsewhere it is yours, and everything but workspaces works without it.
install_vmm() {
	[ "$OSN" = linux ] || return 0
	[ "${INSTALL_ASP_SKIP_VMM:-}" != 1 ] || return 0
	if have cloud-hypervisor || [ -x /usr/local/bin/cloud-hypervisor ]; then
		chv=$( (cloud-hypervisor --version || /usr/local/bin/cloud-hypervisor --version) 2>/dev/null | head -n 1)
		say "    cloud-hypervisor is there already (${chv:-unknown version})"
	else
		case "$ARCH" in
		amd64) asset=cloud-hypervisor-static want=$CH_SHA256_AMD64 ;;
		arm64) asset=cloud-hypervisor-static-aarch64 want=$CH_SHA256_ARM64 ;;
		esac
		churl="${INSTALL_ASP_CH_URL:-https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$CH_VERSION}/$asset"
		say "    installing Cloud Hypervisor $CH_VERSION"
		if fetch "$churl" "$TMP/$asset"; then
			got=$(sha256 "$TMP/$asset")
			[ "$got" = "$want" ] || die "$asset is $got but $want was expected: not installing it"
			install -m 0755 "$TMP/$asset" /usr/local/bin/cloud-hypervisor
		else
			warn "cannot download $churl: install Cloud Hypervisor $CH_VERSION yourself (docs/how-to/install-node.md), point INSTALL_ASP_CH_URL at a mirror, or INSTALL_ASP_SKIP_VMM=1 to skip this"
		fi
	fi
	if ! have virtiofsd && [ ! -x /usr/libexec/virtiofsd ] && [ ! -x /usr/local/bin/virtiofsd ]; then
		pkg_add virtiofsd || warn "no virtiofsd (the Rust one: Ubuntu 24.04 and Debian 13 have it as a package): a sandbox with a workspace will not start; the others do"
	fi
}

# check_node_tools: what the node-agent runs, besides itself. The packages depend on nftables and
# iproute2; a tarball install does not, and a node without nft cannot enforce egress and does not start.
check_node_tools() {
	[ "$OSN" = linux ] || return 0
	for t in nft ip setpriv systemd-run; do
		have "$t" || warn "$t is not installed: the node-agent needs it (nftables, iproute2, util-linux, systemd)"
	done
}

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
		TLS="tls_cert: $(yaml_str "$INSTALL_ASP_TLS_CERT")
tls_key: $(yaml_str "$INSTALL_ASP_TLS_KEY")
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
		TLS="${TLS}client_ca: /var/lib/asp-control-plane/ca.crt
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
	[ -z "${INSTALL_ASP_DATABASE_URL:-}" ] || DBLINE="database_url: $(yaml_str "$INSTALL_ASP_DATABASE_URL")
"
	write_dropin server 10-install.yaml "# Written by install.sh. Every setting: control-plane/README.md; the package's own are in ../server.yaml.
listen_addr: $(yaml_str "$LISTEN")
bootstrap_api_key: $(yaml_str "$KEY")
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
	# Before anything is installed: a certificate that is not the one expected stops the script here.
	if [ -z "${INSTALL_ASP_CA:-}" ] && [ -n "${INSTALL_ASP_CA_SHA256:-}" ]; then
		have openssl || die "INSTALL_ASP_CA_SHA256 needs openssl, to read the certificate of $INSTALL_ASP_SERVER"
		hostport=${INSTALL_ASP_SERVER#*://}
		hostport=${hostport%%/*}
		case "$hostport" in *:*) ;; *) hostport="$hostport:443" ;; esac
		openssl s_client -connect "$hostport" -servername "${hostport%:*}" </dev/null 2>/dev/null | openssl x509 -outform PEM >"$TMP/server-cert.pem" 2>/dev/null ||
			die "cannot read the certificate of $hostport (is the control plane listening there?)"
		got=$(openssl x509 -in "$TMP/server-cert.pem" -noout -fingerprint -sha256 | sed 's/.*=//' | tr -d ':' | tr '[:upper:]' '[:lower:]')
		want=$(printf '%s' "$INSTALL_ASP_CA_SHA256" | tr '[:upper:]' '[:lower:]' | sed 's/^sha256[:=]//' | tr -d ': ')
		[ -n "$got" ] && [ "$got" = "$want" ] || die "the certificate $hostport shows has the SHA-256 $got, not $want: not trusting it"
		INSTALL_ASP_CA="$TMP/server-cert.pem"
		say "    trusting the certificate of $hostport (SHA-256 $got)"
	fi
	install_component asp-node-agent
	install_vmm
	check_node_tools
	NODE_ID="${INSTALL_ASP_NODE_ID:-$(hostname -s 2>/dev/null || hostname)}"
	ENDPOINT="${INSTALL_ASP_ENDPOINT:-https://$NODE_ID:9443}"
	CA=""
	if [ -n "${INSTALL_ASP_CA:-}" ]; then
		[ -r "$INSTALL_ASP_CA" ] || die "INSTALL_ASP_CA: cannot read $INSTALL_ASP_CA"
		mkdir -p "$ETC"
		[ "$INSTALL_ASP_CA" = "$ETC/cp-ca.pem" ] || cp "$INSTALL_ASP_CA" "$ETC/cp-ca.pem"
		chmod 0644 "$ETC/cp-ca.pem"
		CA="control_plane_ca: $(yaml_str "$ETC/cp-ca.pem")
"
	fi
	write_dropin agent 10-install.yaml "# Written by install.sh. Every setting: node-agent -h; the package's own are in ../agent.yaml.
control_plane_url: $(yaml_str "$INSTALL_ASP_SERVER")
node_id: $(yaml_str "$NODE_ID")
agent_tls_listen: 0.0.0.0:9443
endpoint: $(yaml_str "$ENDPOINT")
${CA}" || true
	# The enroll token has a file of its own, so that it can go once the node has enrolled.
	ENROLL=""
	if [ -n "${INSTALL_ASP_TOKEN:-}" ]; then
		if write_dropin agent 20-enroll.yaml "# Written by install.sh. It goes away once the node has enrolled: the token must not stay in a file.
enroll: true
enroll_token: $(yaml_str "$INSTALL_ASP_TOKEN")
"; then ENROLL=1; fi
	fi
	if [ "${INSTALL_ASP_SKIP_IMAGE:-}" != 1 ]; then
		say "    pulling the guest kernel and image of $VERSION"
		# asp image pull reads INSTALL_ASP_URL's layout: the files of the release.
		"$BIN_ASP" image pull --version "$VERSION" --base-url "$BASE_URL" || warn "no guest image: sudo asp image pull --version $VERSION (or INSTALL_ASP_SKIP_IMAGE=1 to bring your own)"
	fi
	if want_start; then
		systemctl daemon-reload
		# Only what the service says from here on counts: the journal keeps an earlier install's lines.
		since=$(date '+%Y-%m-%d %H:%M:%S')
		systemctl enable --now asp-node-agent >/dev/null 2>&1 || warn "the node-agent did not start: journalctl -u asp-node-agent"
		i=0
		log=""
		while [ "$i" -lt 60 ]; do
			log=$(journalctl -u asp-node-agent --since "$since" --no-pager -o cat 2>/dev/null) || log=""
			case $log in *'registered with control plane'*) break ;; esac
			i=$((i + 1))
			sleep 1
		done
		case $log in
		*'registered with control plane'*) say "    the node registered" ;;
		*)
			last=$(printf '%s\n' "$log" | grep -E ' (ERROR|WARN) ' | tail -n 1 | cut -c1-240)
			warn "the node has not registered with the control plane${last:+. The last problem it logged: $last}"
			warn "journalctl -u asp-node-agent has the rest, and sudo asp doctor says what this host lacks"
			;;
		esac
		# The token has done its job once the node has enrolled, and must not stay in a file. A node that has
		# not enrolled needs it for its next try (the service restarts itself).
		if [ -n "$ENROLL" ] && [ -f "$ETC/agent.yaml.d/20-enroll.yaml" ]; then
			case $log in
			*'enrolled node_id='* | *'node already enrolled'*)
				rm -f "$ETC/agent.yaml.d/20-enroll.yaml" "$ETC/agent.yaml.d/20-enroll.yaml.bak"
				say "    the enroll token is out of $ETC/agent.yaml.d"
				;;
			*) warn "the node has not enrolled: the token stays in $ETC/agent.yaml.d/20-enroll.yaml for the next try (it is single use and expires); remove the file once asp node list shows the node" ;;
			esac
		fi
	else
		say "    not started (INSTALL_ASP_NO_START, or no systemd): systemctl enable --now asp-node-agent"
	fi
	say ""
	say "Check this host: sudo asp doctor"
fi

if has_role standalone; then
	say "==> this host: a control plane and a node"
	PROFILE="${INSTALL_ASP_PROFILE:-default}"
	case "$PROFILE" in default | lab) ;; *) die "INSTALL_ASP_PROFILE: $PROFILE is not default or lab" ;; esac
	if [ "$PROFILE" = default ] && [ ! -c /dev/kvm ]; then
		warn "no /dev/kvm: this host cannot run sandboxes, so asp-server runs the control plane alone (INSTALL_ASP_PROFILE=lab runs a node without VMs)"
	fi
	install_component asp-control-plane
	install_component asp-node-agent
	install_component asp-server
	if [ "$PROFILE" = default ] && [ -c /dev/kvm ]; then
		install_vmm
		check_node_tools
	fi
	body=""
	[ -z "${INSTALL_ASP_LISTEN:-}" ] || body="${body}listen: $(yaml_str "$INSTALL_ASP_LISTEN")
"
	[ -z "${INSTALL_ASP_TLS_SAN:-}" ] || body="${body}tls_san: $(yaml_str "$INSTALL_ASP_TLS_SAN")
"
	[ -z "${INSTALL_ASP_NODE_ID:-}" ] || body="${body}node_id: $(yaml_str "$INSTALL_ASP_NODE_ID")
"
	[ "$PROFILE" = default ] || body="${body}profile: $PROFILE
"
	if [ -n "$body" ]; then
		write_dropin standalone 10-install.yaml "# Written by install.sh. Every setting: asp-server -h; the package's own are in ../standalone.yaml.
$body" || true
	fi
	if [ "$PROFILE" = default ] && [ "${INSTALL_ASP_SKIP_IMAGE:-}" != 1 ] && [ -c /dev/kvm ]; then
		say "    pulling the guest kernel and image of $VERSION"
		"$BIN_ASP" image pull --version "$VERSION" --base-url "$BASE_URL" || warn "no guest image: sudo asp image pull --version $VERSION (or INSTALL_ASP_SKIP_IMAGE=1 to bring your own)"
	fi
	if want_start; then
		systemctl daemon-reload
		since=$(date '+%Y-%m-%d %H:%M:%S')
		systemctl enable --now asp-server >/dev/null 2>&1 || warn "asp-server did not start: journalctl -u asp-server"
		i=0
		up=""
		while [ "$i" -lt 90 ]; do
			if journalctl -u asp-server --since "$since" --no-pager -o cat 2>/dev/null | grep -qE 'registered with control plane'; then
				up=1
				say "    the control plane and the node are up"
				break
			fi
			i=$((i + 1))
			sleep 1
		done
		if [ -z "$up" ]; then
			last=$(journalctl -u asp-server --since "$since" --no-pager -o cat 2>/dev/null | grep -E ' (ERROR|WARN) ' | tail -n 1 | cut -c1-240)
			warn "the node has not registered with the control plane${last:+. The last problem it logged: $last}"
			warn "journalctl -u asp-server has the rest, and sudo asp doctor says what this host lacks"
		fi
	else
		say "    not started (INSTALL_ASP_NO_START, or no systemd): systemctl enable --now asp-server"
	fi
	say ""
	say "The asp command of this host is configured by asp-server (/etc/asp/asp.yaml). As root:"
	say "  asp session start && asp session exec --cmd 'id'"
	say "To let a user do it: sudo usermod -aG asp <user>   (then log in again)"
	say "To add another host: set INSTALL_ASP_LISTEN=0.0.0.0:8443 here, and run: asp node enroll-token"
fi

if [ "$ROLE" = cli ]; then
	say ""
	say "Next: export ASP_CONTROL_PLANE_URL=https://<your control plane> and ASP_API_KEY=<key>, then: asp session start"
fi
say "==> done"
