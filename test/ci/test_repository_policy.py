import unittest
from copy import deepcopy
from pathlib import Path

from dependency_licenses import validate_dependency_changes
from repository_policy import (
    PolicyError,
    parse_yaml,
    path_violation,
    validate_dependency_review,
    validate_dependabot,
    validate_workflow,
)


SHA = "1" * 40


def workflow_fixture(extra_step: str = "", comments: str = "") -> str:
    return f"""name: Example Check
{comments}
on:
  pull_request:
  push:
    branches: [main]
permissions:
  contents: read
concurrency:
  group: fixture
  cancel-in-progress: true
jobs:
  test:
    name: Example Check
    runs-on: ubuntu-24.04
    steps:
      - name: Check out source
        uses: actions/checkout@{SHA}
        with:
          persist-credentials: false
{extra_step}
"""


class StructuralYamlTests(unittest.TestCase):
    def test_base_loader_preserves_on_and_ignores_dangerous_comments(self) -> None:
        document = parse_yaml(
            workflow_fixture(
                comments="# pull_request_target: ${{secrets.KEY}}\n# uses: bad/action@main"
            ),
            "example",
        )
        self.assertIn("on", document)
        self.assertEqual(validate_workflow(document, "example"), [])

    def test_comments_cannot_supply_missing_structure(self) -> None:
        document = parse_yaml(
            workflow_fixture(
                comments="# pull_request:\n# permissions:\n#   contents: read"
            )
            .replace("  pull_request:\n", "")
            .replace("permissions:\n  contents: read\n", ""),
            "example",
        )
        self.assertTrue(
            any("triggers are required" in issue for issue in validate_workflow(document, "example"))
        )
        self.assertTrue(
            any("root `permissions`" in issue for issue in validate_workflow(document, "example"))
        )

    def test_duplicate_keys_and_merge_keys_fail_closed(self) -> None:
        with self.assertRaises(PolicyError):
            parse_yaml("name: one\nname: two\n", "duplicate")
        with self.assertRaises(PolicyError):
            parse_yaml(
                "defaults: &defaults\n  value: one\nroot:\n  <<: *defaults\n",
                "merge",
            )

    def test_secret_expression_forms_and_environment_are_rejected(self) -> None:
        for expression in ("${{secrets.KEY}}", "${{ secrets['KEY'] }}"):
            document = parse_yaml(
                workflow_fixture(
                    extra_step=f"""      - name: Dangerous step
        run: command
        env:
          VALUE: "{expression}"
"""
                ),
                "example",
            )
            self.assertTrue(
                any("`secrets` expression" in issue for issue in validate_workflow(document, "example"))
            )
        document = parse_yaml(
            workflow_fixture().replace(
                "runs-on: ubuntu-24.04", "environment: production\n    runs-on: ubuntu-24.04"
            ),
            "example",
        )
        self.assertTrue(any("environment" in issue for issue in validate_workflow(document, "example")))

    def test_floating_action_and_checkout_credentials_are_rejected(self) -> None:
        document = parse_yaml(
            workflow_fixture().replace(f"actions/checkout@{SHA}", "actions/checkout@main"),
            "example",
        )
        issues = validate_workflow(document, "example")
        self.assertTrue(any("40-character SHA" in issue for issue in issues))

        document = parse_yaml(
            workflow_fixture().replace(
                "persist-credentials: false", "persist-credentials: true"
            ),
            "example",
        )
        self.assertTrue(
            any(
                "persist-credentials: false" in issue
                for issue in validate_workflow(document, "example")
            )
        )

    def test_unsafe_trigger_concurrency_and_rpc_input_are_rejected(self) -> None:
        document = parse_yaml(workflow_fixture(), "example")
        document["on"]["pull_request_target"] = ""
        document["concurrency"]["cancel-in-progress"] = "false"
        document["jobs"]["test"]["steps"].append(
            {
                "name": "Dangerous RPC",
                "run": "command ${{ vars['RPC_URL'] }} --rpc-url https://mainnet.invalid",
            }
        )
        issues = validate_workflow(document, "example")
        self.assertTrue(any("forbidden triggers" in issue for issue in issues))
        self.assertTrue(any("cancel-in-progress" in issue for issue in issues))
        self.assertTrue(any("RPC" in issue for issue in issues))


class PathPolicyTests(unittest.TestCase):
    def test_case_insensitive_secret_and_operational_paths_are_rejected(self) -> None:
        forbidden = (
            ".ENV",
            "nested/wallet.KEY",
            ".ssh/id_ed25519",
            "nested/ARTIFACTS/contracts/RescuerV2.json",
            "nested/STATE/ledger.db",
            "test/ci/__PYCACHE__/policy.PYC",
            "keys/UTC--test",
        )
        for path in forbidden:
            with self.subTest(path=path):
                self.assertIsNotNone(path_violation(path))

    def test_exact_public_files_are_allowed(self) -> None:
        self.assertIsNone(path_violation(".env.example"))
        self.assertIsNone(path_violation("artifacts/contracts/RescuerV2.json"))


class DependabotTests(unittest.TestCase):
    def test_comments_do_not_trigger_auto_merge_and_structure_is_required(self) -> None:
        updates = []
        for ecosystem in ("gomod", "npm", "github-actions"):
            updates.append(
                f"""
  - package-ecosystem: {ecosystem}
    directory: /
    schedule:
      interval: weekly
    groups:
      safe:
        patterns: ['*']
        update-types: [minor, patch]
"""
            )
        config = parse_yaml(
            "version: 2\n# enable-auto-merge: true\nupdates:\n" + "".join(updates),
            "dependabot",
        )
        self.assertEqual(validate_dependabot(config, "dependabot"), [])
        config["updates"][0]["auto-merge"] = "true"
        self.assertTrue(
            any(
                "automatic merge" in issue
                for issue in validate_dependabot(config, "dependabot")
            )
        )
        config["updates"][1]["directory"] = "/nested"
        config["updates"][2]["package-ecosystem"] = "npm"
        issues = validate_dependabot(config, "dependabot")
        self.assertTrue(any("root directory" in issue for issue in issues))
        self.assertTrue(any("unique" in issue for issue in issues))


class DependencyReviewPolicyTests(unittest.TestCase):
    def test_actual_options_pass_and_deny_list_mutation_fails(self) -> None:
        workflow = parse_yaml(
            Path(".github/workflows/security.yml").read_text("utf-8"),
            "security",
        )
        self.assertEqual(validate_dependency_review(workflow, "security"), [])

        changed = deepcopy(workflow)
        steps = changed["jobs"]["dependency-review"]["steps"]
        review = next(step for step in steps if step.get("id") == "dependency-review")
        review["with"]["deny-licenses"] = "GPL-3.0-only"
        self.assertTrue(
            any(
                "dependency review options must fail closed" in issue
                for issue in validate_dependency_review(changed, "security")
            )
        )


class DependencyLicenseTests(unittest.TestCase):
    def test_missing_and_unknown_added_licenses_fail_closed(self) -> None:
        changes = [
            {"change_type": "added", "license": None},
            {"change_type": "added", "license": "NOASSERTION"},
            {"change_type": "added", "license": "LicenseRef-clearlydefined-OTHER"},
        ]
        self.assertEqual(len(validate_dependency_changes(changes)), 3)

    def test_known_added_and_unknown_removed_licenses_are_allowed(self) -> None:
        changes = [
            {"change_type": "added", "license": "MIT"},
            {"change_type": "removed", "license": None},
        ]
        self.assertEqual(validate_dependency_changes(changes), [])


if __name__ == "__main__":
    unittest.main()
