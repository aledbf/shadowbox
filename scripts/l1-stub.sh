#!/bin/bash
# Baked into the L1 image and never changed.  The real agent arrives on
# the 9p payload share, so iterating on it needs no image surgery: the
# first version of this project edited the image with debugfs instead and
# left the filesystem inconsistent.
set -u
echo "L1-STUB: mounting the payload share"
mkdir -p /mnt/payload
if ! mount -t 9p -o trans=virtio,version=9p2000.L payload /mnt/payload; then
	echo "L1-STUB: could not mount the payload share"
	echo "PVMTEST-RESULT: fail stage=1 reason=no-payload"
	poweroff -f
fi
if [ ! -x /mnt/payload/agent.sh ]; then
	echo "L1-STUB: no agent on the share"
	echo "PVMTEST-RESULT: fail stage=1 reason=no-agent"
	poweroff -f
fi
exec /bin/bash /mnt/payload/agent.sh
