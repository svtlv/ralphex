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

// missingSessionMarkers are lowercase substrings that mean the resumed session does not exist:
// claude prints "No conversation found with session ID ...", codex prints
// "no rollout found for thread id ..." on stderr.
var missingSessionMarkers = []string{"no conversation found", "no rollout found for thread id"}

// reviewRound owns the session of one review round. the first Run starts a session with the
// full prompt, later Runs resume it with resumeRoundPrompt, so a retry does not repeat the
// research. the id lives in memory only, nothing is written to the progress log.
type reviewRound struct {
	parent         context.Context
	exec           Executor
	sess           SessionExecutor // nil when the executor cannot resume
	sessionTimeout time.Duration

	id       string // session id, empty when no session is known
	timedOut bool   // the current session already timed out once
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
	if r.id == "" {
		return r.runNew(ctx, prompt)
	}

	result := r.sess.RunSession(ctx, resumeRoundPrompt, executor.SessionSpec{ID: r.id, Resume: true})
	if r.shouldFallBack(result) {
		fbCtx, cancel := r.fallbackContext()
		defer cancel()
		return r.runNew(fbCtx, prompt)
	}
	if result.SessionID != "" {
		r.id = result.SessionID
	}
	return result
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
// only a plain error counts: pattern errors and a canceled phase context never fall back, and
// empty output is not evidence of a missing session.
func (r *reviewRound) shouldFallBack(result executor.Result) bool {
	if result.Error == nil || r.parent.Err() != nil {
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
	// only text the CLI produced itself counts. Output and RecentText hold the model's own words,
	// which can quote a marker when ralphex reviews its own source.
	text := strings.ToLower(result.Error.Error() + "\n" + result.DiagnosticText)
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
