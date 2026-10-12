// SPDX-License-Identifier: MIT
pragma solidity 0.8.36;

import {RescuerV2} from "../../contracts/RescuerV2.sol";

interface Vm {
    struct Log {
        bytes32[] topics;
        bytes data;
        address emitter;
    }

    struct SignedDelegation {
        uint8 v;
        bytes32 r;
        bytes32 s;
        uint64 nonce;
        address implementation;
    }

    function addr(uint256 privateKey) external pure returns (address);
    function deal(address account, uint256 newBalance) external;
    function expectRevert(bytes4 revertData) external;
    function expectRevert(bytes calldata revertData) external;
    function getRecordedLogs() external returns (Log[] memory);
    function prank(address sender) external;
    function recordLogs() external;
    function signAndAttachDelegation(address implementation, uint256 privateKey)
        external
        returns (SignedDelegation memory signedDelegation);
}

abstract contract RescuerV2TestBase {
    error AssertionFailed();

    Vm internal constant vm = Vm(address(uint160(uint256(keccak256("hevm cheat code")))));
    uint256 internal constant SOURCE_TEST_KEY = uint256(keccak256("guard-daemon deterministic source test key"));

    address internal destination;
    address internal outsider;
    address internal source;
    address internal sponsor;
    RescuerV2 internal implementation;

    function setUp() public virtual {
        source = vm.addr(SOURCE_TEST_KEY);
        destination = address(0xD3571);
        sponsor = address(0x5A0050);
        outsider = address(0xBAD);
        implementation = new RescuerV2(destination, sponsor);
        vm.signAndAttachDelegation(address(implementation), SOURCE_TEST_KEY);
    }

    function _assertEq(address actual, address expected) internal pure {
        if (actual != expected) revert AssertionFailed();
    }

    function _assertEq(bytes32 actual, bytes32 expected) internal pure {
        if (actual != expected) revert AssertionFailed();
    }

    function _assertEq(uint256 actual, uint256 expected) internal pure {
        if (actual != expected) revert AssertionFailed();
    }

    function _assertFalse(bool value) internal pure {
        if (value) revert AssertionFailed();
    }

    function _assertTrue(bool value) internal pure {
        if (!value) revert AssertionFailed();
    }

    function _assertRevertSelector(bytes memory result, bytes4 expected) internal pure {
        if (result.length < 4) revert AssertionFailed();

        bytes4 actual;
        assembly ("memory-safe") {
            actual := mload(add(result, 0x20))
        }
        if (actual != expected) revert AssertionFailed();
    }

    function _tokens(address token) internal pure returns (address[] memory result) {
        result = new address[](1);
        result[0] = token;
    }
}
