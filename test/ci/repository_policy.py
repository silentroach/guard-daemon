#!/usr/bin/env python3

from __future__ import annotations

import argparse
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
from typing import Any, Iterator

import yaml
from yaml.nodes import MappingNode, Node, ScalarNode, SequenceNode


ACTION_SHA = re.compile(r"^[^/@\s]+/[^/@\s]+(?:/[^@\s]+)?@[0-9a-f]{40}$")
CYRILLIC = re.compile(r"[А-Яа-яЁё]")
DEPENDENCY_REVIEW_ACTION = (
    "actions/dependency-review-action@2031cfc080254a8a887f58cffee85186f0e49e48"
)
SETUP_PYTHON_ACTION = "actions/setup-python@5fda3b95a4ea91299a34e894583c3862153e4b97"
ALLOWED_LICENSES = frozenset(
    {
        "0BSD",
        "Apache-2.0",
        "BSD-2-Clause",
        "BSD-3-Clause",
        "BlueOak-1.0.0",
        "CC0-1.0",
        "ISC",
        "MIT",
        "MIT-0",
        "Python-2.0",
        "Unicode-3.0",
        "Unlicense",
        "Zlib",
    }
)
UNSAFE_TRIGGERS = {
    "deployment",
    "deployment_status",
    "pull_request_target",
    "release",
    "repository_dispatch",
    "workflow_call",
    "workflow_run",
}
ALLOWED_TRIGGERS = {"pull_request", "push", "schedule"}
SECRET_EXPRESSION = re.compile(
    r"\$\{\{.*?\bsecrets\s*(?:\.|\[)", re.IGNORECASE | re.DOTALL
)
PRODUCTION_NETWORK = re.compile(
    r"(?:mainnet|alchemy|infura|quicknode|ankr\.com|drpc|--rpc-url|--broadcast)",
    re.IGNORECASE,
)
SENSITIVE_INPUT_VALUE = re.compile(
    r"(?:private[_-]?key|mnemonic|seed[_-]?phrase|deployer[_-]?(?:key|token)|"
    r"credentials?|rpc[_-]?(?:url|http|ws)|production[_-]?rpc|mainnet[_-]?rpc)",
    re.IGNORECASE,
)
AUTO_MERGE = re.compile(
    r"(?:auto[-_ ]?merge|enable-auto-merge|merge[^\n]*--auto)", re.IGNORECASE
)
DEPLOYMENT_INPUT_KEY = re.compile(
    r"(?:privatekey|mnemonic|seedphrase|deployer|credential|"
    r"rpcurl|rpchttp|rpcws|productionrpc|mainnetrpc)",
    re.IGNORECASE,
)
ZERO_SHA = "0" * 40


class PolicyError(ValueError):
    pass


def _convert_node(node: Node, source: str) -> Any:
    if isinstance(node, ScalarNode):
        return node.value
    if isinstance(node, SequenceNode):
        return [_convert_node(item, source) for item in node.value]
    if isinstance(node, MappingNode):
        result: dict[str, Any] = {}
        for key_node, value_node in node.value:
            if not isinstance(key_node, ScalarNode):
                raise PolicyError(f"{source}: ключ YAML mapping должен быть строкой")
            key = key_node.value
            if key in result:
                raise PolicyError(f"{source}: повторяющийся ключ YAML: {key}")
            if key == "<<":
                raise PolicyError(f"{source}: YAML merge keys запрещены")
            result[key] = _convert_node(value_node, source)
        return result
    raise PolicyError(f"{source}: неподдерживаемый YAML node {type(node).__name__}")


def parse_yaml(text: str, source: str) -> dict[str, Any]:
    # BaseLoader сохраняет `on` строкой и не применяет YAML 1.1 boolean coercion.
    documents = list(yaml.compose_all(text, Loader=yaml.BaseLoader))
    if len(documents) != 1 or documents[0] is None:
        raise PolicyError(f"{source}: требуется ровно один непустой YAML document")
    result = _convert_node(documents[0], source)
    if not isinstance(result, dict):
        raise PolicyError(f"{source}: корень YAML должен быть mapping")
    return result


def _walk(value: Any, path: tuple[str, ...] = ()) -> Iterator[tuple[tuple[str, ...], Any]]:
    yield path, value
    if isinstance(value, dict):
        for key, child in value.items():
            yield from _walk(child, (*path, key))
    elif isinstance(value, list):
        for index, child in enumerate(value):
            yield from _walk(child, (*path, str(index)))


def _mapping(value: Any, label: str, issues: list[str]) -> dict[str, Any]:
    if isinstance(value, dict):
        return value
    issues.append(f"{label} должен быть mapping")
    return {}


def _sequence(value: Any, label: str, issues: list[str]) -> list[Any]:
    if isinstance(value, list):
        return value
    issues.append(f"{label} должен быть sequence")
    return []


def _require_russian_name(value: Any, label: str, issues: list[str]) -> None:
    if not isinstance(value, str) or not CYRILLIC.search(value):
        issues.append(f"{label} должен быть русскоязычным")


def _csv(value: Any) -> frozenset[str]:
    if not isinstance(value, str):
        return frozenset()
    return frozenset(item.strip() for item in value.split(",") if item.strip())


def validate_workflow(workflow: dict[str, Any], source: str) -> list[str]:
    issues: list[str] = []
    _require_russian_name(workflow.get("name"), f"{source}: name", issues)

    triggers = _mapping(workflow.get("on"), f"{source}: on", issues)
    trigger_names = set(triggers)
    if not {"pull_request", "push"}.issubset(trigger_names):
        issues.append(f"{source}: обязательны triggers pull_request и push")
    unknown_triggers = trigger_names - ALLOWED_TRIGGERS
    if unknown_triggers or trigger_names & UNSAFE_TRIGGERS:
        issues.append(f"{source}: запрещённые triggers: {sorted(unknown_triggers)}")
    push = _mapping(triggers.get("push"), f"{source}: on.push", issues)
    branches = _sequence(push.get("branches"), f"{source}: on.push.branches", issues)
    if "main" not in branches:
        issues.append(f"{source}: push обязан включать защищённую ветку main")

    if workflow.get("permissions") != {"contents": "read"}:
        issues.append(f"{source}: top-level permissions должны быть ровно contents: read")
    concurrency = _mapping(
        workflow.get("concurrency"), f"{source}: concurrency", issues
    )
    if concurrency.get("cancel-in-progress") != "true" or not concurrency.get("group"):
        issues.append(
            f"{source}: concurrency требует group и cancel-in-progress: true"
        )

    jobs = _mapping(workflow.get("jobs"), f"{source}: jobs", issues)
    action_count = 0
    checkout_count = 0
    for job_id, raw_job in jobs.items():
        job = _mapping(raw_job, f"{source}: jobs.{job_id}", issues)
        _require_russian_name(job.get("name"), f"{source}: jobs.{job_id}.name", issues)
        if "permissions" in job:
            issues.append(f"{source}: jobs.{job_id} не должен переопределять permissions")
        if "environment" in job:
            issues.append(f"{source}: jobs.{job_id} не должен использовать environment")
        reusable = job.get("uses")
        if reusable is not None:
            action_count += 1
            if not isinstance(reusable, str) or not ACTION_SHA.fullmatch(reusable):
                issues.append(
                    f"{source}: jobs.{job_id}.uses требует полный 40-символьный SHA"
                )
        steps = _sequence(job.get("steps"), f"{source}: jobs.{job_id}.steps", issues)
        for index, raw_step in enumerate(steps):
            step = _mapping(
                raw_step, f"{source}: jobs.{job_id}.steps[{index}]", issues
            )
            _require_russian_name(
                step.get("name"),
                f"{source}: jobs.{job_id}.steps[{index}].name",
                issues,
            )
            uses = step.get("uses")
            if uses is None:
                continue
            action_count += 1
            if not isinstance(uses, str) or not ACTION_SHA.fullmatch(uses):
                issues.append(
                    f"{source}: jobs.{job_id}.steps[{index}].uses требует полный 40-символьный SHA"
                )
                continue
            if uses.startswith("actions/checkout@"):
                checkout_count += 1
                options = _mapping(
                    step.get("with"),
                    f"{source}: checkout step with",
                    issues,
                )
                if options.get("persist-credentials") != "false":
                    issues.append(
                        f"{source}: checkout обязан задавать persist-credentials: false"
                    )

    if action_count == 0 or checkout_count == 0:
        issues.append(f"{source}: workflow обязан использовать закреплённый checkout")

    for path, value in _walk(workflow):
        if isinstance(value, dict):
            for key in value:
                normalized_key = re.sub(r"[^a-z0-9]", "", key.casefold())
                if key == "environment":
                    issues.append(f"{source}: environment запрещён в {'.'.join(path)}")
                if normalized_key != "persistcredentials" and DEPLOYMENT_INPUT_KEY.search(
                    normalized_key
                ):
                    issues.append(
                        f"{source}: deployment/RPC input запрещён: {'.'.join((*path, key))}"
                    )
        if not isinstance(value, str):
            continue
        if SECRET_EXPRESSION.search(value):
            issues.append(f"{source}: secrets expression запрещён в {'.'.join(path)}")
        if PRODUCTION_NETWORK.search(value):
            issues.append(f"{source}: RPC рабочей сети или развёртывание запрещены в {'.'.join(path)}")
        if SENSITIVE_INPUT_VALUE.search(value):
            issues.append(f"{source}: credential/deployment/RPC input запрещён в {'.'.join(path)}")
        if AUTO_MERGE.search(value):
            issues.append(f"{source}: auto-merge запрещён в {'.'.join(path)}")
        if value == "true" and path and path[-1] == "continue-on-error":
            issues.append(f"{source}: continue-on-error: true запрещён")
    return issues


def validate_dependency_review(workflow: dict[str, Any], source: str) -> list[str]:
    issues: list[str] = []
    jobs = workflow.get("jobs")
    if not isinstance(jobs, dict) or not isinstance(jobs.get("dependency-review"), dict):
        return [f"{source}: отсутствует job dependency-review"]
    job = jobs["dependency-review"]
    if job.get("if") != "github.event_name == 'pull_request'":
        issues.append(f"{source}: dependency-review разрешён только для pull_request")
    steps = job.get("steps") if isinstance(job.get("steps"), list) else []
    review_steps = [
        step
        for step in steps
        if isinstance(step, dict) and step.get("uses") == DEPENDENCY_REVIEW_ACTION
    ]
    if len(review_steps) != 1:
        return [*issues, f"{source}: требуется ровно один pinned dependency review action"]
    setup = [
        step
        for step in steps
        if isinstance(step, dict) and step.get("uses") == SETUP_PYTHON_ACTION
    ]
    if len(setup) != 1 or setup[0].get("with") != {
        "python-version": "${{ env.PYTHON_VERSION }}"
    }:
        issues.append(f"{source}: dependency review требует pinned setup-python")
    review = review_steps[0]
    if review.get("id") != "dependency-review":
        issues.append(f"{source}: dependency review step требует стабильный id")
    options = review.get("with") if isinstance(review.get("with"), dict) else {}
    expected_options = {
        "allow-licenses",
        "comment-summary-in-pr",
        "fail-on-scopes",
        "fail-on-severity",
        "license-check",
        "vulnerability-check",
        "warn-only",
    }
    if set(options) != expected_options:
        issues.append(f"{source}: dependency review options должны быть fail-closed")
    if options.get("fail-on-severity") != "high":
        issues.append(f"{source}: dependency review должен блокировать High/Critical")
    if _csv(options.get("fail-on-scopes")) != {
        "runtime",
        "development",
        "unknown",
    }:
        issues.append(f"{source}: dependency review должен проверять все scopes")
    if _csv(options.get("allow-licenses")) != ALLOWED_LICENSES:
        issues.append(f"{source}: dependency review permissive allow-list изменён")
    expected_flags = {
        "comment-summary-in-pr": "never",
        "license-check": "true",
        "vulnerability-check": "true",
        "warn-only": "false",
    }
    for key, expected in expected_flags.items():
        if options.get(key) != expected:
            issues.append(f"{source}: dependency review требует {key}: {expected}")

    license_gate = [
        step
        for step in steps
        if isinstance(step, dict)
        and step.get("run") == "python test/ci/dependency_licenses.py"
    ]
    if len(license_gate) != 1 or license_gate[0].get("env") != {
        "DEPENDENCY_CHANGES": "${{ steps.dependency-review.outputs.dependency-changes }}"
    }:
        issues.append(f"{source}: отсутствует fail-closed gate неизвестных лицензий")
    return issues


def validate_repository_policy_job(
    workflow: dict[str, Any], source: str
) -> list[str]:
    jobs = workflow.get("jobs")
    if not isinstance(jobs, dict) or not isinstance(jobs.get("repository-policy"), dict):
        return [f"{source}: отсутствует job repository-policy"]
    steps = jobs["repository-policy"].get("steps")
    if not isinstance(steps, list):
        return [f"{source}: repository-policy.steps должен быть sequence"]
    issues: list[str] = []
    setup = [
        step
        for step in steps
        if isinstance(step, dict) and step.get("uses") == SETUP_PYTHON_ACTION
    ]
    if len(setup) != 1 or setup[0].get("with") != {
        "python-version": "${{ env.PYTHON_VERSION }}"
    }:
        issues.append(f"{source}: repository policy требует pinned setup-python")
    required_commands = {
        "python -m pip install --disable-pip-version-check --only-binary=:all: --require-hashes --requirement test/ci/requirements.txt",
        "python -B -m unittest discover -s test/ci -p 'test_*.py'",
        "bash test/ci/repository-policy.sh",
    }
    commands = {
        step.get("run")
        for step in steps
        if isinstance(step, dict) and isinstance(step.get("run"), str)
    }
    if not required_commands.issubset(commands):
        issues.append(f"{source}: repository policy installation/tests/wrapper неполны")
    return issues


def validate_dependabot(config: dict[str, Any], source: str) -> list[str]:
    issues: list[str] = []
    if config.get("version") != "2" or set(config) != {"version", "updates"}:
        issues.append(f"{source}: требуется Dependabot version 2 без лишних root keys")
    updates = _sequence(config.get("updates"), f"{source}: updates", issues)
    if len(updates) != 3:
        issues.append(f"{source}: требуется ровно три ecosystem")
    seen: set[str] = set()
    for index, raw_update in enumerate(updates):
        update = _mapping(raw_update, f"{source}: updates[{index}]", issues)
        ecosystem = update.get("package-ecosystem")
        if ecosystem not in {"gomod", "npm", "github-actions"} or ecosystem in seen:
            issues.append(f"{source}: ecosystem должен быть уникальным и разрешённым")
        elif isinstance(ecosystem, str):
            seen.add(ecosystem)
        if update.get("directory") != "/":
            issues.append(f"{source}: каждая ecosystem должна проверять root directory")
        schedule = _mapping(update.get("schedule"), f"{source}: schedule", issues)
        if schedule.get("interval") != "weekly":
            issues.append(f"{source}: каждая ecosystem должна запускаться weekly")
        groups = _mapping(update.get("groups"), f"{source}: groups", issues)
        if len(groups) != 1:
            issues.append(f"{source}: minor/patch updates требуют одну группу")
        for group in groups.values():
            group_config = _mapping(group, f"{source}: group", issues)
            if set(group_config.get("update-types", [])) != {"minor", "patch"}:
                issues.append(f"{source}: major updates должны оставаться отдельными")
    for path, value in _walk(config):
        if isinstance(value, str) and AUTO_MERGE.search(value):
            issues.append(f"{source}: auto-merge запрещён в {'.'.join(path)}")
        if isinstance(value, dict):
            for key in value:
                if AUTO_MERGE.search(key):
                    issues.append(f"{source}: auto-merge key запрещён в {'.'.join(path)}")
    return issues


def path_violation(path: str) -> str | None:
    if path == ".env.example" or path == "artifacts/contracts/RescuerV2.json":
        return None
    normalized = path.casefold()
    parts = PurePosixPath(normalized).parts
    basename = parts[-1] if parts else normalized

    if "artifacts" in parts:
        return "разрешён только точный канонический артефакт в корне"
    if any(part == ".env" or part.startswith(".env.") for part in parts):
        return "запрещён файл окружения"
    ssh_key_names = {
        "id_dsa",
        "id_dsa.pub",
        "id_ecdsa",
        "id_ecdsa.pub",
        "id_ed25519",
        "id_ed25519.pub",
        "id_rsa",
        "id_rsa.pub",
    }
    if any(part in ssh_key_names for part in parts):
        return "запрещён файл SSH-ключа"
    if any(part.startswith("utc--") for part in parts):
        return "запрещён файл EVM keystore"
    if any(
        part.endswith(suffix)
        for part in parts
        for suffix in (".key", ".pem", ".p12", ".pfx", ".jks", ".keystore")
    ):
        return "запрещён файл ключа"
    if any(
        part
        in {".aws", ".gnupg", ".ssh", ".terraform", "credentials", "keys", "keystore", "secrets"}
        for part in parts
    ):
        return "запрещён каталог учётных данных или ключей"
    if any(
        part
        in {
            "__pycache__",
            "broadcast",
            "build",
            "cache",
            "coverage",
            "data",
            "dist",
            "out",
            "state",
        }
        for part in parts
    ):
        return "запрещён эксплуатационный или сгенерированный каталог"
    if any(
        parts[index] == "deployments"
        and parts[index + 1] in {"local", "operator", "tmp"}
        for index in range(len(parts) - 1)
    ):
        return "запрещён эксплуатационный каталог развёртывания"
    if re.fullmatch(r".*\.(?:db(?:-.*)?|sqlite|sqlite3|log|pyc)", basename):
        return "запрещён эксплуатационный или сгенерированный файл"
    if basename.startswith(("deployment-manifest.local.", "deployment-manifest.tmp.")):
        return "запрещён локальный манифест развёртывания"
    return None


def _production_surface(path: str) -> bool:
    normalized = path.casefold()
    if normalized in {"package.json", "package-lock.json"}:
        return True
    if normalized.endswith("_test.go"):
        return False
    return normalized.startswith(
        ("cmd/", "internal/", "contracts/", "scripts/", "artifacts/", "deployments/")
    ) and normalized.endswith((".go", ".sol", ".ts", ".json"))


def _git(root: Path, *arguments: str, check: bool = True) -> bytes:
    process = subprocess.run(
        ["git", "-C", str(root), *arguments],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if check and process.returncode != 0:
        message = process.stderr.decode("utf-8", "replace").strip()
        raise PolicyError(f"git {' '.join(arguments)}: {message}")
    return process.stdout


def _paths(output: bytes) -> list[str]:
    return [
        item.decode("utf-8", "surrogateescape")
        for item in output.split(b"\0")
        if item
    ]


def collect_repository_paths(root: Path, base_sha: str | None) -> tuple[list[str], list[str], str]:
    all_paths = _paths(
        _git(root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
    )
    changed: list[str] = []
    if base_sha and base_sha != ZERO_SHA:
        if not re.fullmatch(r"[0-9a-f]{40}", base_sha):
            raise PolicyError("CI_BASE_SHA должен быть полным 40-символьным SHA commit")
        process = subprocess.run(
            ["git", "-C", str(root), "cat-file", "-e", f"{base_sha}^{{commit}}"],
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        if process.returncode != 0:
            raise PolicyError("CI_BASE_SHA задан, но не разрешается в commit")
        changed.extend(_paths(_git(root, "diff", "--name-only", "-z", f"{base_sha}...HEAD")))
        source = f"Базовый commit для сравнения: {base_sha}."
    elif base_sha == ZERO_SHA:
        changed.extend(_paths(_git(root, "ls-files", "-z", "--cached")))
        source = "Первый push: сравнение заменено проверкой полного индекса."
    elif subprocess.run(
        ["git", "-C", str(root), "rev-parse", "--verify", "HEAD^"],
        check=False,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode == 0:
        changed.extend(_paths(_git(root, "diff", "--name-only", "-z", "HEAD^", "HEAD")))
        source = "Локальное сравнение использует резервный base HEAD^."
    else:
        changed.extend(_paths(_git(root, "ls-files", "-z", "--cached")))
        source = "Первый commit: сравнение заменено проверкой полного индекса."
    changed.extend(_paths(_git(root, "diff", "--name-only", "-z")))
    changed.extend(_paths(_git(root, "diff", "--cached", "--name-only", "-z")))
    changed.extend(_paths(_git(root, "ls-files", "-z", "--others", "--exclude-standard")))
    return all_paths, changed, source


def validate_repository(root: Path, base_sha: str | None) -> list[str]:
    issues: list[str] = []
    all_paths, changed_paths, diff_description = collect_repository_paths(root, base_sha)
    print(diff_description)

    for path in all_paths:
        violation = path_violation(path)
        if violation:
            issues.append(f"{path}: {violation}")
        if _production_surface(path):
            file_path = root / path
            if file_path.is_file() and re.search(
                r"permit", file_path.read_text("utf-8", errors="replace"), re.IGNORECASE
            ):
                issues.append(f"{path}: permit identifier вернулся в рабочую поверхность")
    for path in changed_paths:
        file_path = root / path
        if file_path.exists() or file_path.is_symlink():
            violation = path_violation(path)
            if violation:
                issues.append(f"изменённый путь {path}: {violation}")

    workflow_paths = sorted(
        path
        for path in all_paths
        if path.startswith(".github/workflows/") and path.endswith((".yml", ".yaml"))
    )
    if not workflow_paths:
        issues.append("Не найдено ни одного workflow")
    for path in workflow_paths:
        try:
            workflow = parse_yaml((root / path).read_text("utf-8"), path)
        except (OSError, PolicyError, yaml.YAMLError) as error:
            issues.append(str(error))
            continue
        issues.extend(validate_workflow(workflow, path))
        if path == ".github/workflows/security.yml":
            issues.extend(validate_dependency_review(workflow, path))
            issues.extend(validate_repository_policy_job(workflow, path))

    dependabot_path = root / ".github/dependabot.yml"
    try:
        dependabot = parse_yaml(dependabot_path.read_text("utf-8"), str(dependabot_path))
        issues.extend(validate_dependabot(dependabot, ".github/dependabot.yml"))
    except (OSError, PolicyError, yaml.YAMLError) as error:
        issues.append(str(error))

    public_count = 0
    task10_path = root / "docs/plans/2026-07-30-security/tasks/10-operations-release.md"
    task10 = task10_path.read_text("utf-8") if task10_path.is_file() else ""
    for path in all_paths:
        file_path = root / path
        if path.endswith(".md") and file_path.is_file():
            public_count += 1
            text = file_path.read_text("utf-8", errors="replace")
            first_line = text.splitlines()[0] if text.splitlines() else ""
            if path == "README.md" and not CYRILLIC.search(first_line):
                assigned = re.search(r"Статус: `(PENDING|IN PROGRESS)`", task10) and (
                    "Полный перевод и переработка `README.md`" in task10
                )
                if not assigned:
                    issues.append("README.md: исключение допустимо только до Task 10")
                else:
                    print("README.md временно разрешён до полной миграции Task 10.")
            elif not first_line.startswith("# ") or not CYRILLIC.search(first_line):
                issues.append(f"{path}: первый заголовок должен быть русскоязычным")
        elif path in {".env.example", "guard-daemon-HELP_RU.txt"} and file_path.is_file():
            public_count += 1
            if not CYRILLIC.search(file_path.read_text("utf-8", errors="replace")):
                issues.append(f"{path}: отсутствует русский операторский текст")
    if public_count == 0:
        issues.append("Область русскоязычной документации оказалась пустой")
    return issues


def main() -> int:
    parser = argparse.ArgumentParser(description="Проверка политики репозитория")
    parser.add_argument("--root", type=Path)
    parser.add_argument("--base-sha", default=os.environ.get("CI_BASE_SHA"))
    arguments = parser.parse_args()
    root = arguments.root or Path(
        subprocess.check_output(["git", "rev-parse", "--show-toplevel"], text=True).strip()
    )
    try:
        issues = validate_repository(root.resolve(), arguments.base_sha)
    except PolicyError as error:
        print(f"Нарушение политики репозитория: {error}", file=sys.stderr)
        return 1
    for issue in issues:
        print(f"Нарушение политики репозитория: {issue}", file=sys.stderr)
    if issues:
        print(f"Обнаружено нарушений: {len(issues)}.", file=sys.stderr)
        return 1
    print(
        "Отслеживаемый набор, рабочее дерево и разница CI соответствуют политике репозитория."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
