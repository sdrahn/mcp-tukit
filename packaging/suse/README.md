# Packaging for openSUSE and SLES (OBS)

`mcp-server-tukit.spec`, `_service` and `mcp-server-tukit-rpmlintrc` are
ready to be used in an Open Build Service project, the same way as
mcp-gateway's (its `packaging/suse/README.md`). CI builds the same spec
in openSUSE Tumbleweed and Leap 16 containers (job `opensuse` in
`.github/workflows/ci.yml`), with rpmlint and an installation test, and
runs the packages' functional tests in a MicroOS VM (job `vm`).

## Setting up the OBS package

The package build-requires `mcp-gateway-selinux` (the interface
`mcp_gateway_backend_template` its SELinux module is built with), and
its subpackages require `mcp-gateway` and `tukitd`. Build it in the
project that builds mcp-gateway, or in one that links or aggregates it:

```bash
osc mkpac mcp-server-tukit       # in the project with mcp-gateway, e.g. home:<you>
cd mcp-server-tukit
cp <repo>/packaging/suse/{mcp-server-tukit.spec,_service,mcp-server-tukit-rpmlintrc} .
osc service manualrun            # fetch sources, set version, vendor Go modules
osc vc -m "Initial package"      # creates mcp-server-tukit.changes
osc add *
osc commit
```

The services run in `manual` mode: `tar_scm` fetches `main` from GitHub
and names the tarball `mcp-server-tukit-0.1.0+git<date>.<hash>.tar.gz`,
`set_version` puts that version into the spec, and `go_modules` creates
`vendor.tar.gz`, because OBS builds have no network access. Re-run
`osc service manualrun` to update, then `osc vc` and commit. The spec,
`_service` and the rpmlintrc in the OBS package are copies: copy them
again whenever they change here, before running the services.

Build targets: openSUSE Tumbleweed (MicroOS), Leap 16.0 (for Leap Micro
6.2 and SLES 16), SL Micro 6.2 and SLES 16.1 once mcp-gateway builds
there. The repositories of a project for Leap Micro build against
Leap 16 / SLES 16 with the Micro repositories added, as in the project
configuration.

## Packages

| Package | Contents |
|---|---|
| `mcp-server-tukit` | the server, `/usr/bin/mcp-server-tukit`; requires `tukitd` |
| `mcp-server-tukit-gateway` | the gateway setup: definition `/usr/share/mcp-gateway/servers.d/tukit.yaml`, roles `tukit-reader` and `tukit-operator`, the D-Bus policy `/usr/share/dbus-1/system.d/mcp-tukit.conf`, the account `mcp-tukit` (sysusers); requires `mcp-gateway`, and pulls in `-selinux` where `selinux-policy-targeted` is installed |
| `mcp-server-tukit-selinux` | the module `mcp_tukit` (domain `mcpsrv_tukit_t`), installed with the `%selinux_*` macros; requires `mcp-gateway-selinux` |
| `mcp-server-tukit-tests` | the functional tests for openQA and other test systems (`/usr/libexec/mcp-server-tukit/tests/run-tests.sh --disposable --phase 1|2 --results DIR`, TAP and JUnit results) and their client; they change the system for good, so only for test systems ([test/openqa](../../test/openqa/README.md)) |

On a transactional system:

```bash
transactional-update pkg install mcp-server-tukit-gateway
reboot
```

All in one transaction: an SELinux module installed in a transaction
takes effect at the next boot, and a program installed before its module
keeps the wrong label (mcp-gateway user guide, chapter 2).

## Before submitting to openSUSE:Factory or SLE

- **mcp-gateway first:** the package builds against
  `mcp-gateway-selinux` and is useful only with the gateway.
- **D-Bus policy:** `mcp-tukit.conf` allows the account `mcp-tukit` to
  call tukitd, which otherwise allows root only. D-Bus policy files need
  review and whitelisting by the SUSE security team (a bug for the
  security team's D-Bus audit); `mcp-server-tukit-rpmlintrc` filters
  the rpmlint errors only to keep devel and home builds working. The
  argument for the review: only the gateway starts processes as
  `mcp-tukit` (no login, no shell), its policy makes every change wait
  for a human's approval, and the server's process runs without
  capabilities in a confined domain.
- **Licenses:** MIT; the binary also contains the vendored godbus
  (BSD-2-Clause), stated in `License:`.
- **openQA:** the test module and its plan are in
  [test/openqa](../../test/openqa/README.md); a product's maintainers
  want it scheduled before the package goes in.
