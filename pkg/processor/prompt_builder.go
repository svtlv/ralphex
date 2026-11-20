package processor

import (
	"strings"

	"github.com/svtlv/ralphex/pkg/config"
	"github.com/svtlv/ralphex/pkg/status"
)

type promptBuilder struct {
	cfg                    Config
	log                    Logger
	locator                *planLocator
	codexFrontmatterWarned map[string]bool
}

type promptBuilderOpts struct {
	cfg     Config
	log     Logger
	locator *planLocator
}

func newPromptBuilder(opts promptBuilderOpts) *promptBuilder {
	cfg := opts.cfg
	if cfg.AppConfig == nil {
		cfg.AppConfig = &config.Config{}
	}
	locator := opts.locator
	if locator == nil {
		locator = newPlanLocator(cfg)
	}
	return &promptBuilder{cfg: cfg, log: opts.log, locator: locator}
}

func (b *promptBuilder) TaskPrompt() string {
	return b.prependCodexTaskGuidance(b.replacePromptVariables(b.cfg.AppConfig.TaskPrompt))
}

func (b *promptBuilder) FirstReviewPrompt() string {
	return b.prependCodexReviewGuidance(b.replacePromptVariables(b.cfg.AppConfig.ReviewFirstPrompt))
}

func (b *promptBuilder) SecondReviewPrompt(prefix string) string {
	return prefix + b.prependCodexReviewGuidance(b.replacePromptVariables(b.cfg.AppConfig.ReviewSecondPrompt))
}

func (b *promptBuilder) CodexReviewPrompt(isFirst bool, claudeResponse string) string {
	return b.replaceVariablesWithIteration(b.cfg.AppConfig.CodexReviewPrompt, isFirst, claudeResponse)
}

func (b *promptBuilder) CodexEvaluationPrompt(codexOutput string) string {
	prompt := b.replacePromptVariables(b.cfg.AppConfig.CodexPrompt)
	return strings.ReplaceAll(prompt, "{{CODEX_OUTPUT}}", codexOutput)
}

func (b *promptBuilder) CustomReviewPrompt(isFirst bool, claudeResponse string) string {
	return b.replaceVariablesWithIteration(b.cfg.AppConfig.CustomReviewPrompt, isFirst, claudeResponse)
}

func (b *promptBuilder) CustomEvaluationPrompt(customOutput string) string {
	prompt := b.replacePromptVariables(b.cfg.AppConfig.CustomEvalPrompt)
	return strings.ReplaceAll(prompt, "{{CUSTOM_OUTPUT}}", customOutput)
}

func (b *promptBuilder) PlanPrompt() string {
	prompt := b.cfg.AppConfig.MakePlanPrompt
	prompt = strings.ReplaceAll(prompt, "{{PLAN_DESCRIPTION}}", b.cfg.PlanDescription)
	result := b.replaceBaseVariables(prompt)
	return b.appendCommitTrailerInstruction(result)
}

// PlanReviewPrompt renders the plan review prompt for the given plan file.
// the plan path is explicit because the locator has no plan file in plan mode.
// the description is substituted after the base variables, so placeholders in it stay literal.
func (b *promptBuilder) PlanReviewPrompt(planFile, description string) string {
	prompt := strings.ReplaceAll(b.cfg.AppConfig.PlanReviewPrompt, "{{PLAN_FILE}}", planFile)
	prompt = b.replaceBaseVariables(prompt)
	return strings.ReplaceAll(prompt, "{{PLAN_DESCRIPTION}}", description)
}

// PlanRevisionPrompt renders the Go-side prompt that tells the planner to apply review findings in place.
// it is not a prompt file: make_plan.txt exits at once when the plan exists, so it cannot drive a revision.
func (b *promptBuilder) PlanRevisionPrompt(planFile, description, findings string) string {
	var sb strings.Builder
	sb.WriteString("Revise an existing implementation plan.\n\n")
	sb.WriteString("The plan file already exists at: " + planFile + "\n")
	if strings.TrimSpace(description) != "" {
		sb.WriteString("Plan goal: " + description + "\n")
	}
	sb.WriteString("\nRules:\n")
	sb.WriteString("- Read the plan file. Read the progress file " + b.getProgressFileRef() + " for earlier questions and answers.\n")
	sb.WriteString("- Apply only the findings listed below. Do not make other changes.\n")
	sb.WriteString("- Edit the plan file in place. Do not create a new file.\n")
	sb.WriteString("- Keep the plan format. The Task and Iteration keywords in section headers stay in English." +
		" Checkboxes belong only in Task sections.\n")
	sb.WriteString("- Do not emit PLAN_DRAFT or QUESTION signals.\n")
	sb.WriteString("- When the plan file is updated, end your response with " + status.PlanReady + ".\n")
	sb.WriteString("- Do not use markdown formatting in your response text.\n\n")
	sb.WriteString("Findings to apply:\n" + findings + "\n")
	return b.appendCommitTrailerInstruction(sb.String())
}

func (b *promptBuilder) FinalizePrompt() string {
	return b.replacePromptVariables(b.cfg.AppConfig.FinalizePrompt)
}
