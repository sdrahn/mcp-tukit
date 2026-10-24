#!/bin/bash
# Functional tests of mcp-server-tukit behind mcp-gateway on a real
# transactional system (openSUSE MicroOS, Leap Micro, SL Micro, SLES 16.1
# in immutable mode) with SELinux enforcing: the instance in the
# gateway's sandbox with CAP_SYS_ADMIN only and in its SELinux domain,
# the roles and approvals, and
# real changes through tukitd: a package installed into a new snapshot,
# active after a reboot, a rollback, deleting and cleaning up snapshots,
# and no SELinux denials. Package mcp-server-tukit-tests installs it as
# /usr/libexec/mcp-server-tukit/tests/run-tests.sh for openQA and other
# test systems; the CI VM runs the same (test/vm/run-vm.sh). Next to it:
#   tukitcall   the test client (test/tukitcall)
#
# Usage: run-tests.sh --disposable --phase 1|reboot|2 [--results DIR] [--package NAME]
#
# A change takes effect only at the next boot, so the tests come in
# phases, and the reboot between them is the server's own:
#   1       set up users and roles, start the gateway, check the instance
#           and the policies, install a package into a new snapshot (and
#           fail to, and have nothing to do)
#   reboot  the reboot tool: refusals, then a reboot into the new
#           snapshot, which goes down about 10 s after this phase ends;
#           the caller waits for the system to come back
#   2       the package is in the running system, the reboot went through
#           the tool's unit; roll back, delete and clean up snapshots, a
#           command in a snapshot (with -allow-commands)
# Each phase keeps what the next needs in /var/lib/mcp-tukit-tests.
#
# It runs as root on a system that is there for testing: it creates
# users (tuki, rita, ned), replaces the gateway's role data, installs a
# package, changes the default snapshot and sets REBOOT_METHOD=systemd in
# /etc/transactional-update.conf, and does not undo any of it.
# --disposable says that this is fine.
#   --results DIR   writes results.tap (TAP) and results.xml (JUnit) there
#   --package NAME  the package to install (default: the first of tree,
#                   figlet, sl, hello the repositories have and the
#                   system has not)
# Every check prints PASS, FAIL or SKIP; the script exits 1 if any failed,
# 2 on wrong usage.
set -u
dir=$(cd "$(dirname "$0")" && pwd)
state=/var/lib/mcp-tukit-tests
failed=0
disposable='' phase='' results='' package=''
usage() {
	echo "usage: $0 --disposable --phase 1|reboot|2 [--results DIR] [--package NAME]" >&2
	exit 2
}
while [ $# -gt 0 ]; do
	case $1 in
	--disposable) disposable=1 ;;
	--phase) phase=${2:?}; shift ;;
	--results) results=${2:?}; shift ;;
	--package) package=${2:?}; shift ;;
	*) usage ;;
	esac
	shift
done
case $phase in 1 | reboot | 2) ;; *) usage ;; esac
if [ -z "$disposable" ]; then
	echo "$0: changes this system for good (users, role data, packages, the default snapshot);" >&2
	echo "run it on a system that is there for testing, with --disposable" >&2
	exit 2
fi
[ "$(id -u)" = 0 ] || { echo "$0: runs as root" >&2; exit 2; }
mkdir -p "$state"

# The results, for TAP and JUnit: one line per check (section, status,
# description, seconds since the check before).
current_section=start
last_check=$SECONDS
results_list=()
record() {
	results_list+=("$current_section"$'\t'"$1"$'\t'"$2"$'\t'"$((SECONDS - last_check))")
	last_check=$SECONDS
}
pass() { echo "PASS: $*"; record pass "$*"; }
fail() { echo "FAIL: $*"; failed=1; record fail "$*"; fail_context; }
skip() { echo "SKIP: $*"; record skip "$*"; }
fail_context() {
	journalctl --since -30s --no-pager -o short-precise 2>/dev/null |
		grep -E 'mcp|tukit' | grep -E 'level=(ERROR|WARN)|[Ff]ailed|status=[0-9]|code=exited|[Ee]rror' |
		tail -15 | sed 's/^/    | /'
}
check() { # check <description> <command...>
	local desc=$1
	shift
	if "$@"; then pass "$desc"; else fail "$desc"; fi
}
section() {
	printf '\n=== %s\n' "$*"
	current_section="phase $phase: $*"
}
eventually() { # eventually <seconds> <command...>
	local deadline=$((SECONDS + $1))
	shift
	until "$@"; do
		[ "$SECONDS" -ge "$deadline" ] && return 1
		sleep 0.5
	done
}
die() {
	echo "FATAL: $*"
	exit 1
}

# --- helpers ---------------------------------------------------------------

label() { ps -o label= -p "$1" 2>/dev/null | tr -d ' '; }
proc_user() { ps -o user= -p "$1" 2>/dev/null | tr -d ' '; }
file_label() { stat -c %C "$1" 2>/dev/null; }
has_type() { [[ $1 == *":$2:"* ]]; }
proc_has_type() { has_type "$(label "$1")" "$2"; }
file_has_type() { has_type "$(file_label "$1")" "$2"; }
categories() { sed -n 's/.*:s0:\(c[0-9]*,c[0-9]*\)$/\1/p' <<<"$1"; }
# has_sys_admin <pid>: the process holds CAP_SYS_ADMIN (bit 21), which
# tukitd requires of its callers. cap_set <pid> <set>: the process's
# capability set (CapEff, CapBnd, ...) in hex.
has_sys_admin() {
	local caps
	caps=$(sed -n 's/^CapEff:[[:space:]]*//p' "/proc/$1/status" 2>/dev/null)
	[ -n "$caps" ] && (((0x$caps >> 21) & 1))
}
cap_set() { sed -n "s/^$2:[[:space:]]*//p" "/proc/$1/status" 2>/dev/null; }
no_new_privs() { grep -q '^NoNewPrivs:[[:space:]]*1' "/proc/$1/status" 2>/dev/null; }
# lists_capability: the gateway lists the server with CAP_SYS_ADMIN and
# not as privileged.
lists_capability() {
	control tuki GET /v1/servers |
		jq -e '.[] | select(.name == "tukit") | .capabilities == ["CAP_SYS_ADMIN"] and (.privileged | not)' >/dev/null
}

# The snapshots as tukit itself lists them, independent of the server:
# "number default active" per line.
# default and active are "yes" where they hold (snapper's CSV; other
# spellings taken as well).
snapshots() {
	tukit --fields=number,default,active snapshots 2>/dev/null |
		awk -F'\t' 'function b(v) { v = tolower(v); return (v == "yes" || v == "true" || v == "1" || v == "*") ? "yes" : "no" }
			$1 ~ /^[0-9]+$/ && $1 != 0 { print $1, b($2), b($3) }'
}
default_snapshot() { snapshots | awk '$2 == "yes" { print $1 }'; }
active_snapshot() { snapshots | awk '$3 == "yes" { print $1 }'; }
snapshot_count() { snapshots | wc -l; }
has_snapshot() { snapshots | awk '{ print $1 }' | grep -qx "$1"; }
# in_snapshot <n> <package>: the package is installed in snapshot n.
in_snapshot() { rpm --root "/.snapshots/$1/snapshot" -q "$2" >/dev/null 2>&1; }

# call <user> <tukitcall args...>: sets $out (the result, JSON) and $rc (0
# success, 1 request error or refused, 2 tool error); also in files, for
# calls run in the background (tool_approved).
call() {
	local user=$1
	shift
	runuser -u "$user" -- "$dir/tukitcall" "$@" >"$state/out" 2>"$state/err"
	rc=$?
	echo "$rc" >"$state/rc"
	out=$(cat "$state/out")
	echo "  [$user] rc=$rc ${out:0:400} $(head -c 300 "$state/err")"
}
tool() { call "$1" --method tools/call --params "{\"name\":\"$2\",\"arguments\":$3}"; }
# control <user> <method> <path> [body]: the gateway's control API.
control() {
	runuser -u "$1" -- curl -s --unix-socket /run/mcp-gateway/control.sock -X "$2" \
		${4:+--data-binary "$4"} "http://gw$3"
}
# tool_approved <user> <tool> <args>: tool, approved (once) by <user>
# through the control API while it waits (approvers: self).
tool_approved() {
	local id
	rm -f "$state/rc"
	tool "$@" &
	local pid=$!
	for _ in $(seq 60); do
		id=$(control "$1" GET /v1/approvals | jq -r '.[0].id // empty' 2>/dev/null)
		[ -n "$id" ] && break
		sleep 1
	done
	echo "  approval: ${id:-none}"
	[ -n "$id" ] && control "$1" POST "/v1/approvals/$id" '{"decision":"approve","scope":"once"}' >/dev/null
	wait "$pid"
	rc=$(cat "$state/rc" 2>/dev/null || echo 1)
	out=$(cat "$state/out" 2>/dev/null)
}
succeeded_with() { [ "$rc" = 0 ] && grep -qF -- "$1" <<<"$out"; }
tool_error_with() { [ "$rc" = 2 ] && grep -qF -- "$1" <<<"$out"; }
fails() { ! "$@" >/dev/null 2>&1; }
gone() { ! has_snapshot "$1"; }
# A refusal by the gateway: a request error, or (for a tool the policy
# does not allow) a tool error the gateway writes.
refused() {
	! grep -qE "dial unix|backend unavailable" <<<"$out" &&
		{ [ "$rc" = 1 ] || { [ "$rc" = 2 ] && grep -qF '"mcp-gateway: ' <<<"$out"; }; }
}
# structured <field>: a field of the result's structuredContent (false
# stays false; a missing field is empty).
structured() { jq -r ".structuredContent.$1 | if . == null then empty else . end" <<<"$out" 2>/dev/null; }
listed() { jq -e --arg n "$1" '.tools | map(.name) | index($n)' <<<"$out" >/dev/null 2>&1; }

# instance_pid: main PID of a running tukit instance (not the discovery
# one: its principal is tuki's or rita's; any will do for the checks).
instance_pid() {
	local unit pid
	for unit in $(systemctl list-units --type=service --state=running --plain --no-legend 'mcp-tukit-*' | awk '{print $1}'); do
		pid=$(systemctl show -p MainPID --value "$unit")
		[ "$pid" != 0 ] && { echo "$pid"; return; }
	done
}

# audit_since <types>: kernel audit records since $since (epoch).
audit_since() {
	local line ts
	ausearch --input-logs -m "$1" -ts yesterday --raw 2>/dev/null | while IFS= read -r line; do
		ts=${line#*msg=audit(}
		ts=${ts%%.*}
		[[ $ts =~ ^[0-9]+$ ]] && [ "$ts" -ge "$since" ] && printf '%s\n' "$line"
	done
}
no_tukit_denials() {
	! audit_since AVC,USER_AVC,SELINUX_ERR | grep -E 'mcpsrv_tukit_|mcpgw_' | grep -v 'permissive=1' | grep -q .
}

# pick_package [<not this one>]: a package to install, which the
# repositories have and the system has not.
pick_package() {
	local p
	for p in tree figlet sl hello cowsay; do
		[ "$p" = "${1:-}" ] && continue
		rpm -q "$p" >/dev/null 2>&1 && continue
		zypper -n --no-refresh se -x "$p" >/dev/null 2>&1 || zypper -n se -x "$p" >/dev/null 2>&1 || continue
		echo "$p"
		return
	done
}

wait_socket() { eventually 60 test -S /run/mcp-gateway/mcp.sock; }

denials_section() {
	section "SELinux denials"
	local avc
	avc=$(audit_since AVC,USER_AVC,SELINUX_ERR)
	grep -E 'avc:|type=SELINUX_ERR' <<<"$avc" |
		sed -E 's/^.*(avc: |type=SELINUX_ERR)/\1/; s/ (pid|ino)=[0-9]+//g' |
		sort | uniq -c | sort -rn | sed 's/^/  /' | head -40
	check "no denials involving the tukit server or the gateway" no_tukit_denials
}

# --- phase 1 -----------------------------------------------------------------

phase1() {
	section "System"
	grep PRETTY_NAME /etc/os-release
	uname -r
	check "SELinux is enforcing" test "$(getenforce)" = Enforcing
	if findmnt -no OPTIONS / | tr ',' '\n' | grep -qx ro; then
		pass "the root file system is read-only (a transactional system)"
	else
		skip "the root file system is writable: not a transactional system (tukit works, the tests run)"
	fi
	check "tukitd is installed" test -x /usr/sbin/tukitd
	rpm -q tukit tukitd transactional-update 2>/dev/null | sed 's/^/  /'
	echo "  tukit snapshots (number, default, active):"
	tukit --fields=number,default,active snapshots 2>&1 | sed 's/^/    /' | tail -10

	section "Install"
	local p
	for p in mcp-gateway mcp-gateway-selinux mcp-server-tukit mcp-server-tukit-gateway mcp-server-tukit-selinux \
		mcp-server-tukit-tests jq curl; do
		rpm -q "$p" >/dev/null 2>&1 || die "package $p is not installed"
	done
	echo "  $(rpm -q mcp-gateway mcp-server-tukit | tr '\n' ' ')"
	systemctl enable --now auditd >/dev/null 2>&1
	check "SELinux module mcp_tukit is loaded" bash -c 'semodule -l | grep -qx mcp_tukit'
	check "SELinux module mcp_gateway is loaded" bash -c 'semodule -l | grep -qx mcp_gateway'
	echo "  $(file_label /usr/bin/mcp-server-tukit) /usr/bin/mcp-server-tukit"
	# As the package left it: a wrong label is a packaging bug (or the
	# program was installed before its module, chapter 2 of the gateway's
	# user guide).
	check "/usr/bin/mcp-server-tukit is labeled mcpsrv_tukit_exec_t" file_has_type /usr/bin/mcp-server-tukit mcpsrv_tukit_exec_t
	local def=/usr/share/mcp-gateway/profiles/tukit.yaml
	check "the definition is installed" test -f "$def"
	check "0.1's name links to it" test "$(readlink /usr/share/mcp-gateway/profiles/tukit-privileged.yaml)" = tukit.yaml
	# A package never gives a server a capability by itself (mcp-gateway
	# accepts definitions with sandbox.capabilities only from /etc).
	check "no definition is active from the package alone" test ! -e /usr/share/mcp-gateway/servers.d/tukit.yaml
	check "the roles are installed" test -f /usr/share/mcp-gateway/policy/mcp/profiles/tukit/data.yaml
	local unit=/usr/lib/systemd/system/mcp-tukit-reboot.service
	echo "  $(file_label "$unit") $unit"
	check "the reboot unit is installed" test -f "$unit"
	check "the reboot unit is labeled mcpsrv_tukit_unit_t" file_has_type "$unit" mcpsrv_tukit_unit_t
	# The administrator enables it, as the definition says.
	ln -sf "$def" /etc/mcp-gateway/servers.d/tukit.yaml
	check "mcp-gateway --check accepts the definition linked into /etc" bash -c 'mcp-gateway --check >/dev/null 2>&1'
	check "mcp-server-tukit --version" bash -c 'mcp-server-tukit -version | grep -q "^mcp-server-tukit "'

	section "Users and policy"
	local u
	for u in tuki rita ned; do
		id "$u" >/dev/null 2>&1 || useradd -m "$u"
		usermod -aG mcp-users "$u"
	done
	# tuki operates, rita reads, ned has no role on tukit.
	cat >/etc/mcp-gateway/policy/rbac/data.yaml <<'EOF'
roles: {}
bindings:
  users:
    tuki: [tukit-operator]
    rita: [tukit-reader]
    ned: []
approvers:
  default: [self]
EOF
	check "the role data names the shipped roles" bash -c 'mcp-gateway --check-policy-data >/dev/null 2>&1'

	section "Start"
	systemctl enable mcp-gateway.service >/dev/null 2>&1
	systemctl restart mcp-gateway.service
	check "the gateway socket is up" wait_socket

	section "Discovery"
	call tuki --method tools/list
	check "tuki (operator) sees pkg_install" listed pkg_install
	check "tuki sees get_status" listed get_status
	check "tuki sees reboot" listed reboot
	check "tuki sees apply" listed apply
	check "run_in_snapshot is not offered (no -allow-commands)" bash -c '! jq -e ".tools | map(.name) | index(\"run_in_snapshot\")" <<<"$1" >/dev/null' _ "$out"
	call rita --method tools/list
	check "rita (reader) sees list_snapshots" listed list_snapshots
	check "rita does not see pkg_install" bash -c '! jq -e ".tools | map(.name) | index(\"pkg_install\")" <<<"$1" >/dev/null' _ "$out"
	check "rita does not see reboot" bash -c '! jq -e ".tools | map(.name) | index(\"reboot\")" <<<"$1" >/dev/null' _ "$out"
	check "rita does not see apply" bash -c '! jq -e ".tools | map(.name) | index(\"apply\")" <<<"$1" >/dev/null' _ "$out"

	section "Reading"
	local active default
	active=$(active_snapshot) default=$(default_snapshot)
	echo "  tukit: active ${active:-?}, default ${default:-?}, $(snapshot_count) snapshots"
	tool rita get_status '{}'
	check "rita: get_status names the active snapshot" test "$rc" = 0 -a "$(structured active)" = "$active"
	check "get_status agrees with tukit on the default" test "$(structured default)" = "$default"
	tool rita list_snapshots '{}'
	check "rita: list_snapshots has the active snapshot" bash -c "jq -e '.structuredContent.snapshots | map(select(.active)) | .[0].number == \"$active\"' <<<\"\$1\" >/dev/null" _ "$out"

	section "Instance"
	local pid ctx
	pid=$(instance_pid)
	ctx=$(label "${pid:-0}")
	echo "  instance ${pid:-none}: $ctx, user $(proc_user "${pid:-0}")"
	check "an instance runs" test -n "$pid"
	check "it runs in mcpsrv_tukit_t" proc_has_type "${pid:-0}" mcpsrv_tukit_t
	grep -E '^(NoNewPrivs|Cap(Eff|Bnd|Amb)):' "/proc/${pid:-0}/status" 2>/dev/null | sed 's/^/  /'
	check "it runs as root (tukitd's bus policy lets in root only)" test "$(proc_user "${pid:-0}")" = root
	check "it holds CAP_SYS_ADMIN, which tukitd requires" has_sys_admin "${pid:-0}"
	check "... and no other capability" test "$(cap_set "${pid:-0}" CapEff)" = 0000000000200000
	check "... nor can it gain one (bounding set)" test "$(cap_set "${pid:-0}" CapBnd)" = 0000000000200000
	check "it has no_new_privs (the gateway's sandbox)" no_new_privs "${pid:-0}"
	check "it has its MCS pair (not privileged)" test -n "$(categories "$ctx")"
	check "the gateway lists it with CAP_SYS_ADMIN, not as privileged" lists_capability
	check "the gateway's doctor finds nothing failing for tukit" \
		bash -c '! mcp-gateway-admin doctor --server tukit 2>&1 | tee /dev/stderr | grep -qE "^fail"'

	section "tukitd access"
	# tukitd answers only callers with CAP_SYS_ADMIN (sd-bus's check on
	# every method): a user cannot use it, whatever the bus policy says.
	local list='call org.opensuse.tukit /org/opensuse/tukit/Snapshot org.opensuse.tukit.Snapshot List s number'
	# shellcheck disable=SC2086 # list is the busctl arguments
	runuser -u rita -- busctl --system $list 2>&1 | sed 's/^/  rita: /'
	# shellcheck disable=SC2086
	check "a user may not call tukitd" fails runuser -u rita -- busctl --system $list

	section "Refusals"
	local count
	count=$(snapshot_count)
	tool rita pkg_install '{"packages":["vim"]}'
	check "rita (reader) may not install" refused
	tool ned get_status '{}'
	check "ned (no role) may not read" refused
	tool_approved tuki pkg_install '{"packages":["$(touch /root/pwned)"]}'
	check "a package name with shell characters is refused by the server" tool_error_with "not a package name"
	check "... and nothing ran" test ! -e /root/pwned
	check "... and no snapshot was made" test "$(snapshot_count)" = "$count"

	section "Installing a package"
	[ -n "$package" ] || package=$(pick_package)
	[ -n "$package" ] || die "no package to install (none of tree, figlet, sl, hello is available and not installed; use --package)"
	echo "  package: $package"
	local before=$default
	tool_approved tuki pkg_install "{\"packages\":[\"$package\"]}"
	local snap
	snap=$(structured snapshot)
	check "pkg_install is applied after an approval" test "$rc" = 0 -a "$(structured status)" = applied
	check "the result says a reboot is needed" test "$(structured reboot_required)" = true
	check "the new snapshot is the default" test -n "$snap" -a "$(default_snapshot)" = "$snap"
	check "the package is in the new snapshot" in_snapshot "${snap:-0}" "$package"
	check "the running system is unchanged" bash -c "! rpm -q $package >/dev/null 2>&1"
	check "the active snapshot is unchanged" test "$(active_snapshot)" = "$active"
	if [ "$(rpm -q --qf '%{version}' tukitd | cut -d. -f1)" -ge 6 ] 2>/dev/null; then
		check "the snapshot's description names the change" bash -c "snapper --csvout list --columns number,description | grep -q '^$snap,MCP: pkg install $package'"
	else
		skip "the snapshot's description (tukitd before 6.0 takes none)"
	fi
	tool tuki get_status '{}'
	check "get_status: a reboot is pending" test "$(structured reboot_pending)" = true -a "$(structured default)" = "$snap"
	check "the instance logged the change" bash -c "journalctl -o cat --since '-10min' | grep -q 'pkg install $package: snapshot $snap: applied'"

	section "Nothing to do"
	count=$(snapshot_count)
	tool_approved tuki pkg_install "{\"packages\":[\"$package\"]}"
	check "installing it again: no changes" test "$rc" = 0 -a "$(structured status)" = no_changes
	check "... the snapshot was discarded" test "$(snapshot_count)" = "$count"
	check "... the default is still $snap" test "$(default_snapshot)" = "$snap"

	section "A failing change"
	tool_approved tuki pkg_install '{"packages":["mcp-tukit-test-no-such-package"]}'
	check "a missing package fails" test "$rc" = 2 -a "$(structured status)" = failed
	check "... the snapshot was discarded" test "$(snapshot_count)" = "$count"
	check "... the default is still $snap" test "$(default_snapshot)" = "$snap"

	denials_section
	printf 'package=%s\nbefore=%s\nsnap=%s\n' "$package" "$before" "$snap" >"$state/phase1"
}

# --- the reboot ---------------------------------------------------------------

phasereboot() {
	[ -f "$state/phase1" ] || die "phase 1 has not run ($state/phase1 is missing)"
	local package before snap
	# shellcheck disable=SC1091
	. "$state/phase1"
	section "Reboot"
	local active unit=mcp-tukit-reboot.service
	active=$(active_snapshot)
	unit_state() { systemctl show -p ActiveState --value "$unit"; }
	tool rita reboot '{}'
	check "rita (reader) may not reboot" refused
	tool_approved tuki reboot "{\"snapshot\":\"$active\"}"
	check "a reboot into a snapshot that is not the default is refused" tool_error_with "is not the default"
	check "... and the reboot unit did not start" test "$(unit_state)" = inactive
	# The domain may start this unit and the apply unit, no other: the
	# policy's only service rules for it name the units' own types.
	if command -v sesearch >/dev/null; then
		sesearch -A -s mcpsrv_tukit_t -c service 2>/dev/null | sed 's/^/  /'
		check "the domain may start the reboot and apply units only" bash -c \
			'r=$(sesearch -A -s mcpsrv_tukit_t -c service 2>/dev/null); [ -n "$r" ] && ! grep -vE "mcpsrv_tukit_(apply_)?unit_t" <<<"$r" | grep -q allow'
	else
		skip "the domain's service rules (no sesearch, package setools-console)"
	fi
	cat /proc/sys/kernel/random/boot_id >"$state/boot_id"
	printf 'reboot_since=%s\n' "$since" >"$state/reboot"
	# At once, not in rebootmgr's maintenance window (MicroOS runs
	# rebootmgr, and REBOOT_METHOD=auto hands the reboot to it): the
	# administrator's configuration, the last assignment wins.
	echo 'REBOOT_METHOD=systemd' >>/etc/transactional-update.conf
	tool_approved tuki reboot "{\"snapshot\":\"$snap\"}"
	check "the reboot into snapshot $snap starts after an approval" test "$rc" = 0 -a "$(structured started)" = true -a "$(structured default)" = "$snap"
	check "the result names the configured REBOOT_METHOD (systemd)" test "$(structured method)" = systemd
	check "the reboot unit runs" eventually 5 bash -c "systemctl show -p ActiveState --value $unit | grep -qxE 'activating|active'"
	echo "  the system goes down in about 10 s"
}

# --- phase 2 -----------------------------------------------------------------

phase2() {
	[ -f "$state/phase1" ] || die "phase 1 has not run ($state/phase1 is missing)"
	local package before snap
	# shellcheck disable=SC1091
	. "$state/phase1"
	local reboot_since=$since
	if [ -f "$state/reboot" ]; then
		# Denials count from the reboot phase on.
		# shellcheck disable=SC1091
		. "$state/reboot"
		since=$reboot_since
	fi
	section "After the reboot"
	check "SELinux is enforcing" test "$(getenforce)" = Enforcing
	check "the system was rebooted" test "$(cat /proc/sys/kernel/random/boot_id)" != "$(cat "$state/boot_id" 2>/dev/null)"
	if [ "$(journalctl --list-boots --no-pager 2>/dev/null | wc -l)" -ge 2 ]; then
		journalctl -b -1 -u mcp-tukit-reboot.service --no-pager -o short-precise 2>/dev/null | tail -5 | sed 's/^/  /'
		check "the previous boot ended through the reboot tool's unit" \
			bash -c 'journalctl -b -1 -u mcp-tukit-reboot.service -o cat 2>/dev/null | grep -q .'
	else
		skip "the previous boot's journal (not persistent here)"
	fi
	check "the system runs the new snapshot $snap" test "$(active_snapshot)" = "$snap"
	check "the package is installed" rpm -q "$package"
	check "the gateway came up" wait_socket
	tool rita get_status '{}'
	check "get_status: no reboot pending" test "$rc" = 0 -a "$(structured reboot_pending)" = false

	section "Rolling back"
	tool_approved tuki rollback "{\"snapshot\":\"$before\"}"
	local def
	def=$(structured default)
	check "rollback to $before after an approval" test "$rc" = 0 -a -n "$def"
	check "the default is $before (or its copy)" test "$(default_snapshot)" = "$def"
	tool tuki get_status '{}'
	check "get_status: a reboot is pending" test "$(structured reboot_pending)" = true
	# Back to the running snapshot: nothing changes at the next boot.
	tool_approved tuki rollback "{\"snapshot\":\"$snap\"}"
	check "rollback to the running snapshot $snap" test "$rc" = 0
	local now
	now=$(default_snapshot)
	echo "  default now $now"
	if [ "$now" = "$snap" ]; then
		tool_approved tuki reboot '{}'
		check "reboot with nothing pending is refused" tool_error_with "no reboot pending"
	else
		skip "reboot with nothing pending (the rollback made a copy: $now is the default)"
	fi

	section "Deleting and cleaning up"
	tool_approved tuki delete_snapshot "{\"snapshot\":\"$snap\"}"
	check "the running snapshot cannot be deleted" tool_error_with "running system"
	check "... it is still there" has_snapshot "$snap"
	tool_approved tuki delete_snapshot "{\"snapshot\":\"$now\"}"
	if [ "$now" != "$snap" ]; then
		check "the default snapshot cannot be deleted" tool_error_with "default"
	fi
	# A rollback may have made a copy of the snapshot rolled back to
	# (tukitd 6.2): unused now, it goes.
	if [ "$def" != "$before" ] && [ "$def" != "$now" ] && has_snapshot "$def"; then
		tool_approved tuki delete_snapshot "{\"snapshot\":\"$def\"}"
		check "the unused rollback copy $def is deleted" test "$rc" = 0
		check "... it is gone" gone "$def"
	fi
	# A snapshot neither running nor booted next, made without the server
	# (snapper, which tukit uses on these systems).
	local spare=''
	if command -v snapper >/dev/null; then
		spare=$(snapper create --print-number --description "mcp-tukit tests: spare" 2>/dev/null)
	fi
	if [[ $spare =~ ^[0-9]+$ ]] && has_snapshot "$spare"; then
		echo "  spare snapshot: $spare"
		tool rita delete_snapshot "{\"snapshot\":\"$spare\"}"
		check "rita (reader) may not delete a snapshot" refused
		check "... it is still there" has_snapshot "$spare"
		tool_approved tuki delete_snapshot "{\"snapshot\":\"$spare\"}"
		check "an unused snapshot is deleted after an approval" succeeded_with "Snapshot $spare deleted"
		check "... it is gone" gone "$spare"
		check "... the running snapshot is kept" has_snapshot "$snap"
		tool_approved tuki delete_snapshot "{\"snapshot\":\"$spare\"}"
		check "deleting it again: nothing to do, no error" succeeded_with "There is no snapshot $spare"
	else
		skip "deleting a snapshot (snapper could not create a spare one: ${spare:-no snapper})"
	fi
	tool_approved tuki cleanup_snapshots '{}'
	if grep -qF "has no Cleanup" <<<"$out"; then
		skip "cleanup_snapshots (this tukitd has no Cleanup: before 6.2)"
	else
		check "cleanup_snapshots after an approval" test "$rc" = 0
	fi
	check "... the running snapshot is kept" has_snapshot "$snap"

	section "Applying without a reboot"
	tool rita apply '{}'
	check "rita (reader) may not apply" refused
	if [ "$(default_snapshot)" = "$(active_snapshot)" ]; then
		tool_approved tuki apply '{}'
		check "apply with nothing pending is refused" tool_error_with "nothing to apply"
	fi
	local second
	second=$(pick_package "$package")
	if [ -n "$second" ]; then
		echo "  package: $second"
		tool_approved tuki pkg_install "{\"packages\":[\"$second\"]}"
		local applied
		applied=$(structured snapshot)
		check "pkg_install $second into snapshot ${applied:-?}" test "$rc" = 0 -a "$(structured status)" = applied
		check "... not in the running system yet" fails rpm -q "$second"
		tool_approved tuki apply "{\"snapshot\":\"$(active_snapshot)\"}"
		check "apply of a snapshot that is not the default is refused" tool_error_with "is not the default"
		tool_approved tuki apply "{\"snapshot\":\"${applied:-0}\"}"
		check "apply of snapshot ${applied:-?} after an approval" test "$rc" = 0 -a "$(structured applied)" = true
		check "... $second is in the running system now, without a reboot" rpm -q "$second"
		check "... its program runs" bash -c "rpm -ql $second | grep -m1 '^/usr/bin/' | xargs -r test -x"
	else
		skip "apply (no second package to install: none of tree, figlet, sl, hello, cowsay is available and not installed)"
	fi

	section "Commands in a snapshot"
	# -allow-commands, as an administrator enables it: a definition of the
	# same name in /etc replaces the package's.
	local def=/usr/share/mcp-gateway/profiles/tukit.yaml
	rm -f /etc/mcp-gateway/servers.d/tukit.yaml
	sed 's|^command: .*|command: ["/usr/bin/mcp-server-tukit", "-allow-commands"]|' \
		"$def" >/etc/mcp-gateway/servers.d/tukit.yaml
	systemctl reload mcp-gateway.service
	eventually 30 bash -c "runuser -u tuki -- '$dir/tukitcall' --method tools/list | grep -q run_in_snapshot"
	call tuki --method tools/list
	check "run_in_snapshot is offered with -allow-commands" listed run_in_snapshot
	local count default
	count=$(snapshot_count) default=$(default_snapshot)
	tool_approved tuki run_in_snapshot '{"command":["/bin/sh","-c","echo failing on purpose; exit 3"]}'
	check "a failing command: failed" test "$rc" = 2 -a "$(structured exit_code)" = 3
	check "... the snapshot was discarded" test "$(snapshot_count)" = "$count"
	check "... the default is unchanged" test "$(default_snapshot)" = "$default"
	ln -sf "$def" /etc/mcp-gateway/servers.d/tukit.yaml
	systemctl reload mcp-gateway.service

	denials_section
}

# --- results -----------------------------------------------------------------

xml() { sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g; s/"/\&quot;/g' <<<"$1"; }
write_results() {
	local r sec st desc secs n=0 last='' tests fails skips cases=''
	mkdir -p "$1" || return
	{
		echo "1..${#results_list[@]}"
		for r in "${results_list[@]}"; do
			IFS=$'\t' read -r sec st desc secs <<<"$r"
			n=$((n + 1))
			desc=${desc//#/\\#}
			case $st in
			pass) echo "ok $n - $sec: $desc" ;;
			skip) echo "ok $n - $sec: $desc # SKIP" ;;
			*) echo "not ok $n - $sec: $desc" ;;
			esac
		done
	} >"$1/results.tap"
	{
		echo '<?xml version="1.0" encoding="UTF-8"?>'
		printf '<testsuites name="mcp-server-tukit" tests="%d">\n' "${#results_list[@]}"
		for r in "${results_list[@]}" ""; do
			IFS=$'\t' read -r sec st desc secs <<<"$r"
			if [ "$sec" != "$last" ] && [ -n "$last" ]; then
				tests=$(printf '%s\n' "${results_list[@]}" | awk -F'\t' -v s="$last" '$1 == s' | wc -l)
				fails=$(printf '%s\n' "${results_list[@]}" | awk -F'\t' -v s="$last" '$1 == s && $2 == "fail"' | wc -l)
				skips=$(printf '%s\n' "${results_list[@]}" | awk -F'\t' -v s="$last" '$1 == s && $2 == "skip"' | wc -l)
				printf '  <testsuite name="%s" tests="%d" failures="%d" errors="0" skipped="%d">\n' \
					"$(xml "$last")" "$tests" "$fails" "$skips"
				printf '%s' "$cases"
				echo '  </testsuite>'
			fi
			[ -z "$r" ] && break
			[ "$sec" != "$last" ] && cases=
			last=$sec
			cases+=$(printf '    <testcase classname="mcp-server-tukit.%s" name="%s" time="%s">' "$(xml "$sec")" "$(xml "$desc")" "$secs")
			case $st in
			fail) cases+='<failure message="failed"/>' ;;
			skip) cases+='<skipped/>' ;;
			esac
			cases+=$'</testcase>\n'
		done
		echo '</testsuites>'
	} >"$1/results.xml"
	echo "  results: $1/results.tap, $1/results.xml"
}

since=$(date +%s)
"phase$phase"

section "Logs"
journalctl -u mcp-gateway.service -u tukitd.service --since "@$since" --no-pager -o short-precise | tail -40
journalctl --since "@$since" --no-pager -o cat | grep 'mcp-server-tukit' | tail -20

section "Result"
[ -n "$results" ] && write_results "$results"
n_fail=$(printf '%s\n' "${results_list[@]}" | awk -F'\t' '$2 == "fail"' | wc -l)
n_skip=$(printf '%s\n' "${results_list[@]}" | awk -F'\t' '$2 == "skip"' | wc -l)
echo "  ${#results_list[@]} checks: $n_fail failed, $n_skip skipped"
if [ "$failed" = 0 ]; then echo "all checks passed"; else echo "some checks FAILED"; fi
exit "$failed"
