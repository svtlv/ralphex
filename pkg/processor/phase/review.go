package phase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/svtlv/ralphex/pkg/status"
)

// ReviewPhase runs internal review passes.
type ReviewPhase struct {
	cfg            Config
	log            ReviewLogger
	exec           Executor
	policy         Policy
	prompts        ReviewPrompts
	git            *GitState
	phaseHolder    *status.PhaseHolder
	iterationDelay time.Duration
}

// ReviewPhaseOpts contains dependencies for ReviewPhase.
type ReviewPhaseOpts struct {
	Cfg            Config
	Log            ReviewLogger
	Exec           Executor
	Policy         Policy
	Prompts        ReviewPrompts
	Git            *GitState
	PhaseHolder    *status.PhaseHolder
	IterationDelay time.Duration
}

// NewReviewPhase creates an internal review phase engine.
func NewReviewPhase(opts ReviewPhaseOpts) *ReviewPhase {
	return &ReviewPhase{
		cfg: opts.Cfg, log: opts.Log, exec: opts.Exec, policy: opts.Policy,
		prompts: opts.Prompts, git: opts.Git, phaseHolder: opts.PhaseHolder,
		iterationDelay: opts.IterationDelay,
	}
}

// First runs the comprehensive first review pass and applies first-review timeout semantics.
func (p *ReviewPhase) First(ctx context.Context) error {
	if p.phaseHolder != nil {
		p.phaseHolder.Set(status.PhaseReview)
	}
	p.log.PrintSection(p.section(0, ": all findings"))
	return p.run(ctx, p.prompts.FirstReviewPrompt(), "first review pass")
}

// Loop runs critical/major review iterations until review completion or no changed HEAD.
func (p *ReviewPhase) Loop(ctx context.Context, prefix string) error {
	if p.phaseHolder != nil {
		p.phaseHolder.Set(status.PhaseReview)
	}
	maxReviewIterations := max(minReviewIterations, p.cfg.MaxIterations/reviewIterationDivisor)

	execName := p.cfg.executorName()

	// one round per review iteration. a timed-out iteration that did not move HEAD keeps its round,
	// so the retry iteration resumes the same session instead of repeating the research.
	var round *reviewRound
	var roundHead string
	for i := 1; i <= maxReviewIterations; i++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("review: %w", ctx.Err())
		default:
		}

		p.log.PrintSection(p.section(i, ": critical/major"))
		// head can move while the retry backoff sleeps (a commit from another terminal), and the old
		// session would then judge the old commit. a moved or unreadable head starts a new round.
		if round != nil && (roundHead == "" || p.headHash() != roundHead) {
			round = nil
		}
		if round == nil {
			round = p.newRound(ctx)
			roundHead = p.headHash()
			round.startHead = roundHead
		}

		execResult := p.policy.Run(ctx, round.Run, p.prompts.SecondReviewPrompt(prefix), execName)
		result := execResult.Result
		// a rate-limit wait retry can replace the session inside the policy call after HEAD moved, so
		// compare against the HEAD the final session started at, not the one the round began with
		roundHead = round.startHead
		if err := wrapExecutorError(p.policy, result.Error, execName); err != nil {
			return err
		}

		if result.Signal == SignalFailed {
			return errors.New("review failed (FAILED signal received)")
		}

		if IsReviewDone(result.Signal) {
			p.log.Print("%s review complete - no more findings", execName)
			return nil
		}

		if execResult.TimedOut {
			// the timed-out round may already have ended with a commit. HEAD moved or unknown means
			// the round cannot be proven unfinished, so drop it and let the retry start a new session.
			if headAfter := p.headHash(); roundHead == "" || headAfter == "" || headAfter != roundHead {
				round = nil
			} else {
				round.noteTimedOut()
			}
			p.log.Print("session timed out, retrying review iteration after %s...", retryBackoff)
			if err := p.policy.Sleep(ctx, retryBackoff); err != nil {
				return fmt.Errorf("interrupted: %w", err)
			}
			continue
		}

		round = nil // the iteration ended without a timeout, the next one starts a new round
		if roundHead != "" {
			if headAfter := p.headHash(); headAfter == roundHead {
				p.log.Print("%s review complete - no changes detected", execName)
				return nil
			}
		}

		p.log.Print("issues fixed, running another review iteration...")
		if err := p.policy.Sleep(ctx, p.iterationDelay); err != nil {
			return fmt.Errorf("interrupted: %w", err)
		}
	}

	p.log.Print("max %s review iterations reached, continuing...", execName)
	return nil
}

// newRound creates the session helper of one review round, wired to the phase's HEAD reader.
func (p *ReviewPhase) newRound(ctx context.Context) *reviewRound {
	round := newReviewRound(ctx, p.exec, p.sessionTimeout())
	if p.git.hasGit() {
		round.head = p.headHash
	}
	return round
}

func (p *ReviewPhase) headHash() string {
	return p.git.headHash()
}

// sessionTimeout returns the configured per-session timeout, 0 when none applies.
// the review phase always runs claude or codex, so the policy's executor gating always holds.
func (p *ReviewPhase) sessionTimeout() time.Duration {
	if p.cfg.AppConfig == nil {
		return 0
	}
	return p.cfg.AppConfig.SessionTimeout
}

func (p *ReviewPhase) section(iteration int, suffix string) status.Section {
	if p.cfg.isCodexExecutor() {
		return status.NewInternalReviewSection(iteration, suffix)
	}
	return status.NewClaudeReviewSection(iteration, suffix)
}

func (p *ReviewPhase) run(ctx context.Context, prompt, phaseLabel string) error {
	execName := p.cfg.executorName()
	round := p.newRound(ctx)
	execResult := p.policy.Run(ctx, round.Run, prompt, execName)
	result := execResult.Result
	if err := wrapExecutorError(p.policy, result.Error, execName); err != nil {
		return err
	}

	if result.Signal == SignalFailed {
		return errors.New("review failed (FAILED signal received)")
	}

	if execResult.TimedOut {
		if p.cfg.isCodexExecutor() {
			return fmt.Errorf("%s timed out", phaseLabel)
		}
		p.log.Print("warning: %s did not complete cleanly (session timed out), continuing...", phaseLabel)
		return nil
	}

	if !IsReviewDone(result.Signal) {
		p.log.Print("warning: %s did not complete cleanly, continuing...", phaseLabel)
	}

	return nil
}
