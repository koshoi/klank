# Subagents extension

Task delegation to specialized subagents, each running in its own `pi`
process with an isolated context window. Vendored from pi's official
`examples/extensions/subagent` (adapted for the klank image).

## Features

- **Isolated context**: Each subagent runs in a separate `pi` process
- **Streaming output**: See tool calls and progress as they happen
- **Parallel streaming**: All parallel tasks stream updates simultaneously
- **Markdown rendering**: Final output rendered with proper formatting (expanded view)
- **Usage tracking**: Shows turns, tokens, cost, and context usage per agent
- **Abort support**: Ctrl+C propagates to kill subagent processes

## klank integration notes

- Installed by `pi-entrypoint.sh` into `~/.pi/agent/extensions/subagents/`;
  agent definitions land in `~/.pi/agent/agents/`, workflow prompt templates
  in `~/.pi/agent/prompts/`.
- Spawned child processes run with `KLANK_SUBAGENT=1` in their environment;
  the klank bridge extension uses this to disable its `/tmp/pi-inbox` watcher,
  so only the top-level session consumes `klank send` messages (N watchers
  would otherwise race to inject the same message).
- Children run with `--no-session`: transcripts are not persisted. Park and
  revive (surviving container exit) is future work.

## Modes

| Mode | Parameter | Description |
|------|-----------|-------------|
| Single | `{ agent, task }` | One agent, one task |
| Parallel | `{ tasks: [...] }` | Multiple agents run concurrently (max 8, 4 concurrent) |
| Chain | `{ chain: [...] }` | Sequential with `{previous}` placeholder |

## Agent definitions

Markdown files with YAML frontmatter, discovered from
`~/.pi/agent/agents/*.md` (user level; project `.pi/agents/*.md` require
`agentScope: "project"` or `"both"` and, in untrusted projects, confirmation):

```markdown
---
name: my-agent
description: What this agent does
tools: read, grep, find, ls
model: claude-haiku-4-5
---

System prompt for the agent goes here.
```

When `model` is omitted, the subagent inherits the dispatching session's
active model and thinking level.

## Bundled agents

| Agent | Purpose | Model | Tools |
|-------|---------|-------|-------|
| `scout` | Fast codebase recon | Haiku | read, grep, find, ls, bash |
| `planner` | Implementation plans | Sonnet | read, grep, find, ls |
| `reviewer` | Code review | Sonnet | read, grep, find, ls, bash |
| `worker` | General-purpose | Sonnet | (all default) |

## Workflow prompts

| Prompt | Flow |
|--------|------|
| `/implement <query>` | scout → planner → worker |
| `/scout-and-plan <query>` | scout → planner |
| `/implement-and-review <query>` | worker → reviewer → worker |
