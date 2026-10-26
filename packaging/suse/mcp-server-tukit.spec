#
# spec file for package mcp-server-tukit
#
# Built on the Open Build Service for openSUSE Tumbleweed (MicroOS), Leap
# 16 / Leap Micro, SL Micro and SLES 16; see packaging/suse/README.md.
# Sources: the tarball and vendor.tar.gz (Go modules, OBS builds are
# offline) come from the _service file.
#

%define selinuxtype targeted
%define modulename mcp_tukit

Name:           mcp-server-tukit
Version:        0.3.0
Release:        0
Summary:        MCP server for transactional updates with tukit
# MIT for mcp-server-tukit; BSD-2-Clause for the vendored godbus.
License:        MIT AND BSD-2-Clause
Group:          System/Management
URL:            https://github.com/sdrahn/mcp-tukit
Source0:        %{name}-%{version}.tar.gz
Source1:        vendor.tar.gz
Source99:       %{name}-rpmlintrc
BuildRequires:  bzip2
BuildRequires:  golang(API) >= 1.24
BuildRequires:  make
# %%selinux_requires adds this when rpmbuild expands it, after OBS chose
# the build's packages: name it here (as mcp-gateway's spec does).
BuildRequires:  pkgconfig(systemd)
# mcp_gateway_backend_template, from the gateway's interface file.
BuildRequires:  mcp-gateway-selinux
BuildRequires:  selinux-policy-devel
BuildRequires:  systemd-rpm-macros
# The server only talks to tukitd; tukitd does the work.
Requires:       tukitd

%description
An MCP server (stdio) for immutable SUSE systems (openSUSE MicroOS, Leap
Micro, SL Micro, SLES 16.1 in immutable mode): installs, removes and
updates packages in new snapshots as transactional-update does, lists
snapshots, rolls back and cleans up, through tukitd on the system bus.
It is meant to run behind mcp-gateway, in the gateway's sandbox with
CAP_SYS_ADMIN only and its own SELinux domain (package
mcp-server-tukit-gateway).

%package gateway
Summary:        mcp-server-tukit behind mcp-gateway
Group:          System/Management
BuildArch:      noarch
Requires:       %{name} = %{version}
# sandbox.capabilities.
Requires:       mcp-gateway >= 1.8.0
Requires:       (%{name}-selinux if selinux-policy-%{selinuxtype})
# The reboot unit runs transactional-update reboot.
Requires:       transactional-update
%{?systemd_ordering}

%description gateway
Sets up mcp-server-tukit behind mcp-gateway: the server definition
(server tukit, in the gateway's sandbox with CAP_SYS_ADMIN only, which
tukitd requires of its callers, in the SELinux domain mcpsrv_tukit_t),
the roles tukit-reader and tukit-operator (every change approved), and
the units of the reboot and apply tools, mcp-tukit-reboot.service
(transactional-update reboot) and mcp-tukit-apply.service
(transactional-update apply), the only units the domain may start. The definition takes effect
once the administrator links it into /etc/mcp-gateway/servers.d:
  ln -s /usr/share/mcp-gateway/profiles/tukit.yaml \
        /etc/mcp-gateway/servers.d/tukit.yaml

%package selinux
Summary:        SELinux policy module for mcp-server-tukit
License:        MIT
Group:          System/Management
BuildArch:      noarch
Requires:       mcp-gateway-selinux
Requires:       selinux-policy-%{selinuxtype}
Requires(post): selinux-policy-%{selinuxtype}
# %%selinux_requires asks for the build's selinux-policy version and
# release; the module needs the policy version only: drop the release.
%{?_selinux_policy_version:%global _selinux_policy_version %(echo '%{_selinux_policy_version}' | cut -d- -f1)}
%{?selinux_requires}

%description selinux
The SELinux domain mcpsrv_tukit_t for mcp-server-tukit behind
mcp-gateway: the gateway's backend baseline and the D-Bus chat with
tukitd, nothing else.

%package tests
Summary:        Functional tests of mcp-server-tukit, for openQA
Group:          Development/Tools/Other
Requires:       %{name} = %{version}
Requires:       %{name}-gateway = %{version}
Requires:       %{name}-selinux = %{version}
Requires:       mcp-gateway-selinux
# What the tests run: the kernel audit log, SELinux tools, the control
# API of the gateway, JSON, runuser.
Requires:       audit
Requires:       curl
Requires:       jq
Requires:       policycoreutils
Requires:       selinux-tools
Requires:       util-linux

%description tests
The functional tests of mcp-server-tukit behind mcp-gateway on an
installed transactional system with SELinux enforcing, for openQA and
other test systems: the instance's domain and account, the D-Bus
policy, roles and approvals, and real changes through tukitd in two
phases with a reboot between them:
/usr/libexec/mcp-server-tukit/tests/run-tests.sh --disposable --phase 1|2 --results DIR
They change the system for good (users, role data, an installed
package, the default snapshot): only for systems that are there for
testing.

%prep
%autosetup -p1 -a1

%build
export GOFLAGS="-mod=vendor"
%make_build build build-tests VERSION=%{version}
make selinux

%install
export GOFLAGS="-mod=vendor"
%make_install install install-tests install-selinux VERSION=%{version} PREFIX=%{_prefix} \
    LIBEXECDIR=%{_libexecdir} UNITDIR=%{_unitdir} \
    SELINUXDIR=%{_datadir}/selinux/packages/%{selinuxtype}

%check
export GOFLAGS="-mod=vendor"
# The tukit client's tests start a private dbus-daemon, and skip without.
go test ./...

%pre gateway
%service_add_pre mcp-tukit-reboot.service mcp-tukit-apply.service

%post gateway
%service_add_post mcp-tukit-reboot.service mcp-tukit-apply.service

%preun gateway
%service_del_preun mcp-tukit-reboot.service mcp-tukit-apply.service

%postun gateway
# Not restarted on update: they would reboot or apply.
%service_del_postun_without_restart mcp-tukit-reboot.service mcp-tukit-apply.service

%posttrans gateway
# 0.1 shipped the definition as tukit-privileged.yaml, 0.2 kept that name
# as a link to tukit.yaml, and 0.3 no longer has it. A link the
# administrator made to it would dangle, and the gateway would refuse
# its configuration: point it at tukit.yaml, the same definition.
old=%{_datadir}/mcp-gateway/profiles/tukit-privileged.yaml
for f in %{_sysconfdir}/mcp-gateway/servers.d/*.yaml; do
    if [ -L "$f" ] && [ "$(readlink "$f")" = "$old" ]; then
        ln -sfn %{_datadir}/mcp-gateway/profiles/tukit.yaml "$f"
        echo "mcp-server-tukit-gateway: $f now links to tukit.yaml ($old is gone)"
    fi
done
:

%pre selinux
%selinux_relabel_pre -s %{selinuxtype}

%post selinux
%selinux_modules_install -s %{selinuxtype} %{_datadir}/selinux/packages/%{selinuxtype}/%{modulename}.pp.bz2

%postun selinux
if [ $1 -eq 0 ]; then
    %selinux_modules_uninstall -s %{selinuxtype} %{modulename}
fi

%posttrans selinux
%selinux_relabel_post -s %{selinuxtype}

%files
%license LICENSE
%doc README.md
%{_bindir}/mcp-server-tukit

%files gateway
# The gateway owns these directories too; this package does not require
# it at build time.
%dir %{_datadir}/mcp-gateway
%dir %{_datadir}/mcp-gateway/profiles
%dir %{_datadir}/mcp-gateway/policy
%dir %{_datadir}/mcp-gateway/policy/mcp
%dir %{_datadir}/mcp-gateway/policy/mcp/profiles
%{_datadir}/mcp-gateway/profiles/tukit.yaml
%{_datadir}/mcp-gateway/policy/mcp/profiles/tukit
%{_unitdir}/mcp-tukit-reboot.service
%{_unitdir}/mcp-tukit-apply.service

%files selinux
%dir %{_datadir}/selinux/packages/%{selinuxtype}
%{_datadir}/selinux/packages/%{selinuxtype}/%{modulename}.pp.bz2

%files tests
%dir %{_libexecdir}/mcp-server-tukit
%dir %{_libexecdir}/mcp-server-tukit/tests
%{_libexecdir}/mcp-server-tukit/tests/run-tests.sh
%{_libexecdir}/mcp-server-tukit/tests/tukitcall

%changelog
