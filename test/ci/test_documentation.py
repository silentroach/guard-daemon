from __future__ import annotations

import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
LINK = re.compile(r"\[[^]]+\]\(([^)]+)\)")
CYRILLIC = re.compile(r"[А-Яа-яЁё]")


def public_markdown() -> list[Path]:
    paths = [ROOT / "README.md"]
    paths.extend((ROOT / "docs").rglob("*.md"))
    paths.extend((ROOT / "packaging").rglob("*.md"))
    return sorted(paths)


class DocumentationTests(unittest.TestCase):
    def test_relative_links_exist(self) -> None:
        for path in public_markdown():
            text = path.read_text(encoding="utf-8")
            for target in LINK.findall(text):
                with self.subTest(path=path.relative_to(ROOT), target=target):
                    if target.startswith(("https://", "http://", "mailto:", "#")):
                        continue
                    clean_target = target.split("#", 1)[0]
                    self.assertTrue(clean_target, "пустая относительная ссылка")
                    self.assertTrue((path.parent / clean_target).resolve().exists())

    def test_public_headings_are_russian(self) -> None:
        for path in public_markdown():
            first_line = path.read_text(encoding="utf-8").splitlines()[0]
            with self.subTest(path=path.relative_to(ROOT)):
                self.assertTrue(first_line.startswith("# "))
                self.assertRegex(first_line, CYRILLIC)

    def test_legacy_and_duplicate_help_do_not_return(self) -> None:
        self.assertFalse((ROOT / "guard-daemon-HELP_RU.txt").exists())
        active_text = "\n".join(
            path.read_text(encoding="utf-8")
            for path in public_markdown()
            if "plans" not in path.relative_to(ROOT).parts
        )
        for forbidden in (
            "renewDelegation",
            "renewAndSweep",
            "Either both succeed or both fail",
            "No window for bot interception",
            "tokens remain safe on source",
        ):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden.casefold(), active_text.casefold())

    def test_quick_start_cannot_enable_live_actions(self) -> None:
        readme = (ROOT / "README.md").read_text(encoding="utf-8")
        quick_start = readme.split("## Безопасная быстрая проверка", 1)[1].split("\n## ", 1)[0]
        for forbidden in (
            "DRY_RUN=false",
            "--broadcast",
            "SOURCE_PRIVATE_KEY",
            "SPONSOR_PRIVATE_KEY",
            "systemctl start",
            "deployRescuerV2",
        ):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, quick_start)

    def test_documented_commands_use_installed_node_tools(self) -> None:
        for path in public_markdown():
            if "plans" in path.relative_to(ROOT).parts:
                continue
            with self.subTest(path=path.relative_to(ROOT)):
                self.assertNotIn("npx ", path.read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()
