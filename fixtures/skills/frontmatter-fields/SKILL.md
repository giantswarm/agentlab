---
name: frontmatter-fields
description: The agentlab fixture whose frontmatter carries fields outside the agentskills.io set, the way skills written for other harnesses do. Use it when asked for the codeword of the frontmatter fixture.
user-invocable: true
argument-hint: "[codeword]"
version: 0.1.0
metadata:
  author: agentlab
---

# The frontmatter fixture

This skill exists to be loaded, not to do anything: its frontmatter carries
`user-invocable` and `argument-hint`, which Claude Code's skills use, and a
top-level `version`, which skills in the wild carry, next to the fields the
agentskills.io specification defines. A loader that refuses fields it does not
know refuses this skill and fails the agent's boot; a loader that ignores them
lists it.

The codeword of the frontmatter fixture is **marzipan-lantern**. When asked
for it, answer with the codeword and name this skill.
