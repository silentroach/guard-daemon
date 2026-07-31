import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

import { compileContracts } from "./compileContracts.js";

export const verifyCanonicalArtifact = async (
  artifactPath = resolve("artifacts/contracts/RescuerV2.json"),
): Promise<void> => {
  const temporaryDirectory = await mkdtemp(join(tmpdir(), "guard-artifacts-"));
  try {
    const [generatedPath] = compileContracts(temporaryDirectory);
    if (!generatedPath) {
      throw new Error("Компилятор не создал canonical artifact");
    }
    const [generated, tracked] = await Promise.all([
      readFile(generatedPath),
      readFile(artifactPath),
    ]);
    if (!generated.equals(tracked)) {
      throw new Error(
        "Canonical artifact не совпадает с pinned source/compiler/settings",
      );
    }
  } finally {
    await rm(temporaryDirectory, { force: true, recursive: true });
  }
};

const scriptPath = process.argv[1];
if (scriptPath && import.meta.url === pathToFileURL(resolve(scriptPath)).href) {
  await verifyCanonicalArtifact();
  console.log("Canonical artifact воспроизводим и соответствует source tree.");
}
