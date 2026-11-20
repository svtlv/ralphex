package phase

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// noIssuesSentinel is the exact output a plan reviewer prints when it has no findings.
const noIssuesSentinel = "NO ISSUES FOUND"

// planFindingPattern matches one finding line: optional list marker, optional bold around the
// label, severity label, colon, text. the colon may sit inside the bold markers (`**MAJOR:** x`).
var planFindingPattern = regexp.MustCompile(`^(?:[-*•]\s+|\d+[.)]\s+)?\*{0,2}(CRITICAL|MAJOR|MINOR)\*{0,2}:\*{0,2}\s*(\S.*)$`)

// selectionTokenPattern matches one number or one range inside a findings selection answer.
var selectionTokenPattern = regexp.MustCompile(`^(\d+)(?:-(\d+))?$`)

// parsePlanFindings extracts finding lines from reviewer stdout.
// only lines in the `CRITICAL|MAJOR|MINOR: <text>` format are kept, in order, without exact duplicates.
// noIssues is true when the output has the NO ISSUES FOUND sentinel and no findings.
func parsePlanFindings(output string) (findings []string, noIssues bool) {
	seen := make(map[string]struct{})
	sawSentinel := false
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := planFindingPattern.FindStringSubmatch(line); m != nil {
			finding := m[1] + ": " + strings.TrimSpace(m[2])
			if _, dup := seen[finding]; dup {
				continue
			}
			seen[finding] = struct{}{}
			findings = append(findings, finding)
			continue
		}
		if strings.Contains(line, noIssuesSentinel) {
			sawSentinel = true
		}
	}
	return findings, sawSentinel && len(findings) == 0
}

// parseFindingSelection turns a user answer into sorted, unique 1-based finding numbers.
// accepts all, all findings, none, apply none (case-insensitive) and lists such as "1,3", "1 3" or "2-4".
// an empty result with a nil error means "apply nothing".
func parseFindingSelection(answer string, n int) ([]int, error) {
	text := strings.ToLower(strings.TrimSpace(answer))
	switch text {
	case "":
		return nil, errors.New("empty answer")
	case "none", "apply none":
		return nil, nil
	case "all", "all findings":
		all := make([]int, n)
		for i := range all {
			all[i] = i + 1
		}
		return all, nil
	}

	picked := make(map[int]struct{})
	for tok := range strings.FieldsFuncSeq(text, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		m := selectionTokenPattern.FindStringSubmatch(tok)
		if m == nil {
			return nil, fmt.Errorf("unknown selection %q: use all, none, or numbers like 1,3 or 2-4", tok)
		}
		from, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("invalid number %q: %w", m[1], err)
		}
		to := from
		if m[2] != "" {
			if to, err = strconv.Atoi(m[2]); err != nil {
				return nil, fmt.Errorf("invalid number %q: %w", m[2], err)
			}
		}
		if from > to {
			return nil, fmt.Errorf("invalid range %q: start is greater than end", tok)
		}
		if from < 1 || to > n {
			return nil, fmt.Errorf("selection %q is out of range: choose from 1 to %d", tok, n)
		}
		for i := from; i <= to; i++ {
			picked[i] = struct{}{}
		}
	}
	if len(picked) == 0 {
		return nil, errors.New("no finding numbers in answer")
	}

	result := make([]int, 0, len(picked))
	for i := range picked {
		result = append(result, i)
	}
	sort.Ints(result)
	return result, nil
}

// formatFindingsFeedback builds the numbered feedback text for the planner from the selected findings.
// numbers keep the original finding index so they match what the user saw.
func formatFindingsFeedback(findings []string, selected []int) string {
	var sb strings.Builder
	for _, idx := range selected {
		if idx < 1 || idx > len(findings) {
			continue
		}
		fmt.Fprintf(&sb, "%d. %s\n", idx, findings[idx-1])
	}
	return strings.TrimRight(sb.String(), "\n")
}
