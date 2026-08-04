import io
import json
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))

import release_metadata as metadata  # noqa: E402


COMMIT = "a" * 40
TREE = "b" * 40


def lock_fixture() -> dict:
    return {
        "lockfileVersion": 3,
        "name": "fixture",
        "packages": {
            "": {
                "dependencies": {"z-package": "2.0.0"},
                "devDependencies": {"a-package": "1.0.0"},
                "name": "fixture",
                "version": "1.0.0",
            },
            "node_modules/z-package": {
                "dependencies": {"a-package": "1.0.0"},
                "version": "2.0.0",
            },
            "node_modules/a-package": {"dev": True, "version": "1.0.0"},
        },
    }


def go_fixture() -> str:
    return """{"Path":"guard-daemon","Main":true,"Dir":"/private/build/guard-daemon"}
{"Path":"z.example/module","Version":"v2.0.0","Dir":"/private/go/z","Replace":{"Path":"replacement.example/module","Version":"v2.1.0"}}
{"Path":"a.example/module","Version":"v1.0.0","GoMod":"/private/go/a/go.mod"}
"""


def go_graph_fixture() -> str:
    return """guard-daemon z.example/module@v2.0.0
replacement.example/module@v2.1.0 a.example/module@v1.0.0
z.example/module@v2.0.0 go@1.26.0
"""


def go_toolchain_graph_fixture() -> str:
    return "go@1.26.0 toolchain@go1.26.0\n"


def go_mvs_fixture() -> str:
    return """{"Path":"guard-daemon","Main":true}
{"Path":"source.example/module","Version":"v2.0.0"}
{"Path":"old-edge.example/module","Version":"v1.0.0"}
{"Path":"selected-edge.example/module","Version":"v2.0.0"}
"""


def go_mvs_graph_fixture() -> str:
    return """guard-daemon source.example/module@v2.0.0
source.example/module@v1.0.0 old-edge.example/module@v1.0.0
source.example/module@v2.0.0 selected-edge.example/module@v1.0.0
"""


class ReleaseMetadataTests(unittest.TestCase):
    def write_inputs(self, directory: Path) -> tuple[Path, Path, Path]:
        package_lock = directory / "package-lock.json"
        package_lock.write_text(json.dumps(lock_fixture()), encoding="utf-8")
        go_modules = directory / "go-modules.json"
        go_modules.write_text(go_fixture(), encoding="utf-8")
        go_graph = directory / "go-graph.txt"
        go_graph.write_text(go_graph_fixture(), encoding="utf-8")
        return package_lock, go_modules, go_graph

    def write_source_archive(self, directory: Path, builder_content: bytes) -> None:
        archive_path = directory / "guard-daemon-source.tar"
        member = tarfile.TarInfo(
            f"guard-daemon-{COMMIT}/{metadata.BUILDER_SCRIPT_NAME}"
        )
        member.mode = 0o755
        member.mtime = 0
        member.size = len(builder_content)
        with tarfile.open(archive_path, mode="w", format=tarfile.USTAR_FORMAT) as archive:
            archive.addfile(member, io.BytesIO(builder_content))

    def write_release(self, directory: Path) -> None:
        package_lock, go_modules, go_graph = self.write_inputs(directory)
        for name in (
            "guard-daemon-linux-amd64",
            "LICENSE",
            "RescuerV2.json",
            "rescuer-manifest.schema.json",
        ):
            (directory / name).write_bytes(f"fixture:{name}\n".encode())
        builder_content = b"#!/usr/bin/env bash\nexit 0\n"
        self.write_source_archive(directory, builder_content)
        builder_script = directory / "builder-script.sh"
        builder_script.write_bytes(builder_content)
        metadata.write_sbom(
            package_lock,
            go_modules,
            go_graph,
            COMMIT,
            directory / "guard-daemon.cdx.json",
        )
        package_lock.unlink()
        go_modules.unlink()
        go_graph.unlink()
        metadata.write_candidate(
            directory,
            COMMIT,
            TREE,
            directory / "release-candidate.json",
        )
        metadata.write_provenance(
            directory,
            COMMIT,
            TREE,
            builder_script,
            directory / "guard-daemon.intoto.jsonl",
        )
        builder_script.unlink()
        metadata.write_checksums(directory, directory / "SHA256SUMS")

    def test_two_generations_are_identical_and_components_are_sorted(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            first = root / "first"
            second = root / "second"
            first.mkdir()
            second.mkdir()
            self.write_release(first)
            self.write_release(second)

            for name in metadata.OUTPUT_NAMES:
                with self.subTest(name=name):
                    self.assertEqual((first / name).read_bytes(), (second / name).read_bytes())
            document = json.loads((first / "guard-daemon.cdx.json").read_text(encoding="utf-8"))
            references = [component["bom-ref"] for component in document["components"]]
            self.assertEqual(references, sorted(references))

    def test_go_graph_preserves_root_replacement_and_transitive_edge(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            package_lock, go_modules, go_graph = self.write_inputs(directory)
            document = metadata.build_sbom(package_lock, go_modules, go_graph, COMMIT)
            dependencies = {
                entry["ref"]: entry["dependsOn"] for entry in document["dependencies"]
            }
            root_ref = metadata.package_url("generic", "guard-daemon", COMMIT)
            z_ref = metadata.package_url(
                "golang", "replacement.example/module", "v2.1.0"
            )
            a_ref = metadata.package_url("golang", "a.example/module", "v1.0.0")
            components = {
                component["bom-ref"]: component for component in document["components"]
            }

            self.assertIn(z_ref, dependencies[root_ref])
            self.assertNotIn(a_ref, dependencies[root_ref])
            self.assertEqual(dependencies[z_ref], [a_ref])
            self.assertEqual(
                components[z_ref]["properties"],
                [
                    {
                        "name": "guard-daemon:go:replaces",
                        "value": "z.example/module@v2.0.0",
                    }
                ],
            )

    def test_go_graph_ignores_go_toolchain_edge(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            graph = Path(temporary) / "go-graph.txt"
            graph.write_text(go_toolchain_graph_fixture(), encoding="utf-8")
            root_ref = metadata.package_url("generic", "guard-daemon", COMMIT)

            dependencies = metadata.go_dependencies(
                graph, {}, {}, "guard-daemon", root_ref
            )

            self.assertEqual(dependencies[root_ref], set())

    def test_go_graph_ignores_edges_from_non_selected_source_version(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            package_lock = directory / "package-lock.json"
            package_lock.write_text(json.dumps(lock_fixture()), encoding="utf-8")
            go_modules = directory / "go-modules.json"
            go_modules.write_text(go_mvs_fixture(), encoding="utf-8")
            go_graph = directory / "go-graph.txt"
            go_graph.write_text(go_mvs_graph_fixture(), encoding="utf-8")

            document = metadata.build_sbom(package_lock, go_modules, go_graph, COMMIT)
            dependencies = {
                entry["ref"]: entry["dependsOn"] for entry in document["dependencies"]
            }
            source_ref = metadata.package_url(
                "golang", "source.example/module", "v2.0.0"
            )
            old_edge_ref = metadata.package_url(
                "golang", "old-edge.example/module", "v1.0.0"
            )
            selected_edge_ref = metadata.package_url(
                "golang", "selected-edge.example/module", "v2.0.0"
            )
            go_component_refs = {
                component["bom-ref"]
                for component in document["components"]
                if component["purl"].startswith("pkg:golang/")
            }

            self.assertEqual(dependencies[source_ref], [selected_edge_ref])
            self.assertEqual(
                go_component_refs,
                {source_ref, old_edge_ref, selected_edge_ref},
            )

    def test_go_graph_rejects_unknown_target_from_non_selected_source_version(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            package_lock = directory / "package-lock.json"
            package_lock.write_text(json.dumps(lock_fixture()), encoding="utf-8")
            go_modules = directory / "go-modules.json"
            go_modules.write_text(go_mvs_fixture(), encoding="utf-8")
            go_graph = directory / "go-graph.txt"
            go_graph.write_text(
                "source.example/module@v1.0.0 unknown.example/module@v1.0.0\n",
                encoding="utf-8",
            )

            with self.assertRaisesRegex(
                metadata.MetadataError,
                "go mod graph ссылается на неизвестный модуль: unknown.example/module",
            ):
                metadata.build_sbom(package_lock, go_modules, go_graph, COMMIT)

    def test_provenance_records_executed_builder_digest(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            self.write_release(directory)
            provenance = json.loads(
                (directory / "guard-daemon.intoto.jsonl").read_text(encoding="utf-8")
            )
            dependencies = provenance["predicate"]["buildDefinition"][
                "resolvedDependencies"
            ]
            expected = metadata.sha256_archive_member(
                directory / "guard-daemon-source.tar",
                f"guard-daemon-{COMMIT}/{metadata.BUILDER_SCRIPT_NAME}",
            )
            self.assertEqual(dependencies[0]["digest"], {"sha256": expected})

    def test_generated_metadata_has_no_timestamps_or_absolute_paths(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            self.write_release(directory)
            generated = b"".join(
                (directory / name).read_bytes()
                for name in (
                    "guard-daemon.cdx.json",
                    "release-candidate.json",
                    "guard-daemon.intoto.jsonl",
                )
            )
            self.assertNotIn(b"timestamp", generated.lower())
            self.assertNotIn(str(directory).encode(), generated)
            self.assertNotIn(b"/private/", generated)

    def test_full_commit_validation_rejects_noncanonical_values(self) -> None:
        invalid = (
            "",
            "a" * 39,
            "a" * 41,
            "A" * 40,
            "g" * 40,
            "HEAD",
        )
        for value in invalid:
            with self.subTest(value=value), self.assertRaises(metadata.MetadataError):
                metadata.validate_full_hash(value, "release commit")
        self.assertEqual(metadata.validate_full_hash(COMMIT, "release commit"), COMMIT)

    def test_candidate_uses_exact_schema_and_contract_path(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            for name in metadata.ARTIFACT_PATHS.values():
                (directory / name).write_bytes(name.encode())

            candidate = metadata.build_candidate(directory, COMMIT, TREE)
            expected_artifacts = {
                field: {
                    "path": name,
                    "sha256": f"sha256:{metadata.sha256_file(directory / name)}",
                }
                for field, name in metadata.ARTIFACT_PATHS.items()
            }
            self.assertEqual(
                candidate,
                {
                    "artifacts": expected_artifacts,
                    "releaseCommit": COMMIT,
                    "releaseTree": TREE,
                    "schemaVersion": "1",
                    "target": {
                        "cgoEnabled": False,
                        "goarch": "amd64",
                        "goos": "linux",
                    },
                },
            )
            self.assertEqual(candidate["artifacts"]["contract"]["path"], "RescuerV2.json")

    def test_checksums_use_c_order_and_exclude_themselves(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            for name in metadata.CONTENT_NAMES:
                (directory / name).write_bytes(name.encode())
            output = directory / "SHA256SUMS"
            metadata.write_checksums(directory, output)

            lines = output.read_text(encoding="ascii").splitlines()
            names = [line.split("  ", 1)[1] for line in lines]
            self.assertEqual(names, sorted(metadata.CONTENT_NAMES))
            self.assertNotIn("SHA256SUMS", names)
            first = output.read_bytes()
            metadata.write_checksums(directory, output)
            self.assertEqual(output.read_bytes(), first)

    def test_verify_rejects_unknown_output_file(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            self.write_release(directory)
            metadata.verify_release(directory)
            (directory / "unexpected.log").write_text("not allowed\n", encoding="utf-8")
            with self.assertRaisesRegex(
                metadata.MetadataError, "неизвестные файлы: unexpected.log"
            ):
                metadata.verify_release(directory)


if __name__ == "__main__":
    unittest.main()
