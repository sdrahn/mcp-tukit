# SUSE's openQA tests
#
# Copyright SUSE LLC
# SPDX-License-Identifier: FSFAP
#
# Package: mcp-server-tukit mcp-server-tukit-tests mcp-gateway
# Summary: mcp-server-tukit behind mcp-gateway on a transactional system
#          with SELinux enforcing: install the packages with
#          transactional-update, reboot, phase 1 of the tests (instance
#          domain and account, D-Bus policy, roles and approvals, a
#          package installed into a new snapshot), reboot into it, phase 2
#          (the package active, rollback, deleting and cleaning up
#          snapshots, commands in a snapshot). Each check becomes a result
#          of its own (JUnit).
# Maintainer: mcp-tukit maintainers

use Mojo::Base 'consoletest';
use testapi;
use serial_terminal 'select_serial_terminal';
use transactional qw(trup_call process_reboot);
use utils qw(zypper_ar);

my $tests   = '/usr/libexec/mcp-server-tukit/tests/run-tests.sh';
my $results = '/var/tmp/mcp-tukit-tests';

# run_phase(n): one phase of the tests; dies if they could not run, and
# leaves the failed checks to the JUnit results.
sub run_phase {
    my ($n) = @_;
    my $dir = "$results/phase-$n";
    assert_script_run("mkdir -p $dir");
    my $package = get_var('MCP_TUKIT_PACKAGE') ? ' --package ' . get_var('MCP_TUKIT_PACKAGE') : '';
    # Exit status 0: all passed; 1: a check failed; 2: wrong usage.
    my $rc = script_run("$tests --disposable --phase $n --results $dir$package > $dir/run.log 2>&1", timeout => 1800);
    upload_logs("$dir/run.log");
    die "run-tests.sh phase $n did not run (exit status $rc)" if $rc == 2 || script_run("test -s $dir/results.xml") != 0;
    upload_logs("$dir/results.tap");
    parse_extra_log(XUnit => "$dir/results.xml");
}

sub run {
    select_serial_terminal;
    # The tests need SELinux enforcing, as the products ship it.
    assert_script_run('test "$(getenforce)" = Enforcing');
    # The packages, from the repository the job adds (MCP_TUKIT_REPO) or
    # the product's; the gateway comes as a dependency.
    if (my $repo = get_var('MCP_TUKIT_REPO')) {
        zypper_ar($repo, name => 'mcp-tukit', no_gpg_check => 1);
    }
    trup_call('pkg install mcp-server-tukit-tests');
    process_reboot(trigger => 1);
    select_serial_terminal;
    run_phase(1);
    # Phase 2 runs in the snapshot phase 1 made the default.
    process_reboot(trigger => 1);
    select_serial_terminal;
    run_phase(2);
}

sub post_fail_hook {
    my ($self) = @_;
    select_serial_terminal;
    script_run("mkdir -p $results");
    script_run("journalctl --no-pager -o short-precise -b -u mcp-gateway -u mcp-opa -u tukitd -u 'mcp-*' > $results/journal.txt 2>&1");
    upload_logs("$results/journal.txt", failok => 1);
    script_run("ausearch -m AVC,USER_AVC,SELINUX_ERR -ts boot > $results/avc.txt 2>&1");
    upload_logs("$results/avc.txt", failok => 1);
    script_run("tukit snapshots --fields number,default,active,date,description > $results/snapshots.txt 2>&1");
    upload_logs("$results/snapshots.txt", failok => 1);
    $self->SUPER::post_fail_hook;
}

sub test_flags {
    return {fatal => 0};
}

1;
