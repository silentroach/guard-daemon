// SPDX-License-Identifier: MIT
pragma solidity 0.8.36;

import {RescuerV2} from "../../../contracts/RescuerV2.sol";
import {RescuerV2TestBase, Vm} from "../RescuerV2TestBase.sol";
import {StandardToken} from "../mocks/RescuerV2Mocks.sol";

contract SweepHandler {
    Vm private constant vm = Vm(address(uint160(uint256(keccak256("hevm cheat code")))));

    address private immutable destination;
    address private immutable source;
    StandardToken private immutable token;

    uint256 public totalMinted;

    constructor(address source_, address destination_, StandardToken token_) {
        source = source_;
        destination = destination_;
        token = token_;
    }

    function depositAndSweep(uint96 amount, address caller) external {
        token.mint(source, amount);
        totalMinted += amount;

        address[] memory tokens = new address[](1);
        tokens[0] = address(token);
        vm.prank(caller);
        RescuerV2(payable(source)).sweepAll(tokens);
    }

    function expectedDestination() external view returns (address) {
        return destination;
    }
}

contract RescuerV2InvariantTest is RescuerV2TestBase {
    address[] private targets;
    SweepHandler private handler;
    StandardToken private token;

    function setUp() public override {
        super.setUp();
        token = new StandardToken();
        handler = new SweepHandler(source, destination, token);
        targets.push(address(handler));
    }

    function targetContracts() external view returns (address[] memory) {
        return targets;
    }

    function invariantPermissionlessCallsCannotRedirectTokens() public view {
        _assertEq(handler.expectedDestination(), destination);
        _assertEq(token.balanceOf(source), 0);
        _assertEq(token.balanceOf(address(handler)), 0);
        _assertEq(token.balanceOf(destination), handler.totalMinted());
    }
}
