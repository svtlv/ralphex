package phase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/svtlv/ralphex/pkg/config"
	"github.com/svtlv/ralphex/pkg/executor"
	"github.com/svtlv/ralphex/pkg/status"
)

const (
	minReviewIterations    = 3
	reviewIterationDivisor = 10
	minCodexIterations     = 3
	codexIterationDivisor  = 5
	maxCodexSummaryLen     = 5000
	minPlanIterations      = 5
	planIterationDivisor   = 5
)

//go:generate moq -out mocks/plan_review_logger.go -pkg mocks -skip-ensure -fmt goimports . PlanReviewLogger
//go:generate moq -out mocks/plan_review_prompts.go -pkg mocks -skip-ensure -fmt goimports . PlanReviewPrompts
//go:generate moq -out mocks/plan_finder.go -pkg mocks -skip-ensure -fmt goimports . PlanFinder
//go:generate moq -out mocks/session_executor.go -pkg mocks -skip-ensure -fmt goimports . SessionExecutor

// retryBackoff is the pause before re-running an iteration that timed out or hit
// a transient retry pattern, applied in the task and review retry loops.
const retryBackoff = 5 * time.Second

// Config contains the runner settings consumed by phase engines.
type Config struct {
	PlanDescription       string
	MaxIterations         int
	MaxExternalIterations int
	ReviewPatience        int
	CodexEnabled          bool
	ExternalReviewToolSet bool
	FinalizeEnabled       bool
	AppConfig             *config.Config
}

func (c Config) isCodexExecutor() bool {
	return c.AppConfig != nil && c.AppConfig.Executor == config.ExecutorCodex
}

func (c Config) executorName() string {
	if c.isCodexExecutor() {
		return "codex"
	}
	return "claude"
}

// wrapExecutorError wraps a phase executor error, routing through pattern-match handling first.
// shared by task, review, and plan-creation phases (distinct from external_review's break-aware variant).
func wrapExecutorError(policy Policy, err error, execName string) error {
	if err == nil {
		return nil
	}
	if patternErr := policy.HandlePatternMatchError(err, execName); patternErr != nil {
		return fmt.Errorf("%s pattern handling: %w", execName, patternErr)
	}
	return fmt.Errorf("%s execution: %w", execName, err)
}

// Executor runs a phase prompt and returns the executor result.
type Executor interface {
	Run(ctx context.Context, prompt string) executor.Result
}

// SessionExecutor is an optional Executor capability: running a prompt inside a named session
// so a retried review round can resume it. executors that do not implement it (mocks, wrapper
// scripts) behave as plain Executors.
type SessionExecutor interface {
	Executor
	// CanResume reports whether the configured command supports session resume.
	CanResume() bool
	// NewSessionID returns an id for a new session, or "" when the executor assigns its own id.
	NewSessionID() string
	// RunSession runs the prompt inside the session described by spec.
	RunSession(ctx context.Context, prompt string, spec executor.SessionSpec) executor.Result
}

// Logger records plain phase progress messages.
type Logger interface {
	Print(format string, args ...any)
}

// TaskLogger records task phase progress.
type TaskLogger interface {
	Logger
	PrintRaw(format string, args ...any)
	PrintSection(section status.Section)
}

// ReviewLogger records internal review phase progress.
type ReviewLogger interface {
	Logger
	PrintSection(section status.Section)
}

// ExternalReviewLogger records external review phase progress and summaries.
type ExternalReviewLogger interface {
	Logger
	PrintSection(section status.Section)
	PrintAligned(text string)
}

// FinalizeLogger records finalize phase progress.
type FinalizeLogger interface {
	Logger
	PrintSection(section status.Section)
}

// PlanCreationLogger records interactive plan creation progress and Q&A history.
type PlanCreationLogger interface {
	Logger
	PrintRaw(format string, args ...any)
	PrintSection(section status.Section)
	LogQuestion(question string, options []string)
	LogAnswer(answer string)
	LogDraftReview(action string, feedback string)
}

// InputCollector collects user input for interactive plan creation.
type InputCollector interface {
	AskQuestion(ctx context.Context, question string, options []string) (string, error)
	AskDraftReview(ctx context.Context, question string, planContent string) (action string, feedback string, err error)
}

// GitChecker reports git state used by review loops.
type GitChecker interface {
	HeadHash() (string, error)
	DiffFingerprint() (string, error)
}

// PlanReviewLogger records plan review phase progress, findings and Q&A history.
type PlanReviewLogger interface {
	Logger
	PrintSection(section status.Section)
	PrintAligned(text string)
	LogQuestion(question string, options []string)
	LogAnswer(answer string)
}

// PlanFinder locates the plan file written by the planner during plan creation.
type PlanFinder interface {
	// FindRecent returns the most recently modified plan file changed after the given time, or "" when none.
	FindRecent(startTime time.Time) string
}

// Deps holds late-bound dependencies shared by phase engines.
type Deps struct {
	Git            GitChecker
	InputCollector InputCollector
	BreakCh        <-chan struct{}
	PauseHandler   func(ctx context.Context) bool
	PlanFinder     PlanFinder
	RepoState      RepoStateReader
}

// ExecutionResult is the execution output plus phase-level timeout metadata.
type ExecutionResult struct {
	Result   executor.Result
	TimedOut bool
}

// Policy applies execution retry, timeout, and sleep behavior for phase engines.
type Policy interface {
	Run(ctx context.Context, run func(context.Context, string) executor.Result, prompt string, toolName string) ExecutionResult
	HandlePatternMatchError(err error, tool string) error
	Sleep(ctx context.Context, d time.Duration) error
}

// TaskPrompts renders task phase prompts.
type TaskPrompts interface {
	TaskPrompt() string
}

// ReviewPrompts renders internal review prompts.
type ReviewPrompts interface {
	FirstReviewPrompt() string
	SecondReviewPrompt(prefix string) string
}

// ExternalReviewPrompts renders external review and evaluation prompts.
type ExternalReviewPrompts interface {
	CodexReviewPrompt(isFirst bool, claudeResponse string) string
	CodexEvaluationPrompt(codexOutput string) string
	CustomReviewPrompt(isFirst bool, claudeResponse string) string
	CustomEvaluationPrompt(customOutput string) string
}

// PlanCreationPrompts renders interactive plan creation prompts.
type PlanCreationPrompts interface {
	PlanPrompt() string
}

// PlanReviewPrompts renders the plan review prompt and the planner revision prompt.
type PlanReviewPrompts interface {
	PlanReviewPrompt(planFile, description string) string
	PlanRevisionPrompt(planFile, description, findings string) string
}

// FinalizePrompts renders finalize prompts.
type FinalizePrompts interface {
	FinalizePrompt() string
}

// Locator resolves the current plan file path.
type Locator interface {
	Path() string
}

// ErrUserAborted is returned when a user aborts task execution after a break signal.
var ErrUserAborted = errors.New("user aborted")

// ErrUserRejectedPlan is returned when a user rejects a plan draft.
var ErrUserRejectedPlan = errors.New("user rejected plan")
