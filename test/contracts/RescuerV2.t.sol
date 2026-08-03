// SPDX-License-Identifier: MIT
pragma solidity 0.8.36;

import {RescuerV2} from "../../contracts/RescuerV2.sol";
import {RescuerV2TestBase, Vm} from "./RescuerV2TestBase.sol";
import {
    CallbackClaimTarget,
    ClaimTarget,
    ConfigurableReturnToken,
    DelegatingImplementation,
    FalseReturnToken,
    GasBurnToken,
    LyingBalanceToken,
    MalformedBalanceToken,
    MockNFT,
    NoReturnToken,
    ReentrantToken,
    RejectingDestination,
    RevertingCallTarget,
    RevertingToken,
    StandardToken
} from "./mocks/RescuerV2Mocks.sol";

contract RescuerV2Test is RescuerV2TestBase {
    function testConstructorRejectsZeroDestination() public {
        vm.expectRevert(RescuerV2.ZeroDestination.selector);
        new RescuerV2(address(0), sponsor);
    }

    function testConstructorRejectsZeroSponsor() public {
        vm.expectRevert(RescuerV2.ZeroSponsor.selector);
        new RescuerV2(destination, address(0));
    }

    function testConstructorSeparatesSponsorAndDestination() public {
        vm.expectRevert(RescuerV2.RolesMustDiffer.selector);
        new RescuerV2(destination, destination);
    }

    function testAttestationGettersInDelegatedContext() public view {
        _assertEq(RescuerV2(payable(source)).destination(), destination);
        _assertEq(RescuerV2(payable(source)).sponsor(), sponsor);
        _assertEq(RescuerV2(payable(source)).self(), address(implementation));
    }

    function testDirectImplementationCallIsRejected() public {
        vm.expectRevert(RescuerV2.DelegationNotActive.selector);
        implementation.sweepAll(new address[](0));
    }

    function testDelegationToDifferentImplementationIsRejected() public {
        DelegatingImplementation other = new DelegatingImplementation(address(implementation));
        vm.signAndAttachDelegation(address(other), SOURCE_TEST_KEY);

        vm.expectRevert(RescuerV2.DelegationNotActive.selector);
        RescuerV2(payable(source)).sweepAll(new address[](0));
    }

    function testPermissionlessSweepUsesOnlyFixedDestination() public {
        StandardToken token = new StandardToken();
        token.mint(source, 17);

        vm.prank(outsider);
        RescuerV2(payable(source)).sweepAll(_tokens(address(token)));

        _assertEq(token.balanceOf(source), 0);
        _assertEq(token.balanceOf(destination), 17);
        _assertEq(token.balanceOf(outsider), 0);
    }

    function testPermissionlessEthSweepUsesOnlyFixedDestination() public {
        vm.deal(source, 3 ether);

        vm.prank(outsider);
        RescuerV2(payable(source)).sweepEth();

        _assertEq(source.balance, 0);
        _assertEq(destination.balance, 3 ether);
        _assertEq(outsider.balance, 0);
    }

    function testEthSweepFailsClosedWhenDestinationRejects() public {
        RejectingDestination rejecting = new RejectingDestination();
        implementation = new RescuerV2(address(rejecting), sponsor);
        vm.signAndAttachDelegation(address(implementation), SOURCE_TEST_KEY);
        vm.deal(source, 1 ether);

        vm.expectRevert(RescuerV2.TransferFailed.selector);
        RescuerV2(payable(source)).sweepEth();
        _assertEq(source.balance, 1 ether);
    }

    function testSponsorCanClaimAndSweep() public {
        StandardToken token = new StandardToken();
        ClaimTarget claim = new ClaimTarget();

        vm.prank(sponsor);
        RescuerV2(payable(source))
            .executeAndSweep(
                address(claim), abi.encodeCall(ClaimTarget.claim, (address(token), 23)), _tokens(address(token))
            );

        _assertEq(token.balanceOf(source), 0);
        _assertEq(token.balanceOf(destination), 23);
    }

    function testSponsorCannotCallZeroTarget() public {
        vm.prank(sponsor);
        vm.expectRevert(RescuerV2.ZeroTarget.selector);
        RescuerV2(payable(source)).executeAndSweep(address(0), bytes(""), new address[](0));
    }

    function testOutsiderCannotExecuteTokenApprove() public {
        StandardToken token = new StandardToken();

        vm.prank(outsider);
        vm.expectRevert(RescuerV2.NotSponsor.selector);
        RescuerV2(payable(source))
            .executeAndSweep(
                address(token), abi.encodeCall(StandardToken.approve, (outsider, type(uint256).max)), new address[](0)
            );

        _assertEq(token.allowance(source, outsider), 0);
    }

    function testOutsiderCannotExecuteTokenTransfer() public {
        StandardToken token = new StandardToken();
        token.mint(source, 31);

        vm.prank(outsider);
        vm.expectRevert(RescuerV2.NotSponsor.selector);
        RescuerV2(payable(source))
            .executeAndSweep(address(token), abi.encodeCall(StandardToken.transfer, (outsider, 31)), new address[](0));

        _assertEq(token.balanceOf(source), 31);
        _assertEq(token.balanceOf(outsider), 0);
    }

    function testOutsiderCannotSetNftOperator() public {
        MockNFT nft = new MockNFT();

        vm.prank(outsider);
        vm.expectRevert(RescuerV2.NotSponsor.selector);
        RescuerV2(payable(source))
            .executeAndSweep(
                address(nft), abi.encodeCall(MockNFT.setApprovalForAll, (outsider, true)), new address[](0)
            );

        _assertFalse(nft.isApprovedForAll(source, outsider));
    }

    function testOutsiderCannotTransferNft() public {
        MockNFT nft = new MockNFT();
        nft.mint(source, 1);

        vm.prank(outsider);
        vm.expectRevert(RescuerV2.NotSponsor.selector);
        RescuerV2(payable(source))
            .executeAndSweep(
                address(nft), abi.encodeCall(MockNFT.transferFrom, (source, outsider, 1)), new address[](0)
            );

        _assertEq(nft.ownerOf(1), source);
    }

    function testClaimCallbackCannotBypassSponsorCheck() public {
        StandardToken token = new StandardToken();
        CallbackClaimTarget claim = new CallbackClaimTarget();

        vm.prank(sponsor);
        RescuerV2(payable(source))
            .executeAndSweep(
                address(claim),
                abi.encodeCall(CallbackClaimTarget.claim, (source, address(token), 41)),
                _tokens(address(token))
            );

        _assertFalse(claim.callbackSucceeded());
        _assertEq(token.balanceOf(destination), 41);
    }

    function testStandardToken() public {
        StandardToken token = new StandardToken();
        token.mint(source, 5);

        RescuerV2(payable(source)).sweepAll(_tokens(address(token)));

        _assertEq(token.balanceOf(destination), 5);
    }

    function testNoReturnToken() public {
        NoReturnToken token = new NoReturnToken();
        token.mint(source, 7);

        RescuerV2(payable(source)).sweepAll(_tokens(address(token)));

        _assertEq(token.balanceOf(destination), 7);
    }

    function testFalseReturnTokenFailsClosed() public {
        FalseReturnToken token = new FalseReturnToken();
        token.mint(source, 11);

        vm.expectRevert(abi.encodeWithSelector(RescuerV2.TokenTransferFailed.selector, address(token)));
        RescuerV2(payable(source)).sweepAll(_tokens(address(token)));

        _assertEq(token.balanceOf(source), 11);
        _assertEq(token.balanceOf(destination), 0);
    }

    function testRevertingTokenFailsClosed() public {
        RevertingToken token = new RevertingToken();
        token.mint(source, 13);

        vm.expectRevert(abi.encodeWithSelector(RescuerV2.TokenTransferFailed.selector, address(token)));
        RescuerV2(payable(source)).sweepAll(_tokens(address(token)));

        _assertEq(token.balanceOf(source), 13);
    }

    function testMalformedBalanceFailsClosed() public {
        MalformedBalanceToken token = new MalformedBalanceToken();

        vm.expectRevert(abi.encodeWithSelector(RescuerV2.TokenBalanceQueryFailed.selector, address(token)));
        RescuerV2(payable(source)).sweepAll(_tokens(address(token)));
    }

    function testLyingBalanceTokenCanFabricateApparentTransfer() public {
        LyingBalanceToken token = new LyingBalanceToken(source, destination, 17);

        RescuerV2(payable(source)).sweepAll(_tokens(address(token)));

        _assertTrue(token.transferCalled());
        _assertEq(token.balanceOf(source), 0);
        _assertEq(token.balanceOf(destination), 17);
    }

    function testGasBurnTokenFailsWithinOuterGasLimit() public {
        GasBurnToken token = new GasBurnToken();

        (bool success, bytes memory result) =
            source.call{gas: 500_000}(abi.encodeCall(RescuerV2.sweepAll, (_tokens(address(token)))));

        _assertFalse(success);
        _assertRevertSelector(result, RescuerV2.TokenTransferFailed.selector);
    }

    function testMalformedReturnFailsClosed() public {
        ConfigurableReturnToken token = new ConfigurableReturnToken();
        token.mint(source, 19);
        token.setResponse(hex"01", false);

        vm.expectRevert(abi.encodeWithSelector(RescuerV2.TokenTransferFailed.selector, address(token)));
        RescuerV2(payable(source)).sweepAll(_tokens(address(token)));

        _assertEq(token.balanceOf(source), 19);
        _assertEq(token.balanceOf(destination), 0);
    }

    function testNonCanonicalBoolFailsClosed() public {
        ConfigurableReturnToken token = new ConfigurableReturnToken();
        token.mint(source, 29);
        token.setResponse(abi.encode(uint256(2)), false);

        vm.expectRevert(abi.encodeWithSelector(RescuerV2.TokenTransferFailed.selector, address(token)));
        RescuerV2(payable(source)).sweepAll(_tokens(address(token)));

        _assertEq(token.balanceOf(source), 29);
    }

    function testTokenCallbackCannotExecuteSponsorPath() public {
        StandardToken nested = new StandardToken();
        ReentrantToken token = new ReentrantToken(source, address(nested));
        token.mint(source, 37);
        nested.mint(source, 43);

        RescuerV2(payable(source)).sweepAll(_tokens(address(token)));

        _assertTrue(token.sweepCallbackSucceeded());
        _assertFalse(token.executeCallbackSucceeded());
        _assertEq(token.balanceOf(destination), 37);
        _assertEq(nested.balanceOf(destination), 43);
    }

    function testExternalCallFailureIsNormalized() public {
        RevertingCallTarget target = new RevertingCallTarget();
        target.setReason(abi.encodeWithSignature("Error(string)", "untrusted"));

        vm.prank(sponsor);
        vm.expectRevert(abi.encodeWithSelector(RescuerV2.ExternalCallFailed.selector, address(target)));
        RescuerV2(payable(source)).executeAndSweep(address(target), hex"12345678", new address[](0));
    }

    function testSweepEventIdentifiesSourceTokenDestinationAndAmount() public {
        StandardToken token = new StandardToken();
        token.mint(source, 47);
        vm.recordLogs();

        RescuerV2(payable(source)).sweepAll(_tokens(address(token)));

        Vm.Log[] memory logs = vm.getRecordedLogs();
        bytes32 signature = keccak256("Swept(address,address,address,uint256)");
        bool found;
        for (uint256 i; i < logs.length; ++i) {
            if (logs[i].emitter != source || logs[i].topics[0] != signature) continue;

            _assertEq(logs[i].topics[1], bytes32(uint256(uint160(source))));
            _assertEq(logs[i].topics[2], bytes32(uint256(uint160(address(token)))));
            _assertEq(logs[i].topics[3], bytes32(uint256(uint160(destination))));
            _assertEq(abi.decode(logs[i].data, (uint256)), 47);
            found = true;
        }
        _assertTrue(found);
    }
}
