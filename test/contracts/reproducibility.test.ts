import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import {
  copyFile,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  rm,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { basename, join, resolve } from "node:path";
import test from "node:test";

import { compileContracts } from "../../scripts/compileContracts.js";

interface ContractArtifact {
  readonly abi: readonly { readonly name?: string; readonly type: string }[];
}

const artifactDigests = async (
  directory: string,
): Promise<Readonly<Record<string, string>>> => {
  const fileNames = (await readdir(directory)).sort();
  const entries = await Promise.all(
    fileNames.map(async (fileName) => {
      const content = await readFile(join(directory, fileName));
      const digest = createHash("sha256").update(content).digest("hex");
      return [fileName, digest] as const;
    }),
  );
  return Object.fromEntries(entries);
};

test("recompilation creates identical artifacts", async () => {
  const temporaryDirectory = await mkdtemp(join(tmpdir(), "guard-contracts-"));
  const firstDirectory = join(temporaryDirectory, "first");
  const secondDirectory = join(temporaryDirectory, "second");
  const firstSource = join(temporaryDirectory, "first-source");
  const secondSource = join(temporaryDirectory, "second-source");

  try {
    await Promise.all([mkdir(firstSource), mkdir(secondSource)]);
    await Promise.all([
      copyFile(
        resolve("contracts/RescuerV2.sol"),
        join(firstSource, "RescuerV2.sol"),
      ),
      copyFile(
        resolve("contracts/RescuerV2.sol"),
        join(secondSource, "RescuerV2.sol"),
      ),
    ]);
    compileContracts(firstDirectory, firstSource);
    compileContracts(secondDirectory, secondSource);

    assert.deepStrictEqual(
      await artifactDigests(firstDirectory),
      await artifactDigests(secondDirectory),
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});

test("the production artifact contains only the allowed Rescuer ABI", async () => {
  const temporaryDirectory = await mkdtemp(join(tmpdir(), "guard-contracts-"));

  try {
    const artifactPaths = compileContracts(temporaryDirectory);
    const artifactPath = artifactPaths[0];
    if (!artifactPath) {
      assert.fail("Compiler did not create the RescuerV2 artifact");
    }
    const artifact = JSON.parse(
      await readFile(artifactPath, "utf8"),
    ) as ContractArtifact;
    const names = artifact.abi.flatMap(({ name }) => (name ? [name] : []));

    assert.deepStrictEqual(
      {
        artifacts: artifactPaths.map((artifactPath) => basename(artifactPath)),
        hasDestination: names.includes("destination"),
        hasLegacyPermitEntry: names.includes("permitAndTransfer"),
        hasSponsor: names.includes("sponsor"),
      },
      {
        artifacts: ["RescuerV2.json"],
        hasDestination: true,
        hasLegacyPermitEntry: false,
        hasSponsor: true,
      },
    );
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
});
