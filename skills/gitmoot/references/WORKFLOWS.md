# Gitmoot Workflows

Three end-to-end walkthroughs. Every command is in
[CLI.md](CLI.md) with its full flags.

## Set up a repository for reviews

```sh
gitmoot init
gitmoot org init
gitmoot repo add owner/repo --path /path/to/checkout
gitmoot org seat add <your-role> --pane <pane-id>
gitmoot reviewer subscribe reviewer-1 --runtime omp --session fresh:reviewer-1 --role <reviewer-role> --repo owner/repo --capability review
gitmoot daemon start
gitmoot doctor
```

`org seat add` acts as a role declared in the org registry, by default the
root role `owner`; on a fresh home it fails `acting role "owner" is not
declared`. `gitmoot org init` declares `owner` (it appends the `[org]` and
`[org.roles."owner"]` block shown in the no-model walkthrough below to
`~/.gitmoot/config.toml`) and then checks Herdr 0.7.5 or later, which
`--pane` needs too. Without Herdr, `org init` still declares `owner` but exits
1, and `--pane` fails `inspect live Herdr panes`: leave `--pane` out and the
seat is created unbound (bind it later by running `org seat add` again with
`--pane`), or follow the no-model walkthrough, which declares `owner`
directly.

The reviewer needs the `review` capability and must not also have `implement`.
An omp reviewer's `--session` is a UUID or `fresh:<suffix>`; omp starts a fresh
session for every job. The daemon picks queued reviews up; `gitmoot daemon
status` shows that it is running.

## A seat's pull request, from push to merge

1. Write the change on your branch, push it, open the pull request.
2. Record that you wrote it, then ask for a review of the exact head:

   ```sh
   gitmoot run record --implementer <seat> --repo owner/repo --type implement --decision implemented --pr 42 --branch my-branch --summary "add retry to the fetcher"
   gitmoot review request --repo owner/repo --pr 42 --head <40-hex-sha>
   ```

   `gitmoot review level --repo owner/repo --pr 42` says whether the head needs
   a review before merge; a `level2_background` head merges first and is
   reviewed after, with `--post-merge`.

3. Keep working. Gitmoot wakes your role when the verdict is saved. To look
   sooner: `gitmoot review status --repo owner/repo --pr 42`.
4. On `changes_requested`, read the findings
   (`gitmoot findings --repo owner/repo --pr 42`), fix them, push, and request
   a review of the new head. The implement record from step 2 covers every
   head of the pull request, so it need not be repeated. The re-review closes
   a finding only by citing its UID (`continues_uid`, see
   [RESULT_CONTRACT.md](RESULT_CONTRACT.md#the-findings-ledger)).
5. On `approved` at the current head with green CI, the gate merges (when
   automatic merge is on) or a `/gitmoot merge` comment asks it to. If it
   holds, the reason names the remedy.

## A review with no model, on a throwaway home

To check the flow without spending a model, register a `shell` reviewer whose
command prints a verdict, and run the job in the foreground. It needs no
GitHub login, no Herdr and no omp, and touches nothing outside `/tmp/gm-demo`.
`--home $H` keeps all state in `$H/.gitmoot`; `HOME` is exported too because
`git` reads it, and so does any `gitmoot` command run without `--home`.

```sh
export HOME=/tmp/gm-demo
H=/tmp/gm-demo/h
mkdir -p $HOME && git init -q -b main $HOME/repo && cd $HOME/repo
git remote add origin https://github.com/acme/widget.git
git config user.name demo && git config user.email demo@example.com
git commit -q --allow-empty -m base && git switch -q -c feat
echo hi > a.txt && git add a.txt && git commit -q -m "add a.txt" && git rev-parse HEAD
gitmoot init --home $H
```

`repo add` reads the checkout's `origin` remote, so the repository needs one;
it is never contacted. `org seat add` acts as a role declared in the org
registry, by default `owner`. `gitmoot org init` declares it but then exits 1
without Herdr, so declare it directly: append to `$H/.gitmoot/config.toml` the
same `[org]` and `[org.roles."owner"]` block `org init` writes (the
`[disk_guard]` lines only matter on a nearly full disk, where the disk guard
would otherwise refuse the request; use them in a throwaway home only):

```toml
[disk_guard]
min_free_percent = 1

[org]
enforce = "warn"

[org.roles."owner"]
scope = ["*"]
merge_rule = "owner"
```

Then register the seat and a shell reviewer, and request a review of the head
printed above:

```sh
gitmoot repo add acme/widget --path $HOME/repo --home $H
gitmoot org seat add me --home $H
gitmoot reviewer subscribe shell-reviewer --runtime shell --session 'echo "{\"gitmoot_result\":{\"decision\":\"approved\",\"summary\":\"Read the whole diff: a.txt adds one line; nothing to run.\",\"findings\":[],\"changes_made\":[],\"tests_run\":[],\"needs\":[],\"evidence\":\"static_only\"}}"' --role reviewer --repo acme/widget --capability review --home $H
gitmoot run record --acting-role me --repo acme/widget --type implement --decision implemented --pr 1 --branch feat --summary "add a.txt" --home $H
gitmoot review request --repo acme/widget --pr 1 --head <sha> --branch feat --reviewer shell-reviewer --role me --home $H
gitmoot run start <run-id> --home $H
gitmoot review status --repo acme/widget --pr 1 --home $H
gitmoot run show <run-id> --home $H
```

- `me` was created by `org seat add`, so it is an org role, not a registered
  agent: record it with `--acting-role me`. `--agent me` fails with
  `agent "me" not found`.
- Name the shell reviewer with `--reviewer`. A reviewer the router picks
  itself runs on omp, not on its shell command.
- The shell session runs sandboxed in a read-only checkout of the head: it can
  read the checkout (`cat a.txt` works) but not other paths (`cat
  /tmp/verdict.json` fails `Permission denied` and the job ends `failed`).
  Print the verdict inline with `echo`. Re-running `reviewer subscribe` with a
  new `--session` replaces the command.
- Pass both `--head` and `--branch` so the request does not need to read the
  pull request from GitHub.
- The request prints ``hold: daemon not running: the review stays queued
  until `gitmoot daemon start` starts the daemon``; `run start` runs it anyway.
- Read `run start` by its last line, `started run <id>: <state>`: that is the job's
  state. An approval may print a line before it such as `job <id> failed: workflow advance failed:
  ...` is about what happens after the verdict is stored (posting it to the
  pull request, binding an approval to the head GitHub reports), and needs
  GitHub. Without it you see a `gh` login or `GH_TOKEN` error, or
  `review approval held: no observed pull request row records a current head`
  (no daemon has observed PR 1, so the gate will not count the approval).
  Both are expected here; the verdict is stored, `review status` shows it,
  and `run show` lists the error under `failure_diagnostics`.
- The demo verdict's 40-character-plus `summary` is what passes the
  `review-verdict-has-evidence` result check; `"summary":"ok"` would fail it.
- To exercise a finding, commit a second change, swap in a `changes_requested`
  verdict (with `"severity":"P2"` and a finding carrying `severity`, `file`,
  `title`), request and run that head, then close the finding at a third head
  with the `continues_uid` verdict in
  [RESULT_CONTRACT.md](RESULT_CONTRACT.md#the-findings-ledger).
- `gitmoot doctor --home $H` exits 1 here: `omp`, `gh auth`, `herdr` and
  `repo remote` fail because none is available. That is expected for this
  walkthrough.
- Clean up with `rm -rf /tmp/gm-demo`.

## Hand off with messages

```sh
gitmoot message send <parent-role> "PR 42 approved at abc1234; ready to merge"
gitmoot message inbox
gitmoot message reply <id> "done"
```

Keep the message ids you still owe an answer in your handoff notes. A message
you send to a busy role stays `queued` until it can take it; you get one notice
if it waits more than 15 minutes.
