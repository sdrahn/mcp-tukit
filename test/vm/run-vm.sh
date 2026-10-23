#!/bin/bash
# Boots a transactional openSUSE image (MicroOS, Leap Micro) with
# QEMU/KVM, installs the packages built from this tree into a new
# snapshot with transactional-update (prepare.sh), reboots, and runs the
# functional tests of mcp-server-tukit-tests: phase 1, the reboot phase
# (the server's reboot tool takes the system down), phase 2 after the
# system is back, as openQA does on a product system
# (test/openqa/mcp_tukit.pm).
#
# Usage: test/vm/run-vm.sh <rpm dir> <opa binary>
#
# <rpm dir> holds mcp-gateway, mcp-gateway-selinux and the packages of
# mcp-server-tukit (all four).
#
# Environment:
#   IMAGE_URL  the image (default: openSUSE MicroOS, kvm-and-xen); it must
#              take a Combustion configuration, as the openSUSE MicroOS
#              and Leap Micro images do
#   WORK       working directory (default: a new temporary directory); the
#              console log, output and results (TAP, JUnit per phase) are
#              left there
#
# The VM is set up by Combustion: a configuration drive labeled
# "combustion" whose script adds the SSH key and enables sshd at the
# first boot. Needs qemu-system-x86_64, qemu-img, xorriso, ssh and
# /dev/kvm.
set -euo pipefail

[ $# = 2 ] || {
	echo "usage: $0 <rpm dir> <opa binary>" >&2
	exit 2
}
rpms=$(realpath "$1")
opa=$(realpath "$2")
here=$(cd "$(dirname "$0")" && pwd)
image_url=${IMAGE_URL:-https://download.opensuse.org/tumbleweed/appliances/openSUSE-MicroOS.x86_64-kvm-and-xen.qcow2}
work=${WORK:-$(mktemp -d)}
mkdir -p "$work"
port=2222
log() { echo "[$(date +%T)] $*"; }

[ -w /dev/kvm ] || {
	echo "/dev/kvm is not available or not writable" >&2
	exit 1
}

log "work directory: $work"
if [ ! -f "$work/base.qcow2" ]; then
	log "downloading $image_url"
	curl -fsSL --retry 3 -o "$work/base.qcow2" "$image_url"
fi
qemu-img create -q -f qcow2 -b "$work/base.qcow2" -F qcow2 "$work/disk.qcow2" 30G

ssh-keygen -q -t ed25519 -N '' -f "$work/key" <<<y >/dev/null
mkdir -p "$work/combustion/combustion"
cat >"$work/combustion/combustion/script" <<EOS
#!/bin/bash
# combustion: network
mkdir -pm700 /root/.ssh
echo '$(cat "$work/key.pub")' >>/root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
echo mcptukit-vmtest >/etc/hostname
systemctl enable sshd.service
EOS
xorriso -as mkisofs -quiet -V combustion -o "$work/combustion.iso" "$work/combustion"

ssh_opts=(-i "$work/key" -p "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
	-o LogLevel=ERROR -o ConnectTimeout=5 -o ServerAliveInterval=15)
scp_opts=(-q -i "$work/key" -P "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR)
vm() { ssh "${ssh_opts[@]}" root@127.0.0.1 "$@"; }

log "booting the VM"
qemu-system-x86_64 -enable-kvm -cpu host -smp 2 -m 4096 \
	-drive file="$work/disk.qcow2",if=virtio \
	-drive file="$work/combustion.iso",media=cdrom,readonly=on \
	-nic user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$port-:22 \
	-display none -serial file:"$work/console.log" \
	-pidfile "$work/qemu.pid" -daemonize
trap 'kill "$(cat "$work/qemu.pid" 2>/dev/null)" 2>/dev/null || true' EXIT

wait_ssh() {
	for _ in $(seq 120); do
		if vm true 2>/dev/null; then
			log "ssh is up"
			return
		fi
		sleep 5
	done
	echo "the VM did not come up; console:" >&2
	tail -50 "$work/console.log" >&2
	exit 1
}
reboot_vm() {
	log "rebooting"
	vm 'systemctl reboot' || true
	for _ in $(seq 60); do
		vm true 2>/dev/null || break
		sleep 2
	done
	sleep 5
	wait_ssh
	log "running snapshot: $(vm 'tukit snapshots --fields number,active 2>/dev/null' | awk -F'\t' '$2 == "yes" { print $1 }' || true)"
}
wait_ssh
vm 'grep PRETTY_NAME /etc/os-release; findmnt -no OPTIONS /'

if [ "$(vm 'getenforce 2>/dev/null || echo Missing')" != Enforcing ]; then
	log "setting up SELinux (state: $(vm 'getenforce 2>/dev/null || echo missing'))"
	vm 'transactional-update --non-interactive setup-selinux' >"$work/selinux.log" 2>&1
	reboot_vm
	for _ in $(seq 30); do
		[ "$(vm 'getenforce 2>/dev/null' || true)" = Enforcing ] && break
		sleep 10
		wait_ssh
	done
fi

log "copying the packages"
vm 'rm -rf /var/tmp/vmtest && mkdir -p /var/tmp/vmtest/rpms'
scp "${scp_opts[@]}" "$here/prepare.sh" "$opa" root@127.0.0.1:/var/tmp/vmtest/
scp "${scp_opts[@]}" "$rpms"/*.rpm root@127.0.0.1:/var/tmp/vmtest/rpms/

log "installing the packages into a new snapshot"
vm 'transactional-update --non-interactive run bash /var/tmp/vmtest/prepare.sh /var/tmp/vmtest' 2>&1 | tee "$work/prepare.log"
[ "${PIPESTATUS[0]}" = 0 ] || exit 1
reboot_vm

# went_down <seconds>: the VM stopped answering within that time.
went_down() {
	local deadline=$((SECONDS + $1))
	while [ "$SECONDS" -lt "$deadline" ]; do
		vm true 2>/dev/null || return 0
		sleep 2
	done
	return 1
}

status=0
for phase in 1 reboot 2; do
	log "running the tests, phase $phase"
	set +e
	vm "/usr/libexec/mcp-server-tukit/tests/run-tests.sh --disposable --phase $phase --results /var/tmp/vmtest/results-$phase" \
		2>&1 | tee "$work/test-$phase.log"
	s=${PIPESTATUS[0]}
	scp "${scp_opts[@]}" root@127.0.0.1:/var/tmp/vmtest/results-$phase/results.tap "$work/results-$phase.tap"
	scp "${scp_opts[@]}" root@127.0.0.1:/var/tmp/vmtest/results-$phase/results.xml "$work/results-$phase.xml"
	set -e
	log "phase $phase exited with $s"
	[ "$s" = 0 ] || status=1
	# The reboot phase ends with the reboot tool's reboot (about 10 s
	# later, with REBOOT_METHOD=auto and no rebootmgr): wait for it; if it
	# does not come, that is a failure, and phase 2 runs after a reboot of
	# our own.
	if [ "$phase" = reboot ]; then
		if went_down 120; then
			log "the reboot tool took the system down"
			sleep 5
			wait_ssh
		else
			log "FAIL: the system did not go down after the reboot tool's call"
			status=1
			reboot_vm
		fi
	fi
done
exit "$status"
