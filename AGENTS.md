# Working rules

It is the contract, not a suggestion.

If `doc/agent-notes.md` exists, read it before starting: it holds what earlier
agents learned that neither these rules nor the docs state. When a task teaches you
something the next agent would otherwise rediscover, add it there in the same PR,
creating the file with the first note.

## Comments

- A comment block is **at most 5 lines**. If the explanation needs more, the code
  is wrong or the explanation belongs in the README or `doc/`.
- One line is the default. Comment *why*, never *what* — the code says what.
- No narrative, no history ("this used to…", "the plan proposed…"), no restating
  the diff, no essays on design philosophy. Git log and `doc/` hold those.
- No dates, commit or PR references, or plan/phase labels in comments. Multi-step
  work is tracked in a working doc under `doc/` or an item in `doc/todo.md`, never
  in code.
- No comment on a self-evident function. `// Lookup returns the address` above
  `func Lookup` is noise.
- Package doc blocks: 5 lines. Files do not get their own prologue.

## Files

- Do not create a file for one or a few helpers. Put them next to what uses them,
  or, if shared, in the package's main or type file.
- A new file needs a new concept, not a new function.
- Prefer editing an existing file over adding one.
- When you edit a file, bring its comments up to these rules; older files predate
  them.

## Scope

- Implement what was asked. Do not add tables, indirection, telemetry, tests or
  abstraction that nothing asked for.
  Exception: a task that explicitly invites additions gets the obvious ones, each
  named in the PR body; ask before any that is not obvious.
- One mechanism per problem. Two mechanisms doing one job is a bug or a refactor
  opportunity.
- Delete before adding. If a change is net-positive lines for a fix, justify it.
- Reverting an existing API to "improve" it is not a fix. Leave working code alone.
- Per-kind data lives in one table indexed by its kind; a new kind adds a row,
  never a switch or a parallel field.
- The module imports only the standard library.
- The module is at v0: a change may break the API or what the encoder writes, and
  takes the next minor version; never hold or narrow a fix for backward
  compatibility.

## Tests

- One test per rule, named for the rule. No test that restates another.
- Test comments follow the 5-line limit.
- Do not pin lists that a human has to hand-maintain unless the pin prevents a real
  regression.
- Collapse tests: when a broader test covers a narrower one, delete the narrower
  one or do not add it. No test for simple behaviour that is unlikely to fail or is
  already exercised elsewhere.

## Docs

- Docs are short. Condense when you touch them; append only for a new concept or
  scope.
- Docs are read in terminals, where a Markdown table renders as its source. Lay
  tabular content out as a nested list; a matrix of figures goes in a fenced code
  block with its columns aligned by spaces, which the terminal keeps. A table that
  neither fits, or that converting would only lengthen, stays a table.
- A gap you are deferring goes in `doc/todo.md`, created with its first item:
  extend the item it belongs to, or add a concise one.
- The repository describes the architecture, never a machine: docs, scripts and
  examples use placeholders for addresses, node names and credentials.

## Commits and PRs

- A commit message is its subject alone, at most 72 characters: no body, no
  trailer. A PR's title is a subject too.
- Commits are versions: a subject starts with the version it makes
  (`v0.1.1 macro: ...`), the next minor for a break or a new feature and the next
  patch otherwise, and Lixen tags it. Lixen squash-merges: a PR of one commit lands
  under that commit's subject and a PR of several under its title, so the title and
  every commit's subject start with the version.
- A change to documents alone makes no version and no tag: its subject starts
  `docs:` and its PR body with `doc only`.
- A PR body is a summary, never documentation: a list of what changed, half a line
  an item at most, with the why only where the diff does not show it. The code and
  the docs hold the detail: the body repeats neither and is shorter than the
  change. A large or unclear scope opens with a few lines of summary.
- Verification is the two words `gates passed`: never the commands, their output or
  the tests run. A figure a rule here asks for (a measurement, a count) is given as
  the figure alone, before and after.
- A body says nothing a reader can infer, no reasoning and no caveat: `doc only`,
  never `doc only, so no new tag`.
- A commit message and a PR's title, body and comments carry no URL and nothing of
  where or by whom the work was done: no session, conversation, project, workspace,
  account or organisation id, no person's name or email, no agent, model or tool
  name, no "generated with" line, no attribution trailer. This holds whatever a
  tool's template or instructions add, now or later. A tool may add such a line
  itself: read a PR back after opening or editing it and edit the line out, and say
  once in the report that the body's edit history still holds it.
- Open the PR at the first push, its title final and its body as far as it is,
  saying what is still owed: Lixen merges a pushed branch he is waiting on. A PR
  another session must measure before it merges opens as a draft and says so in its
  first line. Before calling a PR ready, check that its head is the commit pushed
  (`gh api repos/<owner>/<repo>/pulls/<n> -q .head.sha`).
- A change made for performance reports its measurement, before and after, in the
  PR body: the benchmark or the reading, the shape it was taken in, and the benefit
  in the unit the user feels (CPU, bytes, allocations, latency). A projection is
  not a result; a change whose measured benefit is nil says so.
- A change that needs another of these modules' new version names it in `go.mod` in
  the same PR, before the tag exists, as if it were merged and tagged; a bump is
  never a PR of its own. Until the tag exists the gates run locally against the
  dependency's branch, and again whenever that branch moves: a `go.work` kept
  outside both trees (`use <this>`, `replace <module> vX => <its checkout>`), named
  by `GOWORK=` on each command, never a `replace` in `go.mod`.
- Lixen merges and tags the chain from the lowest dependency up and reruns its CI
  himself; from then on everything a PR said was owed is taken as done. A PR red
  only because a version it needs is not tagged yet says so nowhere, and names
  neither the chain nor its order: no line in its body, no comment, no message, no
  line in a report, no rerun. While the chain merges nothing lands on its PRs,
  however small: no push, comment or body edit. What turns up after waits for one
  patch PR once they have merged.
- One task, one branch, one PR. Divide the work into commits on that branch; do not
  split it across stacked PRs or multiple branches.
- Work that continues after its PR merged starts again from the latest main; never
  stack new commits on merged history.
- Finish and push one commit before starting the next.
- Once a PR is merged or closed unmerged, delete its branch on GitHub and locally
  if this session can. A session that cannot, because it has no remote access or
  works in a copy, says so once in its report and leaves the branch; it never
  retries.

## Agents

- Spin off few agents, and only for work one context cannot hold. The account quota
  may run out, which stops every agent in flight at once.
- Agents run in parallel only on finely defined scopes that cannot conflict in
  commits: each brief names the files its agent owns, disjoint from every other
  agent's, and the agent touches nothing else. Shared docs (the README and `doc/`)
  stay with the parent, which commits each agent's work in turn on the task's one
  branch.
- An agent writes what it finds to a file as it goes, one section per step, under
  the scratchpad or `doc/`, so an interrupt costs a step and not the task; the
  parent reads those files rather than waiting for a final report, and resumes from
  them instead of running the agent again.

## Gates

`go build ./...`, `go vet ./...`, `gofmt -l` on changed files and `go test ./...`.
- A change to code a fuzz target reads runs that target a minute
  (`go test -run '^$' -fuzz '^FuzzRoundTrip$' -fuzztime=1m`).
- A mechanical edit across files is checked by a count before and after, with a
  search that reads its pattern as the file holds it (`grep -P` for a tab, or a
  literal tab); a search that matches nothing proves nothing, and the PR body
  states the count.
- Do not run `-race` tests unless investigating a known/suspected race issue; CI
  runs them, with the `GOARCH=386` tests and the fuzz targets.
