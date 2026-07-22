package doctor

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/subprocess"
)

const OrgMinimumHerdrVersion = "0.7.5"

const orgHerdrUpgradeMessage = "org requires herdr >=0.7.5; upgrade herdr (0.7.0-0.7.4 have delivery bugs)"

var strictSemVerPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// CheckHerdrVersion enforces the org provider's minimum supported Herdr CLI.
// It does not fall back to cockpit availability: versions 0.7.0-0.7.4 are
// present but have delivery bugs and therefore fail this required check.
func CheckHerdrVersion(ctx context.Context, runner subprocess.Runner, minimum string) Check {
	check := Check{Name: "herdr", Required: true}
	if runner == nil {
		check.Detail = orgHerdrUpgradeMessage + "; command runner unavailable"
		return check
	}
	if _, err := runner.LookPath("herdr"); err != nil {
		check.Detail = orgHerdrUpgradeMessage + "; install herdr: " + err.Error()
		return check
	}
	result, err := runner.Run(ctx, "", "herdr", "--version")
	if err != nil {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = err.Error()
		}
		check.Detail = orgHerdrUpgradeMessage + "; `herdr --version` failed: " + detail
		return check
	}
	found, err := parseHerdrVersion(result.Stdout)
	if err != nil {
		check.Detail = orgHerdrUpgradeMessage + "; " + err.Error()
		return check
	}
	minimumVersion, err := parseStrictSemVer(strings.TrimSpace(minimum))
	if err != nil {
		check.Detail = "invalid minimum Herdr version: " + err.Error()
		return check
	}
	foundVersion, _ := parseStrictSemVer(found)
	if compareSemVer(foundVersion, minimumVersion) < 0 {
		check.Detail = fmt.Sprintf("%s; found %s", orgHerdrUpgradeMessage, found)
		return check
	}
	check.OK = true
	check.Detail = fmt.Sprintf("herdr %s (org requires >=%s)", found, minimum)
	return check
}

type semVersion struct {
	major, minor, patch int
	prerelease          []string
}

func parseHerdrVersion(output string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) == 0 {
		return "", fmt.Errorf("malformed `herdr --version` output")
	}
	version := fields[len(fields)-1]
	if _, err := parseStrictSemVer(version); err != nil {
		return "", fmt.Errorf("malformed `herdr --version` output %q", strings.TrimSpace(output))
	}
	return version, nil
}

func parseStrictSemVer(value string) (semVersion, error) {
	match := strictSemVerPattern.FindStringSubmatch(value)
	if match == nil {
		return semVersion{}, fmt.Errorf("%q is not strict SemVer", value)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	version := semVersion{major: major, minor: minor, patch: patch}
	if match[4] != "" {
		version.prerelease = strings.Split(match[4], ".")
		for _, identifier := range version.prerelease {
			if len(identifier) > 1 && identifier[0] == '0' {
				allNumeric := true
				for _, r := range identifier {
					allNumeric = allNumeric && r >= '0' && r <= '9'
				}
				if allNumeric {
					return semVersion{}, fmt.Errorf("%q is not strict SemVer", value)
				}
			}
		}
	}
	return version, nil
}

func compareSemVer(left, right semVersion) int {
	for _, pair := range [][2]int{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) == 0 {
		return 0
	}
	if len(left.prerelease) == 0 {
		return 1
	}
	if len(right.prerelease) == 0 {
		return -1
	}
	for index := 0; index < len(left.prerelease) && index < len(right.prerelease); index++ {
		leftID, rightID := left.prerelease[index], right.prerelease[index]
		leftNumber, leftErr := strconv.Atoi(leftID)
		rightNumber, rightErr := strconv.Atoi(rightID)
		switch {
		case leftErr == nil && rightErr == nil && leftNumber != rightNumber:
			if leftNumber < rightNumber {
				return -1
			}
			return 1
		case leftErr == nil && rightErr != nil:
			return -1
		case leftErr != nil && rightErr == nil:
			return 1
		case leftID < rightID:
			return -1
		case leftID > rightID:
			return 1
		}
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1
	}
	return 0
}
