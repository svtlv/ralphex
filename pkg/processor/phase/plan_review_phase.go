package phase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/svtlv/ralphex/pkg/config"
	"github.com/svtlv/ralphex/pkg/executor"
	"github.com/svtlv/ralphex/pkg/status"
)

const (
	// maxPlanReviewRounds caps the review loop so a preselected reviewer cannot run forever.
	maxPlanReviewRounds = 5
	// maxFindingSelectionAttempts is the number of times the findings question is asked
	// (the first ask plus two re-asks) before an invalid answer is treated as "none".
	maxFindingSelectionAttempts = 3
)

// stable prompt texts of the plan review questions (documented in README).
const (
	planReviewerQuestion   = "Review the plan before implementation?"
	reviewerOptionExternal = "External reviewer"
	reviewerOptionSame     = "Same agent"
	reviewerOptionSkip     = "Skip review"

	findingsQuestion   = "Which findings should be applied?"
	findingsOptionAll  = "All findings"
	findingsOptionNone = "None"
)

// PlanReviewRequest identifies the plan file to review and the task description used for revisions.
type PlanReviewRequest struct {
	PlanFile    string
	Description string
}

// PlanReviewPhase runs the plan review loop: reviewer choice, read-only review with a repo guard,
// findings selection and planner revision. it is shared by plan mode and the standalone plan-review mode.
type PlanReviewPhase struct {
	cfg         Config
	log         PlanReviewLogger
	planner     Executor
	tools       externalTools
	policy      Policy
	prompts     PlanReviewPrompts
	deps        *Deps
	phaseHolder *status.PhaseHolder
}

// PlanReviewPhaseOpts contains dependencies for PlanReviewPhase.
type PlanReviewPhaseOpts struct {
	Cfg         Config
	Log         PlanReviewLogger
	Planner     Executor
	External    Executor
	Custom      *executor.CustomExecutor
	Policy      Policy
	Prompts     PlanReviewPrompts
	Deps        *Deps
	PhaseHolder *status.PhaseHolder
}

// NewPlanReviewPhase creates a plan review phase engine.
func NewPlanReviewPhase(opts PlanReviewPhaseOpts) *PlanReviewPhase {
	return &PlanReviewPhase{
		cfg: opts.Cfg, log: opts.Log, planner: opts.Planner,
		tools:  externalTools{external: opts.External, custom: opts.Custom},
		policy: opts.Policy, prompts: opts.Prompts, deps: opts.Deps, phaseHolder: opts.PhaseHolder,
	}
}

// roundOutcome tells the loop what to do after one review round.
type roundOutcome int

const (
	roundStop    roundOutcome = iota // end the review
	roundApplied                     // findings were applied by a revision session, review again
)

// Run reviews the plan until the user skips, the review finds no issues, no finding is applied,
// a warning path ends the review, or the round cap is reached.
// a failed or timed-out review never fails the run: it logs a warning and returns nil.
// only context cancellation, pattern errors (limit, quota) and input errors are returned.
func (p *PlanReviewPhase) Run(ctx context.Context, req PlanReviewRequest) error {
	if req.PlanFile == "" {
		return errors.New("plan file required for plan review")
	}
	if p.deps == nil || p.deps.InputCollector == nil {
		return errors.New("input collector required for plan review")
	}

	p.setPhase()

	mode := p.reviewerMode()
	if mode == config.PlanReviewerNone {
		p.log.Print("plan review disabled, skipping...")
		return nil
	}

	preselected := ""
	if mode != config.PlanReviewerAsk {
		choice, ok := p.preselectedReviewer(mode)
		if !ok {
			return nil
		}
		preselected = choice
	}

	for round := 1; round <= maxPlanReviewRounds; round++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("plan review: %w", err)
		}

		reviewer := preselected
		if reviewer == "" {
			choice, err := p.askReviewer(ctx)
			if err != nil {
				return err
			}
			reviewer = choice
		}
		if reviewer == reviewerOptionSkip {
			p.log.Print("plan review skipped")
			return nil
		}

		outcome, err := p.runRound(ctx, req, round, reviewer)
		if err != nil {
			return err
		}
		if outcome == roundStop {
			return nil
		}
	}

	p.log.Print("max plan review rounds (%d) reached, continuing...", maxPlanReviewRounds)
	return nil
}

func (p *PlanReviewPhase) setPhase() {
	if p.phaseHolder != nil {
		p.phaseHolder.Set(status.PhasePlan)
	}
}

// reviewerMode returns the normalized plan_reviewer setting; empty or unknown values mean ask.
func (p *PlanReviewPhase) reviewerMode() string {
	if p.cfg.AppConfig == nil {
		return config.PlanReviewerAsk
	}
	mode, err := config.NormalizePlanReviewer(p.cfg.AppConfig.PlanReviewer)
	if err != nil {
		return config.PlanReviewerAsk
	}
	return mode
}

// externalTool returns the external review tool (codex or custom) when one is configured and its
// executor exists, or "" when External reviewer is not available.
func (p *PlanReviewPhase) externalTool() string {
	if p.cfg.isCodexExecutor() {
		return ""
	}
	switch ResolveExternalReviewTool(p.cfg) {
	case externalToolCustom:
		if p.tools.custom != nil {
			return externalToolCustom
		}
	case externalToolDisabled:
	default:
		// CodexEnabled is cleared at startup when the codex binary is missing, so an explicit
		// external_review_tool = codex without a codex install does not offer a reviewer that cannot run
		if p.cfg.CodexEnabled && p.tools.external != nil {
			return externalToolCodex
		}
	}
	return ""
}

// preselectedReviewer maps a non-ask plan_reviewer value to a reviewer option. it logs a warning and
// returns false when the chosen reviewer is unavailable.
func (p *PlanReviewPhase) preselectedReviewer(mode string) (string, bool) {
	if mode == config.PlanReviewerSame {
		return reviewerOptionSame, true
	}
	if p.externalTool() == "" {
		if p.cfg.isCodexExecutor() {
			p.log.Print("warning: plan_reviewer = external is not available with the codex executor, skipping plan review")
		} else {
			p.log.Print("warning: plan_reviewer = external but no external review tool is configured, skipping plan review")
		}
		return "", false
	}
	return reviewerOptionExternal, true
}

func (p *PlanReviewPhase) reviewerOptions() []string {
	options := make([]string, 0, 3)
	if p.externalTool() != "" {
		options = append(options, reviewerOptionExternal)
	}
	return append(options, reviewerOptionSame, reviewerOptionSkip)
}

func (p *PlanReviewPhase) askReviewer(ctx context.Context) (string, error) {
	options := p.reviewerOptions()
	p.log.LogQuestion(planReviewerQuestion, options)
	answer, err := p.deps.InputCollector.AskQuestion(ctx, planReviewerQuestion, options)
	if err != nil {
		return "", fmt.Errorf("collect reviewer choice: %w", err)
	}
	p.log.LogAnswer(answer)

	for _, option := range options {
		if strings.EqualFold(strings.TrimSpace(answer), option) {
			return option, nil
		}
	}
	// a typed "Other" answer that matches no option is treated as skip, so a typo never starts a review
	p.log.Print("warning: unknown reviewer choice %q, skipping plan review", answer)
	return reviewerOptionSkip, nil
}

// runRound runs one review: guarded reviewer call, findings parsing, selection and revision.
func (p *PlanReviewPhase) runRound(ctx context.Context, req PlanReviewRequest, round int, reviewer string) (roundOutcome, error) {
	p.setPhase()
	p.log.PrintSection(status.NewGenericSection(fmt.Sprintf("plan review round %d", round)))

	output, ok, err := p.guardedReview(ctx, req, reviewer)
	if err != nil || !ok {
		return roundStop, err
	}

	findings, noIssues := parsePlanFindings(output)
	if noIssues {
		p.log.Print("plan review found no issues")
		return roundStop, nil
	}
	if len(findings) == 0 {
		p.log.Print("warning: plan review output has no findings in the expected format, skipping")
		p.logRawOutput(output)
		return roundStop, nil
	}

	p.printFindings(findings)
	selected, err := p.askFindingSelection(ctx, len(findings))
	if err != nil {
		return roundStop, err
	}
	if len(selected) == 0 {
		p.log.Print("no findings selected, plan left unchanged")
		return roundStop, nil
	}

	revised, err := p.revise(ctx, req, formatFindingsFeedback(findings, selected))
	if err != nil || !revised {
		return roundStop, err
	}
	return roundApplied, nil
}

// guardedReview runs the reviewer between two repo snapshots and returns its stdout.
// ok is false when the review must end without findings (warning already logged).
func (p *PlanReviewPhase) guardedReview(ctx context.Context, req PlanReviewRequest, reviewer string) (output string, ok bool, err error) {
	before, snapErr := snapshotRepo(p.deps.RepoState, req.PlanFile)
	if snapErr != nil {
		if before.plan == nil {
			p.log.Print("warning: cannot read plan file for plan review: %v", snapErr)
			return "", false, nil
		}
		p.log.Print("warning: repo state unavailable, read-only guard checks the plan file only: %v", snapErr)
	}

	execResult, name := p.runReviewer(ctx, reviewer, p.prompts.PlanReviewPrompt(req.PlanFile, req.Description))

	// the guard check runs first: a reviewer that edited files and then failed still has to be reported
	// and the plan file restored. a pattern error (limit, quota) is still returned after that.
	after, afterErr := snapshotRepo(p.deps.RepoState, req.PlanFile)
	if afterErr != nil && before.hasRepo && !after.hasRepo {
		p.log.Print("warning: repo state unreadable after plan review, read-only guard checks the plan file only: %v", afterErr)
	}
	tripped := p.guardTripped(before, after)

	failed, err := p.checkExecution(ctx, execResult, name)
	if err != nil {
		return "", false, err
	}
	if tripped || failed {
		return "", false, nil
	}
	if strings.TrimSpace(execResult.Result.Output) == "" {
		p.log.Print("warning: plan review returned no output, skipping")
		return "", false, nil
	}
	return execResult.Result.Output, true, nil
}

// runReviewer runs the chosen reviewer and returns its result with the executor name used for error handling.
func (p *PlanReviewPhase) runReviewer(ctx context.Context, reviewer, prompt string) (ExecutionResult, string) {
	if reviewer == reviewerOptionExternal {
		tool := p.externalTool()
		return runExternalTool(ctx, p.policy, p.tools, tool, prompt), tool
	}
	name := p.cfg.executorName()
	return p.policy.Run(ctx, p.planner.Run, prompt, name), name
}

// guardTripped compares the snapshots. on any change it restores the plan file (the only file ralphex owns),
// logs a warning that names the changed paths and returns true so the caller discards the round's findings.
func (p *PlanReviewPhase) guardTripped(before, after repoSnapshot) bool {
	changed, headMoved := diffSnapshots(before, after)
	planEdited := planChanged(before, after)
	if len(changed) == 0 && !headMoved && !planEdited {
		return false
	}

	if planEdited {
		if err := restorePlan(before); err != nil {
			p.log.Print("warning: plan review changed the plan file and it could not be restored: %v", err)
		} else {
			p.log.Print("warning: plan review changed the plan file, restored it from the saved copy")
		}
		changed = withoutPlan(changed, before)
	}
	if len(changed) > 0 || headMoved {
		msg := "warning: plan review is read-only but the repository changed during the review"
		if len(changed) > 0 {
			msg += ": " + strings.Join(changed, ", ")
		}
		if headMoved {
			msg += fmt.Sprintf(" (HEAD moved from %s to %s)", shortHash(before.head), shortHash(after.head))
		}
		p.log.Print("%s", msg)
		p.log.Print("note: ralphex does not revert these changes; check them with git status")
	}
	p.log.Print("warning: findings of this review round are discarded, plan review ends")
	return true
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// checkExecution classifies an executor result. it returns an error only for cancellation and pattern
// matches (rate limit, quota). any other error or a timeout is logged as a warning and reported as
// failed=true, so the caller ends the review without failing the run.
func (p *PlanReviewPhase) checkExecution(ctx context.Context, res ExecutionResult, name string) (failed bool, err error) {
	runErr := res.Result.Error
	if runErr == nil {
		if res.TimedOut {
			p.log.Print("warning: %s session timed out, skipping", name)
		}
		return res.TimedOut, nil
	}
	if ctx.Err() != nil {
		return true, fmt.Errorf("plan review: %w", ctx.Err())
	}
	if patternErr := p.policy.HandlePatternMatchError(runErr, name); patternErr != nil {
		return true, fmt.Errorf("%s pattern handling: %w", name, patternErr)
	}
	p.log.Print("warning: %s plan review failed: %v", name, runErr)
	return true, nil
}

func (p *PlanReviewPhase) printFindings(findings []string) {
	p.log.Print("plan review findings:")
	for i, f := range findings {
		p.log.PrintAligned(fmt.Sprintf("  %d. %s", i+1, f))
	}
}

func (p *PlanReviewPhase) logRawOutput(output string) {
	text := strings.TrimSpace(output)
	if runes := []rune(text); len(runes) > maxCodexSummaryLen {
		text = string(runes[:maxCodexSummaryLen]) + "..."
	}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) != "" {
			p.log.PrintAligned("  " + line)
		}
	}
}

// askFindingSelection asks which findings to apply. an invalid answer is re-asked; after
// maxFindingSelectionAttempts invalid answers it is treated as "none".
func (p *PlanReviewPhase) askFindingSelection(ctx context.Context, n int) ([]int, error) {
	options := []string{findingsOptionAll, findingsOptionNone}
	for attempt := 1; attempt <= maxFindingSelectionAttempts; attempt++ {
		p.log.LogQuestion(findingsQuestion, options)
		answer, err := p.deps.InputCollector.AskQuestion(ctx, findingsQuestion, options)
		if err != nil {
			return nil, fmt.Errorf("collect findings selection: %w", err)
		}
		p.log.LogAnswer(answer)

		selected, parseErr := parseFindingSelection(answer, n)
		if parseErr == nil {
			return selected, nil
		}
		p.log.Print("warning: %v", parseErr)
	}
	p.log.Print("warning: no valid findings selection after %d attempts, applying none", maxFindingSelectionAttempts)
	return nil, nil
}

// revise runs the planner revision session for the chosen findings. it returns true when the planner
// emitted PLAN_READY; any other end is logged as a warning.
func (p *PlanReviewPhase) revise(ctx context.Context, req PlanReviewRequest, feedback string) (bool, error) {
	p.log.Print("applying selected findings to the plan...")
	name := p.cfg.executorName()
	prompt := p.prompts.PlanRevisionPrompt(req.PlanFile, req.Description, feedback)
	res := p.policy.Run(ctx, p.planner.Run, prompt, name)

	failed, err := p.checkExecution(ctx, res, name)
	if err != nil {
		return false, err
	}
	if failed {
		return false, nil
	}
	if !IsPlanReady(res.Result.Signal) {
		p.log.Print("warning: plan revision ended without PLAN_READY, plan review ends")
		return false, nil
	}
	p.log.Print("plan revised")
	return true, nil
}
