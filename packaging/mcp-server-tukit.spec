#
# spec file for package mcp-server-tukit
#
# Built on the Open Build Service for openSUSE Tumbleweed, Leap Micro, SL
# Micro and SLES 16. Sources: the tarball and vendor.tar.gz (Go modules,
# OBS builds are offline) come from the _service file.
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
Group:          System/Management
BuildArch:      noarch
Requires:       mcp-gateway-selinux
Requires:       selinux-policy-%{selinuxtype}
Requires(post): selinux-policy-%{selinuxtype}
%{?_selinux_policy_version:%global _selinux_policy_version %(echo '%{_selinux_policy_version}' | cut -d- -f1)}
%{?selinux_requires}

%description selinux
The SELinux domain mcpsrv_tukit_t for mcp-server-tukit behind
mcp-gateway: the gateway's backend baseline and the D-Bus chat with
tukitd, nothing else.

%prep
%autosetup -p1 -a1

%build
export GOFLAGS="-mod=vendor"
%make_build build VERSION=%{version}
make selinux
%sysusers_generate_pre dist/sysusers.d/mcp-tukit.conf %{name}-gateway mcp-tukit.conf

%install
%make_install install install-selinux VERSION=%{version} PREFIX=%{_prefix} SYSUSERSDIR=%{_sysusersdir} \
    SELINUXDIR=%{_datadir}/selinux/packages/%{selinuxtype}

%check
export GOFLAGS="-mod=vendor"
go test ./internal/server/... ./cmd/...

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
%{_datadir}/mcp-gateway/servers.d/tukit.yaml
%{_datadir}/mcp-gateway/policy/mcp/profiles/tukit
%{_datadir}/dbus-1/system.d/mcp-tukit.conf
%{_sysusersdir}/mcp-tukit.conf

%files selinux
%{_datadir}/selinux/packages/%{selinuxtype}/%{modulename}.pp.bz2

%changelog
