#!/usr/bin/env python3
"""Создание и проверка детерминированных метаданных релиза guard-daemon."""

from __future__ import annotations

import argparse
import base64
import binascii
import hashlib
import json
import re
import sys
import tarfile
from pathlib import Path
from typing import Any
from urllib.parse import quote


FULL_HASH = re.compile(r"[0-9a-f]{40}\Z")
ABSOLUTE_PATH = re.compile(r"(?:/|[A-Za-z]:[\\/])")
TARGET = {"cgoEnabled": False, "goarch": "amd64", "goos": "linux"}

ARTIFACT_PATHS = {
    "binary": "guard-daemon-linux-amd64",
    "contract": "RescuerV2.json",
    "manifestSchema": "rescuer-manifest.schema.json",
    "sbom": "guard-daemon.cdx.json",
    "sourceArchive": "guard-daemon-source.tar",
}
PROVENANCE_SUBJECTS = tuple(
    sorted(
        (
            "guard-daemon-linux-amd64",
            "guard-daemon-source.tar",
            "LICENSE",
            "RescuerV2.json",
            "rescuer-manifest.schema.json",
            "guard-daemon.cdx.json",
            "release-candidate.json",
        )
    )
)
CONTENT_NAMES = tuple(sorted((*PROVENANCE_SUBJECTS, "guard-daemon.intoto.jsonl")))
OUTPUT_NAMES = frozenset((*CONTENT_NAMES, "SHA256SUMS"))

CYCLONEDX_SCHEMA = "https://cyclonedx.org/schema/bom-1.6.schema.json"
INTOTO_STATEMENT = "https://in-toto.io/Statement/v1"
SLSA_PROVENANCE = "https://slsa.dev/provenance/v1"
BUILD_TYPE = "https://guard-daemon.invalid/build-types/release-candidate/v1"
BUILDER_ID = "https://guard-daemon.invalid/release-builder/v1"
BUILDER_SCRIPT_NAME = "scripts/build-release-candidate.sh"


class MetadataError(ValueError):
    """Входные или выходные метаданные релиза некорректны."""


def validate_full_hash(value: Any, label: str) -> str:
    if not isinstance(value, str) or not FULL_HASH.fullmatch(value):
        raise MetadataError(
            f"Значение «{label}» должно состоять ровно из 40 шестнадцатеричных "
            "символов в нижнем регистре"
        )
    return value


def reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise MetadataError(f"Повторяющийся ключ JSON: {key}")
        result[key] = value
    return result


def load_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=reject_duplicate_keys)
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise MetadataError(f"Не удалось прочитать JSON из файла {path.name}: {error}") from error


def load_json_stream(path: Path) -> list[dict[str, Any]]:
    try:
        source = path.read_text(encoding="utf-8")
    except (OSError, UnicodeError) as error:
        raise MetadataError(f"Не удалось прочитать список модулей Go: {error}") from error

    decoder = json.JSONDecoder(object_pairs_hook=reject_duplicate_keys)
    documents: list[dict[str, Any]] = []
    offset = 0
    try:
        while offset < len(source):
            while offset < len(source) and source[offset].isspace():
                offset += 1
            if offset == len(source):
                break
            document, offset = decoder.raw_decode(source, offset)
            if not isinstance(document, dict):
                raise MetadataError("Каждая запись модуля Go должна быть объектом JSON")
            documents.append(document)
    except json.JSONDecodeError as error:
        raise MetadataError(
            f"Некорректная последовательность объектов JSON со списком модулей Go: {error}"
        ) from error
    if not documents:
        raise MetadataError("Список модулей Go пуст")
    return documents


def canonical_pretty(document: Any) -> bytes:
    return (json.dumps(document, ensure_ascii=False, indent=2, sort_keys=True) + "\n").encode(
        "utf-8"
    )


def canonical_compact(document: Any) -> bytes:
    return (
        json.dumps(document, ensure_ascii=False, separators=(",", ":"), sort_keys=True) + "\n"
    ).encode("utf-8")


def write_bytes(path: Path, content: bytes) -> None:
    try:
        path.write_bytes(content)
    except OSError as error:
        raise MetadataError(f"Не удалось записать файл {path.name}: {error}") from error


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as source:
            for block in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(block)
    except OSError as error:
        raise MetadataError(f"Не удалось вычислить хеш файла {path.name}: {error}") from error
    return digest.hexdigest()


def sha256_archive_member(path: Path, member_name: str) -> str:
    try:
        with tarfile.open(path, mode="r:") as archive:
            members = [member for member in archive.getmembers() if member.name == member_name]
            if len(members) != 1 or not members[0].isreg():
                raise MetadataError(
                    f"Source archive не содержит единственный обычный файл: {member_name}"
                )
            source = archive.extractfile(members[0])
            if source is None:
                raise MetadataError(f"Не удалось прочитать файл из source archive: {member_name}")
            digest = hashlib.sha256()
            for block in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(block)
            return digest.hexdigest()
    except (OSError, tarfile.TarError) as error:
        raise MetadataError(f"Не удалось проверить source archive: {error}") from error


def require_regular_file(path: Path) -> None:
    if path.is_symlink() or not path.is_file():
        raise MetadataError(f"Отсутствует обязательный обычный файл: {path.name}")


def package_name(package_path: str) -> str:
    marker = "node_modules/"
    if not package_path.startswith(marker):
        raise MetadataError(f"Некорректный путь в package-lock: {package_path}")
    name = package_path.rsplit(marker, 1)[1]
    if not name or name.startswith("/") or "node_modules/" in name:
        raise MetadataError(f"Некорректный путь в package-lock: {package_path}")
    return name


def package_url(ecosystem: str, name: str, version: str) -> str:
    encoded_name = quote(name, safe="/")
    encoded_version = quote(version, safe="")
    return f"pkg:{ecosystem}/{encoded_name}@{encoded_version}"


def integrity_hash(integrity: Any) -> list[dict[str, str]]:
    if not isinstance(integrity, str) or "-" not in integrity:
        return []
    algorithm, encoded = integrity.split("-", 1)
    cyclonedx_algorithm = {"sha1": "SHA-1", "sha256": "SHA-256", "sha512": "SHA-512"}.get(
        algorithm
    )
    if cyclonedx_algorithm is None:
        return []
    try:
        content = base64.b64decode(encoded, validate=True).hex()
    except (binascii.Error, ValueError):
        raise MetadataError("Некорректное значение integrity в package-lock") from None
    return [{"alg": cyclonedx_algorithm, "content": content}]


def resolve_npm_dependency(
    current_path: str, dependency_name: str, package_refs: dict[str, str]
) -> str | None:
    location = current_path
    while True:
        candidate = f"{location}/node_modules/{dependency_name}" if location else f"node_modules/{dependency_name}"
        if candidate in package_refs:
            return package_refs[candidate]
        nested_marker = location.rfind("/node_modules/")
        if nested_marker < 0:
            if not location:
                return None
            location = ""
        else:
            location = location[:nested_marker]


def npm_components(
    lock: dict[str, Any],
) -> tuple[dict[str, dict[str, Any]], dict[str, set[str]], set[str]]:
    if lock.get("lockfileVersion") != 3 or not isinstance(lock.get("packages"), dict):
        raise MetadataError(
            "Файл package-lock.json должен использовать lockfileVersion 3 и содержать packages"
        )
    packages: dict[str, Any] = lock["packages"]
    root = packages.get("")
    if not isinstance(root, dict):
        raise MetadataError("В package-lock.json отсутствует корневой пакет")

    components: dict[str, dict[str, Any]] = {}
    package_refs: dict[str, str] = {}
    package_data: dict[str, dict[str, Any]] = {}
    for package_path in sorted(packages):
        if package_path == "":
            continue
        if ABSOLUTE_PATH.match(package_path) or package_path.startswith("../"):
            raise MetadataError(f"package-lock содержит небезопасный путь: {package_path}")
        data = packages[package_path]
        if not isinstance(data, dict):
            raise MetadataError(f"Запись package-lock не является объектом: {package_path}")
        if data.get("link") is True:
            raise MetadataError(
                f"Записи с полем link в package-lock недопустимы для релиза: {package_path}"
            )
        name = package_name(package_path)
        version = data.get("version")
        if not isinstance(version, str) or not version:
            raise MetadataError(f"В записи package-lock отсутствует version: {package_path}")
        reference = package_url("npm", name, version)
        component: dict[str, Any] = {
            "bom-ref": reference,
            "name": name,
            "purl": reference,
            "type": "library",
            "version": version,
        }
        hashes = integrity_hash(data.get("integrity"))
        if hashes:
            component["hashes"] = hashes
        if data.get("dev") is True or data.get("optional") is True:
            component["scope"] = "optional"
        else:
            component["scope"] = "required"
        existing = components.get(reference)
        if existing is None or (
            existing.get("scope") == "optional" and component["scope"] == "required"
        ):
            components[reference] = component
        package_refs[package_path] = reference
        package_data[package_path] = data

    dependencies: dict[str, set[str]] = {reference: set() for reference in components}
    for package_path in sorted(package_data):
        source_ref = package_refs[package_path]
        data = package_data[package_path]
        peer_metadata = data.get("peerDependenciesMeta", {})
        if not isinstance(peer_metadata, dict):
            raise MetadataError(
                f"Поле peerDependenciesMeta для {package_path} должно содержать объект"
            )
        for field in ("dependencies", "optionalDependencies", "peerDependencies"):
            declared = data.get(field, {})
            if not isinstance(declared, dict):
                raise MetadataError(f"Поле {field} для {package_path} должно содержать объект")
            for dependency_name in sorted(declared):
                dependency_ref = resolve_npm_dependency(package_path, dependency_name, package_refs)
                if dependency_ref is None:
                    peer_options = peer_metadata.get(dependency_name, {})
                    optional_peer = (
                        field == "peerDependencies"
                        and isinstance(peer_options, dict)
                        and peer_options.get("optional") is True
                    )
                    if field == "optionalDependencies" or optional_peer:
                        continue
                    raise MetadataError(
                        "Не удалось разрешить зависимость package-lock: "
                        f"{package_path} -> {dependency_name}"
                    )
                dependencies[source_ref].add(dependency_ref)

    root_dependencies: set[str] = set()
    for field in ("dependencies", "devDependencies", "optionalDependencies"):
        declared = root.get(field, {})
        if not isinstance(declared, dict):
            raise MetadataError(f"Корневое поле {field} должно содержать объект")
        for dependency_name in sorted(declared):
            dependency_ref = resolve_npm_dependency("", dependency_name, package_refs)
            if dependency_ref is None:
                if field == "optionalDependencies":
                    continue
                raise MetadataError(
                    f"Не удалось разрешить зависимость корневого пакета package-lock: {dependency_name}"
                )
            root_dependencies.add(dependency_ref)
    return components, dependencies, root_dependencies


def go_components(
    modules: list[dict[str, Any]],
) -> tuple[dict[str, dict[str, Any]], dict[str, str], dict[str, str], str]:
    components: dict[str, dict[str, Any]] = {}
    module_refs: dict[str, str] = {}
    selected_source_refs: dict[str, str] = {}
    main_module: str | None = None
    for module in modules:
        path = module.get("Path")
        if not isinstance(path, str) or not path:
            raise MetadataError("В записи модуля Go отсутствует Path")
        if ABSOLUTE_PATH.match(path):
            raise MetadataError("Значение Path модуля Go не должно быть абсолютным")
        if module.get("Main") is True:
            if main_module is not None:
                raise MetadataError("Список модулей Go содержит более одного основного модуля")
            main_module = path
            continue
        version = module.get("Version")
        if not isinstance(version, str) or not version:
            raise MetadataError(f"Для модуля Go не указана версия: {path}")
        component_path = path
        component_version = version
        module_sum = module.get("Sum")
        properties: list[dict[str, str]] = []
        replacement = module.get("Replace")
        if replacement is not None:
            if (
                not isinstance(replacement, dict)
                or not isinstance(replacement.get("Path"), str)
                or not replacement["Path"]
            ):
                raise MetadataError(f"Некорректная замена модуля Go: {path}")
            replacement_path = replacement["Path"]
            if ABSOLUTE_PATH.match(replacement_path):
                raise MetadataError(f"Абсолютный путь замены модуля Go недопустим: {path}")
            replacement_version = replacement.get("Version")
            if not isinstance(replacement_version, str) or not replacement_version:
                raise MetadataError(
                    f"Replacement модуля Go должен иметь закреплённую версию: {path}"
                )
            component_path = replacement_path
            component_version = replacement_version
            module_sum = replacement.get("Sum")
            properties.append(
                {
                    "name": "guard-daemon:go:replaces",
                    "value": f"{path}@{version}",
                }
            )
        reference = package_url("golang", component_path, component_version)
        if reference in components:
            raise MetadataError(f"Повторяющийся фактический модуль Go: {reference}")
        component: dict[str, Any] = {
            "bom-ref": reference,
            "name": component_path,
            "purl": reference,
            "type": "library",
            "version": component_version,
        }
        if isinstance(module_sum, str) and module_sum.startswith("h1:"):
            try:
                sum_content = base64.b64decode(module_sum[3:], validate=True).hex()
            except (binascii.Error, ValueError):
                raise MetadataError(f"Некорректная контрольная сумма модуля Go: {path}") from None
            component["hashes"] = [{"alg": "SHA-256", "content": sum_content}]
        if properties:
            component["properties"] = sorted(
                properties, key=lambda item: (item["name"], item["value"])
            )
        components[reference] = component
        aliases = [path]
        source_nodes = [f"{path}@{version}"]
        if isinstance(replacement, dict):
            aliases.append(replacement["Path"])
            source_nodes.append(f"{replacement['Path']}@{replacement['Version']}")
        for alias in aliases:
            existing_ref = module_refs.get(alias)
            if existing_ref is not None and existing_ref != reference:
                raise MetadataError(f"Путь модуля Go неоднозначен после replacement: {alias}")
            module_refs[alias] = reference
        for source_node in source_nodes:
            existing_ref = selected_source_refs.get(source_node)
            if existing_ref is not None and existing_ref != reference:
                raise MetadataError(
                    f"Узел модуля Go неоднозначен после replacement: {source_node}"
                )
            selected_source_refs[source_node] = reference
    if main_module is None:
        raise MetadataError("В списке модулей Go отсутствует основной модуль")
    return components, module_refs, selected_source_refs, main_module


def go_module_path(value: str) -> str:
    path, separator, version = value.rpartition("@")
    if separator and path and version:
        return path
    if value and "@" not in value:
        return value
    raise MetadataError(f"Некорректный узел go mod graph: {value}")


def go_dependencies(
    graph_path: Path,
    module_refs: dict[str, str],
    selected_source_refs: dict[str, str],
    main_module: str,
    root_ref: str,
) -> dict[str, set[str]]:
    try:
        lines = graph_path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError) as error:
        raise MetadataError(f"Не удалось прочитать go mod graph: {error}") from error
    if not lines:
        raise MetadataError("go mod graph пуст")

    dependencies = {reference: set() for reference in module_refs.values()}
    dependencies[root_ref] = set()
    for line in lines:
        fields = line.split()
        if len(fields) != 2:
            raise MetadataError(f"Некорректная строка go mod graph: {line}")
        source_path = go_module_path(fields[0])
        target_path = go_module_path(fields[1])
        if source_path in {"go", "toolchain"} or target_path in {"go", "toolchain"}:
            continue
        known_source_ref = root_ref if source_path == main_module else module_refs.get(source_path)
        if known_source_ref is None:
            raise MetadataError(f"go mod graph ссылается на неизвестный модуль: {source_path}")
        target_ref = module_refs.get(target_path)
        if target_ref is None:
            raise MetadataError(f"go mod graph ссылается на неизвестный модуль: {target_path}")
        source_ref = (
            root_ref
            if fields[0] == main_module
            else selected_source_refs.get(fields[0])
        )
        if source_ref is None:
            continue
        if source_ref != target_ref:
            dependencies[source_ref].add(target_ref)
    return dependencies


def build_sbom(
    package_lock: Path,
    go_modules: Path,
    go_graph: Path,
    release_commit: str,
) -> dict[str, Any]:
    validate_full_hash(release_commit, "коммит релиза")
    lock = load_json(package_lock)
    if not isinstance(lock, dict):
        raise MetadataError("Корень package-lock.json должен быть объектом")
    npm, npm_dependencies, npm_root_dependencies = npm_components(lock)
    go, go_module_refs, go_selected_source_refs, main_module = go_components(
        load_json_stream(go_modules)
    )
    duplicate_refs = set(npm).intersection(go)
    if duplicate_refs:
        raise MetadataError(f"Повторяющаяся ссылка на компонент: {min(duplicate_refs)}")

    root_ref = package_url("generic", "guard-daemon", release_commit)
    components = {**npm, **go}
    dependency_map = {**npm_dependencies}
    go_dependency_map = go_dependencies(
        go_graph,
        go_module_refs,
        go_selected_source_refs,
        main_module,
        root_ref,
    )
    dependency_map.update(go_dependency_map)
    dependency_map[root_ref].update(npm_root_dependencies)
    dependencies = [
        {"dependsOn": sorted(dependency_map[reference]), "ref": reference}
        for reference in sorted(dependency_map)
    ]
    component_list = sorted(components.values(), key=lambda item: item["bom-ref"])
    return {
        "$schema": CYCLONEDX_SCHEMA,
        "bomFormat": "CycloneDX",
        "components": component_list,
        "dependencies": dependencies,
        "metadata": {
            "component": {
                "bom-ref": root_ref,
                "name": "guard-daemon",
                "properties": [
                    {"name": "guard-daemon:go:module", "value": main_module},
                    {"name": "guard-daemon:releaseCommit", "value": release_commit},
                ],
                "purl": root_ref,
                "type": "application",
                "version": release_commit,
            }
        },
        "specVersion": "1.6",
        "version": 1,
    }


def write_sbom(
    package_lock: Path,
    go_modules: Path,
    go_graph: Path,
    release_commit: str,
    output: Path,
) -> None:
    write_bytes(
        output,
        canonical_pretty(build_sbom(package_lock, go_modules, go_graph, release_commit)),
    )


def artifact_record(directory: Path, name: str) -> dict[str, str]:
    path = directory / name
    require_regular_file(path)
    return {"path": name, "sha256": f"sha256:{sha256_file(path)}"}


def build_candidate(directory: Path, release_commit: str, release_tree: str) -> dict[str, Any]:
    validate_full_hash(release_commit, "коммит релиза")
    validate_full_hash(release_tree, "дерево релиза")
    artifacts = {
        field: artifact_record(directory, path) for field, path in ARTIFACT_PATHS.items()
    }
    return {
        "artifacts": artifacts,
        "releaseCommit": release_commit,
        "releaseTree": release_tree,
        "schemaVersion": "1",
        "target": TARGET,
    }


def write_candidate(directory: Path, release_commit: str, release_tree: str, output: Path) -> None:
    write_bytes(output, canonical_pretty(build_candidate(directory, release_commit, release_tree)))


def build_provenance(
    directory: Path,
    release_commit: str,
    release_tree: str,
    builder_script_sha256: str,
) -> dict[str, Any]:
    validate_full_hash(release_commit, "коммит релиза")
    validate_full_hash(release_tree, "дерево релиза")
    for name in PROVENANCE_SUBJECTS:
        require_regular_file(directory / name)
    subjects = [
        {"digest": {"sha256": sha256_file(directory / name)}, "name": name}
        for name in PROVENANCE_SUBJECTS
    ]
    return {
        "_type": INTOTO_STATEMENT,
        "predicate": {
            "buildDefinition": {
                "buildType": BUILD_TYPE,
                "externalParameters": {
                    "releaseCommit": release_commit,
                    "releaseTree": release_tree,
                    "target": TARGET,
                },
                "internalParameters": {},
                "resolvedDependencies": [
                    {
                        "digest": {"sha256": builder_script_sha256},
                        "name": BUILDER_SCRIPT_NAME,
                    },
                    {
                        "digest": {"sha1": release_commit},
                        "name": "guard-daemon source",
                    }
                ],
            },
            "runDetails": {"builder": {"id": BUILDER_ID}, "metadata": {}},
        },
        "predicateType": SLSA_PROVENANCE,
        "subject": subjects,
    }


def write_provenance(
    directory: Path,
    release_commit: str,
    release_tree: str,
    builder_script: Path,
    output: Path,
) -> None:
    require_regular_file(builder_script)
    write_bytes(
        output,
        canonical_compact(
            build_provenance(
                directory,
                release_commit,
                release_tree,
                sha256_file(builder_script),
            )
        ),
    )


def directory_names(directory: Path) -> set[str]:
    try:
        entries = list(directory.iterdir())
    except OSError as error:
        raise MetadataError(f"Не удалось проверить выходной каталог: {error}") from error
    names: set[str] = set()
    for entry in entries:
        if entry.name in names:
            raise MetadataError(f"Повторяющееся имя выходного файла: {entry.name}")
        names.add(entry.name)
        require_regular_file(entry)
    return names


def require_names(directory: Path, expected: set[str] | frozenset[str]) -> None:
    actual = directory_names(directory)
    missing = sorted(expected - actual)
    unknown = sorted(actual - expected)
    if missing or unknown:
        details = []
        if missing:
            details.append("отсутствуют файлы: " + ", ".join(missing))
        if unknown:
            details.append("неизвестные файлы: " + ", ".join(unknown))
        raise MetadataError(
            "Недопустимый состав выходных файлов релиза (" + "; ".join(details) + ")"
        )


def checksum_content(directory: Path) -> bytes:
    return "".join(f"{sha256_file(directory / name)}  {name}\n" for name in CONTENT_NAMES).encode(
        "ascii"
    )


def write_checksums(directory: Path, output: Path) -> None:
    if output.parent != directory or output.name != "SHA256SUMS":
        raise MetadataError(
            "Файл контрольных сумм должен находиться по пути "
            "<release-directory>/SHA256SUMS"
        )
    actual = directory_names(directory)
    expected = set(CONTENT_NAMES)
    missing = sorted(expected - actual)
    unknown = sorted(actual - expected - {"SHA256SUMS"})
    if missing or unknown:
        details = []
        if missing:
            details.append("отсутствуют файлы: " + ", ".join(missing))
        if unknown:
            details.append("неизвестные файлы: " + ", ".join(unknown))
        raise MetadataError(
            "Недопустимый состав выходных файлов релиза (" + "; ".join(details) + ")"
        )
    write_bytes(output, checksum_content(directory))


def assert_no_nondeterministic_fields(value: Any, path: str = "$") -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if key.lower() in {"timestamp", "startedon", "finishedon", "serialnumber"}:
                raise MetadataError(f"Недетерминированное поле запрещено: {path}.{key}")
            assert_no_nondeterministic_fields(child, f"{path}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            assert_no_nondeterministic_fields(child, f"{path}[{index}]")
    elif isinstance(value, str) and ABSOLUTE_PATH.match(value):
        raise MetadataError(f"Абсолютный путь запрещён в метаданных: {path}")


def properties_are_sorted(value: Any) -> bool:
    return (
        isinstance(value, list)
        and all(
            isinstance(item, dict)
            and isinstance(item.get("name"), str)
            and isinstance(item.get("value"), str)
            for item in value
        )
        and value == sorted(value, key=lambda item: (item["name"], item["value"]))
    )


def verify_sbom(path: Path, expected_commit: str | None = None) -> None:
    document = load_json(path)
    if not isinstance(document, dict):
        raise MetadataError("Корень SBOM должен быть объектом")
    if document.get("$schema") != CYCLONEDX_SCHEMA:
        raise MetadataError("SBOM должен использовать схему CycloneDX 1.6")
    if document.get("bomFormat") != "CycloneDX" or document.get("specVersion") != "1.6":
        raise MetadataError("Формат SBOM должен соответствовать CycloneDX 1.6")
    if document.get("version") != 1:
        raise MetadataError("Версия SBOM должна быть равна 1")
    components = document.get("components")
    dependencies = document.get("dependencies")
    metadata = document.get("metadata")
    if not isinstance(components, list) or not isinstance(dependencies, list):
        raise MetadataError("Поля components и dependencies в SBOM должны быть массивами")
    if not isinstance(metadata, dict) or not isinstance(metadata.get("component"), dict):
        raise MetadataError("В SBOM требуется поле metadata.component")
    component_refs: list[str] = []
    for component in components:
        if not isinstance(component, dict):
            raise MetadataError("Каждый компонент SBOM должен быть объектом")
        reference = component.get("bom-ref")
        if not isinstance(reference, str) or not isinstance(component.get("name"), str):
            raise MetadataError("Каждому компоненту SBOM требуются bom-ref и name")
        if not isinstance(component.get("version"), str) or component.get("type") != "library":
            raise MetadataError(
                "Каждому компоненту-зависимости SBOM требуются type и version"
            )
        if component.get("purl") != reference:
            raise MetadataError(
                "Каждому компоненту-зависимости SBOM требуется соответствующий purl"
            )
        component_refs.append(reference)
        properties = component.get("properties", [])
        if not properties_are_sorted(properties):
            raise MetadataError("Свойства компонента SBOM не отсортированы")
    if component_refs != sorted(component_refs) or len(component_refs) != len(set(component_refs)):
        raise MetadataError("Компоненты SBOM не отсортированы или содержат повторы")

    root_ref = metadata["component"].get("bom-ref")
    if not isinstance(root_ref, str):
        raise MetadataError("Корневому компоненту SBOM требуется bom-ref")
    root_component = metadata["component"]
    root_version = root_component.get("version")
    validate_full_hash(
        root_version if isinstance(root_version, str) else "", "коммит релиза в SBOM"
    )
    if expected_commit is not None and root_version != expected_commit:
        raise MetadataError("Коммит релиза в SBOM не соответствует кандидату на релиз")
    if root_component.get("name") != "guard-daemon" or root_component.get("type") != "application":
        raise MetadataError("Корневой компонент SBOM должен описывать guard-daemon")
    if root_ref != package_url("generic", "guard-daemon", root_version):
        raise MetadataError("Ссылка на корневой компонент SBOM некорректна")
    if root_component.get("purl") != root_ref:
        raise MetadataError("Поле purl корневого компонента SBOM некорректно")
    root_properties = root_component.get("properties")
    if not properties_are_sorted(root_properties):
        raise MetadataError("Свойства корневого компонента SBOM не отсортированы")
    property_map = {item["name"]: item["value"] for item in root_properties}
    if len(root_properties) != 2 or set(property_map) != {
        "guard-daemon:go:module",
        "guard-daemon:releaseCommit",
    }:
        raise MetadataError("Свойства корневого компонента SBOM некорректны")
    if property_map["guard-daemon:releaseCommit"] != root_version or not isinstance(
        property_map["guard-daemon:go:module"], str
    ):
        raise MetadataError("Свойства корневого компонента SBOM не соответствуют его версии")
    known_refs = set(component_refs) | {root_ref}
    dependency_refs: list[str] = []
    for dependency in dependencies:
        if not isinstance(dependency, dict) or set(dependency) != {"dependsOn", "ref"}:
            raise MetadataError("Некорректная запись зависимости SBOM")
        reference = dependency["ref"]
        depends_on = dependency["dependsOn"]
        if reference not in known_refs or not isinstance(depends_on, list):
            raise MetadataError("Зависимость SBOM ссылается на неизвестный компонент")
        if depends_on != sorted(set(depends_on)) or not set(depends_on).issubset(known_refs):
            raise MetadataError(
                "Цели зависимости SBOM не отсортированы или содержат повторы"
            )
        dependency_refs.append(reference)
    if dependency_refs != sorted(dependency_refs) or len(dependency_refs) != len(
        set(dependency_refs)
    ):
        raise MetadataError("Зависимости SBOM не отсортированы или содержат повторы")
    assert_no_nondeterministic_fields(document)
    if path.read_bytes() != canonical_pretty(document):
        raise MetadataError("SBOM в формате JSON сериализован не в каноническом виде")


def verify_candidate(directory: Path) -> dict[str, Any]:
    path = directory / "release-candidate.json"
    document = load_json(path)
    if not isinstance(document, dict) or set(document) != {
        "schemaVersion",
        "releaseCommit",
        "releaseTree",
        "target",
        "artifacts",
    }:
        raise MetadataError("Кандидат на релиз не соответствует версии 1 схемы")
    if document["schemaVersion"] != "1" or document["target"] != TARGET:
        raise MetadataError("Поля schemaVersion или target кандидата на релиз некорректны")
    validate_full_hash(document.get("releaseCommit", ""), "коммит релиза")
    validate_full_hash(document.get("releaseTree", ""), "дерево релиза")
    artifacts = document.get("artifacts")
    if not isinstance(artifacts, dict) or set(artifacts) != set(ARTIFACT_PATHS):
        raise MetadataError("У кандидата на релиз некорректный объект artifacts")
    for field, expected_path in ARTIFACT_PATHS.items():
        record = artifacts[field]
        if not isinstance(record, dict) or set(record) != {"path", "sha256"}:
            raise MetadataError(f"Некорректный артефакт кандидата на релиз: {field}")
        if record["path"] != expected_path or record["sha256"] != (
            f"sha256:{sha256_file(directory / expected_path)}"
        ):
            raise MetadataError(
                f"Артефакт кандидата на релиз не соответствует выходному файлу: {field}"
            )
    assert_no_nondeterministic_fields(document)
    if path.read_bytes() != canonical_pretty(document):
        raise MetadataError(
            "Кандидат на релиз в формате JSON сериализован не в каноническом виде"
        )
    return document


def verify_provenance(directory: Path, candidate: dict[str, Any]) -> None:
    path = directory / "guard-daemon.intoto.jsonl"
    try:
        content = path.read_bytes()
    except OSError as error:
        raise MetadataError(f"Не удалось прочитать сведения о происхождении: {error}") from error
    if not content.endswith(b"\n") or content.count(b"\n") != 1:
        raise MetadataError("Сведения о происхождении должны содержать ровно одну строку JSON")
    document = load_json(path)
    if not isinstance(document, dict) or set(document) != {
        "_type",
        "predicate",
        "predicateType",
        "subject",
    }:
        raise MetadataError("Сведения о происхождении не являются документом in-toto Statement")
    if document["_type"] != INTOTO_STATEMENT or document["predicateType"] != SLSA_PROVENANCE:
        raise MetadataError(
            "Сведения о происхождении должны соответствовать in-toto Statement v1 и "
            "SLSA Provenance v1"
        )
    expected_subjects = [
        {"digest": {"sha256": sha256_file(directory / name)}, "name": name}
        for name in PROVENANCE_SUBJECTS
    ]
    if document["subject"] != expected_subjects:
        raise MetadataError(
            "Поле subject в сведениях о происхождении не соответствует выходным файлам релиза"
        )
    predicate = document.get("predicate")
    if not isinstance(predicate, dict) or set(predicate) != {"buildDefinition", "runDetails"}:
        raise MetadataError("Некорректное поле predicate в SLSA Provenance")
    definition = predicate["buildDefinition"]
    run_details = predicate["runDetails"]
    if not isinstance(definition, dict) or not isinstance(run_details, dict):
        raise MetadataError("Некорректная структура SLSA Provenance")
    if set(definition) != {
        "buildType",
        "externalParameters",
        "internalParameters",
        "resolvedDependencies",
    }:
        raise MetadataError("Некорректная схема buildDefinition в SLSA Provenance")
    external = definition.get("externalParameters")
    if external != {
        "releaseCommit": candidate["releaseCommit"],
        "releaseTree": candidate["releaseTree"],
        "target": TARGET,
    }:
        raise MetadataError(
            "Поле externalParameters в сведениях о происхождении "
            "не соответствуют кандидату на релиз"
        )
    if definition.get("buildType") != BUILD_TYPE or definition.get("internalParameters") != {}:
        raise MetadataError("Некорректное поле buildDefinition в сведениях о происхождении")
    if definition.get("resolvedDependencies") != [
        {
            "digest": {
                "sha256": sha256_archive_member(
                    directory / "guard-daemon-source.tar",
                    "guard-daemon-"
                    + candidate["releaseCommit"]
                    + "/"
                    + BUILDER_SCRIPT_NAME,
                )
            },
            "name": BUILDER_SCRIPT_NAME,
        },
        {
            "digest": {"sha1": candidate["releaseCommit"]},
            "name": "guard-daemon source",
        }
    ]:
        raise MetadataError(
            "Некорректное поле resolvedDependencies в сведениях о происхождении"
        )
    if run_details != {"builder": {"id": BUILDER_ID}, "metadata": {}}:
        raise MetadataError(
            "Поле runDetails в сведениях о происхождении содержит "
            "некорректные детерминированные данные"
        )
    assert_no_nondeterministic_fields(document)
    if content != canonical_compact(document):
        raise MetadataError("Сведения о происхождении сериализованы не в каноническом виде")


def verify_release(directory: Path) -> None:
    require_names(directory, OUTPUT_NAMES)
    expected_checksums = checksum_content(directory)
    try:
        actual_checksums = (directory / "SHA256SUMS").read_bytes()
    except OSError as error:
        raise MetadataError(f"Не удалось прочитать SHA256SUMS: {error}") from error
    if actual_checksums != expected_checksums:
        raise MetadataError(
            "SHA256SUMS некорректен, не отсортирован или не соответствует "
            "выходным файлам релиза"
        )
    candidate = verify_candidate(directory)
    verify_sbom(directory / "guard-daemon.cdx.json", candidate["releaseCommit"])
    verify_provenance(directory, candidate)


def create_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)

    sbom = commands.add_parser("sbom", help="создать SBOM CycloneDX 1.6")
    sbom.add_argument("--package-lock", type=Path, required=True)
    sbom.add_argument("--go-modules", type=Path, required=True)
    sbom.add_argument("--go-graph", type=Path, required=True)
    sbom.add_argument("--release-commit", required=True)
    sbom.add_argument("--output", type=Path, required=True)

    candidate = commands.add_parser("candidate", help="создать метаданные кандидата на релиз")
    candidate.add_argument("--directory", type=Path, required=True)
    candidate.add_argument("--release-commit", required=True)
    candidate.add_argument("--release-tree", required=True)
    candidate.add_argument("--output", type=Path, required=True)

    provenance = commands.add_parser(
        "provenance", help="создать неподписанный документ SLSA Provenance"
    )
    provenance.add_argument("--directory", type=Path, required=True)
    provenance.add_argument("--builder-script", type=Path, required=True)
    provenance.add_argument("--release-commit", required=True)
    provenance.add_argument("--release-tree", required=True)
    provenance.add_argument("--output", type=Path, required=True)

    checksums = commands.add_parser(
        "checksums", help="создать отсортированный файл SHA256SUMS"
    )
    checksums.add_argument("--directory", type=Path, required=True)
    checksums.add_argument("--output", type=Path, required=True)

    verify = commands.add_parser(
        "verify", help="проверить полный набор выходных файлов релиза"
    )
    verify.add_argument("--directory", type=Path, required=True)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = create_parser().parse_args(argv)
    try:
        if args.command == "sbom":
            write_sbom(
                args.package_lock,
                args.go_modules,
                args.go_graph,
                args.release_commit,
                args.output,
            )
        elif args.command == "candidate":
            write_candidate(
                args.directory,
                args.release_commit,
                args.release_tree,
                args.output,
            )
        elif args.command == "provenance":
            write_provenance(
                args.directory,
                args.release_commit,
                args.release_tree,
                args.builder_script,
                args.output,
            )
        elif args.command == "checksums":
            write_checksums(args.directory, args.output)
        elif args.command == "verify":
            verify_release(args.directory)
        else:
            raise MetadataError(f"Неподдерживаемая команда: {args.command}")
    except MetadataError as error:
        print(f"Ошибка метаданных релиза: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
