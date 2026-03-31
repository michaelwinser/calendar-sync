# Agent Prompts

Agent prompt files live in `.claude/agents/` and can be used with Claude Code directly.

## Code Review Agent

**File**: `.claude/agents/code-review.md`
**When**: Before each commit (manual step)
**Invocation**:
```bash
git diff --cached | claude -p "$(cat .claude/agents/code-review.md)"
```

## Architecture Review Agent

**File**: `.claude/agents/architecture-review.md`
**When**: At the start of each milestone, and when major changes are made to the milestone plan
**Invocation**:
```bash
claude -p "$(cat .claude/agents/architecture-review.md) Review the plan in docs/M{n}-plan.md"
```
