---
name: commit-messages
description: Write a git commit message for a given diff or change description. Use when the user asks for a commit message or wants help describing a code change concisely.
---
Write a concise, conventional commit message for the change described.

Rules:
- Summary line under 72 characters, imperative mood ("add", not "added").
- Focus the summary on *why* the change was made, not a restatement of the diff.
- If more context is useful, add a blank line then a short body (1-3 sentences).
- Do not invent details not present in the provided change description.
