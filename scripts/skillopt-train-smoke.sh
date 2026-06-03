#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_BIN="${GO_BIN:-go}"
GO_TOOLCHAIN="${GOTOOLCHAIN:-go1.26.2}"

TEST_PATTERN='TestSkillOptTrainContinueGeneratesOptionsWithManagedAgent|TestSkillOptTrainContinuePublishesCandidateReviewPromotesAndStartsNext|TestSkillOptTrainContinueSyncsHumanCandidatePromotionAndStartsNext|TestSkillOptTrainContinueSyncsHumanCandidatePromotionBeforeReviewPublish|TestSkillOptTrainContinueRequiresReasonForExternalCandidateRejection|TestSkillOptTrainContinuePublishesCandidateReviewAndRejects|TestSkillOptTrainContinueStartNextRejectsEvalRunCollision'

cd "$ROOT_DIR"
GOTOOLCHAIN="$GO_TOOLCHAIN" "$GO_BIN" test ./internal/cli -run "$TEST_PATTERN"

echo "skillopt train smoke passed"
echo "covered: fake managed generation, fake optimizer handoff, fake GitHub candidate review, promote/reject, start-next gates"
