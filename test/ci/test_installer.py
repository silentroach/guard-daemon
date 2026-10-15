from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
INSTALLER = ROOT / "scripts/install-systemd.sh"


def installer_function(name: str) -> str:
    source = INSTALLER.read_text(encoding="utf-8")
    match = re.search(rf"^{name}\(\) \{{\n.*?^\}}", source, re.MULTILINE | re.DOTALL)
    if match is None:
        raise AssertionError(f"missing installer function: {name}")
    return match.group()


class InstallerTests(unittest.TestCase):
    def test_caller_path_cannot_supply_privileged_utilities(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            marker = directory / "executed"
            for name in ("id", "uname", "flock", "install", "systemctl"):
                executable = directory / name
                executable.write_text(f"#!/bin/sh\ntouch '{marker}'\nexit 1\n", encoding="utf-8")
                executable.chmod(0o755)
            result = subprocess.run(
                ["/bin/bash", str(INSTALLER), str(directory), "a" * 40],
                env={**os.environ, "PATH": str(directory)},
                capture_output=True,
                check=False,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(marker.exists())

    def test_cleanup_removes_partial_install_but_not_operator_data(self) -> None:
        for committed in (0, 1):
            with self.subTest(committed=committed), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                release = directory / "release"
                release.mkdir()
                (release / "binary").touch()
                staging = directory / "staging"
                staging.mkdir()
                unit = directory / "unit"
                unit.touch()
                profile = directory / "drop-in/profile"
                profile.parent.mkdir()
                profile.touch()
                data = directory / "operator-data"
                data.write_text("preserve", encoding="utf-8")
                script = installer_function("cleanup") + "\n" + """
staging=$1 release=$2 profile=$3
installed_paths=("$3" "$4")
release_created=1 committed=$5
systemctl() { return 0; }
trap cleanup EXIT
exit 7
"""
                result = subprocess.run(
                    ["/bin/bash", "-c", script, "installer-cleanup", str(staging),
                     str(release), str(profile), str(unit), str(committed)],
                    capture_output=True,
                    check=False,
                )
                self.assertEqual(result.returncode, 7)
                self.assertEqual(release.exists(), bool(committed))
                self.assertEqual(unit.exists(), bool(committed))
                self.assertEqual(profile.exists(), bool(committed))
                self.assertFalse(staging.exists())
                self.assertEqual(data.read_text(encoding="utf-8"), "preserve")

    @unittest.skipUnless(sys.platform == "linux" and os.getuid() == 0, "requires Linux root")
    def test_trusted_path_rejects_writable_ancestors_and_symlinks(self) -> None:
        # /tmp is intentionally untrusted; use a root-controlled parent instead.
        directory = Path(tempfile.mkdtemp(prefix="guard-installer-test-", dir="/root"))
        try:
            child = directory / "tree"
            child.mkdir()
            binary = child / "binary"
            binary.touch()
            script = installer_function("fail") + "\n" + installer_function("trusted_path")
            for mode, symlink, accepted in ((0o755, False, True), (0o777, False, False), (0o755, True, False)):
                with self.subTest(mode=mode, symlink=symlink):
                    child.chmod(mode)
                    path = binary
                    if symlink:
                        path = directory / "link"
                        path.symlink_to(binary)
                    result = subprocess.run(
                        ["/bin/bash", "-c", "set -euo pipefail\n" + script + '\ntrusted_path "$1"',
                         "installer-trust", str(path)],
                        capture_output=True,
                        check=False,
                    )
                    self.assertEqual(result.returncode == 0, accepted)
        finally:
            shutil.rmtree(directory)
