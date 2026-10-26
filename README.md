# mcp-tukit

An [MCP](https://modelcontextprotocol.io/) server for transactional
updates on immutable SUSE systems — openSUSE MicroOS, Leap Micro, SL
Micro, and SLES 16.1 in immutable mode — built on
[tukit](https://github.com/openSUSE/transactional-update), the library
and D-Bus service behind `transactional-update`.

It installs, removes and updates packages in new snapshots of the root
file system, shows the snapshots and whether a reboot is pending, rolls
back, and cleans up, the way `transactional-update` does. It is written
in Go and meant to run behind
[mcp-gateway](https://github.com/sdrahn/mcp-gateway), in the gateway's
sandbox with one capability and an SELinux domain of its own, with
SELinux enforcing.

## Status

The current release is **0.2.0** (2026-10-24), from the branch
`release-0.2`. `main` is 0.3 in development. Releases, with their source
tarballs and notes, are on the
[releases page](https://github.com/sdrahn/mcp-tukit/releases), and the
changes are in [CHANGELOG.md](CHANGELOG.md). Packages come from OBS
([packaging/suse](packaging/suse/README.md)).

| Version | Branch | mcp-gateway | The server runs |
|---|---|---|---|
| 0.2.x | `release-0.2` | 1.8.0 or later | in the gateway's sandbox with `CAP_SYS_ADMIN` only (`sandbox.capabilities`) |
| 0.1.x | `release-0.1` | 1.7.0 or later | as a privileged server (`privileged: true`) |

How it is tested:

- **Unit tests** run against a fake tukitd and a fake systemd on a
  private D-Bus.
- **CI** builds the packages as OBS does in openSUSE Tumbleweed and Leap
  16.0 containers, with rpmlint and an installation test.
- **The functional tests** run in a MicroOS VM with SELinux enforcing
  and tukitd 6.1.3. They install a package into a new snapshot, reboot
  with the server's own tool, apply a snapshot without a reboot, roll
  back and delete snapshots, and check that there are no SELinux
  denials.

Leap Micro, SL Micro and SLES 16.1 are not tested yet: the openQA
module for them is in `test/openqa`, not yet scheduled.

## How it works

```
agent ──MCP──▶ mcp-gateway ──stdio──▶ mcp-server-tukit ──D-Bus──▶ tukitd ──▶ snapshot (zypper, chrooted)
               policy, approvals       mcpsrv_tukit_t                unconfined_service_t
                                       root, CAP_SYS_ADMIN only      (root)
```

The server does **none of the privileged work itself**. Every tool is a
call to tukitd (`org.opensuse.tukit` on the system bus), which creates
the snapshot, mounts it, runs zypper in it and makes it the next
default. The server process needs no mounts, no btrfs ioctls, no files
and no network: its SELinux domain allows the gateway's backend
baseline, the D-Bus chat with tukitd, and starting the reboot and apply
units, and nothing else.

The instance holds **one capability**, `CAP_SYS_ADMIN`, and runs as
root, in the gateway's sandbox otherwise (`sandbox.capabilities`, from
mcp-gateway 1.8): a read-only system, no home directories, no network,
the gateway's system call filter, `no_new_privs`, its MCS pair. tukitd
registers its D-Bus methods without `SD_BUS_VTABLE_UNPRIVILEGED`, so
sd-bus answers only callers holding `CAP_SYS_ADMIN`, whatever the bus
policy says; and tukitd's bus policy lets in root only. The capability
is what tukitd checks; the server never uses it, and its SELinux domain
does not allow it to. Landlock keeps the instance from writing anywhere
as a second wall.

A change goes as `transactional-update` does it:

1. `Open` a new snapshot from the base (by default the **default**
   snapshot, so changes made before a reboot add up — like
   `transactional-update --continue`; `active` starts from the running
   system and discards changes not yet booted).
2. `Call` zypper in it, chrooted, and wait for tukitd's
   `CommandExecuted` / `Error` signal.
3. `Close` it (it becomes the default, active at the next reboot) if
   zypper succeeded — exit codes 0, 102, 103, 106, as
   `transactional-update` accepts them — or `Abort` it if zypper failed
   or had "Nothing to do.", so no empty or broken snapshot is left.

A change is finished even when the client cancels the call or its
connection breaks (the gateway's guidance for servers that change the
system): the snapshot is closed or discarded once zypper ends, within
`-timeout` (2 h). While a change runs, the server sends progress
notifications when the client asked for them. Changes of one instance
are serialized.

The command line passed to tukitd is built from validated arguments
only, every word single-quoted: tukitd splits it with `wordexp(3)`
without `WRDE_NOCMD`, so an unquoted `$(…)` would run as root. Package
arguments must look like package names (`vim`, `vim.x86_64`,
`pattern:base`, `kernel-default>=6.4`); paths, URLs, options and
anything with spaces or shell characters are refused.

## Tools

| Tool | Changes | What |
|---|---|---|
| `get_status` | no | active snapshot, default snapshot, whether a reboot is pending |
| `list_snapshots` | no | snapshots with number, date, description, active/default |
| `pkg_install` | yes | `zypper install` in a new snapshot (`accept_licenses`, `no_recommends`) |
| `pkg_remove` | yes | `zypper remove` in a new snapshot (`clean_deps`) |
| `update_system` | yes | `zypper update` / `dist-upgrade` / `patch` in a new snapshot |
| `rollback` | yes | make a snapshot the default (next boot) |
| `delete_snapshot` | yes | delete a snapshot (never the active or the default one) |
| `cleanup_snapshots` | yes | tukit cleanup |
| `apply` | yes | make the default snapshot's `/usr`, `/etc` and `/boot` the running system's without a reboot, as `transactional-update apply` does (`snapshot`: the one you expect to apply) |
| `reboot` | yes | reboot into the default snapshot, as `transactional-update reboot` does (`snapshot`: the one you expect to boot; `force`) |
| `run_in_snapshot` | yes | any command as root in a new snapshot — **only with `-allow-commands`** |

Changing tools carry `destructiveHint`, reading ones `readOnlyHint`;
results have `structuredContent` with an `outputSchema`.

### Rebooting

A change takes effect at the next boot. `reboot` is a tool of its own,
with an approval of its own for every call (role `tukit-operator`, scope
`once`), so that installing and rebooting are two decisions:

- It refuses when no reboot is pending (the default snapshot is the
  running one) unless `force` is set, when `snapshot` is given and is
  not the default (the agent names what it expects to boot, so a change
  made meanwhile by someone else is not booted unseen), and while a
  change of the same instance is running.
- tukitd has no reboot of its own, and the server stays unprivileged:
  it asks systemd to start one unit, `mcp-tukit-reboot.service`, which
  runs `transactional-update reboot`. So the administrator's
  `REBOOT_METHOD` applies: `systemd` (soft-reboot or kexec where
  `tukit.conf` allows them), rebootmgr's maintenance window (`auto` when
  rebootmgr runs), `kured`, `notify`, `none`. The result names the
  method.
- The unit waits 10 s before it reboots, so that the answer reaches the
  agent; the agent's connection ends with the reboot.
- The server may start this unit and nothing else: SELinux allows the
  domain `service start` on the type `mcpsrv_tukit_unit_t` only, which
  only this unit file has. (systemd lets a caller with `CAP_SYS_ADMIN`
  start units without asking polkit.)
- `-reboot-unit ""` turns the tool off.

### Applying without a reboot

`apply` is the other way to make a change live: it runs
`transactional-update apply`, which mounts the default snapshot's
`/usr`, `/etc` and `/boot` into the running system, so that newly
installed programs can be used at once. It needs an approval of its own
for every call, like `reboot`.

- As with an update on a system without snapshots, running programs
  keep the files they have open and services are not restarted; a new
  kernel or initrd takes effect only at the next reboot. The tool's
  description and result say so.
- It refuses when there is nothing to apply (the default snapshot is
  the running one), when `snapshot` is given and is not the default,
  and while a change of the same instance is running.
- The same way as for `reboot`, the server starts one unit,
  `mcp-tukit-apply.service`; unlike `reboot`, it waits for the unit's
  end and reports whether `transactional-update apply` succeeded (the
  details are in `journalctl -u mcp-tukit-apply.service`). SELinux lets
  the domain start this unit (its own type,
  `mcpsrv_tukit_apply_unit_t`) and the reboot unit, no other.
- `-apply-unit ""` turns the tool off.

Options: `-base` (default base snapshot), `-timeout`, `-zypper` (path
in the snapshot), `-allow-commands`, `-reboot-unit`, `-apply-unit`, `-instructions`
(text that opens the server's instructions), `-check` (connect to tukitd and list the
snapshots), `-version`.

## Behind mcp-gateway

`dist/` holds what a package installs for the gateway (the package
`mcp-server-tukit-gateway` in `packaging/suse/mcp-server-tukit.spec`):

| File | Installed as |
|---|---|
| `dist/gateway/tukit.yaml` | `/usr/share/mcp-gateway/profiles/tukit.yaml` — server `tukit`, `run_as: root`, `sandbox.capabilities: [CAP_SYS_ADMIN]`, `selinux_type: mcpsrv_tukit_t`, no network, no home directories, Landlock (system read-only, no writes); takes effect once linked into `/etc/mcp-gateway/servers.d` (below) |
| `dist/gateway/roles.yaml` | `/usr/share/mcp-gateway/policy/mcp/profiles/tukit/data.yaml` — roles `tukit-reader` and `tukit-operator` |
| `dist/systemd/mcp-tukit-reboot.service` | `/usr/lib/systemd/system/mcp-tukit-reboot.service` — the reboot (`transactional-update reboot`) |
| `dist/systemd/mcp-tukit-apply.service` | `/usr/lib/systemd/system/mcp-tukit-apply.service` — the apply (`transactional-update apply`) |
| `dist/selinux/mcp_tukit.{te,fc}` | module `mcp_tukit` (package `mcp-server-tukit-selinux`) |

The gateway accepts servers with capabilities, as privileged ones, only
from the administrator's directory, so installing the package never
creates one. Enable it by linking the definition there:

```bash
ln -s /usr/share/mcp-gateway/profiles/tukit.yaml /etc/mcp-gateway/servers.d/tukit.yaml
systemctl reload mcp-gateway.service
```

### Who may do what

The three layers of the gateway's chapter 13 apply:

- **The gateway's policy** decides who may call which tool:
  `tukit-reader` gets `get_status` and `list_snapshots`;
  `tukit-operator` gets those and every change **with approval**
  (out-of-band, in Cockpit); `rollback`, `apply`, `reboot` and
  `run_in_snapshot` need an approval of their own each time (scope
  `once`).
- **tukitd** answers callers with `CAP_SYS_ADMIN` only, and does the
  work as root. The gateway's instance is the only such caller it gets
  through MCP; whoever can call it can run commands as root in new
  snapshots, so the gateway's roles and approvals are what guard that.
  The gateway treats a server with capabilities as a privileged one
  here: its calls are allowed without approval only by permissions
  naming server and tool without wildcards, and every decision on it
  goes to the kernel audit log.
- **SELinux** keeps the instance to the D-Bus chat with tukitd and
  starting the reboot and apply units, and does not let it use
  `CAP_SYS_ADMIN` in the kernel.

Bind users or groups to the roles in the gateway's role data:

```yaml
bindings:
  groups:
    sysops: [tukit-operator, systemd-reader]
```

### SELinux

`mcp_tukit.te` uses the gateway's `mcp_gateway_backend_template(tukit)`
(from `mcp-gateway-selinux`) and adds `dbus_system_bus_client` and
`dbus send_msg` both ways between `mcpsrv_tukit_t` and tukitd's domain;
for the reboot and apply tools, the D-Bus chat with systemd and
`service start`/`status` on `mcpsrv_tukit_unit_t` and
`mcpsrv_tukit_apply_unit_t`, the two units' own types, only.
openSUSE's policy has no module for tukitd (`/usr/sbin/tukitd` is
`bin_t`), so systemd runs it as `unconfined_service_t`; a block for a
future `tukitd_t` is included. The instance runs with its own MCS
category pair, like every gateway instance.

On a transactional system, install the server, its SELinux module and
the gateway's in one transaction, then reboot: a module installed in a
transaction takes effect at the next boot, and a program installed
before its module keeps the wrong label (gateway user guide, chapter 2,
"Transactional systems"). `mcp-gateway-admin doctor` checks types and
labels.

```bash
transactional-update pkg install mcp-gateway mcp-server-tukit-gateway
reboot
```

## Building

```bash
make check      # gofmt, go vet, go test -race (the tukit client tests need dbus-daemon)
make build      # bin/mcp-server-tukit
make selinux GATEWAY_IF=../mcp-gateway/selinux/mcp_gateway.if   # dist/selinux/mcp_tukit.pp
make install install-selinux DESTDIR=...
make build-tests install-tests DESTDIR=...   # the functional tests, as packaged
```

## Packaging and tests

| Path | What |
|---|---|
| `packaging/suse/` | the OBS package: spec, `_service`, rpmlintrc and how to set it up ([README](packaging/suse/README.md)); packages `mcp-server-tukit`, `-gateway`, `-selinux`, `-tests` |
| `test/vm/run-tests.sh` | the functional tests on a real transactional system with SELinux enforcing: phase 1, the reboot phase (the tool's own reboot), phase 2 (package `mcp-server-tukit-tests`) |
| `test/tukitcall/` | their client: one MCP request through the gateway's `mcp-connect` |
| `test/vm/run-vm.sh`, `prepare.sh` | CI: a MicroOS VM provisioned by Combustion, the packages installed with `transactional-update`, the phases with the tool's reboot between them |
| `test/openqa/` | the openQA test module and the plan for openQA ([README](test/openqa/README.md)) |

CI (`.github/workflows/ci.yml`): Go tests and lint; the SELinux module
against the reference policy; mcp-gateway's packages built from its
source; this package built as OBS builds it in Tumbleweed and Leap 16
containers, with rpmlint and an installation test; then the VM tests.

Requirements: Go ≥ 1.24; for the module the reference-policy devel files
(`selinux-policy-devel` on openSUSE/SLES) and the gateway's interface
(installed by `mcp-gateway-selinux`, or `GATEWAY_IF`).

The only dependency is [godbus](https://github.com/godbus/dbus) (the
gateway uses it too). The MCP protocol core (`internal/mcp`) follows the
gateway's own servers' (newline-delimited JSON-RPC on stdio,
cancellation, progress). It speaks MCP 2026-07-28: `server/discover`
instead of the `initialize` handshake, the protocol version in each
request's `_meta`, `resultType` in results, and a request naming a
version it does not speak refused with `-32022`. It keeps no state per
connection. Earlier clients are served as before, with `initialize`
and protocol versions 2025-11-25 down to 2024-11-05. mcp-gateway 1.8
probes with `server/discover` and so uses the server without the
handshake.

## Not yet

- The functional tests in openQA on Leap Micro, SL Micro 6.x and SLES
  16.1 (immutable); and a profiling run (`mcp-gateway-admin profile`)
  to confirm the domain needs nothing more.
- tukitd before 6.0 has no `…WithOpts` methods: the server falls back to
  `Open` and the snapshot gets no description.
- What `transactional-update` does beyond tukit is not done: bootloader
  and initrd handling outside package scriptlets, `apply-oci`, `kdump`,
  `register`, `migration`, `setup-selinux`.
- Changes to `/etc` overlays are handled by tukit; files below `/etc`,
  `/var` and `/home` are not this server's business (they are writable
  on the running system).
- No capability at all: that needs tukitd to mark its methods
  `SD_BUS_VTABLE_UNPRIVILEGED` upstream and leave access to its bus
  policy (and polkit).
- Possibly moving `dist/gateway` into mcp-gateway's `profiles/` as
  `mcp-gateway-profile-tukit`, like the other system management servers.

## License

MIT; see [LICENSE](LICENSE).
