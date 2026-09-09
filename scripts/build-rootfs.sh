#!/usr/bin/env bash
#
# Builds the L1 root filesystem: a minimal system with qemu in it, so
# that L1 can be the PVM host running the guest under test.
#
# mmdebstrap can do this without root, but only where unprivileged user
# namespaces are allowed.  Ubuntu 24.04 and later restrict them by
# default (kernel.apparmor_restrict_unprivileged_userns=1), so this
# picks a mode rather than assuming one, says which it picked, and when
# it does end up running as root it hands the results back to the
# invoking user -- otherwise every later unprivileged step trips over
# root-owned files.

source "$(dirname "$0")/lib.sh"
need mmdebstrap "Debian/Ubuntu: apt install mmdebstrap"
need mkfs.ext4  "Debian/Ubuntu: apt install e2fsprogs"

ROOT="$OUT/l1-root"
IMG="$OUT/images/l1-rootfs.ext4"
SIZE="${L1_ROOTFS_SIZE:-4G}"

[ -d "$OUT/modules-host" ] || die "no host modules -- run 'make host-kernel' first"

# Which distribution.  Whatever this machine can actually verify: only
# the Ubuntu keyring is installed here, and bootstrapping Debian without
# debian-archive-keyring fails with NO_PUBKEY on every InRelease.
if [ -n "${ROOTFS_SUITE:-}" ]; then
	SUITE="$ROOTFS_SUITE"
	MIRROR="${ROOTFS_MIRROR:?set ROOTFS_MIRROR alongside ROOTFS_SUITE}"
	COMPONENTS="${ROOTFS_COMPONENTS:-main}"
elif [ -f /usr/share/keyrings/ubuntu-archive-keyring.gpg ]; then
	SUITE="${ROOTFS_SUITE:-$(. /etc/os-release && echo "$VERSION_CODENAME")}"
	MIRROR="http://archive.ubuntu.com/ubuntu"
	COMPONENTS="main,universe"
elif [ -f /usr/share/keyrings/debian-archive-keyring.gpg ]; then
	SUITE=trixie
	MIRROR="http://deb.debian.org/debian"
	COMPONENTS="main"
else
	die "no usable archive keyring: apt install ubuntu-keyring or debian-archive-keyring"
fi

# Which mode.  In order of how little privilege it needs.
if unshare -Ur true 2>/dev/null; then
	MODE=unshare
elif command -v fakechroot >/dev/null 2>&1; then
	MODE=fakechroot
elif [ "$(id -u)" = 0 ]; then
	MODE=root
else
	die "unprivileged user namespaces are restricted on this machine
       (kernel.apparmor_restrict_unprivileged_userns=$(sysctl -n kernel.apparmor_restrict_unprivileged_userns 2>/dev/null || echo '?')),
       so mmdebstrap cannot run unprivileged.  Either:
         apt install fakechroot     and run 'make rootfs' as yourself, or
         sudo make rootfs           -- the results are chowned back to you.
       Only this one step needs it; nothing else in the testbed does."
fi

log "bootstrapping $SUITE from $MIRROR, mode=$MODE"

rm -rf "$ROOT"; mkdir -p "$ROOT"

# Kept deliberately short.  L1 runs one shell script and one qemu; every
# extra package is download time on a machine that will rebuild this.
PACKAGES=systemd-sysv,qemu-system-x86,qemu-utils,kmod,iproute2,procps,less,strace

mmdebstrap --mode="$MODE" --variant=minbase \
	--components="$COMPONENTS" \
	--include="$PACKAGES" \
	"$SUITE" "$ROOT" "$MIRROR"

log "installing the host kernel modules"
cp -a "$OUT/modules-host/lib/modules/." "$ROOT/lib/modules/"

# L1 exists to run one command.  Wire it as a unit rather than an
# interactive login: the harness has to read the result off the serial
# line with no one present.
install -d "$ROOT/opt/pvm"
cat > "$ROOT/etc/systemd/system/pvm-agent.service" <<'UNIT'
[Unit]
Description=PVM testbed agent
After=multi-user.target
[Service]
Type=oneshot
ExecStart=/opt/pvm/agent.sh
StandardOutput=journal+console
StandardError=journal+console
[Install]
WantedBy=multi-user.target
UNIT
install -d "$ROOT/etc/systemd/system/multi-user.target.wants"
ln -sf ../pvm-agent.service \
	"$ROOT/etc/systemd/system/multi-user.target.wants/pvm-agent.service"

install -m 0755 "$TESTBED/scripts/l1-agent.sh" "$ROOT/opt/pvm/agent.sh"

# L1 is disposable and reachable only over its own serial console.
sed -i 's/^root:[^:]*:/root::/' "$ROOT/etc/shadow"
echo pvm-l1 > "$ROOT/etc/hostname"
printf '/dev/vda / ext4 defaults 0 1\n' > "$ROOT/etc/fstab"

log "making $IMG ($SIZE)"
mkdir -p "$OUT/images"
rm -f "$IMG"
mkfs.ext4 -q -L pvm-l1 -d "$ROOT" "$IMG" "$SIZE"

# Under sudo, everything above belongs to root.  Hand it back, or the
# next unprivileged 'make' fails on files it cannot remove.
if [ "${SUDO_UID:-}" ] && [ "${SUDO_GID:-}" ]; then
	log "restoring ownership to uid $SUDO_UID"
	chown -R "$SUDO_UID:$SUDO_GID" "$OUT"
fi

log "L1 rootfs ready: $IMG"
