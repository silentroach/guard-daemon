#!/usr/bin/env python3

import json
import os
import re
import sys
from typing import Any


UNKNOWN_LICENSES = {
    "licenseRef-clearlydefined-other",
    "noassertion",
    "none",
    "other",
    "unknown",
}


def validate_dependency_changes(changes: Any) -> list[str]:
    if not isinstance(changes, list):
        return ["dependency-changes должен быть JSON-массивом"]

    errors: list[str] = []
    for index, change in enumerate(changes):
        if not isinstance(change, dict):
            errors.append(f"dependency-changes[{index}] должен быть объектом")
            continue
        change_type = change.get("change_type")
        if change_type not in {"added", "removed"}:
            errors.append(
                f"dependency-changes[{index}] содержит неизвестный тип изменения"
            )
            continue
        if change_type != "added":
            continue
        license_name = change.get("license")
        if not isinstance(license_name, str) or not license_name.strip():
            errors.append(f"у добавленной зависимости #{index} отсутствует лицензия")
            continue
        normalized = license_name.strip().casefold()
        if normalized in UNKNOWN_LICENSES or "licenseref-clearlydefined-other" in normalized:
            errors.append(
                f"у добавленной зависимости #{index} не определена SPDX-лицензия"
            )
    return errors


def main() -> int:
    raw_changes = os.environ.get("DEPENDENCY_CHANGES")
    if raw_changes is None:
        print("Не задан обязательный вывод dependency-changes.", file=sys.stderr)
        return 1
    try:
        changes = json.loads(raw_changes)
    except json.JSONDecodeError as error:
        print(f"Некорректный JSON dependency-changes: {error}", file=sys.stderr)
        return 1

    errors = validate_dependency_changes(changes)
    for error in errors:
        print(f"Нарушение политики лицензий: {error}", file=sys.stderr)
    if errors:
        return 1
    print("Все добавленные зависимости имеют определённые SPDX-лицензии.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
