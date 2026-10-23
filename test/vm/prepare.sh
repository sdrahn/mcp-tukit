#!/bin/bash
# Installs the packages built from this tree and what the tests need into
# a new snapshot: run-vm.sh runs it with
#   transactional-update --non-interactive run bash prepare.sh <dir>
# so it runs chrooted in the snapshot, which becomes the default at the
# next boot, as on any transactional system. On a product system
# (openQA) the packages come from its repositories instead
# (transactional-update pkg install mcp-server-tukit-tests).
#
# <dir> (below /var/cache, which tukit bind-mounts into the snapshot;
# /var/tmp there is a tmpfs of its own) holds
# rpms/ (the packages: mcp-gateway, mcp-gateway-selinux and
# mcp-server-tukit's) and opa (used if the distribution has no opa
# package).
set -euo pipefail
dir=$1
die() {
	echo "FATAL: $*"
	exit 1
}
zypper -n --gpg-auto-import-keys ref >/tmp/zypper.log 2>&1 ||
	{ tail -n 30 /tmp/zypper.log; die "refreshing the repositories failed"; }
# tukitd (the server's backend), what mcp-server-tukit-tests requires,
# and the gateway's dependencies but opa.
deps="tukitd jq curl audit policycoreutils selinux-tools polkit util-linux"
# shellcheck disable=SC2086 # a list of package names
zypper -n in --no-recommends $deps >/tmp/zypper.log 2>&1 ||
	{ tail -n 30 /tmp/zypper.log; die "installing the dependencies failed"; }
# sesearch, for the check that the domain may start one unit only
# (skipped without it).
zypper -n in --no-recommends setools-console >/dev/null 2>&1 ||
	echo "  setools-console: not installed (the sesearch check is skipped)"
if zypper -n in --no-recommends opa >/dev/null 2>&1; then
	echo "  opa: distribution package"
else
	install -m 0755 "$dir/opa" /usr/bin/opa
	restorecon /usr/bin/opa || true
	echo "  opa: release binary"
fi
# The packages from files, in one go: the SELinux modules are installed
# with the programs they label (a program installed before its module
# would keep the wrong label, mcp-gateway user guide, chapter 2).
rpm -Uvh --nodeps "$dir"/rpms/*.rpm || die "installing the packages failed"
