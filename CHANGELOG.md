# Changelog

All notable changes to mcp-server-tukit. Versions follow
[Semantic Versioning](https://semver.org/).

## Unreleased

- **The sandbox instead of a privileged server.** The gateway definition
  (now `/usr/share/mcp-gateway/profiles/tukit.yaml`) runs the server in
  mcp-gateway's sandbox with `CAP_SYS_ADMIN` and no other capability
  (`sandbox.capabilities`), instead of `privileged: true`: a read-only
  system, no home directories, `no_new_privs`, the system call filter
  and an MCS pair, still as root, which tukitd's bus policy requires.
  Needs mcp-gateway 1.8. `tukit-privileged.yaml`, 0.1's name, is now a
  link to `tukit.yaml`, so a link made to it keeps working and gets the
  sandbox; it goes away in 0.3, so link the new name:
  `ln -sf /usr/share/mcp-gateway/profiles/tukit.yaml /etc/mcp-gateway/servers.d/tukit.yaml`.

## v0.1.0 — 2026-10-24

The first release: an MCP server (stdio) for immutable SUSE systems
(openSUSE MicroOS, Leap Micro, SL Micro, SLES 16.1 in immutable mode)
that makes its changes in new snapshots through tukitd, as
transactional-update does, and runs behind mcp-gateway with SELinux
enforcing.

- **Tools:** `get_status`, `list_snapshots`, `pkg_install`, `pkg_remove`,
  `update_system` (update, dist-upgrade, patch), `rollback`,
  `delete_snapshot`, `cleanup_snapshots` (tukitd 6.2 and later), `apply`
  (the default snapshot live without a reboot), `reboot`, and
  `run_in_snapshot` (any command, only with `-allow-commands`). A change
  that fails, or does nothing, leaves no snapshot.
- **Reboot and apply** run through one unit each,
  `mcp-tukit-reboot.service` and `mcp-tukit-apply.service`; the SELinux
  domain `mcpsrv_tukit_t` may start these two and no other. Both refuse
  when there is nothing to reboot into or apply, and when the snapshot
  the agent names is not the default.
- **Behind mcp-gateway:** a privileged server definition (tukitd answers
  only callers with CAP_SYS_ADMIN), enabled by linking it into
  `/etc/mcp-gateway/servers.d`, still confined by `mcpsrv_tukit_t` and
  Landlock; roles `tukit-reader` and `tukit-operator`, with every change
  approved and every reboot, apply and command approved by itself.
- **Packages** (OBS, `packaging/suse`): `mcp-server-tukit`, `-gateway`,
  `-selinux` and `-tests` (functional tests for openQA, in phases with
  a reboot by the server's own tool between them).
- **Tested** in CI on openSUSE Tumbleweed and Leap 16.0 (package build,
  rpmlint, installation) and in a MicroOS VM with SELinux enforcing; the
  openQA module and plan are in `test/openqa`.

Tested with mcp-gateway's main branch (1.8 in development) and tukitd
6.1.3 (MicroOS).
