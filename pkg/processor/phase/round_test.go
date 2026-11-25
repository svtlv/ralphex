package phase

import (
	"context"
	"errors"
	"fmt"
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
	deadline    time.Time
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
			deadline, hasDeadline := ctx.Deadline()
			calls = append(calls, sessionCall{prompt: prompt, spec: spec, ctxErr: ctx.Err(),
				hasDeadline: hasDeadline, deadline: deadline})
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
		{name: "codex message in diagnostic text", result: executor.Result{
			DiagnosticText: "ERROR: no rollout found for thread id abc", Error: errors.New("codex exited with error: exit status 1")}},
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

// a fallback session starts at the current HEAD, so startHead must follow it. otherwise a fallback
// that times out without a commit looks like a round that moved HEAD and gets dropped.
func TestReviewRound_FallbackUpdatesStartHead(t *testing.T) {
	tests := []struct {
		name     string
		head     func() string // nil leaves the round without a HEAD reader
		wantHead string
	}{
		{name: "moved head becomes the start head", head: func() string { return "new" }, wantHead: "new"},
		{name: "unreadable head becomes unknown", head: func() string { return "" }, wantHead: ""},
		{name: "no head reader keeps the start head", head: nil, wantHead: "old"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2"},
				executor.Result{},
				executor.Result{DiagnosticText: "No conversation found with session ID: id-1", Error: errors.New("exit status 1")},
				executor.Result{Output: "fresh"})
			round := newReviewRound(t.Context(), mock, 0)
			round.head = tt.head
			round.startHead = "old"

			round.Run(t.Context(), "full prompt")
			round.Run(t.Context(), "full prompt")

			require.Len(t, *calls, 3)
			assert.Equal(t, tt.wantHead, round.startHead)
		})
	}
}

// a rate-limit retry after a first attempt that kept no session id starts a new session without any
// HEAD comparison. startHead must still follow the HEAD that session starts at, or a retry that times
// out without a commit looks like a round that moved HEAD and gets dropped.
func TestReviewRound_NewSessionAfterLimitWithoutIDUpdatesStartHead(t *testing.T) {
	mock, calls := scriptedSessions(t, true, []string{"", ""},
		executor.Result{Error: &executor.LimitPatternError{Pattern: "limit"}},
		executor.Result{Output: "second"})
	head := "old"
	round := newReviewRound(t.Context(), mock, 0)
	round.head = func() string { return head }
	round.startHead = head

	round.Run(t.Context(), "full prompt")
	head = "new" // a commit landed while the policy waited out the limit
	round.Run(t.Context(), "full prompt")

	require.Len(t, *calls, 2)
	assert.Equal(t, "full prompt", (*calls)[1].prompt)
	assert.False(t, (*calls)[1].spec.Resume)
	assert.Equal(t, "new", round.startHead)
}

// the first attempt can commit before it hits the rate limit, so HEAD is already new when the limit
// is recorded and the retry sees no further move. the replacement session still starts at that HEAD,
// so startHead must follow it or a retry that times out without a commit gets dropped.
func TestReviewRound_NewSessionAfterLimitWithoutIDRefreshesStaleStartHead(t *testing.T) {
	mock, calls := scriptedSessions(t, true, []string{"", ""},
		executor.Result{Error: &executor.LimitPatternError{Pattern: "limit"}},
		executor.Result{Output: "second"})
	head := "old"
	round := newReviewRound(t.Context(), mock, 0)
	round.head = func() string { return head }
	round.startHead = head

	head = "new" // the first attempt committed before the limit was recorded
	round.Run(t.Context(), "full prompt")
	round.Run(t.Context(), "full prompt")

	require.Len(t, *calls, 2)
	assert.False(t, (*calls)[1].spec.Resume)
	assert.Equal(t, "new", round.startHead)
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
		{name: "codex stderr tail in the error quotes the marker", parent: context.Background(),
			result: executor.Result{Error: errors.New("codex exited with error: exit status 1\nstderr: no rollout found for thread id abc")}},
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
		{name: "error wraps deadline exceeded", parent: context.Background(),
			result: executor.Result{DiagnosticText: "No conversation found", Error: fmt.Errorf("claude: %w", context.DeadlineExceeded)}},
		{name: "error wraps canceled", parent: context.Background(),
			result: executor.Result{DiagnosticText: "No conversation found", Error: fmt.Errorf("claude: %w", context.Canceled)}},
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

func TestReviewRound_NoFallbackWhenAttemptContextExpired(t *testing.T) {
	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-expired.Done()

	// the attempt expired while the CLI printed the message: the policy handles it as a timeout
	mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2"},
		executor.Result{},
		executor.Result{DiagnosticText: "No conversation found", Error: errors.New("exit status 1")})
	round := newReviewRound(t.Context(), mock, time.Minute)

	round.Run(t.Context(), "full")
	got := round.Run(expired, "full")

	assert.Len(t, *calls, 2, "no fallback session may start")
	require.Error(t, got.Error)
	assert.False(t, got.Fallback)
	assert.Len(t, mock.NewSessionIDCalls(), 1)
}

func TestReviewRound_FallbackRunsUnderFreshTimeout(t *testing.T) {
	t.Run("timeout configured", func(t *testing.T) {
		mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2"},
			executor.Result{},
			executor.Result{DiagnosticText: "No conversation found", Error: errors.New("exit status 1")},
			executor.Result{Output: "fresh"})
		round := newReviewRound(t.Context(), mock, time.Hour)

		round.Run(t.Context(), "full")
		attempt, cancel := context.WithTimeout(t.Context(), 5*time.Minute) // the policy deadline of this attempt
		defer cancel()
		attemptDeadline, _ := attempt.Deadline()
		got := round.Run(attempt, "full")

		require.Len(t, *calls, 3)
		require.NoError(t, (*calls)[2].ctxErr)
		require.True(t, (*calls)[2].hasDeadline, "fallback gets its own session timeout")
		assert.True(t, (*calls)[2].deadline.After(attemptDeadline), "fallback deadline is not the attempt deadline")
		assert.Equal(t, "fresh", got.Output)
		assert.True(t, got.Fallback)
		assert.False(t, got.FallbackTimedOut)
	})

	t.Run("no timeout configured", func(t *testing.T) {
		mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2"},
			executor.Result{},
			executor.Result{DiagnosticText: "No conversation found", Error: errors.New("exit status 1")},
			executor.Result{})
		round := newReviewRound(t.Context(), mock, 0)

		round.Run(t.Context(), "full")
		got := round.Run(t.Context(), "full")

		require.Len(t, *calls, 3)
		require.NoError(t, (*calls)[2].ctxErr)
		assert.False(t, (*calls)[2].hasDeadline)
		assert.True(t, got.Fallback)
		assert.False(t, got.FallbackTimedOut)
	})
}

func TestReviewRound_FallbackTimeoutVerdict(t *testing.T) {
	t.Run("fallback hits its own timeout", func(t *testing.T) {
		mock, _ := scriptedSessions(t, true, []string{"id-1", "id-2"},
			executor.Result{},
			executor.Result{DiagnosticText: "No conversation found", Error: errors.New("exit status 1")},
			executor.Result{Error: errors.New("signal: killed")})
		mock.RunSessionFunc = wrapRunSession(mock.RunSessionFunc, 2, func(ctx context.Context) { <-ctx.Done() })
		round := newReviewRound(t.Context(), mock, 20*time.Millisecond)

		round.Run(t.Context(), "full")
		got := round.Run(t.Context(), "full")

		assert.True(t, got.Fallback)
		assert.True(t, got.FallbackTimedOut)
	})

	t.Run("phase context canceled during fallback is not a timeout", func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		mock, _ := scriptedSessions(t, true, []string{"id-1", "id-2"},
			executor.Result{},
			executor.Result{DiagnosticText: "No conversation found", Error: errors.New("exit status 1")},
			executor.Result{Error: errors.New("signal: killed")})
		mock.RunSessionFunc = wrapRunSession(mock.RunSessionFunc, 2, func(ctx context.Context) {
			cancel()
			<-ctx.Done()
		})
		round := newReviewRound(parent, mock, time.Hour)

		round.Run(parent, "full")
		got := round.Run(parent, "full")

		assert.True(t, got.Fallback)
		assert.False(t, got.FallbackTimedOut)
	})
}

// wrapRunSession runs before on the call with the given zero-based index, then delegates to inner.
func wrapRunSession(inner func(context.Context, string, executor.SessionSpec) executor.Result, index int,
	before func(context.Context)) func(context.Context, string, executor.SessionSpec) executor.Result {
	n := 0
	return func(ctx context.Context, prompt string, spec executor.SessionSpec) executor.Result {
		if n == index {
			before(ctx)
		}
		n++
		return inner(ctx, prompt, spec)
	}
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

func TestReviewRound_HeadCheckAfterRateLimit(t *testing.T) {
	limited := executor.Result{Error: &executor.LimitPatternError{Pattern: "limit", HelpCmd: "usage"}}
	tests := []struct {
		name       string
		first      executor.Result
		heads      []string // values returned by head(): after the limited run, then before the resume
		wantResume bool
	}{
		{name: "same head resumes", first: limited, heads: []string{"a", "a"}, wantResume: true},
		{name: "moved head starts a new session", first: limited, heads: []string{"a", "b"}, wantResume: false},
		{name: "unreadable head after the run starts a new session", first: limited, heads: []string{"", ""}, wantResume: false},
		{name: "unreadable head before the resume starts a new session", first: limited, heads: []string{"a", ""}, wantResume: false},
		{name: "run without a limit error does not read head", first: executor.Result{}, heads: nil, wantResume: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock, calls := scriptedSessions(t, true, []string{"id-1", "id-2"}, tt.first, executor.Result{})
			round := newReviewRound(t.Context(), mock, 0)
			idx := 0
			round.head = func() string {
				require.Less(t, idx, len(tt.heads), "unexpected extra head read")
				idx++
				return tt.heads[idx-1]
			}

			round.Run(t.Context(), "full")
			round.Run(t.Context(), "full")

			require.Len(t, *calls, 2)
			if tt.wantResume {
				assert.Equal(t, executor.SessionSpec{ID: "id-1", Resume: true}, (*calls)[1].spec)
				assert.Equal(t, resumeRoundPrompt, (*calls)[1].prompt)
			} else {
				assert.Equal(t, executor.SessionSpec{ID: "id-2"}, (*calls)[1].spec)
				assert.Equal(t, "full", (*calls)[1].prompt)
			}
			assert.Equal(t, len(tt.heads), idx)
		})
	}
}
