<!-- obr-agent-instructions-v1 -->

---

## Obr Workflow Integration

This project uses [obr](https://github.com/jwiegley/obr) for issue tracking.
Issues live in `PLAN.org` — an Org-mode file tracked in git, at `doc/`,
`docs/`, or the project root. `.obr/` is a per-machine cache (SQLite plus
metadata) that ignores itself; never commit anything under it. obr never
commits, pushes, pulls, or installs hooks: exporting and committing are
separate, explicit steps. (A few read-only commands do shell out to git to
report what it sees — `vcs-status`, `changelog`, `orphans` — and none of them
write.)

### Essential Commands

```bash
# View ready issues (open, unblocked, not deferred)
obr ready

# List and search
obr list --status=open # All open issues
obr show <id>          # Full issue details with dependencies
obr search "keyword"   # Full-text search

# Create and update
obr create "Title" -d "..." --type=task --priority=2
obr q "Title"          # Quick capture: create and print only the id
obr update <id> --status=in_progress
obr close <id> --reason="Completed"
obr close <id1> <id2>  # Close multiple issues at once

# Write the tracked surface
obr sync --flush-only  # Write PLAN.org from the database
obr sync --status      # Check whether DB and PLAN.org agree
```

### Workflow Pattern

1. **Start**: Run `obr ready` to find actionable work
2. **Claim**: Use `obr update <id> --status=in_progress`
3. **Work**: Implement the task
4. **Complete**: Use `obr close <id> --reason="..."`
5. **Record**: Run `obr sync --flush-only`, then commit `PLAN.org` with the code

### Key Concepts

- **Dependencies**: Issues can block other issues. `obr ready` shows only open, unblocked work.
- **Priority**: P0=critical, P1=high, P2=medium, P3=low, P4=backlog (use numbers 0-4, not words)
- **Types**: task, bug, feature, epic, chore, docs, question
- **Blocking**: `obr dep add <issue> <depends-on>` to add dependencies
- **Recording discovered work**: create an issue the moment you find work you are not doing now, and link it (`--deps discovered-from:<id>`)

### Session Protocol

**Before ending any session, run this checklist:**

```bash
obr sync --flush-only   # Write issue changes to PLAN.org
git status              # Check what changed
git add <files>         # Stage code changes AND PLAN.org together
git commit -m "..."     # One commit: the change and its issue state
```

### Best Practices

- Check `obr ready` at session start to find available work
- Record dependencies at creation time — they are what make `obr ready` meaningful
- Update status as you work (in_progress → closed)
- Use descriptive titles and set appropriate priority/type
- Commit `PLAN.org` together with the code that changes it; its diff is the review trail
- A fresh clone rebuilds the cache with: `obr init && obr sync --import-only --rebuild`

<!-- end-obr-agent-instructions -->
