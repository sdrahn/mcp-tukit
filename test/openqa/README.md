# mcp-server-tukit in openQA

The functional tests come as the package `mcp-server-tukit-tests`, for
openQA and other systems that test a product with the packages from its
repositories. The CI's VM test runs the same package the same way on
openSUSE MicroOS (`test/vm/run-vm.sh`), so a test that passes in CI is
the one openQA runs.

The server is for transactional systems only, and what it does takes
effect at the next boot. That is what openQA does well and a container
cannot: real snapshots, real reboots, SELinux enforcing as the product
ships it, and the products themselves (SL Micro, SLES 16.1 immutable),
which need registration.

## What the package holds

| Path | What |
|---|---|
| `/usr/libexec/mcp-server-tukit/tests/run-tests.sh` | the tests (`test/vm/run-tests.sh` in the source) |
| `/usr/libexec/mcp-server-tukit/tests/tukitcall` | the test client: one MCP request through the gateway (`mcp-connect`), as the user who runs it |

It requires the server's packages (`mcp-server-tukit`, `-gateway`,
`-selinux`), the gateway's (`mcp-gateway`, `mcp-gateway-selinux`) and
what the tests run: `tukitd`, `jq`, `curl`, `audit`, `policycoreutils`,
`selinux-tools`, `util-linux`.

## Running them

As root, on a transactional system with SELinux enforcing that is there
for testing:

```bash
transactional-update pkg install mcp-server-tukit-tests
reboot
/usr/libexec/mcp-server-tukit/tests/run-tests.sh --disposable --phase 1 --results /var/tmp/mcp-tukit-tests/phase-1
/usr/libexec/mcp-server-tukit/tests/run-tests.sh --disposable --phase reboot --results /var/tmp/mcp-tukit-tests/phase-reboot
# the system reboots by itself about 10 s later
/usr/libexec/mcp-server-tukit/tests/run-tests.sh --disposable --phase 2 --results /var/tmp/mcp-tukit-tests/phase-2
```

- **Three phases; the reboot is the server's own.** Phase 1 sets up users
  (`tuki`, operator; `rita`, reader; `ned`, no role), starts the
  links the privileged definition into `/etc/mcp-gateway/servers.d`
  (the package alone activates nothing), starts the gateway, checks the
  instance (domain `mcpsrv_tukit_t`, root with `CAP_SYS_ADMIN`, which
  tukitd requires, no MCS pair), that a user cannot call tukitd, the roles
  (readers read, nobody else does anything) and the approvals, then
  installs a package into a new snapshot after an approval; installs it
  again (nothing to do: no snapshot kept) and a package that does not
  exist (failed: no snapshot kept); and checks that there are no SELinux
  denials. Phase 2, in the new snapshot: the package is there, rollback
  (and back), deleting snapshots (not the running or the default one;
  a spare one made with snapper is deleted after an approval, and
  deleting it again is no error), cleanup, `apply` (a reader may not;
  a second package installed into a new snapshot is in the running
  system after an approved apply, without a reboot), and
  `run_in_snapshot` after the administrator enables it
  (a failing command leaves no snapshot); again no denials. Between
  them, the reboot phase: a reader may not reboot, a reboot into a
  snapshot that is not the default is refused, the domain may start no
  other unit (SELinux, with `sesearch`), and then, after an approval, the reboot tool
  reboots into phase 1's snapshot with `transactional-update reboot`.
  Phase 2 checks that the system was rebooted, through the tool's unit
  (when the journal is persistent), and that a reboot with nothing
  pending is refused. The reboot phase sets `REBOOT_METHOD=systemd` in
  `/etc/transactional-update.conf`, so that the reboot happens at once:
  with the default `auto`, a system running rebootmgr (MicroOS does)
  reboots in rebootmgr's maintenance window, as it should.
- **They change the system for good:** users, the gateway's role data,
  an installed package, the default snapshot and `REBOOT_METHOD`. Without `--disposable`
  they refuse to start (exit status 2).
- **The package to install:** the first of `tree`, `figlet`, `sl`,
  `hello` that the repositories have and the system has not, or
  `--package NAME`. It must come from a repository the system has: a
  registered SL Micro or SLES has its own.
- **Results:** every check prints `PASS:`, `FAIL:` or `SKIP:`, in
  sections; with `--results DIR`, `results.tap` and `results.xml`
  (JUnit, a test suite per section, named with the phase). Exit status
  0 when nothing failed, 1 otherwise.
- **Time:** a few minutes per phase, mostly zypper; with a slow
  repository more. The module gives each phase 30 minutes.

## The test module

[mcp_tukit.pm](mcp_tukit.pm), for
[os-autoinst-distri-opensuse](https://github.com/os-autoinst/os-autoinst-distri-opensuse)
as `tests/transactional/mcp_tukit.pm`. It adds the repository with the
packages (`MCP_TUKIT_REPO`) if given, installs `mcp-server-tukit-tests`
with `trup_call`, reboots with `process_reboot`, runs phase 1 and the
reboot phase, watches the reboot the server's tool started
(`process_reboot` without `trigger`), runs phase 2, and hands both JUnit results to openQA
(`parse_extra_log`). On failure it uploads the journal of the gateway,
tukitd and the instances, the SELinux denials since boot and the
snapshot list.

Variables:

| Variable | Meaning |
|---|---|
| `MCP_TUKIT_REPO` | a repository with the packages, e.g. the OBS project's for the product (`https://download.opensuse.org/repositories/<project>/<repo>/`); unset: the product's repositories |
| `MCP_TUKIT_PACKAGE` | the package phase 1 installs (default: see above) |

Before submitting the module, check it against the library of the day:
`trup_call`, `process_reboot` (lib/transactional.pm) and `zypper_ar`
(lib/utils.pm) are used as of 2026; `perl -c` passes against stubs only.

## Plan for inclusion

### Step 1: development (now)

- The OBS devel project builds the packages for openSUSE Tumbleweed
  (MicroOS) and Leap 16 / Leap Micro 6.2 (`packaging/suse`).
- CI runs the unit tests, the D-Bus tests against a private bus, the
  RPM build with rpmlint in openSUSE containers, and the functional
  tests in a MicroOS VM with SELinux enforcing, the same package openQA
  will run (`.github/workflows/ci.yml`, jobs `opensuse` and `vm`).
- Manual openQA runs on o3 (openqa.opensuse.org) against the devel
  project: clone a MicroOS job and add the module,
  `openqa-clone-job --within-instance https://openqa.opensuse.org <job> CASEDIR=<fork> MCP_TUKIT_REPO=<repo> SCHEDULE=…,tests/transactional/mcp_tukit`.

### Step 2: openSUSE (o3)

Once the packages are in Factory (mcp-gateway first):

| Product | Flavor / test suite | Schedule |
|---|---|---|
| openSUSE MicroOS (Tumbleweed) | `microos_mcp_tukit` on the `MicroOS-Image` (kvm-and-xen) flavor, x86_64 and aarch64 | after `microos/disk_boot`, `transactional/host_config`: `transactional/mcp_tukit` |
| openSUSE Leap Micro 6.2 | the same suite on the Default image | as above; not in CI: the Default image boots into the initrd's emergency shell under CI's plain QEMU setup (BIOS, no serial console output to say why), so openQA, which boots it as the product is meant to be, is where Leap Micro is tested |

The YAML schedule (`schedule/microos/mcp_tukit.yaml`):

```yaml
name: microos_mcp_tukit
description: >
  mcp-server-tukit behind mcp-gateway: transactional changes through an
  MCP server with SELinux enforcing, across two reboots.
schedule:
  - microos/disk_boot
  - transactional/host_config
  - transactional/mcp_tukit
```

### Step 3: SUSE (osd)

With the packages in SL Micro 6.2 / SLES 16.1 (immutable mode), or in a
module or PackageHub for them:

| Product | Notes |
|---|---|
| SL Micro 6.2 | registered (the package to install comes from SUSE's repositories), x86_64, aarch64, s390x |
| SLES 16.1 in immutable mode | the same suite; the reason this server exists |
| SLES 16.1 (read-write, btrfs) | tukit works there too: the tests run, the "read-only root" check is a SKIP |

### Tests to add

The most valuable first, each a module of its own:

1. **A long update:** `update_system` (`dist-upgrade` on MicroOS, `patch`
   on SL Micro) against a snapshot of the repositories that has
   updates: progress notifications, a call that outlasts the agent's
   connection (the change still finishes), reboot into the result.
2. **Rollback after a broken boot:** an update that health-checker
   rejects (a test package breaking a service), the automatic rollback,
   and get_status telling the agent what happened.
3. **Approvals in Cockpit:** the same changes approved on the gateway's
   Cockpit page with needles, not the control API.
4. **Concurrency:** two operators changing at once (two instances, each
   building on the default): what the second one gets, and that tukitd's
   locking leaves no half-made snapshot.
5. **Upgrades:** the previous release of the packages, an update with
   `transactional-update up`, the server definition and role data kept.
6. **Other architectures and FIPS:** aarch64, s390x (zipl instead of
   GRUB), ppc64le; SLES 16.1 with `fips=1`.
7. **Profiling:** `mcp-gateway-admin profile` of the server, to show the
   SELinux domain needs nothing more than the module gives it.
