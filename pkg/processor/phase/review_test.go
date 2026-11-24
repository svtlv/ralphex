package phase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/svtlv/ralphex/pkg/config"
	"github.com/svtlv/ralphex/pkg/executor"
	"github.com/svtlv/ralphex/pkg/status"
)

const testHeadA = "aaaa00000000000000000000000000000000aaaa"
const testHeadB = "bbbb00000000000000000000000000000000bbbb"

type reviewPhaseTestOpts struct {
	cfg  Config
	exec Executor
	log  *mockLogger
}

func reviewPhaseFromRunner(t *testing.T, opts reviewPhaseTestOpts) (*reviewPhase, *mockLogger) {
	t.Helper()
	if opts.cfg.AppConfig == nil {
		opts.cfg.AppConfig = testAppConfig(t)
	}
	if opts.log == nil {
		opts.log = newMockLogger("progress.txt")
	}
	if opts.exec == nil {
		opts.exec = newTaskPhaseMockExecutor(nil)
	}
	r := newTestRunner(testRunnerOpts{cfg: opts.cfg, log: opts.log, execs: Executors{Task: opts.exec, Review: opts.exec}, holder: &status.PhaseHolder{}})
	phase, ok := r.phases.review.(*reviewPhase)
	require.True(t, ok)
	return phase, opts.log
}

func TestReviewPhase_First_FailedSignal(t *testing.T) {
	exec := newTaskPhaseMockExecutor([]executor.Result{{Output: "error", Signal: status.Failed}})
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: exec})

	err := phase.First(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "FAILED signal")
}

func TestReviewPhase_First_PatternMatchError(t *testing.T) {
	exec := newTaskPhaseMockExecutor([]executor.Result{{Error: &executor.PatternMatchError{Pattern: "limit", HelpCmd: "usage"}}})
	phase, log := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: exec})

	err := phase.First(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "limit")
	assertLogContains(t, log, "detected")
}

func TestReviewPhase_Loop_PatternMatchError(t *testing.T) {
	exec := newTaskPhaseMockExecutor([]executor.Result{{Error: &executor.PatternMatchError{Pattern: "boom", HelpCmd: "usage"}}})
	phase, log := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: exec})

	err := phase.Loop(t.Context(), "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	assertLogContains(t, log, "detected")
}

func TestWrapExecutorError(t *testing.T) {
	t.Run("nil error returns nil", func(t *testing.T) {
		assert.NoError(t, wrapExecutorError(newScriptedTestPolicy(newMockLogger("")), nil, "claude"))
	})

	t.Run("pattern match wraps as pattern handling", func(t *testing.T) {
		policy := newScriptedTestPolicy(newMockLogger(""))
		patternErr := &executor.PatternMatchError{Pattern: "rate limit", HelpCmd: "usage"}
		err := wrapExecutorError(policy, patternErr, "codex")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "codex pattern handling:")
		assert.Contains(t, err.Error(), "rate limit")
	})

	t.Run("plain error wraps as execution", func(t *testing.T) {
		err := wrapExecutorError(newScriptedTestPolicy(newMockLogger("")), errors.New("boom"), "claude")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "claude execution:")
		assert.Contains(t, err.Error(), "boom")
	})
}

func TestReviewPhase_Loop_NoCommitExit(t *testing.T) {
	exec := newTaskPhaseMockExecutor([]executor.Result{{Output: "looked at code, nothing to fix"}})
	phase, log := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: exec})
	phase.git.deps.Git = &gitCheckerMock{
		HeadHashFunc:        func() (string, error) { return "abc123def456abc123def456abc123def456abcd", nil },
		DiffFingerprintFunc: func() (string, error) { return "unchanged-diff", nil },
	}

	err := phase.Loop(t.Context(), "")

	require.NoError(t, err)
	assert.Len(t, exec.RunCalls(), 1)
	assertLogContains(t, log, "no changes detected")
}

func TestReviewPhase_Loop_CommitDetectedContinues(t *testing.T) {
	exec := newTaskPhaseMockExecutor([]executor.Result{{Output: "fixed issues"}, {Output: "review done", Signal: status.ReviewDone}})
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: exec})
	hashes := []string{
		"aaaa00000000000000000000000000000000aaaa",
		"bbbb00000000000000000000000000000000bbbb",
		"bbbb00000000000000000000000000000000bbbb",
	}
	idx := 0
	gitMock := &gitCheckerMock{HeadHashFunc: func() (string, error) {
		require.Less(t, idx, len(hashes), "unexpected extra HeadHash call #%d", idx)
		hash := hashes[idx]
		idx++
		return hash, nil
	}}
	phase.git.deps.Git = gitMock

	err := phase.Loop(t.Context(), "")

	require.NoError(t, err)
	assert.Len(t, exec.RunCalls(), 2)
	assert.Len(t, gitMock.HeadHashCalls(), 3)
}

func TestReviewPhase_Loop_GitCheckerNilSkipsNoCommitCheck(t *testing.T) {
	exec := newTaskPhaseMockExecutor([]executor.Result{{Output: "looking"}, {Output: "looking"}, {Output: "looking"}})
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 30}, exec: exec})

	err := phase.Loop(t.Context(), "")

	require.NoError(t, err)
	assert.Len(t, exec.RunCalls(), 3)
}

func TestReviewPhase_Loop_GitCheckerErrorSkipsNoCommitCheck(t *testing.T) {
	exec := newTaskPhaseMockExecutor([]executor.Result{{Output: "looking"}, {Output: "looking"}, {Output: "looking"}})
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 30}, exec: exec})
	phase.git.deps.Git = &gitCheckerMock{
		HeadHashFunc:        func() (string, error) { return "", errors.New("git HEAD error") },
		DiffFingerprintFunc: func() (string, error) { return "", errors.New("git diff error") },
	}

	err := phase.Loop(t.Context(), "")

	require.NoError(t, err)
	assert.Len(t, exec.RunCalls(), 3)
}

func TestReviewPhase_First_TimeoutWarningStillLogged(t *testing.T) {
	log := newMockLogger("progress.txt")
	exec := newTaskPhaseMockExecutor(nil)
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: exec, log: log})
	phase.policy = newScriptedTestPolicy(log, ExecutionResult{TimedOut: true})

	err := phase.First(t.Context())

	require.NoError(t, err)
	assert.Len(t, exec.RunCalls(), 1)
	assertLogContains(t, log, "session timed out")
}

func TestReviewPhase_Loop_TimeoutContinues(t *testing.T) {
	log := newMockLogger("progress.txt")
	exec := newTaskPhaseMockExecutor(nil)
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: exec, log: log})
	policy := newScriptedTestPolicy(log,
		ExecutionResult{TimedOut: true},
		ExecutionResult{Result: executor.Result{Output: "review done", Signal: status.ReviewDone}},
	)
	phase.policy = policy

	err := phase.Loop(t.Context(), "")

	require.NoError(t, err)
	assert.Len(t, exec.RunCalls(), 2)
	assertLogContains(t, log, "retrying review iteration")
	assert.Equal(t, []time.Duration{retryBackoff}, policy.sleepCalls, "timeout retry waits the backoff once")
}

func TestReviewPhase_First_CodexTimeoutSurfacesAsError(t *testing.T) {
	exec := newTaskPhaseMockExecutor(nil)
	appCfg := testAppConfig(t)
	appCfg.Executor = config.ExecutorCodex
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50, AppConfig: appCfg}, exec: exec})
	phase.policy = newScriptedTestPolicy(newMockLogger(""), ExecutionResult{Result: executor.Result{Output: "partial output"}, TimedOut: true})

	err := phase.First(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "first review pass timed out")
}

func TestReviewPhase_Loop_DoneSignalStopsBeforeTimeoutHandling(t *testing.T) {
	log := newMockLogger("progress.txt")
	exec := newTaskPhaseMockExecutor(nil)
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: exec, log: log})
	phase.policy = newScriptedTestPolicy(log, ExecutionResult{Result: executor.Result{Signal: status.ReviewDone}, TimedOut: true})

	err := phase.Loop(t.Context(), "")

	require.NoError(t, err)
	assert.Len(t, exec.RunCalls(), 1)
	assertLogNotContains(t, log, "retrying review iteration")
}

func assertLogContains(t *testing.T, log *mockLogger, text string) {
	t.Helper()
	for _, call := range log.PrintCalls() {
		if strings.Contains(call.Format, text) {
			return
		}
	}
	assert.Failf(t, "missing log", "expected log containing %q", text)
}

func assertLogNotContains(t *testing.T, log *mockLogger, text string) {
	t.Helper()
	for _, call := range log.PrintCalls() {
		if strings.Contains(call.Format, text) {
			assert.Failf(t, "unexpected log", "did not expect log containing %q", text)
		}
	}
}

// waitRetryPolicy simulates the wait retry of the real retry policy: Run calls run attempts
// times inside one Policy.Run call and returns the last result.
type waitRetryPolicy struct {
	*testPolicy
	attempts int
}

func (p *waitRetryPolicy) Run(ctx context.Context, run func(context.Context, string) executor.Result, prompt, _ string) ExecutionResult {
	var res executor.Result
	for range p.attempts {
		res = run(ctx, prompt)
	}
	return ExecutionResult{Result: res}
}

// headSequence returns a GitChecker whose HeadHash walks the given hashes and then repeats the last one.
func headSequence(hashes ...string) *gitCheckerMock {
	idx := 0
	return &gitCheckerMock{HeadHashFunc: func() (string, error) {
		hash := hashes[min(idx, len(hashes)-1)]
		idx++
		return hash, nil
	}}
}

func TestReviewPhase_First_WaitRetryResumesSession(t *testing.T) {
	sessions, calls := scriptedSessions(t, true, []string{"id-1"},
		executor.Result{Error: &executor.LimitPatternError{Pattern: "limit", HelpCmd: "usage"}},
		executor.Result{Output: "done", Signal: status.ReviewDone})
	log := newMockLogger("progress.txt")
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: sessions, log: log})
	phase.policy = &waitRetryPolicy{testPolicy: newScriptedTestPolicy(log), attempts: 2}

	err := phase.First(t.Context())

	require.NoError(t, err)
	require.Len(t, *calls, 2)
	assert.Equal(t, "first review prompt", (*calls)[0].prompt)
	assert.Equal(t, executor.SessionSpec{ID: "id-1"}, (*calls)[0].spec)
	assert.Equal(t, resumeRoundPrompt, (*calls)[1].prompt)
	assert.Equal(t, executor.SessionSpec{ID: "id-1", Resume: true}, (*calls)[1].spec)
}

func TestReviewPhase_Loop_TimeoutResumesOnceThenNewSession(t *testing.T) {
	sessions, calls := scriptedSessions(t, true, []string{"id-1", "id-2"},
		executor.Result{}, executor.Result{}, executor.Result{}, executor.Result{Signal: status.ReviewDone})
	log := newMockLogger("progress.txt")
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: sessions, log: log})
	phase.git.deps.Git = headSequence(testHeadA) // HEAD never moves
	phase.policy = newScriptedTestPolicy(log,
		ExecutionResult{TimedOut: true}, ExecutionResult{TimedOut: true}, ExecutionResult{TimedOut: true},
		ExecutionResult{Result: executor.Result{Signal: status.ReviewDone}})

	err := phase.Loop(t.Context(), "")

	require.NoError(t, err)
	require.Len(t, *calls, 4)
	assert.Equal(t, executor.SessionSpec{ID: "id-1"}, (*calls)[0].spec, "first try starts a session")
	assert.Equal(t, "second review prompt", (*calls)[0].prompt)
	assert.Equal(t, executor.SessionSpec{ID: "id-1", Resume: true}, (*calls)[1].spec, "first timeout is resumed")
	assert.Equal(t, resumeRoundPrompt, (*calls)[1].prompt)
	assert.Equal(t, executor.SessionSpec{ID: "id-2"}, (*calls)[2].spec, "second timeout in the session starts a new one")
	assert.Equal(t, "second review prompt", (*calls)[2].prompt)
	assert.Equal(t, executor.SessionSpec{ID: "id-2", Resume: true}, (*calls)[3].spec)
}

func TestReviewPhase_Loop_TimeoutWithMovedHeadDoesNotResume(t *testing.T) {
	sessions, calls := scriptedSessions(t, true, []string{"id-1", "id-2"}, executor.Result{}, executor.Result{})
	log := newMockLogger("progress.txt")
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: sessions, log: log})
	// round start A, after the timeout B (the round ended with a commit), next round start B
	phase.git.deps.Git = headSequence(testHeadA, testHeadB, testHeadB)
	phase.policy = newScriptedTestPolicy(log,
		ExecutionResult{TimedOut: true},
		ExecutionResult{Result: executor.Result{Signal: status.ReviewDone}})

	err := phase.Loop(t.Context(), "")

	require.NoError(t, err)
	require.Len(t, *calls, 2)
	assert.Equal(t, executor.SessionSpec{ID: "id-1"}, (*calls)[0].spec)
	assert.Equal(t, executor.SessionSpec{ID: "id-2"}, (*calls)[1].spec, "moved HEAD starts a new session")
	assert.Equal(t, "second review prompt", (*calls)[1].prompt)
}

func TestReviewPhase_Loop_TimeoutWithUnknownHeadDoesNotResume(t *testing.T) {
	sessions, calls := scriptedSessions(t, true, []string{"id-1", "id-2"}, executor.Result{}, executor.Result{})
	log := newMockLogger("progress.txt")
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: sessions, log: log})
	phase.policy = newScriptedTestPolicy(log,
		ExecutionResult{TimedOut: true},
		ExecutionResult{Result: executor.Result{Signal: status.ReviewDone}})

	err := phase.Loop(t.Context(), "") // no git checker: HEAD is unknown

	require.NoError(t, err)
	require.Len(t, *calls, 2)
	assert.False(t, (*calls)[1].spec.Resume)
	assert.Equal(t, "id-2", (*calls)[1].spec.ID)
}

func TestReviewPhase_SessionIDsNotReusedAcrossIterationsOrLoops(t *testing.T) {
	sessions, calls := scriptedSessions(t, true, []string{"id-1", "id-2", "id-3"},
		executor.Result{Output: "fixed issues"},
		executor.Result{Signal: status.ReviewDone},
		executor.Result{Signal: status.ReviewDone})
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: sessions})
	phase.git.deps.Git = headSequence(testHeadA, testHeadB, testHeadB, testHeadB)

	require.NoError(t, phase.Loop(t.Context(), ""))
	require.NoError(t, phase.Loop(t.Context(), ""))

	require.Len(t, *calls, 3)
	for i, want := range []string{"id-1", "id-2", "id-3"} {
		assert.Equal(t, executor.SessionSpec{ID: want}, (*calls)[i].spec)
		assert.Equal(t, "second review prompt", (*calls)[i].prompt)
	}
}

func TestReviewPhase_WrapperExecutorAlwaysGetsFullPrompt(t *testing.T) {
	var prompts []string
	exec := &fakeExecutor{run: func(_ context.Context, prompt string) executor.Result {
		prompts = append(prompts, prompt)
		return executor.Result{}
	}}
	log := newMockLogger("progress.txt")
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50}, exec: exec, log: log})
	phase.git.deps.Git = headSequence(testHeadA)
	phase.policy = &waitRetryPolicy{testPolicy: newScriptedTestPolicy(log), attempts: 2}

	require.NoError(t, phase.First(t.Context()))
	assert.Equal(t, []string{"first review prompt", "first review prompt"}, prompts)

	prompts = nil
	phase.policy = newScriptedTestPolicy(log,
		ExecutionResult{TimedOut: true}, ExecutionResult{Result: executor.Result{Signal: status.ReviewDone}})
	require.NoError(t, phase.Loop(t.Context(), ""))
	assert.Equal(t, []string{"second review prompt", "second review prompt"}, prompts)
}

func TestReviewPhase_SessionTimeoutFromConfig(t *testing.T) {
	appCfg := testAppConfig(t)
	appCfg.SessionTimeout = 3 * time.Minute
	phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{cfg: Config{MaxIterations: 50, AppConfig: appCfg}})
	assert.Equal(t, 3*time.Minute, phase.sessionTimeout())

	phase.cfg.AppConfig = nil
	assert.Zero(t, phase.sessionTimeout())
}
