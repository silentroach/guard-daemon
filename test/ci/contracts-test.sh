#!/usr/bin/env bash

set -euo pipefail

test_list=$(forge test --force --list)
required_tests=(
  RescuerV2Test
  testGasBurnTokenFailsWithinOuterGasLimit
  testLyingBalanceTokenCanFabricateApparentTransfer
  RescuerV2FuzzTest
  testFuzzOptionalReturnData
  RescuerV2InvariantTest
  invariantPermissionlessCallsCannotRedirectTokens
)

for required in "${required_tests[@]}"; do
  case "${test_list}" in
    *"${required}"*) ;;
    *)
      printf 'Обязательный Solidity test не найден: %s\n' "${required}" >&2
      exit 1
      ;;
  esac
done

forge test
