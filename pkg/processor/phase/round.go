package phase

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/svtlv/ralphex/pkg/executor"
)

// resumeRoundPrompt is sent instead of the full review prompt when a retried round resumes its
// session. it must not demand a signal on its own: the default review prompts end a round that
// fixed issues with a plain-text summary and no signal, so completion rules stay with the
// original instructions that the resumed session already holds.
const resumeRoundPrompt = `Your previous session for this review round was interrupted before it finished.
Continue where you stopped. Do not repeat research, file reads or agent runs that already finished.
Finish the review by following the completion rules of the original instructions exactly:
emit a signal only where those instructions require one, and otherwise end as they describe.`

// missingSessionMarkers are lowercase substrings of Result.DiagnosticText that mean the resumed
// session does not exist: claude prints "No conversation found with session ID ...", codex prints
// "no rollout found for thread id ..." on stderr (the codex executor reports only that CLI error line).
var missingSessionMarkers = []string{"no conversation found", "no rollout found for thread id"}

// reviewRound owns the session of one review round. the first Run starts a session with the
// full prompt, later Runs resume it with resumeRoundPrompt, so a retry does not repeat the
// research. the id lives in memory only, nothing is written to the progress log.
type reviewRound struct {
	parent         context.Context
	exec           Executor
	sess           SessionExecutor // nil when the executor cannot resume
	sessionTimeout time.Duration

	// head reads the current HEAD ("" when unknown); nil disables the commit check below
	head func() string

	id       string // session id, empty when no session is known
	timedOut bool   // the current session already timed out once

	// HEAD when the current session started ("" when unknown). the phase sets it when it creates the
	// round, and a replacement session started after a moved HEAD updates it, so the phase judges
	// "did this round commit" against the session it actually ran, not the one it replaced.
	startHead string

	// HEAD recorded when a Run ended on a rate limit. the policy then waits and calls Run again inside
	// one iteration, where the phase cannot see a commit made during the wait.
	limitHead    string
	limitHeadSet bool
}

// newReviewRound creates the helper for one review round. parent is the phase context and
// sessionTimeout is the configured per-session timeout (0 = none). an executor that is not a
// SessionExecutor, or whose CanResume is false, gets the current behavior: every Run is a plain
// exec.Run with the full prompt.
func newReviewRound(parent context.Context, exec Executor, sessionTimeout time.Duration) *reviewRound {
	r := &reviewRound{parent: parent, exec: exec, sessionTimeout: sessionTimeout}
	if se, ok := exec.(SessionExecutor); ok && se.CanResume() {
		r.sess = se
	}
	return r
}

// Run has the signature of Executor.Run, so it can be handed to Policy.Run in place of exec.Run.
func (r *reviewRound) Run(ctx context.Context, prompt string) executor.Result {
	if r.sess == nil {
		return r.exec.Run(ctx, prompt)
	}
	result := r.run(ctx, prompt)
	r.noteLimit(result)
	return result
}

func (r *reviewRound) run(ctx context.Context, prompt string) executor.Result {
	if r.id == "" {
		// no session to resume (the first attempt kept no id, e.g. codex printed none). after a rate
		// limit the new session starts at the current HEAD whether or not it moved since the limit was
		// recorded: the first attempt may have committed before it hit the limit, so the HEAD the phase
		// stored when the round began can already be stale. unknown stays unknown.
		if r.limitHeadSet && r.head != nil {
			r.limitHeadSet = false
			r.startHead = r.head()
		}
		return r.runNew(ctx, prompt)
	}
	if r.headMoved() {
		// a commit landed while the policy waited out a rate limit. the old session judged the old
		// commit, so a new session with the full prompt reviews the current diff.
		r.id = ""
		return r.runNew(ctx, prompt)
	}

	result := r.sess.RunSession(ctx, resumeRoundPrompt, executor.SessionSpec{ID: r.id, Resume: true})
	if r.shouldFallBack(ctx, result) {
		fbCtx, cancel := r.fallbackContext()
		defer cancel()
		if r.head != nil {
			// HEAD may have moved while the resume failed. the fallback session starts at the
			// current HEAD, so the phase must judge its commits against that one.
			r.startHead = r.head()
		}
		fb := r.runNew(fbCtx, prompt)
		// tell the retry policy that this result ran under its own timeout, not the one it set up
		fb.Fallback = true
		fb.FallbackTimedOut = fbCtx.Err() != nil && r.parent.Err() == nil
		return fb
	}
	if result.SessionID != "" {
		r.id = result.SessionID
	}
	return result
}

// noteLimit records HEAD when a Run ended on a rate limit, after the failed attempt and before the
// policy waits, so commits the session made itself are not counted as a change.
func (r *reviewRound) noteLimit(result executor.Result) {
	r.limitHeadSet = false
	if r.head == nil {
		return
	}
	if _, ok := errors.AsType[*executor.LimitPatternError](result.Error); ok {
		r.limitHead = r.head()
		r.limitHeadSet = true
	}
}

// headMoved reports whether HEAD differs from the value recorded at the last rate limit. an unreadable
// HEAD on either side counts as moved, because an unchanged HEAD cannot be proven.
func (r *reviewRound) headMoved() bool {
	if !r.limitHeadSet || r.head == nil {
		return false
	}
	r.limitHeadSet = false
	current := r.head()
	moved := r.limitHead == "" || current == "" || current != r.limitHead
	if moved {
		// the replacement session starts at the current HEAD (unknown stays unknown)
		r.startHead = current
	}
	return moved
}

// noteTimedOut records that the last Run ended in a timeout. the first timeout keeps the
// session so the next Run resumes it. a second timeout in the same session drops it, so the
// next Run starts a new session with the full prompt.
func (r *reviewRound) noteTimedOut() {
	if r.sess == nil || r.id == "" {
		return
	}
	if r.timedOut {
		r.id = ""
		r.timedOut = false
		return
	}
	r.timedOut = true
}

// runNew starts a new session with the full prompt and remembers its id.
func (r *reviewRound) runNew(ctx context.Context, prompt string) executor.Result {
	id := r.sess.NewSessionID()
	result := r.sess.RunSession(ctx, prompt, executor.SessionSpec{ID: id})
	r.timedOut = false
	r.id = id
	if result.SessionID != "" {
		r.id = result.SessionID
	}
	return result
}

// shouldFallBack reports whether a resume attempt failed because the session does not exist.
// only a plain error counts: pattern errors, a canceled phase context and an attempt that was
// canceled or timed out (ctx is the attempt's own context) never fall back, and empty output is not
// evidence of a missing session. an attempt that expired while the CLI printed the message may have
// failed for that reason, so the policy handles it as a timeout and the next call resumes again.
func (r *reviewRound) shouldFallBack(ctx context.Context, result executor.Result) bool {
	if result.Error == nil || r.parent.Err() != nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(result.Error, context.Canceled) || errors.Is(result.Error, context.DeadlineExceeded) {
		return false
	}
	if _, ok := errors.AsType[*executor.PatternMatchError](result.Error); ok {
		return false
	}
	if _, ok := errors.AsType[*executor.LimitPatternError](result.Error); ok {
		return false
	}
	if _, ok := errors.AsType[*executor.RetryPatternError](result.Error); ok {
		return false
	}
	// only DiagnosticText counts: the executors fill it with CLI-produced error text alone. Output and
	// RecentText hold the model's own words, and the codex Error carries a raw stderr tail that echoes
	// model text, so all of them can quote a marker when ralphex reviews its own source.
	text := strings.ToLower(result.DiagnosticText)
	for _, marker := range missingSessionMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// fallbackContext returns the context for the fallback session. it derives from the phase
// context, so a user abort still cancels it, and carries a new full session timeout instead of
// what is left of the failed resume.
func (r *reviewRound) fallbackContext() (context.Context, context.CancelFunc) {
	if r.sessionTimeout <= 0 {
		return context.WithCancel(r.parent)
	}
	return context.WithTimeout(r.parent, r.sessionTimeout)
}
