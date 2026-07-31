// SPDX-License-Identifier: MIT
pragma solidity 0.8.36;

import {RescuerV2} from "../../contracts/RescuerV2.sol";
import {RescuerV2TestBase} from "./RescuerV2TestBase.sol";
import {ArbitraryCallTarget, ConfigurableReturnToken, NoReturnToken, StandardToken} from "./mocks/RescuerV2Mocks.sol";

contract RescuerV2FuzzTest is RescuerV2TestBase {
    function testFuzzTokenArrays(uint8 rawLength, uint256 amount, bytes32 choices) public {
        StandardToken standard = new StandardToken();
        NoReturnToken noReturn = new NoReturnToken();
        StandardToken second = new StandardToken();
        standard.mint(source, amount);
        noReturn.mint(source, amount);
        second.mint(source, amount);

        uint256 length = uint256(rawLength) % 24;
        address[] memory tokens = new address[](length);
        bool[3] memory selected;
        for (uint256 i; i < length; ++i) {
            uint256 choice = uint8(choices[i % 32]) % 3;
            selected[choice] = true;
            if (choice == 0) tokens[i] = address(standard);
            if (choice == 1) tokens[i] = address(noReturn);
            if (choice == 2) tokens[i] = address(second);
        }

        RescuerV2(payable(source)).sweepAll(tokens);

        _assertSweepResult(standard.balanceOf(source), standard.balanceOf(destination), amount, selected[0]);
        _assertSweepResult(noReturn.balanceOf(source), noReturn.balanceOf(destination), amount, selected[1]);
        _assertSweepResult(second.balanceOf(source), second.balanceOf(destination), amount, selected[2]);
        _assertEq(standard.balanceOf(outsider), 0);
        _assertEq(noReturn.balanceOf(outsider), 0);
        _assertEq(second.balanceOf(outsider), 0);
    }

    function testFuzzOptionalReturnData(bytes calldata response, uint96 amount, bool reverts) public {
        ConfigurableReturnToken token = new ConfigurableReturnToken();
        token.mint(source, amount);
        token.setResponse(response, reverts);

        (bool success,) = source.call(abi.encodeCall(RescuerV2.sweepAll, (_tokens(address(token)))));
        bool accepted =
            amount == 0 || (!reverts && (response.length == 0 || (response.length == 32 && _word(response) == 1)));
        if (success != accepted) revert AssertionFailed();

        if (accepted) {
            _assertEq(token.balanceOf(source), 0);
            _assertEq(token.balanceOf(destination), amount);
        } else {
            _assertEq(token.balanceOf(source), amount);
            _assertEq(token.balanceOf(destination), 0);
        }
    }

    function testFuzzSponsorForwardsExactCalldataAndValue(bytes calldata data, uint96 value) public {
        ArbitraryCallTarget target = new ArbitraryCallTarget();
        vm.deal(sponsor, value);

        vm.prank(sponsor);
        RescuerV2(payable(source)).executeAndSweep{value: value}(address(target), data, new address[](0));

        _assertEq(target.dataHash(), keccak256(data));
        _assertEq(target.received(), value);
    }

    function testFuzzArbitraryCallerCannotReachExternalCall(address caller, bytes calldata data) public {
        ArbitraryCallTarget target = new ArbitraryCallTarget();
        if (caller == sponsor) caller = outsider;

        vm.prank(caller);
        (bool success, bytes memory result) =
            source.call(abi.encodeCall(RescuerV2.executeAndSweep, (address(target), data, new address[](0))));

        _assertFalse(success);
        _assertRevertSelector(result, RescuerV2.NotSponsor.selector);
        _assertEq(target.received(), 0);
        _assertEq(target.dataHash(), bytes32(0));
    }

    function _word(bytes calldata data) private pure returns (uint256 value) {
        assembly ("memory-safe") {
            value := calldataload(data.offset)
        }
    }

    function _assertSweepResult(uint256 sourceBalance, uint256 destinationBalance, uint256 amount, bool selected)
        private
        pure
    {
        _assertEq(sourceBalance, selected ? 0 : amount);
        _assertEq(destinationBalance, selected ? amount : 0);
    }
}
