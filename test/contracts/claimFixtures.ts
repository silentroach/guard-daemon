import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import solc from "solc";

interface CompilerOutput {
  readonly contracts?: Readonly<
    Record<
      string,
      Readonly<
        Record<
          string,
          {
            readonly evm: {
              readonly deployedBytecode: { readonly object: string };
            };
          }
        >
      >
    >
  >;
  readonly errors?: readonly {
    readonly severity: string;
    readonly formattedMessage: string;
  }[];
}

export const compileClaimFixtures = (): Readonly<Record<string, string>> => {
  const source = "test/contracts/mocks/RescuerV2Mocks.sol";
  const output = JSON.parse(
    solc.compile(
      JSON.stringify({
        language: "Solidity",
        sources: {
          [source]: { content: readFileSync(source, "utf8") },
          "contracts/RescuerV2.sol": {
            content: readFileSync("contracts/RescuerV2.sol", "utf8"),
          },
        },
        settings: {
          evmVersion: "prague",
          optimizer: { enabled: true, runs: 200 },
          metadata: { appendCBOR: false, bytecodeHash: "none" },
          outputSelection: {
            [source]: {
              StandardToken: ["evm.deployedBytecode.object"],
              ClaimTarget: ["evm.deployedBytecode.object"],
            },
          },
        },
      }),
    ),
  ) as CompilerOutput;
  const errors = output.errors?.filter((error) => error.severity === "error");
  if (errors?.length)
    throw new Error(errors.map((error) => error.formattedMessage).join("\n"));
  const contracts = output.contracts?.[source];
  const token = contracts?.["StandardToken"];
  const claim = contracts?.["ClaimTarget"];
  if (!token || !claim)
    throw new Error("Claim test fixtures were not compiled");
  return {
    StandardToken: "0x" + token.evm.deployedBytecode.object,
    ClaimTarget: "0x" + claim.evm.deployedBytecode.object,
  };
};

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(resolve(process.argv[1])).href
) {
  writeFileSync(
    "internal/rescue/testdata/claim-contracts.json",
    JSON.stringify(compileClaimFixtures(), undefined, 2) + "\n",
  );
}
