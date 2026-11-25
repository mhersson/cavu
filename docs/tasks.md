# Tasks

Add a task and run it. `task run` prints each event on a line of its own as it is recorded, beginning at your newest action on the task, such as its start or an answer, says why the task waits when it does, and ends with the task's status. With the switch `publish` on it ends once the task's pull request opens, with the pull request's address and a line saying that `cavu serve` watches it from there, so start `cavu serve` ([Running the server](running.md)); on a task whose pull request is watched already it prints the same and returns at once. `task show` also says when the task needs you, and prints the task's dependencies and why it waits, the turns, the duration, the budget it used since you last acted, and the files of the task's change, a binary file with its size, at most 50 of them and then the total, with the paths in it that a person should see before a merge, each kind on a line of its own: workflows under `.github/workflows/`, agent instructions (`AGENTS.md`, `CLAUDE.md` and files under a `.claude/` directory, in any directory), deleted ones too, and files over 1 MiB that the change adds. Once the task has a pull request, `task show` prints its address, its state, its checks with the failed ones named and, while CAVU watches it, what the watch waits for; the change is then the one CAVU published to the pull request last.

With the project's switch `publish` on, the default, CAVU acts on GitHub under the account of the token it holds (for a GitHub App, the App's bot). Once the task's change is exported it pushes the task's branch, `cavu/` and a slug of the title (`cavu/fix-the-login-redirect` for the task below; when GitHub already has a branch of that name that CAVU did not push for the task, the first free name with `-2` to `-9` added), and opens a draft pull request with a text it writes, whose contents [Security](security.md) lists. `cavu serve` then watches the pull request, and may mark it ready for review, push the commits of fix runs to the branch, and comment on it after a follow-up run ([The pull request](#the-pull-request)). CAVU never forces a push, never merges, never closes a pull request and never deletes a branch.

```
cavu task add web --title "Fix the login redirect" --body-file task.md
cavu task run ID
cavu task show ID
cavu task diff ID
```

A run that is no [fix run](#the-pull-request) opens with a readiness turn: the agent reads the task and the repository and changes nothing. When a fact it needs is in neither, the task stops with its questions, and `task show` prints each with the answer the agent would assume. `cavu project set web --switch readiness=off` turns the readiness turn off.

A change that passes verify in a run that is no fix run is reviewed: a fresh agent session reads the task and the diff, changes nothing, and judges whether the change does what the task asked and whether it is well built. A critical or important finding sends the change back for a fix, the verify command and a review of the fix, at most five times; the last two fixes are a fresh session. Findings never stop a task: after the fifth fix, after a fix that changes nothing, or when a review gives no verdict, the change is exported as it is, and `task show` prints the findings left open, the minor ones, and why the newest review gave no verdict. The reviewer runs on `model_review` and `effort_review`, the fresh fix sessions on `model_strong` and `effort_strong`, and each of the four is the work value when unset. `cavu project set web --switch review=off` turns the review off.

A task can wait for others: `--depends-on ID[,ID]` makes it wait until every task it names is done, and stops it when one of them is cancelled. `--start` starts it as it is added.

```
cavu task add web --title "Test the login redirect" --depends-on ID --start
```

`cavu task edit ID` changes only the title, body or dependencies it is given, until a new or restored task runs or is cancelled; `--depends-on ""` removes every dependency.

```
cavu task edit ID --depends-on 3,4
```

Images, such as screenshots of a bug, are attached to a task in the web app: paste, drop or choose them under **Images** in the **New task** form, or on a draft's page under its body; on a phone, **Take a photo** or **Choose an image**. CAVU keeps PNG, JPEG, GIF and WebP images of at most 10 MiB each, and refuses another type or a larger file. A task's images can be added and removed until a new or restored task runs or is cancelled. With images, **Create** creates the task as a draft, uploads them, and then starts it when **Start it now** is ticked; when an image fails to upload, the task stays a draft and its page names the image, which you can attach again there before you press **Start**. Each agent session that reads the task gets its images as files outside the repository's checkout, named in its prompt, so none enters the change.

`task run` drives only the task it names, so run a dependency first: on a task that waits for one, `task run` prints why and waits until that task is done or you interrupt it.

With the switch `publish` on, `task run` ends where the pull request opens, and `cavu serve` records its merge, which makes the task `done`; `cavu task done` is refused for a task with a pull request. With `cavu project set web --switch publish=off`, a task in `in_review` has its change ready: read the diff, fetch its branch into your clone from the bundle `task show` prints (`git fetch BUNDLE 'refs/heads/cavu/*:refs/heads/cavu/*'`; `GET /api/tasks/ID/change?format=bundle` hands over the same bundle, and `format=diff` the diff), then push the branch and merge it yourself. Then record the merge, which makes the task `done`:

```
cavu task done ID
```

When a task stops, `task run` exits 3 and prints the reason, what it means and what clears it:

```
cavu task answer ID < answer.txt    # the agent asked a question
cavu task retry ID                  # run it again after fixing the cause
cavu task cancel ID                 # give up on it
```

Then `cavu task run ID` again for an answer or a retry. A retry of one of the pull request's stops but `push_rejected` is taken up by `cavu serve` at its next look, and `task run` returns at once there ([below](#the-pull-request)). A verb that drives containers holds the data directory's lock while it runs, so a second one is refused with a message naming the lock.

Open a cancelled task in the web app and press **Restore to draft**. The task keeps its number, details, dependencies, images and history. If the cancelled run is still releasing its resources, restoration finishes automatically after cleanup. Press **Start** when you want a fresh run. If an earlier run recorded a base commit, the fresh run uses that commit.

## The pull request

`task run` ends where the pull request opens. From there `cavu serve` watches it ([Running the server](running.md)), so keep one running; when it starts, it looks at once at every pull request CAVU watches.

The pull request opens as a draft. GitHub does not offer draft pull requests in every repository: when GitHub answers the draft with status 422, CAVU asks once more without the draft, so the pull request opens ready for review and CAVU's ready-marking below then changes nothing.

While no fix run works on it, `cavu serve` looks at the pull request every 10 seconds while its checks run, or while none has started within the 3 minutes below, and every minute otherwise, under a stop too; a fix run looks once before it pushes. A look reads the pull request, the check runs and commit statuses of its newest commit but those the project ignores, and the reviewers' requests; once GitHub has refused a personal access token the Checks API, it reads the jobs of the commit's GitHub Actions workflow runs in place of the check runs ([GitHub access](github-access.md#checks-and-a-fine-grained-token)). After a look that fails, the next comes 10 seconds later, twice as long after each further failure in a row up to 5 minutes, and while GitHub limits CAVU's requests, not before the limit lifts and at most an hour later; the API's `waiting` holds the newest look's error and the time of the next look.

A merge seen at any look makes the task `done`, and a close without a merge cancels it. A commit on the branch that CAVU did not push, once a look has shown it, stops the task as `taken_over` as soon as no stop is open, unless a retry of `conflict` or `taken_over` gave the branch back to CAVU. Otherwise CAVU acts only on a look taken after its own newest push or ready-marking and after a person's newest action on the task, so a retry of a stop below takes effect at the next look, at most a minute later while GitHub answers. Then, in this order:

1. A conflict with the base branch stops the task as `conflict`.
2. Failed checks start a fix run once every check has finished, at most one for each head of the pull request and two since a person last acted; then the task stops as `ci_failed`. A check that was cancelled counts as failed, one that was skipped or neutral as passed.
3. Reviewers' requests ([who may make one](security.md)) that no run was given start a follow-up run for them, at most three since a person last acted; then the task stops as `changes_requested`.
4. Checks that have not finished are waited for until `ci_timeout_minutes` (120 when unset) after CAVU's newest push, its ready-marking or a person's newest action, whichever came last; then the task stops as `ci_timeout`.
5. When no check has started on a project without `no_ci`, CAVU waits 3 minutes, marks the pull request ready for review in case the repository's CI skips a draft, waits 3 minutes more, and then stops the task as `no_checks`. CAVU marks a task's pull request ready only once and never forgets it: after that, 3 minutes with no check stop the task.
6. When the checks passed, or none started on a project with `no_ci`, CAVU marks the draft ready for review, once.
7. Then the pull request waits for a person to merge it.

A fix run starts from the pull request's newest commit and goes on with the task's agent session, with no readiness turn and no review. A fix run on failed checks gets what each failed check reported, for an Actions job the end of its log, and a follow-up run gets each request with its author and the file and line it is on. As in any run, its change must pass the verify command unless the project waives it, and its commits go on top of the pull request's, never in their place: a session whose work does not descend from the commit it started at fails, and no push is forced. Once a follow-up run's work is over, its commits pushed or the code left as it is, CAVU posts one comment on the pull request under the account of the token it holds (for a GitHub App, the App's bot): the task's id, the agent's summary, which the agent is told to begin with what it changed for each request, and whether it changed the code. CAVU writes inside no review thread and resolves none; a reviewer's next request, a new review that requests changes or a new comment in an unresolved thread, starts the next follow-up run. For the fix runs, CAVU keeps the task's home volume, which holds the agent's session, until the task is done or cancelled, and removes it then. `cavu task cancel ID` leaves the pull request open, and CAVU no longer watches it.

Each stop of the pull request explains itself and clears with `cavu task retry ID`, which gives the task its caps again. A retry of `push_rejected` pushes again. A retry of any other is taken up at the next look, unless, for a stop other than `conflict` or `taken_over`, a look has shown a commit on the branch that CAVU did not push, which then stops the task as `taken_over` at once; a retry of `conflict` or `taken_over` gives CAVU the commit that the next look shows at the head of the branch.

- `push_rejected`: GitHub refused the push of the task's branch, or refused a GitHub App a token for it, or the branch's name and its `-2` to `-9` are all taken; the explanation quotes the reason. Fix the cause and retry: CAVU kept the run, and the retry pushes from it.
- `ci_failed`: the checks still fail after the fix runs, and the explanation names the failed ones. A retry allows two more fix runs.
- `ci_timeout`: the checks did not finish within `ci_timeout_minutes`. A retry waits that long again; `cavu settings set ci_timeout_minutes 0` removes the limit, and a larger number raises it.
- `no_checks`: no check started within 3 minutes of CAVU's newest push, its ready-marking or a person's newest action, after CAVU had marked the pull request ready. A retry waits 3 minutes again. For a repository whose pull requests may start no check, as one with no CI or with CI only for some paths, `cavu project set web --no-ci on` removes the wait: a pull request with no check is then marked ready at once, and checks that do run are still acted on.
- `conflict`: the pull request conflicts with its base branch. CAVU resolves no conflict: resolve it on the pull request's branch and retry, and CAVU goes on from your commit.
- `taken_over`: the branch holds a commit CAVU did not push, such as one a person pushed while no `conflict` stop was open. A retry gives the branch back to CAVU at the commit it then holds; the commits of a run that was open and not yet pushed are dropped. A commit you push while a stop other than `conflict` or `taken_over` is open stops the task as `taken_over` after that stop's retry, and a second retry then gives CAVU your commit.
- `changes_requested`: reviewers still ask for changes after three follow-up runs. A retry allows three more.

A check that fails or never finishes whatever the change, such as a job that never gets a runner, is left out with `cavu project set web --ignore-check NAME`, by its name as GitHub shows it.

## Budgets

A task stops when its sessions and reviews have used `task_max_turns` turns (default 1000) or run for `task_timeout_minutes` minutes (default 300) since a person last started, answered, retried, cancelled or restored it, and each session gets what is left. The stop is `budget` or `timeout`, and `cavu task retry ID` gives the task its whole budget again. The two guard against a runaway, so their defaults are high. A project's cap applies to its own tasks over the setting:

```
cavu settings set task_max_turns 400
cavu project set web --cap task_timeout_minutes=60    # --cap task_timeout_minutes= removes it
```

In key mode money can be capped too, and a money cap needs prices. `model_prices` maps a model id to what your endpoint charges for it, in US dollars per million tokens. One price under `*` prices every model without its own and is enough for a rough cap, for instance your most expensive model's. For an exact cap, price each model by the id a response reports: the dated id, not an alias, and Claude Code's small background model too, which answers under its own id. A subagent can answer under another model than the session's, so with a cap set its model needs a price too; the default under `*` gives it one. With a cap set, a model without a price, or an answer the proxy cannot count, stops the task as `budget`, and the stop names the model or the problem.

```
cavu settings set model_prices '{"*": {"input": 15, "output": 75, "cache_read": 1.5, "cache_write": 18.75}}'
cavu settings set task_budget_usd 5           # what a task may spend since a person last acted
cavu settings set daily_budget_usd 40         # what all tasks may spend in the server's local day
cavu project set web --cap task_budget_usd=0  # no task cap for this project's tasks; the daily cap still holds them
```

There is no money cap unless one is set, and none under a subscription: unset or `0` turns a cap off, and a project's `0` lifts the instance's task cap for its own tasks. A session ends once it spends what the task or the day had left when it began. A task that spent its cap stops as `budget`, and `cavu task retry ID` gives it its whole budget again; once the day spent its cap, tasks wait until the next local midnight. With tasks running side by side, the daily cap can be passed by what the others in flight still spend, at most one task's budget each. `task show` prints what the task spent of its cap and what the day spent of the daily cap.
