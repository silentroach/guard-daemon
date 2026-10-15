import assert from "node:assert/strict";
import test from "node:test";
import { toBeHex } from "ethers";
import { estimateBaseDeploymentFees } from "../../../scripts/baseFees.js";
import { parseCLIOptions } from "../../../scripts/deployRescuerV2.js";

test("Base deployment requires explicit estimated fee mode", () => {
  const args = [
    "--chain-id",
    "8453",
    "--destination",
    "0x1000000000000000000000000000000000000001",
    "--sponsor",
    "0x2000000000000000000000000000000000000002",
    "--manifest",
    "/manifest.json",
    "--release-candidate",
    "/release-candidate.json",
    "--gas-limit",
    "1000000",
    "--max-fee-per-gas-wei",
    "2000000000",
    "--max-priority-fee-per-gas-wei",
    "100000000",
    "--max-total-cost-wei",
    "3000000000000000",
    "--broadcast",
  ];
  assert.throws(() => parseCLIOptions(args), /requires --fee-mode estimated/);
  const options = parseCLIOptions([...args, "--fee-mode", "estimated"], {
    DEPLOYMENT_RPC_URL: "https://rpc.example.invalid",
  });
  assert.equal(options.chainId, 8453n);
});

test("Base deployment estimates L1 and operator fees at the same block", async () => {
  const hash = "0x" + "12".repeat(32);
  const fake: Parameters<typeof estimateBaseDeploymentFees>[0] = {
    getBlock: async () => ({ hash }),
    call: async (request) => {
      assert.equal(request.blockTag, hash);
      return toBeHex(
        request.data?.toString().startsWith("0xf1c7a58b") ? 7n : 3n,
        32,
      );
    },
  };
  assert.equal(await estimateBaseDeploymentFees(fake, 3000, 1000000n), 10n);
  await assert.rejects(
    estimateBaseDeploymentFees(
      { getBlock: fake.getBlock, call: async () => "0x" },
      3000,
      1000000n,
    ),
    /Malformed Base fee/,
  );
});
