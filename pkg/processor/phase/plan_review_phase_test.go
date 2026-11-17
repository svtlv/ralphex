package phase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/svtlv/ralphex/pkg/config"
	"github.com/svtlv/ralphex/pkg/executor"
	"github.com/svtlv/ralphex/pkg/processor/phase/mocks"
	"github.com/svtlv/ralphex/pkg/status"
)

const (
	findingsOutput = "MAJOR: Task 2 - wrong test command\nMINOR: Overview - typo"
	planReadyRes   = "revised"
)

// eventLog records logger events in call order so tests can assert sequencing and content.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, fmt.Sprintf(format, args...))
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (l *eventLog) count(prefix, contains string) int {
	n := 0
	for _, e := range l.all() {
		if strings.HasPrefix(e, prefix) && strings.Contains(e, contains) {
			n++
		}
	}
	return n
}

func (l *eventLog) has(prefix, contains string) bool { return l.count(prefix, contains) > 0 }

// fakeCustomRunner is a CustomRunner that records the prompt file path and replays a fixed output.
type fakeCustomRunner struct {
	output     string
	promptFile string
	promptText string
}

func (f *fakeCustomRunner) Run(_ context.Context, _, promptFile string) (io.Reader, func() error, error) {
	f.promptFile = promptFile
	if data, err := os.ReadFile(promptFile); err == nil { //nolint:gosec // test temp file
		f.promptText = string(data)
	}
	return strings.NewReader(f.output), func() error { return nil }, nil
}

type planReviewEnv struct {
	t        *testing.T
	reader   *fakeRepoReader
	planFile string
	events   *eventLog
	log      *mocks.PlanReviewLoggerMock
	prompts  *mocks.PlanReviewPromptsMock
	input    *inputCollectorMock
	planner  *executorMock
	external *executorMock
	custom   *executor.CustomExecutor
	runner   *fakeCustomRunner
	cfg      Config
	holder   *status.PhaseHolder
	policy   *testPolicy
	answers  []string
	// reviewer, when set, answers every reviewer question; answers then feeds only the findings question
	reviewer string
}

// newPlanReviewEnv builds a phase fixture: a temp repo with a plan file and mocked collaborators.
// reviewer is the answer to every reviewer question; "" takes reviewer answers from e.answers too.
// planner and external executors return no output until a test sets RunFunc.
func newPlanReviewEnv(t *testing.T, reviewer string) *planReviewEnv {
	t.Helper()
	reader, planFile := guardEnv(t)
	events := &eventLog{}
	appCfg := testAppConfig(t)
	appCfg.ExternalReviewTool = "codex"

	e := &planReviewEnv{
		t: t, reader: reader, planFile: planFile, events: events,
		planner: &executorMock{}, external: &executorMock{}, holder: &status.PhaseHolder{}, reviewer: reviewer,
		cfg: Config{CodexEnabled: true, ExternalReviewToolSet: true, AppConfig: appCfg},
	}
	e.log = &mocks.PlanReviewLoggerMock{
		PrintFunc: func(format string, args ...any) { events.add("print: "+format, args...) },
		PrintAlignedFunc: func(text string) {
			events.add("aligned: %s", text)
		},
		PrintSectionFunc: func(section status.Section) { events.add("section: %s", section.Label) },
		LogQuestionFunc: func(question string, options []string) {
			events.add("question: %s | %s", question, strings.Join(options, ","))
		},
		LogAnswerFunc: func(answer string) { events.add("answer: %s", answer) },
	}
	e.prompts = &mocks.PlanReviewPromptsMock{
		PlanReviewPromptFunc: func(planFile, description string) string {
			return "review prompt " + planFile + " " + description
		},
		PlanRevisionPromptFunc: func(planFile, description, findings string) string {
			return "revision prompt " + findings
		},
	}
	e.input = &inputCollectorMock{AskQuestionFunc: func(_ context.Context, question string, _ []string) (string, error) {
		if question == planReviewerQuestion && e.reviewer != "" {
			return e.reviewer, nil
		}
		if len(e.answers) == 0 {
			return "", errors.New("no more mock answers")
		}
		a := e.answers[0]
		e.answers = e.answers[1:]
		return a, nil
	}}
	e.policy = newTestPolicy(e.cfg, &printAdapter{log: events})
	return e
}

// printAdapter adapts the event log to the Logger interface used by the test policy.
type printAdapter struct{ log *eventLog }

func (p *printAdapter) Print(format string, args ...any) { p.log.add("print: "+format, args...) }

func (e *planReviewEnv) withCustom(output string) {
	e.runner = &fakeCustomRunner{output: output}
	e.custom = &executor.CustomExecutor{Script: "/bin/fake-review", ErrorPatterns: []string{"boom"}}
	e.custom.SetRunner(e.runner)
	e.cfg.AppConfig.ExternalReviewTool = "custom"
}

func (e *planReviewEnv) phase() *PlanReviewPhase {
	return NewPlanReviewPhase(PlanReviewPhaseOpts{
		Cfg: e.cfg, Log: e.log, Planner: e.planner, External: e.externalIface(), Custom: e.custom,
		Policy: e.policy, Prompts: e.prompts,
		Deps:        &Deps{InputCollector: e.input, RepoState: e.reader},
		PhaseHolder: e.holder,
	})
}

// externalIface returns nil when the external executor is disabled, avoiding a typed-nil interface.
func (e *planReviewEnv) externalIface() Executor {
	if e.external == nil {
		return nil
	}
	return e.external
}

func (e *planReviewEnv) run(ctx context.Context) error {
	e.t.Helper()
	return e.phase().Run(ctx, PlanReviewRequest{PlanFile: e.planFile, Description: "add feature"})
}

// tamperPlan simulates a reviewer that rewrites the plan file.
func (e *planReviewEnv) tamperPlan() {
	e.t.Helper()
	require.NoError(e.t, os.WriteFile(e.planFile, []byte("# tampered\n"), 0o600))
}

func (e *planReviewEnv) readPlan() string {
	e.t.Helper()
	data, err := os.ReadFile(e.planFile)
	require.NoError(e.t, err)
	return string(data)
}

func fixedResults(results ...executor.Result) func(context.Context, string) executor.Result {
	i := 0
	return func(context.Context, string) executor.Result {
		if i >= len(results) {
			return executor.Result{}
		}
		r := results[i]
		i++
		return r
	}
}

// findingsCalls returns only the findings question calls, leaving out the reviewer question.
func findingsCalls(m *inputCollectorMock) []string {
	out := []string{}
	for _, c := range m.AskQuestionCalls() {
		if c.Question == findingsQuestion {
			out = append(out, c.Question)
		}
	}
	return out
}

func questionTexts(m *inputCollectorMock) []string {
	calls := m.AskQuestionCalls()
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Question)
	}
	return out
}

func TestPlanReviewPhase_Run_AskExternalApplyThenSkip(t *testing.T) {
	e := newPlanReviewEnv(t, "")
	e.answers = []string{reviewerOptionExternal, findingsOptionAll, reviewerOptionSkip}
	e.external.RunFunc = fixedResults(executor.Result{Output: findingsOutput})
	e.planner.RunFunc = fixedResults(executor.Result{Output: planReadyRes, Signal: status.PlanReady})

	require.NoError(t, e.run(t.Context()))

	assert.Equal(t, []string{planReviewerQuestion, findingsQuestion, planReviewerQuestion}, questionTexts(e.input))
	calls := e.input.AskQuestionCalls()
	assert.Equal(t, []string{reviewerOptionExternal, reviewerOptionSame, reviewerOptionSkip}, calls[0].Options)
	assert.Equal(t, []string{findingsOptionAll, findingsOptionNone}, calls[1].Options)
	assert.Len(t, e.external.RunCalls(), 1)
	require.Len(t, e.planner.RunCalls(), 1)
	assert.Equal(t, "revision prompt 1. MAJOR: Task 2 - wrong test command\n2. MINOR: Overview - typo", e.planner.RunCalls()[0].Prompt)
	assert.True(t, e.events.has("print: plan review skipped", ""))
	assert.True(t, e.events.has("print: plan revised", ""))
}

func TestPlanReviewPhase_Run_SameAgentReviewer(t *testing.T) {
	e := newPlanReviewEnv(t, "")
	e.answers = []string{reviewerOptionSame, findingsOptionNone}
	e.planner.RunFunc = fixedResults(executor.Result{Output: findingsOutput})

	require.NoError(t, e.run(t.Context()))

	assert.Empty(t, e.external.RunCalls())
	require.Len(t, e.planner.RunCalls(), 1)
	assert.Contains(t, e.planner.RunCalls()[0].Prompt, "review prompt")
	assert.True(t, e.events.has("print: no findings selected", ""))
}

func TestPlanReviewPhase_Run_CustomReviewerGetsPromptFile(t *testing.T) {
	e := newPlanReviewEnv(t, reviewerOptionExternal)
	e.withCustom(findingsOutput)
	e.answers = []string{findingsOptionNone}

	require.NoError(t, e.run(t.Context()))

	require.NotNil(t, e.runner)
	assert.NotEmpty(t, e.runner.promptFile, "custom script must receive a prompt file path")
	assert.Contains(t, e.runner.promptText, "review prompt "+e.planFile)
	assert.Empty(t, e.external.RunCalls())
	assert.Empty(t, e.planner.RunCalls())
	assert.Equal(t, []string{planReviewerQuestion, findingsQuestion}, questionTexts(e.input), "the reviewer question is always asked")
}

func TestPlanReviewPhase_Run_UnavailableExternal(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *planReviewEnv)
	}{
		{
			name:  "tool none",
			setup: func(e *planReviewEnv) { e.cfg.AppConfig.ExternalReviewTool = "none" },
		},
		{
			name:  "codex enabled but no executor",
			setup: func(e *planReviewEnv) { e.external = nil },
		},
		{
			name:  "explicit codex tool but codex binary missing",
			setup: func(e *planReviewEnv) { e.cfg.CodexEnabled = false },
		},
		{
			name:  "custom tool without script",
			setup: func(e *planReviewEnv) { e.cfg.AppConfig.ExternalReviewTool = "custom" },
		},
		{
			name: "codex executor skips external",
			setup: func(e *planReviewEnv) {
				e.cfg.AppConfig.Executor = config.ExecutorCodex
				e.cfg.AppConfig.ExternalReviewTool = "none"
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlanReviewEnv(t, "")
			tc.setup(e)
			e.answers = []string{reviewerOptionSkip}

			require.NoError(t, e.run(t.Context()))

			calls := e.input.AskQuestionCalls()
			require.Len(t, calls, 1)
			assert.Equal(t, []string{reviewerOptionSame, reviewerOptionSkip}, calls[0].Options, "External reviewer must not be offered")
			assert.Empty(t, e.planner.RunCalls())
		})
	}
}

func TestPlanReviewPhase_Run_AskHidesUnavailableExternal(t *testing.T) {
	e := newPlanReviewEnv(t, "")
	e.cfg.AppConfig.ExternalReviewTool = "none"
	e.answers = []string{reviewerOptionSkip}

	require.NoError(t, e.run(t.Context()))

	calls := e.input.AskQuestionCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{reviewerOptionSame, reviewerOptionSkip}, calls[0].Options)
}

func TestPlanReviewPhase_Run_AskHidesExternalWhenCodexBinaryMissing(t *testing.T) {
	e := newPlanReviewEnv(t, "")
	e.cfg.CodexEnabled = false // the factory clears it when the codex binary is not found
	e.answers = []string{reviewerOptionSkip}

	require.NoError(t, e.run(t.Context()))

	calls := e.input.AskQuestionCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{reviewerOptionSame, reviewerOptionSkip}, calls[0].Options)
}

func TestPlanReviewPhase_Run_AskUnknownAnswerSkips(t *testing.T) {
	e := newPlanReviewEnv(t, "")
	e.answers = []string{"banana"}

	require.NoError(t, e.run(t.Context()))

	assert.Empty(t, e.planner.RunCalls())
	assert.True(t, e.events.has("print: warning: unknown reviewer choice", ""))
}

func TestPlanReviewPhase_Run_InputErrors(t *testing.T) {
	t.Run("reviewer question error", func(t *testing.T) {
		e := newPlanReviewEnv(t, "")
		err := e.run(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "collect reviewer choice")
	})
	t.Run("findings question error", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		e.planner.RunFunc = fixedResults(executor.Result{Output: findingsOutput})
		err := e.run(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "collect findings selection")
	})
	t.Run("missing plan file argument", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		err := e.phase().Run(t.Context(), PlanReviewRequest{})
		require.Error(t, err)
	})
	t.Run("missing input collector", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		p := e.phase()
		p.deps = &Deps{}
		require.Error(t, p.Run(t.Context(), PlanReviewRequest{PlanFile: e.planFile}))
	})
}

func TestPlanReviewPhase_Run_ReviewerFailuresDoNotFailRun(t *testing.T) {
	tests := []struct {
		name    string
		result  ExecutionResult
		want    string
		wantNot bool
	}{
		{name: "error", result: ExecutionResult{Result: executor.Result{Error: errors.New("exit 1")}}, want: "plan review failed"},
		{name: "timeout", result: ExecutionResult{TimedOut: true}, want: "timed out"},
		{name: "empty output", result: ExecutionResult{Result: executor.Result{Output: "  \n"}}, want: "no output"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlanReviewEnv(t, reviewerOptionSame)
			e.policy = newScriptedTestPolicy(&printAdapter{log: e.events}, tc.result)

			require.NoError(t, e.run(t.Context()))

			assert.True(t, e.events.has("print: warning:", tc.want), e.events.all())
			assert.Empty(t, findingsCalls(e.input))
		})
	}
}

func TestPlanReviewPhase_Run_PatternErrorIsNotSwallowed(t *testing.T) {
	t.Run("reviewer", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		e.planner.RunFunc = fixedResults(executor.Result{Error: &executor.LimitPatternError{Pattern: "rate limit", HelpCmd: "claude /usage"}})

		err := e.run(t.Context())

		var limitErr *executor.LimitPatternError
		require.ErrorAs(t, err, &limitErr)
		assert.Contains(t, err.Error(), "pattern handling")
		assert.True(t, e.events.has("print: error: detected", "rate limit"), "policy handler must log the match")
	})
	t.Run("custom reviewer pattern error", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionExternal)
		e.withCustom("boom")
		e.runner.output = "boom"
		e.custom.SetRunner(&failingRunner{out: "boom happened"})

		err := e.run(t.Context())

		var patternErr *executor.PatternMatchError
		require.ErrorAs(t, err, &patternErr)
	})
	t.Run("revision session", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		e.answers = []string{findingsOptionAll}
		e.planner.RunFunc = fixedResults(
			executor.Result{Output: findingsOutput},
			executor.Result{Error: &executor.PatternMatchError{Pattern: "quota", HelpCmd: "claude /status"}},
		)

		err := e.run(t.Context())

		var patternErr *executor.PatternMatchError
		require.ErrorAs(t, err, &patternErr)
	})
}

// failingRunner makes the custom script exit non-zero after printing a message.
type failingRunner struct{ out string }

func (f *failingRunner) Run(_ context.Context, _, _ string) (io.Reader, func() error, error) {
	return strings.NewReader(f.out), func() error { return errors.New("exit status 1") }, nil
}

func TestPlanReviewPhase_Run_ContextCancel(t *testing.T) {
	t.Run("before first round", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := e.run(ctx)

		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, e.planner.RunCalls())
	})
	t.Run("during reviewer call", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		ctx, cancel := context.WithCancel(t.Context())
		e.planner.RunFunc = func(context.Context, string) executor.Result {
			cancel()
			return executor.Result{Error: context.Canceled}
		}

		err := e.run(ctx)

		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestPlanReviewPhase_Run_NoIssuesAndUnparseable(t *testing.T) {
	t.Run("no issues", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		e.planner.RunFunc = fixedResults(executor.Result{Output: "looks fine\nNO ISSUES FOUND"})

		require.NoError(t, e.run(t.Context()))

		assert.True(t, e.events.has("print: plan review found no issues", ""))
		assert.Empty(t, findingsCalls(e.input))
	})
	t.Run("unparseable output is logged raw with a warning", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		e.planner.RunFunc = fixedResults(executor.Result{Output: "I think the plan is mostly fine."})

		require.NoError(t, e.run(t.Context()))

		assert.True(t, e.events.has("print: warning: plan review output has no findings", ""))
		assert.True(t, e.events.has("aligned:", "I think the plan is mostly fine."))
		assert.Empty(t, findingsCalls(e.input))
	})
}

func TestPlanReviewPhase_Run_ReviewerOutputNotLoggedTwice(t *testing.T) {
	e := newPlanReviewEnv(t, reviewerOptionSame)
	e.answers = []string{findingsOptionNone}
	e.planner.RunFunc = fixedResults(executor.Result{Output: "intro prose\n" + findingsOutput + "\n" + findingsOutput})

	require.NoError(t, e.run(t.Context()))

	assert.False(t, e.events.has("aligned:", "intro prose"), "raw reviewer output is streamed by the executor, not logged here")
	assert.Equal(t, 1, e.events.count("aligned:", "wrong test command"), "findings are printed once, numbered and deduplicated")
	assert.True(t, e.events.has("aligned:", "  1. MAJOR: Task 2 - wrong test command"))
	assert.True(t, e.events.has("aligned:", "  2. MINOR: Overview - typo"))
}

func TestPlanReviewPhase_Run_SectionPerRound(t *testing.T) {
	e := newPlanReviewEnv(t, reviewerOptionSame)
	e.answers = []string{findingsOptionAll, findingsOptionAll}
	e.planner.RunFunc = fixedResults(
		executor.Result{Output: findingsOutput},
		executor.Result{Signal: status.PlanReady},
		executor.Result{Output: findingsOutput},
		executor.Result{Signal: status.PlanReady},
		executor.Result{Output: noIssuesSentinel},
	)

	require.NoError(t, e.run(t.Context()))

	assert.True(t, e.events.has("section:", "plan review round 1"))
	assert.True(t, e.events.has("section:", "plan review round 2"))
	assert.True(t, e.events.has("section:", "plan review round 3"))
	assert.Equal(t, 3, e.events.count("section:", ""))
	assert.Equal(t, status.PhasePlan, e.holder.Get())
}

func TestPlanReviewPhase_Run_RoundCap(t *testing.T) {
	e := newPlanReviewEnv(t, reviewerOptionSame)
	for range maxPlanReviewRounds {
		e.answers = append(e.answers, findingsOptionAll)
	}
	e.planner.RunFunc = func(_ context.Context, prompt string) executor.Result {
		if strings.HasPrefix(prompt, "revision prompt") {
			return executor.Result{Signal: status.PlanReady}
		}
		return executor.Result{Output: findingsOutput}
	}

	require.NoError(t, e.run(t.Context()))

	assert.Equal(t, maxPlanReviewRounds, e.events.count("section:", "plan review round"))
	assert.True(t, e.events.has("print: max plan review rounds", ""))
}

func TestPlanReviewPhase_Run_FindingSelection(t *testing.T) {
	t.Run("subset keeps original numbers", func(t *testing.T) {
		e := newPlanReviewEnv(t, "")
		e.answers = []string{reviewerOptionSame, "2", reviewerOptionSkip}
		e.planner.RunFunc = fixedResults(
			executor.Result{Output: findingsOutput},
			executor.Result{Signal: status.PlanReady},
		)

		require.NoError(t, e.run(t.Context()))

		require.Len(t, e.planner.RunCalls(), 2)
		assert.Equal(t, "revision prompt 2. MINOR: Overview - typo", e.planner.RunCalls()[1].Prompt)
	})
	t.Run("invalid answer is re-asked then accepted", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		e.answers = []string{"banana", "9", findingsOptionNone}
		e.planner.RunFunc = fixedResults(executor.Result{Output: findingsOutput})

		require.NoError(t, e.run(t.Context()))

		assert.Equal(t, 3, e.events.count("question:", findingsQuestion))
		assert.Equal(t, 2, e.events.count("print: warning:", ""))
		assert.Len(t, e.planner.RunCalls(), 1)
	})
	t.Run("three invalid answers count as none", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		e.answers = []string{"banana", "9", "x y"}
		e.planner.RunFunc = fixedResults(executor.Result{Output: findingsOutput})

		require.NoError(t, e.run(t.Context()))

		assert.Equal(t, maxFindingSelectionAttempts, e.events.count("question:", findingsQuestion))
		assert.True(t, e.events.has("print: warning: no valid findings selection", ""))
		assert.Len(t, e.planner.RunCalls(), 1, "no revision session runs")
	})
}

func TestPlanReviewPhase_Run_RevisionFailures(t *testing.T) {
	tests := []struct {
		name   string
		result ExecutionResult
		want   string
	}{
		{name: "no PLAN_READY", result: ExecutionResult{Result: executor.Result{Output: "done"}}, want: "without PLAN_READY"},
		{name: "failed signal", result: ExecutionResult{Result: executor.Result{Signal: status.Failed}}, want: "without PLAN_READY"},
		{name: "timeout", result: ExecutionResult{TimedOut: true}, want: "timed out"},
		{name: "error", result: ExecutionResult{Result: executor.Result{Error: errors.New("exit 1")}}, want: "plan review failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlanReviewEnv(t, reviewerOptionSame)
			e.answers = []string{findingsOptionAll}
			e.planner.RunFunc = fixedResults(executor.Result{Output: findingsOutput})
			e.policy = newScriptedTestPolicy(&printAdapter{log: e.events},
				ExecutionResult{Result: executor.Result{Output: findingsOutput}}, tc.result)

			require.NoError(t, e.run(t.Context()))

			assert.True(t, e.events.has("print: warning:", tc.want), e.events.all())
			assert.Equal(t, 1, e.events.count("section:", ""), "review ends, no second round")
		})
	}
}

func TestPlanReviewPhase_Run_GuardRepoChanges(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(e *planReviewEnv)
		wantWarn     []string
		wantPlanBack bool
	}{
		{
			name: "code file edited",
			mutate: func(e *planReviewEnv) {
				writeGuardFile(e.t, e.reader.root, "main.go", "package main")
				e.reader.status = zStatus("?? docs/plans/p.md", "?? main.go")
			},
			wantWarn: []string{"repository changed", "main.go"},
		},
		{
			name:     "reviewer commit moves HEAD",
			mutate:   func(e *planReviewEnv) { e.reader.head = "bbbbbbbbbbbb" },
			wantWarn: []string{"repository changed", "HEAD moved from aaa to bbbbbbbb"},
		},
		{
			name:         "plan file edited is restored",
			mutate:       func(e *planReviewEnv) { e.tamperPlan() },
			wantWarn:     []string{"restored it from the saved copy"},
			wantPlanBack: true,
		},
		{
			name: "plan edited and code file edited",
			mutate: func(e *planReviewEnv) {
				e.tamperPlan()
				writeGuardFile(e.t, e.reader.root, "main.go", "package main")
				e.reader.status = zStatus("?? docs/plans/p.md", "?? main.go")
			},
			wantWarn:     []string{"restored it from the saved copy", "main.go"},
			wantPlanBack: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlanReviewEnv(t, reviewerOptionSame)
			e.answers = []string{findingsOptionAll}
			e.planner.RunFunc = func(context.Context, string) executor.Result {
				tc.mutate(e)
				return executor.Result{Output: findingsOutput}
			}

			require.NoError(t, e.run(t.Context()))

			for _, w := range tc.wantWarn {
				assert.True(t, e.events.has("print: warning:", w) || e.events.has("print: warning: plan review is read-only", w), "want %q in %v", w, e.events.all())
			}
			assert.True(t, e.events.has("print: warning: findings of this review round are discarded", ""))
			assert.Empty(t, findingsCalls(e.input), "findings of a tripped round are never shown")
			assert.Len(t, e.planner.RunCalls(), 1, "no revision session runs")
			if tc.wantPlanBack {
				assert.Equal(t, "# plan\n", e.readPlan())
			}
		})
	}
}

func TestPlanReviewPhase_Run_GuardPlanPathNotListedTwice(t *testing.T) {
	e := newPlanReviewEnv(t, reviewerOptionSame)
	e.planner.RunFunc = func(context.Context, string) executor.Result {
		e.tamperPlan()
		writeGuardFile(e.t, e.reader.root, "main.go", "package main")
		e.reader.status = zStatus("?? docs/plans/p.md", "?? main.go")
		return executor.Result{Output: findingsOutput}
	}

	require.NoError(t, e.run(t.Context()))

	assert.Equal(t, 0, e.events.count("print: warning: plan review is read-only", "docs/plans/p.md"), "restored plan path stays out of the changed-paths warning")
	assert.Equal(t, 1, e.events.count("print: warning: plan review is read-only", "main.go"))
}

func TestPlanReviewPhase_Run_GuardCatchesCodeEditWhenReviewerDeletesPlan(t *testing.T) {
	e := newPlanReviewEnv(t, reviewerOptionSame)
	e.planner.RunFunc = func(context.Context, string) executor.Result {
		require.NoError(t, os.Remove(e.planFile))
		writeGuardFile(e.t, e.reader.root, "main.go", "package main")
		e.reader.status = zStatus("?? main.go")
		return executor.Result{Output: findingsOutput}
	}

	require.NoError(t, e.run(t.Context()))

	assert.Equal(t, "# plan\n", e.readPlan(), "deleted plan is restored")
	assert.Equal(t, 1, e.events.count("print: warning: plan review is read-only", "main.go"), "code edit is still reported")
	assert.Equal(t, 0, e.events.count("print: warning: repo state unreadable", ""), "repo state was readable")
	assert.Empty(t, findingsCalls(e.input), "findings of a tripped round are never shown")
}

func TestPlanReviewPhase_Run_GuardIgnoresRalphexDir(t *testing.T) {
	e := newPlanReviewEnv(t, reviewerOptionSame)
	e.answers = []string{findingsOptionNone}
	e.planner.RunFunc = func(context.Context, string) executor.Result {
		writeGuardFile(e.t, e.reader.root, ".ralphex/progress/progress-x.txt", "log")
		e.reader.status = zStatus("?? docs/plans/p.md", "?? .ralphex/progress/progress-x.txt")
		return executor.Result{Output: findingsOutput}
	}

	require.NoError(t, e.run(t.Context()))

	assert.Len(t, findingsCalls(e.input), 1, "a .ralphex change does not trip the guard")
}

func TestPlanReviewPhase_Run_GuardFallsBackToPlanBytesOnly(t *testing.T) {
	t.Run("reader error before review", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		e.reader.headErr = errors.New("not a git repository")
		e.answers = []string{findingsOptionNone}
		e.planner.RunFunc = fixedResults(executor.Result{Output: findingsOutput})

		require.NoError(t, e.run(t.Context()))

		assert.True(t, e.events.has("print: warning: repo state unavailable", "not a git repository"))
		assert.Len(t, findingsCalls(e.input), 1, "review still runs")
	})
	t.Run("plan edit is still caught without repo state", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		e.reader.headErr = errors.New("not a git repository")
		e.planner.RunFunc = func(context.Context, string) executor.Result {
			e.tamperPlan()
			return executor.Result{Output: findingsOutput}
		}

		require.NoError(t, e.run(t.Context()))

		assert.Equal(t, "# plan\n", e.readPlan())
		assert.Empty(t, findingsCalls(e.input))
	})
	t.Run("reader error after review", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		e.answers = []string{findingsOptionNone}
		e.planner.RunFunc = func(context.Context, string) executor.Result {
			e.reader.statusErr = errors.New("git died")
			return executor.Result{Output: findingsOutput}
		}

		require.NoError(t, e.run(t.Context()))

		assert.True(t, e.events.has("print: warning: repo state unreadable after plan review", "git died"))
		assert.Len(t, findingsCalls(e.input), 1)
	})
	t.Run("nil reader", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		e.answers = []string{findingsOptionNone}
		e.planner.RunFunc = fixedResults(executor.Result{Output: findingsOutput})
		p := e.phase()
		p.deps = &Deps{InputCollector: e.input}

		require.NoError(t, p.Run(t.Context(), PlanReviewRequest{PlanFile: e.planFile}))

		assert.Len(t, findingsCalls(e.input), 1)
	})
	t.Run("plan file missing", func(t *testing.T) {
		e := newPlanReviewEnv(t, reviewerOptionSame)
		require.NoError(t, os.Remove(e.planFile))

		require.NoError(t, e.run(t.Context()))

		assert.True(t, e.events.has("print: warning: cannot read plan file", ""))
		assert.Empty(t, e.planner.RunCalls())
	})
}

func TestPlanReviewPhase_Run_GuardCoversFailedReviewer(t *testing.T) {
	e := newPlanReviewEnv(t, reviewerOptionSame)
	e.planner.RunFunc = func(context.Context, string) executor.Result {
		e.tamperPlan()
		return executor.Result{Error: errors.New("exit 1")}
	}

	require.NoError(t, e.run(t.Context()))

	assert.Equal(t, "# plan\n", e.readPlan(), "plan is restored even when the reviewer failed")
}

func TestPlanReviewPhase_Run_ExecutorNameFollowsConfig(t *testing.T) {
	var names []string
	e := newPlanReviewEnv(t, reviewerOptionSame)
	e.cfg.AppConfig.Executor = config.ExecutorCodex
	e.planner.RunFunc = fixedResults(executor.Result{Output: noIssuesSentinel})
	e.policy = newScriptedTestPolicy(&printAdapter{log: e.events})
	rec := &nameRecordingPolicy{Policy: e.policy, names: &names}
	p := e.phase()
	p.policy = rec

	require.NoError(t, p.Run(t.Context(), PlanReviewRequest{PlanFile: e.planFile}))

	assert.Equal(t, []string{"codex"}, names)
}

type nameRecordingPolicy struct {
	Policy
	names *[]string
}

func (p *nameRecordingPolicy) Run(ctx context.Context, run func(context.Context, string) executor.Result, prompt, tool string) ExecutionResult {
	*p.names = append(*p.names, tool)
	return p.Policy.Run(ctx, run, prompt, tool)
}

func TestPlanReviewPhase_Run_LogsQuestionsAnswersAndFindings(t *testing.T) {
	e := newPlanReviewEnv(t, "")
	e.answers = []string{reviewerOptionSame, "1,2", reviewerOptionSkip}
	e.planner.RunFunc = fixedResults(
		executor.Result{Output: findingsOutput},
		executor.Result{Signal: status.PlanReady},
	)

	require.NoError(t, e.run(t.Context()))

	events := e.events.all()
	assert.Contains(t, events, "question: "+planReviewerQuestion+" | External reviewer,Same agent,Skip review")
	assert.Contains(t, events, "question: "+findingsQuestion+" | All findings,None")
	assert.Contains(t, events, "answer: 1,2")
	assert.Equal(t, 2, e.events.count("answer: "+reviewerOptionSame, "")+e.events.count("answer: "+reviewerOptionSkip, ""))
	assert.True(t, e.events.has("aligned:", "  1. MAJOR: Task 2 - wrong test command"))
	assert.Len(t, e.log.LogQuestionCalls(), 3)
	assert.Len(t, e.log.LogAnswerCalls(), 3)
}

func TestResolveReviewerHelpers(t *testing.T) {
	t.Run("short hash", func(t *testing.T) {
		assert.Equal(t, "abcdef12", shortHash("abcdef1234567"))
		assert.Equal(t, "abc", shortHash("abc"))
	})
}
