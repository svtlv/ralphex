package processor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/svtlv/ralphex/pkg/config"
	"github.com/svtlv/ralphex/pkg/processor/phase"
)

// compile-time check that the prompt builder satisfies the plan review consumer interface.
var _ phase.PlanReviewPrompts = (*promptBuilder)(nil)

func TestPromptBuilder_FinalPrompts(t *testing.T) {
	appCfg := &config.Config{
		TaskPrompt:         "task {{PLAN_FILE}} {{PROGRESS_FILE}}",
		ReviewFirstPrompt:  "first {{GOAL}}",
		ReviewSecondPrompt: "second {{DEFAULT_BRANCH}}",
		CodexReviewPrompt:  "codex {{DIFF_INSTRUCTION}} {{PREVIOUS_REVIEW_CONTEXT}}",
		CodexPrompt:        "eval {{CODEX_OUTPUT}} {{GOAL}}",
		CustomReviewPrompt: "custom {{DIFF_INSTRUCTION}} {{PREVIOUS_REVIEW_CONTEXT}}",
		CustomEvalPrompt:   "custom eval {{CUSTOM_OUTPUT}}",
		MakePlanPrompt:     "make {{PLAN_DESCRIPTION}} {{PLANS_DIR}}",
		FinalizePrompt:     "finalize {{GOAL}}",
		PlansDir:           "custom/plans",
	}
	cfg := Config{
		PlanFile: "docs/plans/test.md", ProgressPath: "progress.txt", PlanDescription: "add feature",
		DefaultBranch: "main", AppConfig: appCfg,
	}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})

	assert.Equal(t, "task docs/plans/test.md progress.txt", builder.TaskPrompt())
	assert.Equal(t, "first implementation of plan at docs/plans/test.md", builder.FirstReviewPrompt())
	assert.Equal(t, "prefix: second main", builder.SecondReviewPrompt("prefix: "))
	assert.Contains(t, builder.CodexReviewPrompt(true, ""), "git diff main...HEAD")
	assert.Contains(t, builder.CustomReviewPrompt(false, "fixed"), "PREVIOUS REVIEW CONTEXT")
	assert.Equal(t, "eval findings implementation of plan at docs/plans/test.md", builder.CodexEvaluationPrompt("findings"))
	assert.Equal(t, "custom eval custom findings", builder.CustomEvaluationPrompt("custom findings"))
	assert.Equal(t, "make add feature custom/plans", builder.PlanPrompt())
	assert.Equal(t, "finalize implementation of plan at docs/plans/test.md", builder.FinalizePrompt())
}

func TestPromptBuilder_NilConfigDependencies(t *testing.T) {
	builder := newPromptBuilder(promptBuilderOpts{cfg: Config{}, log: newMockLogger()})

	assert.NotPanics(t, func() {
		assert.Empty(t, builder.TaskPrompt())
		assert.Empty(t, builder.FirstReviewPrompt())
		assert.Empty(t, builder.CodexEvaluationPrompt("findings"))
		assert.Empty(t, builder.FinalizePrompt())
	})
}

func TestPromptBuilder_CodexTaskGuidance(t *testing.T) {
	appCfg := &config.Config{TaskPrompt: "do work", Executor: config.ExecutorCodex}
	cfg := Config{AppConfig: appCfg}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})

	prompt := builder.TaskPrompt()
	assert.True(t, strings.HasPrefix(prompt, codexTaskGuidance))
	assert.Contains(t, prompt, "do work")
}

func TestPromptBuilder_PlanReviewPrompt(t *testing.T) {
	appCfg := &config.Config{
		PlanReviewPrompt: "review {{PLAN_FILE}} for {{PLAN_DESCRIPTION}} log {{PROGRESS_FILE}} base {{DEFAULT_BRANCH}}",
	}

	t.Run("explicit plan path differs from locator", func(t *testing.T) {
		cfg := Config{PlanFile: "docs/plans/other.md", ProgressPath: "progress.txt", DefaultBranch: "main", AppConfig: appCfg}
		builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger()})

		got := builder.PlanReviewPrompt("docs/plans/new.md", "add feature")
		assert.Equal(t, "review docs/plans/new.md for add feature log progress.txt base main", got)
	})

	t.Run("works without plan file in config", func(t *testing.T) {
		cfg := Config{AppConfig: appCfg, DefaultBranch: "master"}
		builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger()})

		got := builder.PlanReviewPrompt("docs/plans/new.md", "goal")
		assert.Contains(t, got, "review docs/plans/new.md for goal")
		assert.Contains(t, got, "(no progress file available)")
		assert.NotContains(t, got, "{{")
	})

	t.Run("description placeholders stay literal", func(t *testing.T) {
		cfg := Config{AppConfig: appCfg, DefaultBranch: "main", ProgressPath: "p.txt"}
		builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger()})

		got := builder.PlanReviewPrompt("a.md", "use {{DEFAULT_BRANCH}} text")
		assert.Contains(t, got, "for use {{DEFAULT_BRANCH}} text")
	})

	t.Run("no commit trailer for read-only review", func(t *testing.T) {
		withTrailer := &config.Config{PlanReviewPrompt: "review {{PLAN_FILE}}", CommitTrailer: "Co-authored-by: x"}
		builder := newPromptBuilder(promptBuilderOpts{cfg: Config{AppConfig: withTrailer}, log: newMockLogger()})

		assert.Equal(t, "review a.md", builder.PlanReviewPrompt("a.md", ""))
	})
}

func TestPromptBuilder_PlanRevisionPrompt(t *testing.T) {
	cfg := Config{ProgressPath: "progress-x.txt", AppConfig: &config.Config{}}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger()})

	got := builder.PlanRevisionPrompt("docs/plans/x.md", "add feature", "1. MAJOR: Task 2 - wrong path")

	assert.Contains(t, got, "already exists at: docs/plans/x.md")
	assert.Contains(t, got, "Plan goal: add feature")
	assert.Contains(t, got, "progress-x.txt")
	assert.Contains(t, got, "1. MAJOR: Task 2 - wrong path")
	assert.Contains(t, got, "Apply only the findings")
	assert.Contains(t, got, "Do not create a new file")
	assert.Contains(t, got, "Task and Iteration keywords")
	assert.Contains(t, got, "Do not emit PLAN_DRAFT or QUESTION")
	assert.Contains(t, got, "<<<RALPHEX:PLAN_READY>>>")
	assert.NotContains(t, got, "When making git commits")

	t.Run("omits goal line when description is empty", func(t *testing.T) {
		assert.NotContains(t, builder.PlanRevisionPrompt("a.md", "  ", "f"), "Plan goal:")
	})

	t.Run("appends commit trailer", func(t *testing.T) {
		c := Config{AppConfig: &config.Config{CommitTrailer: "Co-authored-by: ralphex <noreply@ralphex.com>"}}
		b := newPromptBuilder(promptBuilderOpts{cfg: c, log: newMockLogger()})

		got := b.PlanRevisionPrompt("a.md", "d", "f")
		assert.True(t, strings.HasSuffix(got, "Co-authored-by: ralphex <noreply@ralphex.com>"))
		assert.Contains(t, got, "When making git commits")
	})

	t.Run("findings with placeholders stay literal", func(t *testing.T) {
		got := builder.PlanRevisionPrompt("a.md", "d", "1. MINOR: x - {{PLAN_FILE}}")
		assert.Contains(t, got, "{{PLAN_FILE}}")
	})
}
