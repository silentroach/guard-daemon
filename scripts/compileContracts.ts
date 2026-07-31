import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

import solc from "solc";

interface CompilerDiagnostic {
  readonly formattedMessage: string;
  readonly severity: string;
}

interface CompilerContract {
  readonly abi: readonly unknown[];
  readonly evm: {
    readonly bytecode: { readonly object: string };
    readonly deployedBytecode: { readonly object: string };
  };
}

interface CompilerOutput {
  readonly contracts?: Readonly<
    Record<string, Readonly<Record<string, CompilerContract>>>
  >;
  readonly errors?: readonly CompilerDiagnostic[];
}

const compilerVersion = "0.8.36";
const contracts = [
  { contractName: "RescuerV2", sourceName: "RescuerV2.sol" },
] as const;

const compilerSettings = {
  evmVersion: "prague",
  metadata: {
    appendCBOR: false,
    bytecodeHash: "none",
    useLiteralContent: true,
  },
  optimizer: {
    enabled: true,
    runs: 200,
  },
  outputSelection: {
    "*": {
      "*": ["abi", "evm.bytecode.object", "evm.deployedBytecode.object"],
    },
  },
} as const;

export const compileContracts = (
  outputDirectory: string,
): readonly string[] => {
  const actualCompilerVersion = solc.version();
  if (!actualCompilerVersion.startsWith(`${compilerVersion}+commit.`)) {
    throw new Error(
      `Ожидался solc ${compilerVersion}, получен ${actualCompilerVersion}`,
    );
  }

  const sources = Object.fromEntries(
    contracts.map(({ sourceName }) => [
      sourceName,
      { content: readFileSync(resolve("contracts", sourceName), "utf8") },
    ]),
  );
  const input = {
    language: "Solidity",
    settings: compilerSettings,
    sources,
  };
  const output = JSON.parse(
    solc.compile(JSON.stringify(input)),
  ) as CompilerOutput;
  const errors = output.errors?.filter(({ severity }) => severity === "error");
  if (errors?.length) {
    throw new Error(
      errors.map(({ formattedMessage }) => formattedMessage).join("\n"),
    );
  }

  rmSync(outputDirectory, { force: true, recursive: true });
  mkdirSync(outputDirectory, { recursive: true });
  return contracts.map(({ contractName, sourceName }) => {
    const contract = output.contracts?.[sourceName]?.[contractName];
    if (!contract) {
      throw new Error(`Компилятор не вернул ${sourceName}:${contractName}`);
    }

    const artifact = {
      abi: contract.abi,
      bytecode: `0x${contract.evm.bytecode.object}`,
      compilerVersion: actualCompilerVersion,
      contractName,
      deployedBytecode: `0x${contract.evm.deployedBytecode.object}`,
      settings: compilerSettings,
      sourceName,
    };
    const artifactPath = resolve(outputDirectory, `${contractName}.json`);
    writeFileSync(artifactPath, `${JSON.stringify(artifact, null, 2)}\n`, {
      encoding: "utf8",
      mode: 0o600,
    });
    return artifactPath;
  });
};

const scriptPath = process.argv[1];
if (scriptPath && import.meta.url === pathToFileURL(resolve(scriptPath)).href) {
  const outputDirectory = resolve("build/contracts");
  const artifactPaths = compileContracts(outputDirectory);
  console.log(`Скомпилировано контрактов: ${artifactPaths.length}`);
  console.log(`Артефакты: ${outputDirectory}`);
}
