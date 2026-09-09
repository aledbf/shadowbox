#!/usr/bin/env bash
#
# Builds the L1 root filesystem: a Debian minbase with qemu in it, so
# that L1 can be the PVM host running the guest under test.
#
# mmdebstrap is used because it needs no root, and mkfs.ext4 -d turns the
# result into an image without root either.  A run of this script that
# asks for sudo is a bug.

source "$(dirname "$0")/lib.sh"
need mmdebstrap "Debian/Ubuntu: apt install mmdebstrap"
need mkfs.ext4  "Debian/Ubuntu: apt install e2fsprogs"

SUITE="${DEBIAN_SUITE:-trixie}"
ROOT="$OUT/l1-root"
IMG="$OUT/images/l1-rootfs.ext4"
SIZE="${L1_ROOTFS_SIZE:-4G}"

[ -d "$OUT/modules-host" ] || die "no host modules -- run 'make host-kernel' first"

rm -rf "$ROOT"; mkdir -p "$ROOT"

log "bootstrapping $SUITE into $ROOT (no root required)"
mmdebstrap --variant=minbase \
	--include=systemd-sysv,qemu-system-x86,qemu-utils,kmod,iproute2,procps,\
kbd,less,strace,python3,ca-certificates,curl,trace-cmd,linux-perf \
	"$SUITE" "$ROOT"

log "installing the host kernel modules"
cp -a "$OUT/modules-host/lib/modules/." "$ROOT/lib/modules/"

# L1 exists to run one command.  Wire it as a unit rather than hand
# editing an interactive login: the harness has to be able to read the
# result off the serial line without a human present.
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
ln -sf ../pvm-agent.service \
	"$ROOT/etc/systemd/system/multi-user.target.wants/pvm-agent.service"

cp "$TESTBED/scripts/l1-agent.sh" "$ROOT/opt/pvm/agent.sh"
chmod +x "$ROOT/opt/pvm/agent.sh"

echo 'root:x:0:0:root:/root:/bin/bash' > /dev/null   # minbase already has this
sed -i 's/^root:[^:]*:/root::/' "$ROOT/etc/shadow"   # empty root password, L1 is disposable
echo pvm-l1 > "$ROOT/etc/hostname"
printf '/dev/vda / ext4 defaults 0 1\n' > "$ROOT/etc/fstab"

log "making $IMG ($SIZE)"
mkdir -p "$OUT/images"
rm -f "$IMG"
mkfs.ext4 -q -L pvm-l1 -d "$ROOT" "$IMG" "$SIZE"

log "L1 rootfs ready: $IMG"
