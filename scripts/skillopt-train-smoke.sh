#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_BIN="${GO_BIN:-go}"
GO_TOOLCHAIN="${GOTOOLCHAIN:-go1.26.2}"

CONTRACT_TESTS=(
  TestEvaluatorProfileAndFailurePacketContractsRoundTrip
  TestImportCandidatePackageRejectsNoCandidateMetadata
  TestImportCandidatePackageRejectsUnchangedContent
  TestEvaluatorScoreJudgePromptFieldsRoundTrip
  TestEvaluatorScoreJudgePromptFieldsOmitEmpty
)

DB_TESTS=(
  TestSkillOptJudgeOutcomeCRUDRoundTrip
  TestDecideSkillOptTrainCandidateCapturesJudgeOutcome
  TestDecideSkillOptTrainCandidateSkipsCaptureWithoutJudgeSignal
)

CLI_TESTS=(
  TestSkillOptTrainContinueGeneratesOptionsWithManagedAgent
  TestSkillOptTrainContinueGeneratesRequiredVuePreviewBundles
  TestSkillOptTrainContinueAllowsOptionalVuePreviewFallback
  TestSkillOptTrainContinueFailsRequiredVuePreviewForProseOutput
  TestSkillOptFeedbackGitHubCommandsEnforceTrainReviewRepo
  TestPublishGitHubPagesPreviewRestoresCheckoutOnCommitFailure
  TestSkillOptPreviewRouteSlugsUnsafeSegments
  TestTrustedVueViteScaffoldUsesRelativeBase
  TestSkillOptGitHubPagesURLHandlesProjectAndUserPages
  TestSkillOptJudgeReportRendersMatrixAndAgreement
  TestSkillOptTrainContinueRecordsNoCandidateResult
  TestSkillOptTrainContinueRerunsOptimizerAfterNoCandidate
  TestSkillOptTrainContinuePublishesCandidateReviewPromotesAndStartsNext
  TestSkillOptTrainContinueSyncsHumanCandidatePromotionAndStartsNext
  TestSkillOptTrainContinueSyncsHumanCandidatePromotionBeforeReviewPublish
  TestSkillOptTrainContinueRequiresReasonForExternalCandidateRejection
  TestSkillOptTrainContinuePublishesCandidateReviewAndRejects
  TestSkillOptTrainContinueStartNextRejectsEvalRunCollision
  TestSkillOptTrainContinueReportsRecoveryAfterOptimizerFailureArtifacts
  TestSkillOptTrainRecoverImportsCandidateArtifacts
  TestSkillOptTrainRecoverRecordsNoCandidateArtifacts
  TestSkillOptTrainRecoverRejectsCandidateBeforePackageState
  TestSkillOptTrainContinueReportsStaleOptimizerLock
  TestSkillOptReviewWatcherImportsValidYAML
  TestSkillOptReviewWatcherCommentsInvalidYAMLDeduped
  TestSkillOptReviewWatcherPostsStaleNoticeOnce
  TestSkillOptReviewWatcherDoesNotStaleBeforeThreshold
  TestSkillOptReviewWatcherStalesAfterUnrelatedComment
  TestSkillOptReviewWatcherImportsFeedbackInsteadOfStaleNotice
  TestSkillOptReviewWatcherKeepsImportedWhenTrainReviewLockBusy
  TestSkillOptReviewWatcherRetriesImportedAckAndClose
  TestSkillOptReviewWatcherCloseDecisionKeepsBlockedReviewOpen
)

cd "$ROOT_DIR"
IFS='|'
CONTRACT_TEST_PATTERN="${CONTRACT_TESTS[*]}"
CLI_TEST_PATTERN="${CLI_TESTS[*]}"
DB_TEST_PATTERN="${DB_TESTS[*]}"
unset IFS

GOTOOLCHAIN="$GO_TOOLCHAIN" "$GO_BIN" test ./internal/skillopt -run "$CONTRACT_TEST_PATTERN"
GOTOOLCHAIN="$GO_TOOLCHAIN" "$GO_BIN" test ./internal/db -run "$DB_TEST_PATTERN"
GOTOOLCHAIN="$GO_TOOLCHAIN" "$GO_BIN" test ./internal/cli -run "$CLI_TEST_PATTERN"

echo "skillopt train smoke passed"
echo "covered: fake managed generation, required/optional previews, review-repo enforcement, preview publication rollback, fake optimizer handoff, no-candidate gate details, fake GitHub candidate review, promote/reject, start-next gates, recovery/status phases, watched review import/close/continue, invalid feedback, stale notices"
