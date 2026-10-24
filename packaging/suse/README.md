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

The services run in `manual` mode: on `main`, `tar_scm` fetches
`main` and names the tarball after the next minor release and the
commit, `mcp-server-tukit-0.2.0+git<date>.<hash>.tar.gz`; on a release
branch it fetches that branch and takes the version from its latest tag
(below). `set_version` puts the version into the spec, and `go_modules`
creates `vendor.tar.gz`, because OBS builds have no network access.

### Releases

Each release has a branch `release-X.Y` and tags `vX.Y.Z`, as
mcp-gateway's; tags are made on the release branch, never on `main`.
Pushing a tag publishes a GitHub release with
`mcp-server-tukit-X.Y.Z.tar.gz`, `vendor.tar.gz` and `SHA256SUMS`
(`.github/workflows/release.yml`). On a release branch, `_service`
fetches that branch and takes the version from its latest tag, so the
package version is `X.Y.Z`.

1. **A new minor release X.Y:** create `release-X.Y` from `main`. On the
   branch, `_service` builds from `release-X.Y` with `@PARENT_TAG@`, the
   spec says `Version: X.Y.0`, and `CHANGELOG.md` has the section
   `## vX.Y.0 — <date>` at the top (below `## Unreleased`). On `main`,
   `_service` and the spec move to X.(Y+1) (`X.(Y+1).0+git%cd.%h`).
2. **A fix release X.Y.Z:** merge the fix into `release-X.Y` (and
   `main`), set `Version: X.Y.Z` and add its CHANGELOG.md section on the
   branch.
3. **Check, tag, push:** `tools/check-release vX.Y.Z` on the branch says
   what still differs from the release (the spec's `Version`, the first
   CHANGELOG.md section, `_service`). Then tag the branch's head and push
   the tag:
   ```bash
   git fetch origin release-X.Y
   git tag -a vX.Y.Z -m "mcp-server-tukit X.Y.Z" origin/release-X.Y
   git push origin vX.Y.Z
   ```
   The Release workflow runs the same check, refuses a tag that fails
   it, runs the tests, publishes the release with the tag's CHANGELOG.md
   section as notes, then downloads the files again and checks them
   against `SHA256SUMS` and `git archive` of the tag.
4. **OBS:** take the spec, `_service` and the rpmlintrc of the release's
   tag, run the services, and commit with the release's CHANGELOG.md
   section:
   ```bash
   tag=vX.Y.Z
   for f in mcp-server-tukit.spec _service mcp-server-tukit-rpmlintrc; do
     curl -sSfL -o "$f" "https://raw.githubusercontent.com/sdrahn/mcp-tukit/$tag/packaging/suse/$f"
   done
   osc service manualrun
   osc vc -m "Update to ${tag#v}"
   osc commit
   ```

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
