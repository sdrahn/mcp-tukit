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
Version:        0.1.0
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
# mcp_gateway_backend_template, from the gateway's interface file.
BuildRequires:  mcp-gateway-selinux
BuildRequires:  selinux-policy-devel
BuildRequires:  sysuser-tools
# The server only talks to tukitd; tukitd does the work.
Requires:       tukitd

%description
An MCP server (stdio) for immutable SUSE systems (openSUSE MicroOS, Leap
Micro, SL Micro, SLES 16.1 in immutable mode): installs, removes and
updates packages in new snapshots as transactional-update does, lists
snapshots, rolls back and cleans up, through tukitd on the system bus.
It holds no privileges of its own and is meant to run behind mcp-gateway
(package mcp-server-tukit-gateway).

%package gateway
Summary:        mcp-server-tukit behind mcp-gateway
Group:          System/Management
BuildArch:      noarch
Requires:       %{name} = %{version}
Requires:       mcp-gateway
Requires:       (%{name}-selinux if selinux-policy-%{selinuxtype})
%sysusers_requires

%description gateway
Sets up mcp-server-tukit behind mcp-gateway: the server definition
(server tukit, in the gateway's sandbox as the account mcp-tukit, SELinux
domain mcpsrv_tukit_t), the roles tukit-reader and tukit-operator (every
change approved), and the D-Bus policy that lets mcp-tukit call tukitd.

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
%sysusers_generate_pre dist/sysusers.d/mcp-tukit.conf %{name}-gateway mcp-tukit.conf

%install
export GOFLAGS="-mod=vendor"
%make_install install install-tests install-selinux VERSION=%{version} PREFIX=%{_prefix} \
    LIBEXECDIR=%{_libexecdir} SYSUSERSDIR=%{_sysusersdir} \
    SELINUXDIR=%{_datadir}/selinux/packages/%{selinuxtype}

%check
export GOFLAGS="-mod=vendor"
# The tukit client's tests start a private dbus-daemon, and skip without.
go test ./...

%pre gateway -f %{name}-gateway.pre

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
%dir %{_datadir}/mcp-gateway/servers.d
%dir %{_datadir}/mcp-gateway/policy
%dir %{_datadir}/mcp-gateway/policy/mcp
%dir %{_datadir}/mcp-gateway/policy/mcp/profiles
%{_datadir}/mcp-gateway/servers.d/tukit.yaml
%{_datadir}/mcp-gateway/policy/mcp/profiles/tukit
%dir %{_datadir}/dbus-1
%dir %{_datadir}/dbus-1/system.d
%{_datadir}/dbus-1/system.d/mcp-tukit.conf
%{_sysusersdir}/mcp-tukit.conf

%files selinux
%dir %{_datadir}/selinux/packages/%{selinuxtype}
%{_datadir}/selinux/packages/%{selinuxtype}/%{modulename}.pp.bz2

%files tests
%dir %{_libexecdir}/mcp-server-tukit
%dir %{_libexecdir}/mcp-server-tukit/tests
%{_libexecdir}/mcp-server-tukit/tests/run-tests.sh
%{_libexecdir}/mcp-server-tukit/tests/tukitcall

%changelog
