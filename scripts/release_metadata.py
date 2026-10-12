#!/usr/bin/env python3
"""Create and verify deterministic guard-daemon release metadata."""

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
    """Release input or output metadata is invalid."""


def validate_full_hash(value: Any, label: str) -> str:
    if not isinstance(value, str) or not FULL_HASH.fullmatch(value):
        raise MetadataError(
            f"Value '{label}' must contain exactly 40 lowercase hexadecimal "
            "characters"
        )
    return value


def reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise MetadataError(f"Duplicate JSON key: {key}")
        result[key] = value
    return result


def load_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=reject_duplicate_keys)
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise MetadataError(f"Failed to read JSON from {path.name}: {error}") from error


def load_json_stream(path: Path) -> list[dict[str, Any]]:
    try:
        source = path.read_text(encoding="utf-8")
    except (OSError, UnicodeError) as error:
        raise MetadataError(f"Failed to read the Go module list: {error}") from error

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
                raise MetadataError("Every Go module entry must be a JSON object")
            documents.append(document)
    except json.JSONDecodeError as error:
        raise MetadataError(
            f"Invalid JSON object stream in the Go module list: {error}"
        ) from error
    if not documents:
        raise MetadataError("Go module list is empty")
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
        raise MetadataError(f"Failed to write {path.name}: {error}") from error


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as source:
            for block in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(block)
    except OSError as error:
        raise MetadataError(f"Failed to hash {path.name}: {error}") from error
    return digest.hexdigest()


def sha256_archive_member(path: Path, member_name: str) -> str:
    try:
        with tarfile.open(path, mode="r:") as archive:
            members = [member for member in archive.getmembers() if member.name == member_name]
            if len(members) != 1 or not members[0].isreg():
                raise MetadataError(
                    f"Source archive does not contain exactly one regular file: {member_name}"
                )
            source = archive.extractfile(members[0])
            if source is None:
                raise MetadataError(f"Failed to read a file from the source archive: {member_name}")
            digest = hashlib.sha256()
            for block in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(block)
            return digest.hexdigest()
    except (OSError, tarfile.TarError) as error:
        raise MetadataError(f"Failed to inspect the source archive: {error}") from error


def require_regular_file(path: Path) -> None:
    if path.is_symlink() or not path.is_file():
        raise MetadataError(f"Required regular file is missing: {path.name}")


def package_name(package_path: str) -> str:
    marker = "node_modules/"
    if not package_path.startswith(marker):
        raise MetadataError(f"Invalid package-lock path: {package_path}")
    name = package_path.rsplit(marker, 1)[1]
    if not name or name.startswith("/") or "node_modules/" in name:
        raise MetadataError(f"Invalid package-lock path: {package_path}")
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
        raise MetadataError("Invalid integrity value in package-lock") from None
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
            "package-lock.json must use lockfileVersion 3 and contain packages"
        )
    packages: dict[str, Any] = lock["packages"]
    root = packages.get("")
    if not isinstance(root, dict):
        raise MetadataError("package-lock.json does not contain a root package")

    components: dict[str, dict[str, Any]] = {}
    package_refs: dict[str, str] = {}
    package_data: dict[str, dict[str, Any]] = {}
    for package_path in sorted(packages):
        if package_path == "":
            continue
        if ABSOLUTE_PATH.match(package_path) or package_path.startswith("../"):
            raise MetadataError(f"package-lock contains an unsafe path: {package_path}")
        data = packages[package_path]
        if not isinstance(data, dict):
            raise MetadataError(f"package-lock entry is not an object: {package_path}")
        if data.get("link") is True:
            raise MetadataError(
                f"package-lock entries with link are not allowed in a release: {package_path}"
            )
        name = package_name(package_path)
        version = data.get("version")
        if not isinstance(version, str) or not version:
            raise MetadataError(f"package-lock entry has no version: {package_path}")
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
                f"peerDependenciesMeta for {package_path} must contain an object"
            )
        for field in ("dependencies", "optionalDependencies", "peerDependencies"):
            declared = data.get(field, {})
            if not isinstance(declared, dict):
                raise MetadataError(f"Field {field} for {package_path} must contain an object")
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
                        "Failed to resolve package-lock dependency: "
                        f"{package_path} -> {dependency_name}"
                    )
                dependencies[source_ref].add(dependency_ref)

    root_dependencies: set[str] = set()
    for field in ("dependencies", "devDependencies", "optionalDependencies"):
        declared = root.get(field, {})
        if not isinstance(declared, dict):
            raise MetadataError(f"Root field {field} must contain an object")
        for dependency_name in sorted(declared):
            dependency_ref = resolve_npm_dependency("", dependency_name, package_refs)
            if dependency_ref is None:
                if field == "optionalDependencies":
                    continue
                raise MetadataError(
                    f"Failed to resolve root package-lock dependency: {dependency_name}"
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
            raise MetadataError("Go module entry has no Path")
        if ABSOLUTE_PATH.match(path):
            raise MetadataError("Go module Path must not be absolute")
        if module.get("Main") is True:
            if main_module is not None:
                raise MetadataError("Go module list contains more than one main module")
            main_module = path
            continue
        version = module.get("Version")
        if not isinstance(version, str) or not version:
            raise MetadataError(f"Go module has no version: {path}")
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
                raise MetadataError(f"Invalid Go module replacement: {path}")
            replacement_path = replacement["Path"]
            if ABSOLUTE_PATH.match(replacement_path):
                raise MetadataError(f"Absolute Go module replacement path is not allowed: {path}")
            replacement_version = replacement.get("Version")
            if not isinstance(replacement_version, str) or not replacement_version:
                raise MetadataError(
                    f"Go module replacement must have a pinned version: {path}"
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
            raise MetadataError(f"Duplicate effective Go module: {reference}")
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
                raise MetadataError(f"Invalid Go module checksum: {path}") from None
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
                raise MetadataError(f"Go module path is ambiguous after replacement: {alias}")
            module_refs[alias] = reference
        for source_node in source_nodes:
            existing_ref = selected_source_refs.get(source_node)
            if existing_ref is not None and existing_ref != reference:
                raise MetadataError(
                    f"Go module node is ambiguous after replacement: {source_node}"
                )
            selected_source_refs[source_node] = reference
    if main_module is None:
        raise MetadataError("Go module list has no main module")
    return components, module_refs, selected_source_refs, main_module


def go_module_path(value: str) -> str:
    path, separator, version = value.rpartition("@")
    if separator and path and version:
        return path
    if value and "@" not in value:
        return value
    raise MetadataError(f"Invalid go mod graph node: {value}")


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
        raise MetadataError(f"Failed to read go mod graph: {error}") from error
    if not lines:
        raise MetadataError("go mod graph is empty")

    dependencies = {reference: set() for reference in module_refs.values()}
    dependencies[root_ref] = set()
    for line in lines:
        fields = line.split()
        if len(fields) != 2:
            raise MetadataError(f"Invalid go mod graph line: {line}")
        source_path = go_module_path(fields[0])
        target_path = go_module_path(fields[1])
        if source_path in {"go", "toolchain"} or target_path in {"go", "toolchain"}:
            continue
        known_source_ref = root_ref if source_path == main_module else module_refs.get(source_path)
        if known_source_ref is None:
            raise MetadataError(f"go mod graph references an unknown module: {source_path}")
        target_ref = module_refs.get(target_path)
        if target_ref is None:
            raise MetadataError(f"go mod graph references an unknown module: {target_path}")
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
    validate_full_hash(release_commit, "release commit")
    lock = load_json(package_lock)
    if not isinstance(lock, dict):
        raise MetadataError("package-lock.json root must be an object")
    npm, npm_dependencies, npm_root_dependencies = npm_components(lock)
    go, go_module_refs, go_selected_source_refs, main_module = go_components(
        load_json_stream(go_modules)
    )
    duplicate_refs = set(npm).intersection(go)
    if duplicate_refs:
        raise MetadataError(f"Duplicate component reference: {min(duplicate_refs)}")

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
    validate_full_hash(release_commit, "release commit")
    validate_full_hash(release_tree, "release tree")
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
    validate_full_hash(release_commit, "release commit")
    validate_full_hash(release_tree, "release tree")
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
        raise MetadataError(f"Failed to inspect the output directory: {error}") from error
    names: set[str] = set()
    for entry in entries:
        if entry.name in names:
            raise MetadataError(f"Duplicate output filename: {entry.name}")
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
            details.append("missing files: " + ", ".join(missing))
        if unknown:
            details.append("unknown files: " + ", ".join(unknown))
        raise MetadataError(
            "Invalid release output file set (" + "; ".join(details) + ")"
        )


def checksum_content(directory: Path) -> bytes:
    return "".join(f"{sha256_file(directory / name)}  {name}\n" for name in CONTENT_NAMES).encode(
        "ascii"
    )


def write_checksums(directory: Path, output: Path) -> None:
    if output.parent != directory or output.name != "SHA256SUMS":
        raise MetadataError(
            "Checksum file must be located at "
            "<release-directory>/SHA256SUMS"
        )
    actual = directory_names(directory)
    expected = set(CONTENT_NAMES)
    missing = sorted(expected - actual)
    unknown = sorted(actual - expected - {"SHA256SUMS"})
    if missing or unknown:
        details = []
        if missing:
            details.append("missing files: " + ", ".join(missing))
        if unknown:
            details.append("unknown files: " + ", ".join(unknown))
        raise MetadataError(
            "Invalid release output file set (" + "; ".join(details) + ")"
        )
    write_bytes(output, checksum_content(directory))


def assert_no_nondeterministic_fields(value: Any, path: str = "$") -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            if key.lower() in {"timestamp", "startedon", "finishedon", "serialnumber"}:
                raise MetadataError(f"Nondeterministic field is forbidden: {path}.{key}")
            assert_no_nondeterministic_fields(child, f"{path}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            assert_no_nondeterministic_fields(child, f"{path}[{index}]")
    elif isinstance(value, str) and ABSOLUTE_PATH.match(value):
        raise MetadataError(f"Absolute path is forbidden in metadata: {path}")


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
        raise MetadataError("SBOM root must be an object")
    if document.get("$schema") != CYCLONEDX_SCHEMA:
        raise MetadataError("SBOM must use the CycloneDX 1.6 schema")
    if document.get("bomFormat") != "CycloneDX" or document.get("specVersion") != "1.6":
        raise MetadataError("SBOM format must be CycloneDX 1.6")
    if document.get("version") != 1:
        raise MetadataError("SBOM version must be 1")
    components = document.get("components")
    dependencies = document.get("dependencies")
    metadata = document.get("metadata")
    if not isinstance(components, list) or not isinstance(dependencies, list):
        raise MetadataError("SBOM components and dependencies fields must be arrays")
    if not isinstance(metadata, dict) or not isinstance(metadata.get("component"), dict):
        raise MetadataError("SBOM requires metadata.component")
    component_refs: list[str] = []
    for component in components:
        if not isinstance(component, dict):
            raise MetadataError("Every SBOM component must be an object")
        reference = component.get("bom-ref")
        if not isinstance(reference, str) or not isinstance(component.get("name"), str):
            raise MetadataError("Every SBOM component requires bom-ref and name")
        if not isinstance(component.get("version"), str) or component.get("type") != "library":
            raise MetadataError(
                "Every SBOM dependency component requires type and version"
            )
        if component.get("purl") != reference:
            raise MetadataError(
                "Every SBOM dependency component requires a matching purl"
            )
        component_refs.append(reference)
        properties = component.get("properties", [])
        if not properties_are_sorted(properties):
            raise MetadataError("SBOM component properties are not sorted")
    if component_refs != sorted(component_refs) or len(component_refs) != len(set(component_refs)):
        raise MetadataError("SBOM components are unsorted or contain duplicates")

    root_ref = metadata["component"].get("bom-ref")
    if not isinstance(root_ref, str):
        raise MetadataError("SBOM root component requires bom-ref")
    root_component = metadata["component"]
    root_version = root_component.get("version")
    validate_full_hash(
        root_version if isinstance(root_version, str) else "", "SBOM release commit"
    )
    if expected_commit is not None and root_version != expected_commit:
        raise MetadataError("SBOM release commit does not match the release candidate")
    if root_component.get("name") != "guard-daemon" or root_component.get("type") != "application":
        raise MetadataError("SBOM root component must describe guard-daemon")
    if root_ref != package_url("generic", "guard-daemon", root_version):
        raise MetadataError("SBOM root component reference is invalid")
    if root_component.get("purl") != root_ref:
        raise MetadataError("SBOM root component purl is invalid")
    root_properties = root_component.get("properties")
    if not properties_are_sorted(root_properties):
        raise MetadataError("SBOM root component properties are not sorted")
    property_map = {item["name"]: item["value"] for item in root_properties}
    if len(root_properties) != 2 or set(property_map) != {
        "guard-daemon:go:module",
        "guard-daemon:releaseCommit",
    }:
        raise MetadataError("SBOM root component properties are invalid")
    if property_map["guard-daemon:releaseCommit"] != root_version or not isinstance(
        property_map["guard-daemon:go:module"], str
    ):
        raise MetadataError("SBOM root component properties do not match its version")
    known_refs = set(component_refs) | {root_ref}
    dependency_refs: list[str] = []
    for dependency in dependencies:
        if not isinstance(dependency, dict) or set(dependency) != {"dependsOn", "ref"}:
            raise MetadataError("Invalid SBOM dependency entry")
        reference = dependency["ref"]
        depends_on = dependency["dependsOn"]
        if reference not in known_refs or not isinstance(depends_on, list):
            raise MetadataError("SBOM dependency references an unknown component")
        if depends_on != sorted(set(depends_on)) or not set(depends_on).issubset(known_refs):
            raise MetadataError(
                "SBOM dependency targets are unsorted or contain duplicates"
            )
        dependency_refs.append(reference)
    if dependency_refs != sorted(dependency_refs) or len(dependency_refs) != len(
        set(dependency_refs)
    ):
        raise MetadataError("SBOM dependencies are unsorted or contain duplicates")
    assert_no_nondeterministic_fields(document)
    if path.read_bytes() != canonical_pretty(document):
        raise MetadataError("JSON SBOM is not serialized canonically")


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
        raise MetadataError("Release candidate does not match schema version 1")
    if document["schemaVersion"] != "1" or document["target"] != TARGET:
        raise MetadataError("Release candidate schemaVersion or target field is invalid")
    validate_full_hash(document.get("releaseCommit", ""), "release commit")
    validate_full_hash(document.get("releaseTree", ""), "release tree")
    artifacts = document.get("artifacts")
    if not isinstance(artifacts, dict) or set(artifacts) != set(ARTIFACT_PATHS):
        raise MetadataError("Release candidate has an invalid artifacts object")
    for field, expected_path in ARTIFACT_PATHS.items():
        record = artifacts[field]
        if not isinstance(record, dict) or set(record) != {"path", "sha256"}:
            raise MetadataError(f"Invalid release candidate artifact: {field}")
        if record["path"] != expected_path or record["sha256"] != (
            f"sha256:{sha256_file(directory / expected_path)}"
        ):
            raise MetadataError(
                f"Release candidate artifact does not match the output file: {field}"
            )
    assert_no_nondeterministic_fields(document)
    if path.read_bytes() != canonical_pretty(document):
        raise MetadataError(
            "JSON release candidate is not serialized canonically"
        )
    return document


def verify_provenance(directory: Path, candidate: dict[str, Any]) -> None:
    path = directory / "guard-daemon.intoto.jsonl"
    try:
        content = path.read_bytes()
    except OSError as error:
        raise MetadataError(f"Failed to read provenance: {error}") from error
    if not content.endswith(b"\n") or content.count(b"\n") != 1:
        raise MetadataError("Provenance must contain exactly one JSON line")
    document = load_json(path)
    if not isinstance(document, dict) or set(document) != {
        "_type",
        "predicate",
        "predicateType",
        "subject",
    }:
        raise MetadataError("Provenance is not an in-toto Statement document")
    if document["_type"] != INTOTO_STATEMENT or document["predicateType"] != SLSA_PROVENANCE:
        raise MetadataError(
            "Provenance must conform to in-toto Statement v1 and "
            "SLSA Provenance v1"
        )
    expected_subjects = [
        {"digest": {"sha256": sha256_file(directory / name)}, "name": name}
        for name in PROVENANCE_SUBJECTS
    ]
    if document["subject"] != expected_subjects:
        raise MetadataError(
            "Provenance subject field does not match the release output files"
        )
    predicate = document.get("predicate")
    if not isinstance(predicate, dict) or set(predicate) != {"buildDefinition", "runDetails"}:
        raise MetadataError("Invalid predicate field in SLSA Provenance")
    definition = predicate["buildDefinition"]
    run_details = predicate["runDetails"]
    if not isinstance(definition, dict) or not isinstance(run_details, dict):
        raise MetadataError("Invalid SLSA Provenance structure")
    if set(definition) != {
        "buildType",
        "externalParameters",
        "internalParameters",
        "resolvedDependencies",
    }:
        raise MetadataError("Invalid buildDefinition schema in SLSA Provenance")
    external = definition.get("externalParameters")
    if external != {
        "releaseCommit": candidate["releaseCommit"],
        "releaseTree": candidate["releaseTree"],
        "target": TARGET,
    }:
        raise MetadataError(
            "Provenance externalParameters field does not match the release candidate"
        )
    if definition.get("buildType") != BUILD_TYPE or definition.get("internalParameters") != {}:
        raise MetadataError("Invalid buildDefinition field in provenance")
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
            "Invalid resolvedDependencies field in provenance"
        )
    if run_details != {"builder": {"id": BUILDER_ID}, "metadata": {}}:
        raise MetadataError(
            "Provenance runDetails field contains invalid deterministic data"
        )
    assert_no_nondeterministic_fields(document)
    if content != canonical_compact(document):
        raise MetadataError("Provenance is not serialized canonically")


def verify_release(directory: Path) -> None:
    require_names(directory, OUTPUT_NAMES)
    expected_checksums = checksum_content(directory)
    try:
        actual_checksums = (directory / "SHA256SUMS").read_bytes()
    except OSError as error:
        raise MetadataError(f"Failed to read SHA256SUMS: {error}") from error
    if actual_checksums != expected_checksums:
        raise MetadataError(
            "SHA256SUMS is invalid, unsorted, or does not match the release output files"
        )
    candidate = verify_candidate(directory)
    verify_sbom(directory / "guard-daemon.cdx.json", candidate["releaseCommit"])
    verify_provenance(directory, candidate)


def create_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)

    sbom = commands.add_parser("sbom", help="create a CycloneDX 1.6 SBOM")
    sbom.add_argument("--package-lock", type=Path, required=True)
    sbom.add_argument("--go-modules", type=Path, required=True)
    sbom.add_argument("--go-graph", type=Path, required=True)
    sbom.add_argument("--release-commit", required=True)
    sbom.add_argument("--output", type=Path, required=True)

    candidate = commands.add_parser("candidate", help="create release candidate metadata")
    candidate.add_argument("--directory", type=Path, required=True)
    candidate.add_argument("--release-commit", required=True)
    candidate.add_argument("--release-tree", required=True)
    candidate.add_argument("--output", type=Path, required=True)

    provenance = commands.add_parser(
        "provenance", help="create an unsigned SLSA Provenance document"
    )
    provenance.add_argument("--directory", type=Path, required=True)
    provenance.add_argument("--builder-script", type=Path, required=True)
    provenance.add_argument("--release-commit", required=True)
    provenance.add_argument("--release-tree", required=True)
    provenance.add_argument("--output", type=Path, required=True)

    checksums = commands.add_parser(
        "checksums", help="create a sorted SHA256SUMS file"
    )
    checksums.add_argument("--directory", type=Path, required=True)
    checksums.add_argument("--output", type=Path, required=True)

    verify = commands.add_parser(
        "verify", help="verify the complete release output file set"
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
            raise MetadataError(f"Unsupported command: {args.command}")
    except MetadataError as error:
        print(f"Release metadata error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
