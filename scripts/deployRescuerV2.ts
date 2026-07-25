/**
 * scripts/deployRescuerV2.ts
 * 
 * Компилирует и деплоит RescuerV2 на все сети.
 * RescuerV2 используется как forwarder для EIP-7702 делегации.
 * 
 * Usage:
 *   npx tsx scripts/deployRescuerV2.ts
 * 
 * Поддерживает Infura/Alchemy RPC через .env переменные
 */

import { ethers } from "ethers";
import solc from "solc";
import { readFileSync, writeFileSync } from "fs";
import { resolve } from "path";
import * as dotenv from "dotenv";

dotenv.config();

interface NetworkConfig {
  name: string;
  rpc: string;
  chainId: number;
  envKey: string;
}

// Get RPC endpoints from env or use defaults
const INFURA_KEY = process.env.INFURA_API_KEY || "";
const ALCHEMY_KEY = process.env.ALCHEMY_API_KEY || "";

const getRPC = (name: string, defaultRpc: string): string => {
  if (INFURA_KEY) {
    const infraMap: Record<string, string> = {
      "Ethereum": `https://mainnet.infura.io/v3/${INFURA_KEY}`,
      "Arbitrum": `https://arbitrum-mainnet.infura.io/v3/${INFURA_KEY}`,
      "Optimism": `https://optimism-mainnet.infura.io/v3/${INFURA_KEY}`,
      "Polygon": `https://polygon-mainnet.infura.io/v3/${INFURA_KEY}`,
      "Base":     `https://base-mainnet.infura.io/v3/${INFURA_KEY}`,
    };
    if (infraMap[name]) return infraMap[name];
  }
  if (ALCHEMY_KEY) {
    const alchemyMap: Record<string, string> = {
      "Ethereum": `https://eth-mainnet.g.alchemy.com/v2/${ALCHEMY_KEY}`,
      "Arbitrum": `https://arb-mainnet.g.alchemy.com/v2/${ALCHEMY_KEY}`,
      "Optimism": `https://opt-mainnet.g.alchemy.com/v2/${ALCHEMY_KEY}`,
      "Polygon":  `https://polygon-mainnet.g.alchemy.com/v2/${ALCHEMY_KEY}`,
      "Base":     `https://base-mainnet.g.alchemy.com/v2/${ALCHEMY_KEY}`,
    };
    if (alchemyMap[name]) return alchemyMap[name];
  }
  return defaultRpc;
};

const NETWORKS: NetworkConfig[] = [
  { name: "Ethereum", rpc: getRPC("Ethereum", "https://ethereum-rpc.publicnode.com"), chainId: 1, envKey: "RESCUER_ETHEREUM" },
  { name: "Base", rpc: getRPC("Base", "https://mainnet.base.org"), chainId: 8453, envKey: "RESCUER_BASE" },
  { name: "Arbitrum", rpc: getRPC("Arbitrum", "https://arbitrum.drpc.org"), chainId: 42161, envKey: "RESCUER_ARBITRUM" },
  { name: "Optimism", rpc: getRPC("Optimism", "https://mainnet.optimism.io"), chainId: 10, envKey: "RESCUER_OPTIMISM" },
  { name: "Polygon", rpc: getRPC("Polygon", "https://polygon.drpc.org"), chainId: 137, envKey: "RESCUER_POLYGON" },
  { name: "BNB", rpc: "https://bsc-rpc.publicnode.com", chainId: 56, envKey: "RESCUER_BNB" },
  { name: "Ink", rpc: "https://ink.drpc.org", chainId: 57073, envKey: "RESCUER_INK" },
  { name: "Linea", rpc: "https://linea.drpc.org", chainId: 59144, envKey: "RESCUER_LINEA" },
  { name: "Scroll", rpc: "https://rpc.scroll.io", chainId: 534352, envKey: "RESCUER_SCROLL" },
];
// NOTE: this is a full redeploy across all 9 active networks — critical:
// the onlySponsor access-control fix on executeAndSweep needs to be live
// everywhere, not just where the last (Superfluid-specific) redeploy
// happened to target. Soneium/Metis excluded here since they're separately
// tracked (deploy those explicitly if/when needed again).

// PermitSweeper is only actively configured on the subset of networks
// where the daemon has PERMIT_SWEEPER_* set in .env — not all 10.
const PERMIT_NETWORKS: NetworkConfig[] = [
  // Temporarily empty — this redeploy round is only for RescuerV2's
  // payable executeAndSweep fix on Base. PermitSweeper is unaffected;
  { name: "Ethereum", rpc: getRPC("Ethereum", "https://ethereum-rpc.publicnode.com"), chainId: 1, envKey: "PERMIT_SWEEPER_ETHEREUM" },
  { name: "Base", rpc: getRPC("Base", "https://mainnet.base.org"), chainId: 8453, envKey: "PERMIT_SWEEPER_BASE" },
  { name: "Arbitrum", rpc: getRPC("Arbitrum", "https://arbitrum.drpc.org"), chainId: 42161, envKey: "PERMIT_SWEEPER_ARBITRUM" },
  { name: "Optimism", rpc: getRPC("Optimism", "https://mainnet.optimism.io"), chainId: 10, envKey: "PERMIT_SWEEPER_OPTIMISM" },
  { name: "Polygon", rpc: getRPC("Polygon", "https://polygon.drpc.org"), chainId: 137, envKey: "PERMIT_SWEEPER_POLYGON" },
  { name: "Ink", rpc: "https://ink.drpc.org", chainId: 57073, envKey: "PERMIT_SWEEPER_INK" },
];
// NOTE: PermitSweeper's constructor now requires an `owner` argument
// (the access-control fix on rescueTokens — anyone could previously drain
// stray token balances from this contract). Redeploying now closes that
// even though permitAndTransfer() isn't currently called by the daemon's
// sweep logic — this just ensures the fix is live whenever it is used.

function compileContract(
  contractName: string,
  fileName: string
): { bytecode: string; abi: any[] } {
  console.log(`📦 Compiling ${fileName}...\n`);

  const source = readFileSync(resolve(`./contracts/${fileName}`), "utf-8");

  const input = {
    language: "Solidity",
    sources: { [fileName]: { content: source } },
    settings: {
      optimizer: { enabled: true, runs: 200 },
      outputSelection: { [fileName]: { [contractName]: ["evm.bytecode.object", "abi"] } },
    },
  };

  const output = JSON.parse(solc.compile(JSON.stringify(input)));

  if (output.errors?.length) {
    for (const err of output.errors) {
      if (err.severity === "error") {
        console.error("❌ Compilation error:");
        console.error(err.formattedMessage);
        process.exit(1);
      }
    }
  }

  const contract = output.contracts?.[fileName]?.[contractName];
  if (!contract) {
    console.error(`❌ Contract ${contractName} not found in compilation output`);
    process.exit(1);
  }

  const bytecode = contract.evm.bytecode.object;
  const abi = contract.abi;

  console.log(`✅ Compiled: ${bytecode.length / 2} bytes`);
  console.log(`✅ ABI: ${abi.length} items\n`);

  return { bytecode: `0x${bytecode}`, abi };
}

async function deployOnNetwork(
  network: NetworkConfig,
  bytecode: string,
  abi: any[],
  constructorArgs: string[],
  constructorArgLabels: string[]
): Promise<string | null> {
  try {
    console.log(`\n🚀 Deploying on ${network.name} (Chain ${network.chainId})...`);

    const sponsorKey = process.env.SPONSOR_PRIVATE_KEY;
    if (!sponsorKey) {
      console.error("❌ SPONSOR_PRIVATE_KEY not set in .env");
      return null;
    }

    for (let i = 0; i < constructorArgs.length; i++) {
      if (!constructorArgs[i]) {
        console.error(`❌ ${constructorArgLabels[i]} not set in .env`);
        return null;
      }
    }

    const provider = new ethers.JsonRpcProvider(network.rpc);
    const signer = new ethers.Wallet(sponsorKey, provider);

    console.log(`   Signer: ${signer.address}`);
    for (let i = 0; i < constructorArgs.length; i++) {
      console.log(`   ${constructorArgLabels[i]}: ${constructorArgs[i]}`);
    }

    const balance = await provider.getBalance(signer.address);
    const balanceEth = ethers.formatEther(balance);
    console.log(`   Balance: ${balanceEth} ETH`);

    if (parseFloat(balanceEth) < 0.0001) {
      console.warn(`   ⚠️  Low balance! Need at least 0.0001 ETH for gas`);
      return null;
    }

    const factory = new ethers.ContractFactory(abi, bytecode, signer);
    const contract = await factory.deploy(...constructorArgs);

    const deployTx = contract.deploymentTransaction();
    if (!deployTx) {
      console.error(`   ❌ No deployment tx`);
      return null;
    }

    console.log(`   📝 Tx: ${deployTx.hash}`);
    console.log(`   ⏳ Waiting for confirmation...`);

    const receipt = await deployTx.wait(2);
    if (!receipt || !receipt.contractAddress) {
      console.error(`   ❌ Deployment failed or no address`);
      return null;
    }

    const address = receipt.contractAddress;
    console.log(`   ✅ Deployed at: ${address}`);

    const code = await provider.getCode(address);
    if (code === "0x") {
      console.error(`   ❌ Code not found at address (deployment may have failed)`);
      return null;
    }

    return address;
  } catch (error: any) {
    console.error(`   ❌ Error: ${error.message}`);
    return null;
  }
}

function saveAddressesToEnv(addresses: Map<string, string>) {
  let envContent = readFileSync(".env", "utf-8");

  for (const [key, address] of addresses) {
    const pattern = new RegExp(`^${key}=.*$`, "m");
    const line = `${key}=${address}`;

    if (pattern.test(envContent)) {
      envContent = envContent.replace(pattern, line);
    } else {
      envContent += `\n${line}`;
    }
  }

  writeFileSync(".env", envContent);
  console.log("\n✅ Saved to .env");
}

async function main() {
  console.log("═══════════════════════════════════════════════════════");
  console.log("  RescuerV2 + PermitSweeper Multi-Network Deployer");
  console.log("═══════════════════════════════════════════════════════");
  
  if (INFURA_KEY) {
    console.log(`✅ Using Infura RPC (INFURA_API_KEY set)`);
  } else if (ALCHEMY_KEY) {
    console.log(`✅ Using Alchemy RPC (ALCHEMY_API_KEY set)`);
  } else {
    console.log(`⚠️  Using public RPC endpoints (Ankr fallback)`);
  }
  console.log();

  const destination = process.env.DESTINATION_ADDRESS ?? "";
  const sponsorAddress = process.env.SPONSOR_PRIVATE_KEY
    ? new ethers.Wallet(process.env.SPONSOR_PRIVATE_KEY).address
    : "";

  const addresses = new Map<string, string>();

  // ── RescuerV2 ──────────────────────────────────────────────────────────
  console.log("─".repeat(60));
  console.log("  RescuerV2 (EIP-7702 delegation-based rescue)");
  console.log("─".repeat(60));

  const rescuerCompiled = compileContract("RescuerV2", "RescuerV2.sol");
  const rescuerResults: { network: string; address: string | null }[] = [];

  for (const network of NETWORKS) {
    const address = await deployOnNetwork(
      network, rescuerCompiled.bytecode, rescuerCompiled.abi,
      [destination, sponsorAddress], ["Destination", "Sponsor"]
    );
    rescuerResults.push({ network: network.name, address });
    if (address) addresses.set(network.envKey, address);
  }

  // ── PermitSweeper ─────────────────────────────────────────────────────
  console.log("\n" + "─".repeat(60));
  console.log("  PermitSweeper (EIP-2612 permit-based rescue)");
  console.log("─".repeat(60));

  const permitCompiled = compileContract("PermitSweeper", "PermitSweeper.sol");
  const permitResults: { network: string; address: string | null }[] = [];

  for (const network of PERMIT_NETWORKS) {
    const address = await deployOnNetwork(
      network, permitCompiled.bytecode, permitCompiled.abi,
      [sponsorAddress], ["Owner"]
    );
    permitResults.push({ network: network.name, address });
    if (address) addresses.set(network.envKey, address);
  }

  // ── Summary ───────────────────────────────────────────────────────────
  console.log("\n" + "═".repeat(60));
  console.log("📊 DEPLOYMENT SUMMARY");
  console.log("═".repeat(60));

  console.log("\nRescuerV2:");
  for (const result of rescuerResults) {
    const status = result.address ? "✅" : "❌";
    console.log(`${status} ${result.network.padEnd(12)} ${result.address || "(failed)"}`);
  }

  console.log("\nPermitSweeper:");
  for (const result of permitResults) {
    const status = result.address ? "✅" : "❌";
    console.log(`${status} ${result.network.padEnd(12)} ${result.address || "(failed)"}`);
  }

  const rescuerFailed = rescuerResults.filter(r => !r.address);
  const rescuerSucceeded = rescuerResults.filter(r => r.address);

  if (addresses.size > 0) {
    saveAddressesToEnv(addresses);
  }

  // RescuerV2 carries the critical onlySponsor access-control fix — its
  // success/failure is reported and gated SEPARATELY from PermitSweeper
  // (and from the generic "did anything at all deploy" check), so a lone
  // successful PermitSweeper deployment can never mask a total RescuerV2
  // failure behind a generic "Ready to use!" message.
  if (rescuerFailed.length > 0) {
    console.log("\n" + "⚠".repeat(20));
    console.log("🚨 CRITICAL: RescuerV2 deployment FAILED on " + rescuerFailed.length + " network(s):");
    for (const r of rescuerFailed) {
      console.log(`   ❌ ${r.network} — .env still points at the OLD contract there (if any),`);
      console.log(`      which does NOT have the onlySponsor access-control fix.`);
    }
    console.log("   DO NOT run the daemon against these networks with real keys");
    console.log("   until RescuerV2 is redeployed successfully there.");
    console.log("⚠".repeat(20));
  }

  if (rescuerSucceeded.length > 0) {
    console.log(`\n✅ RescuerV2 deployed successfully on ${rescuerSucceeded.length}/${rescuerResults.length} network(s) — onlySponsor fix is live there.`);
  } else if (rescuerResults.length > 0) {
    console.log("\n❌ RescuerV2 deployment did not succeed on ANY network. The critical");
    console.log("   access-control fix is NOT live anywhere. Do not use with real keys.");
  }

  if (addresses.size === 0) {
    console.log("\n❌ No successful deployments at all — nothing saved to .env.");
  }

  console.log("═".repeat(60) + "\n");
}

main().catch((err) => {
  console.error("Fatal error:", err);
  process.exit(1);
});
