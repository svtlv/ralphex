package phase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/svtlv/ralphex/pkg/executor"
	"github.com/svtlv/ralphex/pkg/processor/phase/mocks"
)

// sessionCall records one RunSession call made by the round under test.
type sessionCall struct {
	prompt      string
	spec        executor.SessionSpec
	ctxErr      error
	hasDeadline bool
}

// scriptedSessions builds a SessionExecutorMock that returns the given results in order and
// records every call. newIDs are handed out by NewSessionID in order ("" once exhausted).
func scriptedSessions(t *testing.T, canResume bool, newIDs []string, results ...executor.Result) (*mocks.SessionExecutorMock, *[]sessionCall) {
	t.Helper()
	var calls []sessionCall
	idIdx := 0
	mock := &mocks.SessionExecutorMock{
		CanResumeFunc: func() bool { return canResume },
		NewSessionIDFunc: func() string {
			if idIdx >= len(newIDs) {
				return ""
			}
			idIdx++
			return newIDs[idIdx-1]
		},
		RunSessionFunc: func(ctx context.Context, prompt string, spec executor.SessionSpec) executor.Result {
			_, hasDeadline := ctx.Deadline()
			calls = append(calls, sessionCall{prompt: prompt, spec: spec, ctxErr: ctx.Err(), hasDeadline: hasDeadline})
			require.LessOrEqual(t, len(calls), len(results), "unexpected extra RunSession call")
			res := results[len(calls)-1]
			if res.SessionID == "" && spec.ID != "" {
				res.SessionID = spec.ID
			}
			return res
		},
		RunFunc: func(context.Context, string) executor.Result {
			t.Fatal("RunSession executor must not be called through Run")
			return executor.Result{}
		},
	}
	return mock, &calls
}

func TestReviewRound_PassthroughWithoutSessions(t *testing.T) {
	t.Run("executor is not a session executor", func(t *testing.T) {
		var prompts []string
		exec := &fakeExecutor{run: func(_ context.Context, prompt string) executor.Result {
			prompts = append(prompts, prompt)
			return executor.Result{Output: "ok"}
		}}
		round := newReviewRound(t.Context(), exec, 0)
		round.Run(t.Context(), "full")
		round.Run(t.Context(), "full")
		round.noteTimedOut()
		round.Run(t.Context(), "full")
		assert.Equal(t, []string{"full", "full", "full"}, prompts)
	})

	t.Run("can not resume", func(t *testing.T) {
		mock, calls := scriptedSessions(t, false, nil)
		var prompts []string
		mock.RunFunc = func(_ context.Context, prompt string) executor.Result {
			prompts = append(prompts, prompt)
			return executor.Result{}
		}
		round := newReviewRound(t.Context(), mock, 0)
		round.Run(t.Context(), "full")
		round.Run(t.Context(), "full")
		assert.Equal(t, []string{"full", "full"}, prompts)
		assert.Empty(t, *calls)
		assert.Empty(t, mock.NewSessionIDCalls())
	})
}

func TestReviewRound_FirstAndResumeCalls(t *testing.T) {
	tests := []struct {
		name      string
		newIDs    []string
		firstRes  executor.Result
		wantFirst executor.SessionSpec
		wantNext  executor.SessionSpec // zero value means the second call starts a new session
		wantNewID string               // id expected for the second call's new session
	}{
		{
			name:      "claude style id from NewSessionID",
			newIDs:    []string{"id-1"},
			wantFirst: executor.SessionSpec{ID: "id-1"},
			wantNext:  executor.SessionSpec{ID: "id-1", Resume: true},
		},
		{
			name:      "codex style id assigned by the executor",
			firstRes:  executor.Result{SessionID: "thread-9"},
			wantFirst: executor.SessionSpec{},
			wantNext:  executor.SessionSpec{ID: "thread-9", Resume: true},
		},
		{
			name:      "codex run that reported no id starts a new session again",
			wantFirst: executor.SessionSpec{},
			wantNext:  executor.SessionSpec{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock, calls := scriptedSessions(t, true, tt.newIDs, tt.firstRes, executor.Result{})
			round := newReviewRound(t.Context(), mock, 0)

			round.Run(t.Context(), "full prompt")
			round.Run(t.Context(), "full prompt")

			require.Len(t, *calls, 2)
			assert.Equal(t, "full prompt", (*calls)[0].prompt)
			assert.Equal(t, tt.wantFirst, (*calls)[0].spec)
			assert.Equal(t, tt.wantNext, (*calls)[1].spec)
			if tt.wantNext.Resume {
				assert.Equal(t, resumeRoundPrompt, (*calls)[1].prompt)
			} else {
				assert.Equal(t, "full prompt", (*calls)[1].prompt)
			}
		})
	}
}

func TestReviewRound_ResumeKeepsResultAndErrors(t *testing.T) {
	patternErr := &executor.PatternMatchError{Pattern: "boom", HelpCmd: "claude /usage"}
	mock, calls := scriptedSessions(t, true, []string{"id-1"},
		executor.Result{Output: "first", Error: &executor.LimitPatternError{Pattern: "limit"}},
		executor.Result{Output: "second", Signal: "REVIEW_DONE"},
		executor.Result{Error: patternErr},
	)
	round := newReviewRound(t.Context(), mock, 0)

	first := round.Run(t.Context(), "full")
	require.Error(t, first.Error)
	second := round.Run(t.Context(), "full")
	assert.Equal(t, "second", second.Output)
	assert.Equal(t, "REVIEW_DONE", second.Signal)
	third := round.Run(t.Context(), "full")
	assert.Same(t, patternErr, third.Error)
	assert.Len(t, *calls, 3)
}

func TestReviewRound_FallbackOnMissingSession(t *testing.T) {
	tests := []struct {
		name   string
		result executor.Result
	}{
		{name: "claude message in diagnostic text", result: executor.Result{
			DiagnosticText: "No conversation found with session ID: id-1", Error: errors.New("exit status 1")}},
		{name: "claude message is case-insensitive", result: executor.Result{
			DiagnosticText: "NO CONVERSATION FOUND", Error: errors.New("exit status 1")}},
		{name: "codex message in error stderr tail", result: executor.Result{
			Error: errors.New("codex exited with error: exit status 1\nstderr: ERROR: no rollout found for thread id abc")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2"},
				executor.Result{}, tt.result, executor.Result{Output: "fresh", Signal: "REVIEW_DONE"})
			round := newReviewRound(t.Context(), mock, 0)

			round.Run(t.Context(), "full prompt")
			got := round.Run(t.Context(), "full prompt")

			require.Len(t, *calls, 3)
			assert.Equal(t, executor.SessionSpec{ID: "id-1", Resume: true}, (*calls)[1].spec)
			assert.Equal(t, executor.SessionSpec{ID: "id-2"}, (*calls)[2].spec)
			assert.Equal(t, "full prompt", (*calls)[2].prompt)
			assert.Equal(t, "fresh", got.Output)
			assert.NoError(t, got.Error)
		})
	}
}

func TestReviewRound_NoFallback(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name   string
		parent context.Context
		result executor.Result
	}{
		{name: "empty output with plain error", parent: context.Background(),
			result: executor.Result{Error: errors.New("exit status 1")}},
		{name: "unrelated error text", parent: context.Background(),
			result: executor.Result{Output: "something else", Error: errors.New("exit status 1")}},
		{name: "marker without error", parent: context.Background(),
			result: executor.Result{DiagnosticText: "No conversation found"}},
		{name: "model output quotes the marker", parent: context.Background(),
			result: executor.Result{Output: "round.go matches No conversation found", Error: errors.New("exit status 1")}},
		{name: "recent model text quotes the marker", parent: context.Background(),
			result: executor.Result{RecentText: "no rollout found for thread id", Error: errors.New("signal: killed")}},
		{name: "pattern error", parent: context.Background(),
			result: executor.Result{DiagnosticText: "No conversation found", Error: &executor.PatternMatchError{Pattern: "x"}}},
		{name: "limit error", parent: context.Background(),
			result: executor.Result{DiagnosticText: "No conversation found", Error: &executor.LimitPatternError{Pattern: "x"}}},
		{name: "retry error", parent: context.Background(),
			result: executor.Result{DiagnosticText: "No conversation found", Error: &executor.RetryPatternError{Pattern: "x"}}},
		{name: "phase context canceled", parent: canceled,
			result: executor.Result{DiagnosticText: "No conversation found", Error: errors.New("context canceled")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2"}, executor.Result{}, tt.result)
			round := newReviewRound(tt.parent, mock, 0)

			round.Run(context.Background(), "full")
			got := round.Run(context.Background(), "full")

			assert.Len(t, *calls, 2, "no fallback session may start")
			assert.Equal(t, tt.result.Error, got.Error)
			assert.Len(t, mock.NewSessionIDCalls(), 1)
		})
	}
}

func TestReviewRound_FallbackRunsUnderFreshTimeout(t *testing.T) {
	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-expired.Done()

	t.Run("timeout configured", func(t *testing.T) {
		mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2"},
			executor.Result{},
			executor.Result{DiagnosticText: "No conversation found", Error: errors.New("exit status 1")},
			executor.Result{Output: "fresh"})
		round := newReviewRound(t.Context(), mock, time.Minute)

		round.Run(t.Context(), "full")
		got := round.Run(expired, "full") // the policy context of this attempt is already expired

		require.Len(t, *calls, 3)
		require.Error(t, (*calls)[1].ctxErr, "resume ran under the expired context")
		require.NoError(t, (*calls)[2].ctxErr, "fallback must not inherit the expired context")
		assert.True(t, (*calls)[2].hasDeadline, "fallback gets its own session timeout")
		assert.Equal(t, "fresh", got.Output)
	})

	t.Run("no timeout configured", func(t *testing.T) {
		mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2"},
			executor.Result{},
			executor.Result{DiagnosticText: "No conversation found", Error: errors.New("exit status 1")},
			executor.Result{})
		round := newReviewRound(t.Context(), mock, 0)

		round.Run(t.Context(), "full")
		round.Run(expired, "full")

		require.Len(t, *calls, 3)
		require.NoError(t, (*calls)[2].ctxErr)
		assert.False(t, (*calls)[2].hasDeadline)
	})
}

func TestReviewRound_FallbackFollowedByResume(t *testing.T) {
	mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2"},
		executor.Result{},
		executor.Result{DiagnosticText: "No conversation found", Error: errors.New("exit status 1")},
		executor.Result{},
		executor.Result{})
	round := newReviewRound(t.Context(), mock, 0)

	round.Run(t.Context(), "full")
	round.Run(t.Context(), "full") // resume fails, fallback starts id-2
	round.Run(t.Context(), "full") // resumes id-2

	require.Len(t, *calls, 4)
	assert.Equal(t, executor.SessionSpec{ID: "id-2", Resume: true}, (*calls)[3].spec)
	assert.Equal(t, resumeRoundPrompt, (*calls)[3].prompt)
}

func TestReviewRound_FallbackSessionTimesOutThenResumes(t *testing.T) {
	mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2"},
		executor.Result{},
		executor.Result{DiagnosticText: "No conversation found", Error: errors.New("exit status 1")},
		executor.Result{Error: errors.New("signal: killed")}, // fallback session id-2 times out
		executor.Result{})
	round := newReviewRound(t.Context(), mock, 0)

	round.Run(t.Context(), "full")
	round.Run(t.Context(), "full") // resume fails, fallback starts id-2 and times out
	round.noteTimedOut()           // first timeout of id-2 keeps it
	round.Run(t.Context(), "full") // resumes id-2

	require.Len(t, *calls, 4)
	assert.Equal(t, executor.SessionSpec{ID: "id-2"}, (*calls)[2].spec)
	assert.Equal(t, executor.SessionSpec{ID: "id-2", Resume: true}, (*calls)[3].spec)
	assert.Equal(t, resumeRoundPrompt, (*calls)[3].prompt)
}

func TestReviewRound_NoteTimedOut(t *testing.T) {
	mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2", "id-3"},
		executor.Result{}, executor.Result{}, executor.Result{}, executor.Result{}, executor.Result{})
	round := newReviewRound(t.Context(), mock, 0)

	round.Run(t.Context(), "full") // new session id-1
	round.noteTimedOut()           // first timeout keeps the session
	round.Run(t.Context(), "full") // resumes id-1
	round.noteTimedOut()           // second timeout drops it
	round.Run(t.Context(), "full") // new session id-2, full prompt
	round.Run(t.Context(), "full") // resumes id-2
	round.noteTimedOut()
	round.Run(t.Context(), "full") // resumes id-2: the timeout count restarted with the new session

	require.Len(t, *calls, 5)
	want := []struct {
		prompt string
		spec   executor.SessionSpec
	}{
		{"full", executor.SessionSpec{ID: "id-1"}},
		{resumeRoundPrompt, executor.SessionSpec{ID: "id-1", Resume: true}},
		{"full", executor.SessionSpec{ID: "id-2"}},
		{resumeRoundPrompt, executor.SessionSpec{ID: "id-2", Resume: true}},
		{resumeRoundPrompt, executor.SessionSpec{ID: "id-2", Resume: true}},
	}
	for i, w := range want {
		assert.Equal(t, w.prompt, (*calls)[i].prompt, "call %d prompt", i)
		assert.Equal(t, w.spec, (*calls)[i].spec, "call %d spec", i)
	}
}

func TestReviewRound_NoteTimedOutWithoutSession(t *testing.T) {
	round := newReviewRound(t.Context(), &fakeExecutor{run: func(context.Context, string) executor.Result {
		return executor.Result{}
	}}, 0)
	assert.NotPanics(t, round.noteTimedOut)

	mock, calls := scriptedSessions(t, true, []string{"id-1"}, executor.Result{})
	fresh := newReviewRound(t.Context(), mock, 0)
	fresh.noteTimedOut() // nothing has run yet
	fresh.Run(t.Context(), "full")
	assert.Equal(t, executor.SessionSpec{ID: "id-1"}, (*calls)[0].spec)
}

func TestResumeRoundPrompt_KeepsOriginalCompletionRules(t *testing.T) {
	lower := strings.ToLower(resumeRoundPrompt)
	assert.Contains(t, lower, "do not repeat")
	assert.Contains(t, lower, "completion rules of the original instructions")
	assert.Contains(t, lower, "only where those instructions require one")
	assert.NotContains(t, resumeRoundPrompt, "<<<RALPHEX:", "the continue prompt must not demand a signal on its own")
}

// fakeExecutor is a plain Executor without session support.
type fakeExecutor struct {
	run func(ctx context.Context, prompt string) executor.Result
}

func (f *fakeExecutor) Run(ctx context.Context, prompt string) executor.Result {
	return f.run(ctx, prompt)
}
