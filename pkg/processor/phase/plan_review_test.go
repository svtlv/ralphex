package phase

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePlanFindings(t *testing.T) {
	tests := []struct {
		name         string
		output       string
		wantFindings []string
		wantNoIssues bool
	}{
		{name: "plain findings in order", output: "MAJOR: Task 2 - wrong command\nMINOR: Overview - typo", wantFindings: []string{"MAJOR: Task 2 - wrong command", "MINOR: Overview - typo"}},
		{name: "doubled stdout is deduplicated", output: "CRITICAL: Task 1 - bad\nMAJOR: Task 2 - worse\nCRITICAL: Task 1 - bad\nMAJOR: Task 2 - worse", wantFindings: []string{"CRITICAL: Task 1 - bad", "MAJOR: Task 2 - worse"}},
		{name: "prose mixed with findings", output: "I read the plan.\nMAJOR: Task 3 - missing step\nLooks fine otherwise.\n", wantFindings: []string{"MAJOR: Task 3 - missing step"}},
		{name: "bold labels", output: "**CRITICAL:** Task 1 - bad\n**MAJOR**: Task 2 - worse", wantFindings: []string{"CRITICAL: Task 1 - bad", "MAJOR: Task 2 - worse"}},
		{name: "list markers", output: "- MAJOR: a\n* MINOR: b\n1. CRITICAL: c\n2) MAJOR: d", wantFindings: []string{"MAJOR: a", "MINOR: b", "CRITICAL: c", "MAJOR: d"}},
		{name: "no issues output", output: "NO ISSUES FOUND\n", wantNoIssues: true},
		{name: "no issues with prose", output: "Checked everything.\nNO ISSUES FOUND", wantNoIssues: true},
		{name: "findings win over sentinel", output: "MAJOR: x - y\nNO ISSUES FOUND", wantFindings: []string{"MAJOR: x - y"}},
		{name: "unparseable output", output: "The plan looks mostly fine, maybe check task 4."},
		{name: "empty output", output: ""},
		{name: "label without text is not a finding", output: "MAJOR:\nMINOR:   "},
		{name: "lowercase label is not a finding", output: "major: something"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings, noIssues := parsePlanFindings(tc.output)
			assert.Equal(t, tc.wantFindings, findings)
			assert.Equal(t, tc.wantNoIssues, noIssues)
		})
	}
}

func TestParseFindingSelection(t *testing.T) {
	tests := []struct {
		name    string
		answer  string
		n       int
		want    []int
		wantErr string
	}{
		{name: "all", answer: "all", n: 3, want: []int{1, 2, 3}},
		{name: "all findings option", answer: "All findings", n: 2, want: []int{1, 2}},
		{name: "none", answer: "none", n: 3},
		{name: "apply none", answer: "Apply None", n: 3},
		{name: "none option", answer: "None", n: 3},
		{name: "comma list", answer: "1,3", n: 3, want: []int{1, 3}},
		{name: "space list", answer: "1 3", n: 3, want: []int{1, 3}},
		{name: "comma and space", answer: "1, 3", n: 4, want: []int{1, 3}},
		{name: "range", answer: "2-4", n: 5, want: []int{2, 3, 4}},
		{name: "range and number", answer: "1, 3-4", n: 5, want: []int{1, 3, 4}},
		{name: "unsorted with duplicates", answer: "3,1,3", n: 3, want: []int{1, 3}},
		{name: "surrounding space", answer: "  2  ", n: 3, want: []int{2}},
		{name: "out of range high", answer: "4", n: 3, wantErr: "out of range"},
		{name: "zero", answer: "0", n: 3, wantErr: "out of range"},
		{name: "range end out of range", answer: "2-9", n: 3, wantErr: "out of range"},
		{name: "reversed range", answer: "4-2", n: 5, wantErr: "start is greater"},
		{name: "unknown text", answer: "maybe", n: 3, wantErr: "unknown selection"},
		{name: "mixed unknown", answer: "1,foo", n: 3, wantErr: "unknown selection"},
		{name: "empty", answer: "  ", n: 3, wantErr: "empty answer"},
		{name: "only separators", answer: ",", n: 3, wantErr: "no finding numbers"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFindingSelection(tc.answer, tc.n)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFormatFindingsFeedback(t *testing.T) {
	findings := []string{"MAJOR: a - one", "MINOR: b - two", "CRITICAL: c - three"}

	assert.Equal(t, "1. MAJOR: a - one\n3. CRITICAL: c - three", formatFindingsFeedback(findings, []int{1, 3}))
	assert.Equal(t, "2. MINOR: b - two", formatFindingsFeedback(findings, []int{2}))
	assert.Empty(t, formatFindingsFeedback(findings, nil))
	assert.Equal(t, "1. MAJOR: a - one", formatFindingsFeedback(findings, []int{0, 1, 9}), "out of range indexes are skipped")
}
