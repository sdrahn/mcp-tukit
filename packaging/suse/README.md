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

The services run in `manual` mode: `tar_scm` fetches the release tag
named in `_service` (`v0.1.0`) from GitHub and names the tarball after
it, `mcp-server-tukit-0.1.0.tar.gz`, `set_version` puts that version
into the spec, and `go_modules` creates `vendor.tar.gz`, because OBS
builds have no network access. The spec, `_service` and the rpmlintrc
in the OBS package are copies: copy them again whenever they change
here, before running the services, then `osc vc` and commit. Each
release's GitHub page also has the tarball and `vendor.tar.gz` with
their SHA-256 sums, to compare with what the services made.

## Releasing

1. On a branch: set `Version:` in the spec, `revision` in `_service`
   (`vX.Y.Z`) and a section `## vX.Y.Z — <date>` at the top of
   `CHANGELOG.md` (below `## Unreleased`); `tools/check-release vX.Y.Z`
   says what still differs. Merge it once CI is green.
2. Tag the merge on `main` and push the tag:
   `git tag -a vX.Y.Z -m "mcp-server-tukit X.Y.Z" && git push origin vX.Y.Z`.
   The Release workflow checks the files again, runs the tests and
   publishes the GitHub release with the sources and the changelog's
   section as notes.
3. Update the OBS package as above.

Build targets: openSUSE Tumbleweed (MicroOS), Leap 16.0 (for Leap Micro
6.2 and SLES 16), SL Micro 6.2 and SLES 16.1 once mcp-gateway builds
there. The repositories of a project for Leap Micro build against
Leap 16 / SLES 16 with the Micro repositories added, as in the project
configuration.

## Packages

| Package | Contents |
|---|---|
| `mcp-server-tukit` | the server, `/usr/bin/mcp-server-tukit`; requires `tukitd` |
| `mcp-server-tukit-gateway` | the gateway setup: the privileged definition `/usr/share/mcp-gateway/profiles/tukit-privileged.yaml` (the administrator links it into `/etc/mcp-gateway/servers.d`), roles `tukit-reader` and `tukit-operator`, and the units of the reboot and apply tools, `mcp-tukit-reboot.service` (`transactional-update reboot`) and `mcp-tukit-apply.service` (`transactional-update apply`); requires `mcp-gateway`, `transactional-update`, and pulls in `-selinux` where `selinux-policy-targeted` is installed |
| `mcp-server-tukit-selinux` | the module `mcp_tukit` (domain `mcpsrv_tukit_t`), installed with the `%selinux_*` macros; requires `mcp-gateway-selinux` |
| `mcp-server-tukit-tests` | the functional tests for openQA and other test systems (`/usr/libexec/mcp-server-tukit/tests/run-tests.sh --disposable --phase 1|reboot|2 --results DIR`, TAP and JUnit results) and their client; they change the system for good, so only for test systems ([test/openqa](../../test/openqa/README.md)) |

On a transactional system:

```bash
transactional-update pkg install mcp-server-tukit-gateway
reboot
ln -s /usr/share/mcp-gateway/profiles/tukit-privileged.yaml /etc/mcp-gateway/servers.d/tukit.yaml
systemctl reload mcp-gateway.service
```

All in one transaction: an SELinux module installed in a transaction
takes effect at the next boot, and a program installed before its module
keeps the wrong label (mcp-gateway user guide, chapter 2).

## Before submitting to openSUSE:Factory or SLE

- **mcp-gateway first:** the package builds against
  `mcp-gateway-selinux` and is useful only with the gateway.
- **No D-Bus policy or polkit rule:** the server reaches tukitd as a
  privileged (root) server, which tukitd's own policy allows, and
  systemd lets it start the reboot unit without polkit; nothing in the
  package needs the security team's whitelisting. The package installs
  no active server: the administrator enables the privileged
  definition, as for mcp-gateway's privileged zypp and snapper servers.
- **Licenses:** MIT; the binary also contains the vendored godbus
  (BSD-2-Clause), stated in `License:`.
- **openQA:** the test module and its plan are in
  [test/openqa](../../test/openqa/README.md); a product's maintainers
  want it scheduled before the package goes in.
