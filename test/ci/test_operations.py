from __future__ import annotations

import json
import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


def run_health_check(
    health: object,
    metrics: object,
    expected_http: str,
    expected_status: str,
    expected_alert: str = "",
) -> subprocess.CompletedProcess[str]:
    with tempfile.TemporaryDirectory() as directory_name:
        directory = Path(directory_name)
        binary_directory = directory / "bin"
        binary_directory.mkdir()
        health_path = directory / "health.json"
        metrics_path = directory / "metrics.json"
        health_path.write_text(
            health if isinstance(health, str) else json.dumps(health),
            encoding="utf-8",
        )
        metrics_path.write_text(
            metrics if isinstance(metrics, str) else json.dumps(metrics),
            encoding="utf-8",
        )
        curl = binary_directory / "curl"
        curl.write_text(
            """#!/usr/bin/env bash
set -euo pipefail
output=''
url=''
while (($#)); do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --max-time|--unix-socket|--write-out) shift 2 ;;
    --silent|--show-error) shift ;;
    http://*) url=$1; shift ;;
    *) exit 90 ;;
  esac
done
case "$url" in
  http://localhost/healthz) cp "$HEALTH_FIXTURE" "$output"; printf '%s' "$HEALTH_HTTP" ;;
  http://localhost/metrics) cp "$METRICS_FIXTURE" "$output"; printf '%s' "$METRICS_HTTP" ;;
  *) exit 91 ;;
esac
""",
            encoding="utf-8",
        )
        curl.chmod(0o755)
        sleep = binary_directory / "sleep"
        sleep.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        sleep.chmod(0o755)
        environment = os.environ.copy()
        environment.update(
            {
                "HEALTH_FIXTURE": str(health_path),
                "HEALTH_HTTP": expected_http,
                "METRICS_FIXTURE": str(metrics_path),
                "METRICS_HTTP": "200",
                "PATH": f"{binary_directory}:{environment['PATH']}",
                "TMPDIR": str(directory),
            }
        )
        arguments = [
            "bash",
            str(ROOT / "scripts/check-systemd-health.sh"),
            expected_http,
            expected_status,
        ]
        if expected_alert:
            arguments.append(expected_alert)
        return subprocess.run(
            arguments,
            text=True,
            capture_output=True,
            check=False,
            env=environment,
        )


class OperationsPolicyTests(unittest.TestCase):
    def test_systemd_unit_preserves_privilege_and_fence_boundaries(self) -> None:
        unit = (ROOT / "packaging/systemd/guard-daemon.service").read_text(encoding="utf-8")
        required = (
            "Type=simple",
            "User=guard-daemon",
            "Group=guard-daemon",
            "NoNewPrivileges=yes",
            "CapabilityBoundingSet=\n",
            "AmbientCapabilities=\n",
            "ProtectSystem=strict",
            "ProtectHome=yes",
            "PrivateDevices=yes",
            "PrivateTmp=no",
            "StateDirectory=guard-daemon",
            "StateDirectoryMode=0700",
            "ReadOnlyPaths=/var/lib/guard-daemon-operator",
            "InaccessiblePaths=/var/lib/guard-daemon-operator/deployment-recovery "
            "/var/lib/guard-daemon-operator/deployment-manifests",
            "ReadWritePaths=/var/lib/guard-daemon /var/tmp/guard-daemon-leases-v1",
            "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
            "TimeoutStopSec=120s",
        )
        for value in required:
            with self.subTest(value=value):
                self.assertIn(value, unit)
        for forbidden in (
            "User=root",
            "DynamicUser=yes",
            "PrivateTmp=yes",
            "Type=notify",
            "ExecReload=",
            "IPAddressDeny=",
        ):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, unit)
        read_write_paths = next(line for line in unit.splitlines() if line.startswith("ReadWritePaths="))
        self.assertNotIn("/var/lib/guard-daemon-operator", read_write_paths)

    def test_systemd_configuration_credentials_and_state_are_fixed(self) -> None:
        unit = (ROOT / "packaging/systemd/guard-daemon.service").read_text(encoding="utf-8")
        public_config = "Environment=GUARD_DAEMON_CONFIG=/etc/guard-daemon/guard-daemon.env"
        state_env = "EnvironmentFile=/usr/lib/guard-daemon/guard-daemon.state.env"
        source_credential = (
            "LoadCredential=source-private-key:/etc/guard-daemon/credentials/source-private-key"
        )
        sponsor_credential = (
            "LoadCredential=sponsor-private-key:/etc/guard-daemon/credentials/sponsor-private-key"
        )
        configuration_directives = [
            line
            for line in unit.splitlines()
            if line.startswith(("Environment", "LoadCredential"))
        ]
        self.assertEqual(
            configuration_directives,
            [public_config, state_env, source_credential, sponsor_credential],
        )
        self.assertNotIn("EnvironmentFile=/etc/guard-daemon/guard-daemon.env", unit)
        self.assertEqual(
            (ROOT / "packaging/systemd/guard-daemon.state.env").read_text(encoding="utf-8"),
            "STATE_DIRECTORY=/var/lib/guard-daemon\n",
        )
        runbook = (ROOT / "docs/operations/runbook.md").read_text(encoding="utf-8")
        self.assertIn(
            'install -D -o root -g root -m 0644 "$TOOLING_TARGET/packaging/systemd/'
            'guard-daemon.state.env" '
            "/usr/lib/guard-daemon/guard-daemon.state.env",
            runbook,
        )

    def test_systemd_paths_have_restrictive_modes(self) -> None:
        tmpfiles = (ROOT / "packaging/systemd/guard-daemon.tmpfiles").read_text(encoding="utf-8")
        self.assertIn("d /var/lib/guard-daemon 0700 guard-daemon guard-daemon -", tmpfiles)
        self.assertIn("d /var/lib/guard-daemon-operator 0755 root root -", tmpfiles)
        self.assertIn(
            "d /var/lib/guard-daemon-operator/deployment-recovery 0700 root root -", tmpfiles
        )
        self.assertIn(
            "d /var/lib/guard-daemon-operator/deployment-manifests 0700 root root -", tmpfiles
        )
        self.assertNotIn("d /var/lib/guard-daemon/deployment-recovery", tmpfiles)
        self.assertIn("d /var/tmp/guard-daemon-leases-v1 0700 guard-daemon guard-daemon -", tmpfiles)
        self.assertIn("f /etc/guard-daemon/guard-daemon.env 0640 root guard-daemon -", tmpfiles)
        self.assertIn("d /etc/guard-daemon/credentials 0700 root root -", tmpfiles)
        self.assertIn(
            "f /etc/guard-daemon/credentials/source-private-key 0600 root root -", tmpfiles
        )
        self.assertIn(
            "f /etc/guard-daemon/credentials/sponsor-private-key 0600 root root -", tmpfiles
        )
        self.assertNotIn("live-disabled-after-restore", tmpfiles)

    def test_systemd_health_gate_validates_complete_snapshots(self) -> None:
        healthy_network = {
            "status": "healthy_idle",
            "rpc_degraded": False,
            "budget_blocked": False,
            "ambiguous_rescue": False,
            "stopped": False,
        }
        stopped_network = {**healthy_network, "status": "stopped", "stopped": True}
        healthy = {"status": "healthy_idle", "networks": {"1": healthy_network}}
        healthy_metrics = {"networks": {"1": {}}, "alerts": [], "active_alerts": 0}
        stopped = {"status": "stopped", "networks": {"1": stopped_network}}
        stopped_metrics = {
            "networks": {"1": {}},
            "alerts": [{"chain_id": "1", "code": "paid_actions_stopped"}],
            "active_alerts": 1,
        }
        cases = (
            ("healthy", healthy, healthy_metrics, "200", "healthy_idle", "", True),
            (
                "stopped",
                stopped,
                stopped_metrics,
                "503",
                "stopped",
                "paid_actions_stopped",
                True,
            ),
            ("malformed health", "{", healthy_metrics, "200", "healthy_idle", "", False),
            (
                "different network sets",
                healthy,
                {**healthy_metrics, "networks": {"2": {}}},
                "200",
                "healthy_idle",
                "",
                False,
            ),
            (
                "missing stopped alert",
                stopped,
                {"networks": {"1": {}}, "alerts": [], "active_alerts": 0},
                "503",
                "stopped",
                "paid_actions_stopped",
                False,
            ),
            (
                "numeric alert chain",
                stopped,
                {
                    "networks": {"1": {}},
                    "alerts": [{"chain_id": 1, "code": "paid_actions_stopped"}],
                    "active_alerts": 1,
                },
                "503",
                "stopped",
                "paid_actions_stopped",
                False,
            ),
            (
                "duplicate stopped alert",
                stopped,
                {
                    "networks": {"1": {}},
                    "alerts": [
                        {"chain_id": "1", "code": "paid_actions_stopped"},
                        {"chain_id": "1", "code": "paid_actions_stopped"},
                    ],
                    "active_alerts": 2,
                },
                "503",
                "stopped",
                "paid_actions_stopped",
                False,
            ),
        )
        for name, health, metrics, http, status, alert, accepted in cases:
            with self.subTest(name=name):
                result = run_health_check(health, metrics, http, status, alert)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)

    def test_runbooks_keep_emergency_and_backup_fail_closed(self) -> None:
        runbook = (ROOT / "docs/operations/runbook.md").read_text(encoding="utf-8")
        backup = (ROOT / "docs/operations/backup-restore.md").read_text(encoding="utf-8")
        for statement in (
            "сначала постоянно запретить автозапуск, затем остановить",
            "DRY_RUN=false",
            "EMERGENCY_STOP=true",
            "503/stopped",
        ):
            self.assertIn(statement, runbook)
        self.assertIn("Восстановленное состояние в этом выпуске никогда не возвращается в signing", backup)
        self.assertIn("Устаревший снимок запрещено извлекать", backup)
        self.assertIn("снимок устарел", runbook)
        self.assertIn("запрещено извлекать в", runbook)
        self.assertIn("подписанной транзакции", backup)
        self.assertIn("/var/lib/guard-daemon-operator/live-disabled-after-restore", backup)
        self.assertNotIn("/var/lib/guard-daemon/.live-disabled-after-restore", backup)
        self.assertIn("root:root 755", backup)
        self.assertIn("root:root 700", backup)
        self.assertIn("root:root 444", backup)
        self.assertNotIn("вернуть ключи", backup)
        self.assertIn("Процедуры удаления", backup)
        self.assertIn("marker в этом выпуске нет", backup)
        self.assertNotIn("RPC_BROADCAST_HTTP_[0-9]+", runbook)
        self.assertNotIn("RPC_BROADCAST_HTTP_[0-9]+", backup)

    def test_release_install_is_staged_and_never_overwrites_target(self) -> None:
        runbook = (ROOT / "docs/operations/runbook.md").read_text(encoding="utf-8")
        install = runbook[runbook.index("## Установка") : runbook.index("## Предварительная проверка")]

        candidate_staging = install.index('CANDIDATE_STAGING="$(mktemp -d')
        trusted_digest = install.index("TRUSTED_SHA256SUMS_SHA256='<")
        cleanup = install.index("trap 'if test -n \"$RELEASE_STAGING\"")
        checksum_authentication = install.index("sha256sum --check --strict -")
        source_extraction = install.index('tar --extract --file="$CANDIDATE_STAGING/guard-daemon-source.tar"')
        verify = install.index('"$TOOLING_STAGING/scripts/release_metadata.py" verify')
        release_id = install.index('RELEASE_ID="$("$PYTHON_BINARY"')
        binary_identity = install.index('CANDIDATE_STAGING/guard-daemon-linux-amd64" --version')
        target_absent = install.index('test ! -e "$RELEASE_TARGET"')
        candidate_publish = install.index('mv -Tn -- "$CANDIDATE_STAGING" "$CANDIDATE_TARGET"')
        tooling_publish = install.index('mv -Tn -- "$TOOLING_STAGING" "$TOOLING_TARGET"')
        staging = install.index('RELEASE_STAGING="$(mktemp -d')
        staged_binary = install.index('"$RELEASE_STAGING/guard-daemon"')
        target_recheck = install.rindex('test ! -e "$RELEASE_TARGET"')
        publish = install.index('mv -Tn -- "$RELEASE_STAGING" "$RELEASE_TARGET"')
        current = install.index('ln -s "releases/$RELEASE_ID"')

        self.assertLess(trusted_digest, candidate_staging)
        self.assertLess(candidate_staging, cleanup)
        self.assertLess(cleanup, checksum_authentication)
        self.assertLess(checksum_authentication, source_extraction)
        self.assertLess(source_extraction, verify)
        self.assertLess(verify, release_id)
        self.assertLess(release_id, binary_identity)
        self.assertLess(binary_identity, target_absent)
        self.assertLess(target_absent, candidate_publish)
        self.assertLess(candidate_publish, tooling_publish)
        self.assertLess(tooling_publish, staging)
        self.assertLess(staging, staged_binary)
        self.assertLess(staged_binary, target_recheck)
        self.assertLess(target_recheck, publish)
        self.assertLess(publish, current)
        self.assertIn('test "${#RELEASE_ID}" -eq 40', install)
        self.assertIn('case "$RELEASE_ID" in *[!0-9a-f]*) exit 1 ;; esac', install)
        self.assertGreater(install.count('test ! -e "$RELEASE_TARGET"'), 1)
        self.assertNotIn('install -d -o root -g root -m 0755 "$RELEASE_TARGET"', install)
        self.assertNotIn('mv -Tf -- "$RELEASE_STAGING" "$RELEASE_TARGET"', install)
        self.assertNotIn('$RELEASE_SOURCE/guard-daemon-linux-amd64" --version', install)
        self.assertNotIn("git clone", install)
        self.assertNotIn("python3 -I -B scripts/release_metadata.py verify", install)
        self.assertIn('"$TOOLING_TARGET/packaging/systemd/guard-daemon.service"', install)
        self.assertIn('case "$ACTIVATE_RELEASE" in true|false)', install)
        self.assertIn('if test "$ACTIVATE_RELEASE" = true; then', install)

    def test_executable_operations_blocks_enable_strict_mode(self) -> None:
        for relative_path in (
            "packaging/systemd/README.md",
            "docs/deployment/operator-activation.md",
            "docs/operations/runbook.md",
            "docs/operations/backup-restore.md",
        ):
            document = (ROOT / relative_path).read_text(encoding="utf-8")
            blocks = re.findall(r"```bash\n(.*?)\n```", document, flags=re.DOTALL)
            self.assertTrue(blocks, relative_path)
            for block in blocks:
                with self.subTest(path=relative_path, command=block.splitlines()[-1]):
                    self.assertEqual(block.splitlines()[0], "set -euo pipefail")
                    syntax = subprocess.run(
                        ["bash", "-n"],
                        input=block,
                        text=True,
                        capture_output=True,
                        check=False,
                    )
                    self.assertEqual(syntax.returncode, 0, syntax.stderr)
                    if "systemctl start guard-daemon.service" in block:
                        self.assertIn("test ", block[: block.index("systemctl start guard-daemon.service")])

    def test_critical_operations_are_ordered_fail_closed(self) -> None:
        runbook = (ROOT / "docs/operations/runbook.md").read_text(encoding="utf-8")
        backup = (ROOT / "docs/operations/backup-restore.md").read_text(encoding="utf-8")

        for heading, next_heading in (
            ("## Аварийная остановка платных действий", "## Обновление"),
            ("## Обновление", "## Откат"),
            ("## Откат", "## Статус проверки процедуры"),
        ):
            section = runbook[runbook.index(heading) : runbook.index(next_heading)]
            stop = section.index("systemctl stop guard-daemon.service")
            stopped_pid = section.index("--property=MainPID --value")
            edit = section.index("sudoedit /etc/guard-daemon/guard-daemon.env")
            start = section.index("systemctl start guard-daemon.service")
            with self.subTest(heading=heading):
                self.assertLess(section.index("systemctl mask guard-daemon.service"), stop)
                self.assertLess(stop, stopped_pid)
                self.assertLess(stopped_pid, edit)
                self.assertLess(edit, start)
                self.assertLess(start, section.index("HEALTH_CHECK=/usr/lib/guard-daemon/check-systemd-health.sh"))
                if heading in ("## Обновление", "## Откат"):
                    self.assertLess(section.index("readlink -f"), start)
                if heading == "## Обновление":
                    self.assertIn("ACTIVATE_RELEASE=false", section)
                    self.assertLess(
                        section.index("packaging/systemd/guard-daemon.service"),
                        section.index("mv -Tf /usr/lib/guard-daemon/current.new"),
                    )

        snapshot = backup[backup.index("## Создание согласованного снимка") : backup.index("## Восстановление")]
        snapshot_commands_match = re.search(r"```bash\n(.*?)\n```", snapshot, flags=re.DOTALL)
        self.assertIsNotNone(snapshot_commands_match)
        snapshot_commands = snapshot_commands_match.group(1)
        self.assertIn("--directory=/var/lib --exclude='guard-daemon/diagnostics.sock' guard-daemon", snapshot_commands)
        self.assertNotIn("guard-daemon-operator", snapshot_commands)
        self.assertLess(snapshot.index("systemctl mask guard-daemon.service"), snapshot.index("systemctl stop"))
        self.assertLess(snapshot.index("systemctl stop guard-daemon.service"), snapshot.index("--property=MainPID --value"))
        self.assertLess(snapshot.index("--property=MainPID --value"), snapshot.index("tar --create"))

        restore = backup[backup.index("## Восстановление") :]
        stop = restore.index("systemctl stop guard-daemon.service")
        stopped_pid = restore.index("--property=MainPID --value")
        edit = restore.index("sudoedit /etc/guard-daemon/guard-daemon.env")
        checksum = restore.index("sha256sum --check")
        archive_list = restore.index("tar --list")
        marker = restore.index(
            'RESTORE_MARKER_TMP="$(mktemp "$OPERATOR_DIRECTORY/.live-disabled-after-restore.tmp.XXXXXX")"'
        )
        file_fsync = restore.index("os.fsync(descriptor)", marker)
        publish_marker = restore.index('mv -T -- "$RESTORE_MARKER_TMP" "$RESTORE_MARKER"')
        directory_fsync = restore.index("os.fsync(descriptor)", publish_marker)
        quarantine = restore.index("mv -T -- /var/lib/guard-daemon")
        extract = restore.index("tar --extract")
        start = restore.index("systemctl start guard-daemon.service")
        self.assertLess(restore.index("systemctl mask guard-daemon.service"), stop)
        self.assertLess(stop, stopped_pid)
        self.assertLess(stopped_pid, edit)
        self.assertLess(restore.index("TRUSTED_ARCHIVE_SHA256='<"), checksum)
        self.assertLess(edit, checksum)
        self.assertLess(checksum, archive_list)
        self.assertLess(archive_list, marker)
        self.assertLess(marker, file_fsync)
        self.assertLess(file_fsync, publish_marker)
        self.assertLess(publish_marker, directory_fsync)
        self.assertLess(directory_fsync, quarantine)
        self.assertLess(quarantine, extract)
        self.assertLess(extract, start)
        health_check = restore.index("HEALTH_CHECK=/usr/lib/guard-daemon/check-systemd-health.sh")
        self.assertLess(start, health_check)
        self.assertLess(health_check, restore.index("systemctl enable"))
        self.assertNotIn("systemctl start", restore[:extract])

    def test_systemd_ci_uses_package_env_and_release_layout_without_starting(self) -> None:
        workflow = (ROOT / ".github/workflows/security.yml").read_text(encoding="utf-8")
        step = workflow[
            workflow.index("- name: Статически проверить unit systemd") : workflow.index("  reproducible-artifacts:")
        ]
        state_install = (
            "sudo install -o root -g root -m 0644 packaging/systemd/guard-daemon.state.env "
            "/usr/lib/guard-daemon/guard-daemon.state.env"
        )
        release_binary = "/usr/lib/guard-daemon/releases/ci-placeholder/guard-daemon"
        current_link = "sudo ln -s releases/ci-placeholder /usr/lib/guard-daemon/current"
        sysusers = "sudo systemd-sysusers packaging/systemd/guard-daemon.sysusers"
        account_check = "scripts/verify-systemd-account.sh"
        tmpfiles = "sudo systemd-tmpfiles --create packaging/systemd/guard-daemon.tmpfiles"
        self.assertIn(state_install, step)
        self.assertIn(release_binary, step)
        self.assertIn(current_link, step)
        self.assertIn(sysusers, step)
        self.assertIn(account_check, step)
        self.assertIn(tmpfiles, step)
        self.assertLess(step.index(release_binary), step.index(current_link))
        self.assertLess(step.index(sysusers), step.index(account_check))
        self.assertLess(step.index(account_check), step.index(tmpfiles))
        self.assertLess(step.index(state_install), step.index("systemd-analyze verify"))
        self.assertNotIn("useradd", step)
        self.assertNotIn("systemctl start", step)

    def test_release_documents_match_generated_names(self) -> None:
        artifacts = (ROOT / "docs/release/artifacts.md").read_text(encoding="utf-8")
        for name in (
            "guard-daemon-linux-amd64",
            "guard-daemon-source.tar",
            "guard-daemon.cdx.json",
            "guard-daemon.intoto.jsonl",
            "release-candidate.json",
            "rescuer-manifest.schema.json",
            "SHA256SUMS",
        ):
            with self.subTest(name=name):
                self.assertIn(f"`{name}`", artifacts)

    def test_system_account_database_checks_fail_closed(self) -> None:
        script = ROOT / "scripts/verify-systemd-account.sh"
        valid = subprocess.run(
            [
                "bash",
                "-c",
                f"set -euo pipefail; source {script!s}; "
                "validate_passwd_entries 'guard-daemon:x:500:500::/var/lib/guard-daemon:/usr/sbin/nologin' 500 500; "
                "validate_group_entries 'guard-daemon:x:500:' 500",
            ],
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(valid.returncode, 0, valid.stderr)
        cases = (
            (
                "partial passwd enumeration",
                'getent() { printf "%s\\n" "guard-daemon:x:500:500::/var/lib/guard-daemon:/usr/sbin/nologin"; return 42; }; read_identity_database passwd',
            ),
            (
                "duplicate passwd name",
                'validate_passwd_entries $\'guard-daemon:x:500:500::/var/lib/guard-daemon:/usr/sbin/nologin\\nguard-daemon:x:501:501::/var/lib/guard-daemon:/usr/sbin/nologin\' 500 500',
            ),
            (
                "duplicate uid",
                'validate_passwd_entries $\'guard-daemon:x:500:500::/var/lib/guard-daemon:/usr/sbin/nologin\\nother:x:500:600::/nonexistent:/usr/sbin/nologin\' 500 500',
            ),
            (
                "partial group enumeration",
                'getent() { printf "%s\\n" "guard-daemon:x:500:"; return 42; }; read_identity_database group',
            ),
            (
                "duplicate gid",
                "validate_group_entries $'guard-daemon:x:500:\\nother:x:500:' 500",
            ),
        )
        for name, command in cases:
            with self.subTest(name=name):
                result = subprocess.run(
                    [
                        "bash",
                        "-c",
                        f"set -euo pipefail; source {script!s}; {command}",
                    ],
                    text=True,
                    capture_output=True,
                    check=False,
                )
                self.assertNotEqual(result.returncode, 0, result.stdout)


if __name__ == "__main__":
    unittest.main()
