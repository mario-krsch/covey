# Live test kit: project recipes (spec/30)

> **Not for merging.** This directory exists to test the branch
> `feature/project-recipes` on a real instance. Drop its commit before the
> branch goes into a pull request.

It covers what the automated tests cannot: a real Docker runner, real Claude
Code, a real browser and a real Flutter window. Two fixture projects are the
targets:

| Fixture | What it is | Deliberate defect | Known red test (baseline) |
|---|---|---|---|
| `fixtures/web-postgres` | Node shop on Postgres: migrations, seeds, an API-token secret, `GET /health` | an empty item name crashes with a 500 and a stack trace | `parses a legacy price without cents` |
| `fixtures/flutter-desktop` | Flutter task list with stable widget keys and the marionette binding | an empty task title is accepted | `an empty title is refused` |

Both were run locally before this commit: the web shop against `postgres:16`
the way `env_up` sets it up (migrate, seed, health 200, empty name → 500), and
the Flutter app in the `dev-flutter-desktop` image, driven by `marionette_mcp`
through the `ws_uri` (connect, list widgets, enter text, tap).

## 1. Build covey and the images from this branch

The images carry `coveyd`, and the new daemon code (recipe actions, askpass,
MCP servers) only exists in this branch. **Build them here; do not pull.**

```sh
git clone -b feature/project-recipes git@github.com:mario-krsch/covey.git
cd covey
make dev-db
make bootstrap                              # web + covey + coveyd
make sandbox-image                          # base: the web fixture, the QA agent
make sandbox-image-dev-flutter-desktop      # dev-flutter + desktop: the Flutter fixture
```

An untagged build takes its images from the published catalogue, which is built
from upstream `main` and has the old `coveyd`. Point the profiles at the images
just built, in `.env` or the environment of `make run`:

```sh
COVEY_SANDBOX_IMAGE_BASE=covey-sandbox:latest
COVEY_SANDBOX_IMAGE_DEV_FLUTTER=covey-sandbox-dev-flutter:latest
COVEY_SANDBOX_IMAGE_DEV_FLUTTER_DESKTOP=covey-sandbox-dev-flutter-desktop:latest
```

Then `make run`. `covey doctor` should report no image problems. Inside a
sandbox, `coveyd version` names the commit of this branch.

## 2. Put the fixtures where an agent can check them out

Create two projects on the GitLab (or GitHub) your instance has as a target
system, e.g. `live-test-web` and `live-test-flutter`, and push each fixture
directory as the root of its repository, on `main`:

```sh
cd live-test/fixtures/web-postgres
git init -b main && git add -A && git commit -m "Live test fixture"
git remote add origin <your forge>/live-test-web.git && git push -u origin main
```

Then open one merge request per project that changes something small (a text,
a price), so the QA agent has something to accept.

## 3. Configure the instance

1. **Workplaces → Services beside the sandbox:** allow `postgres:*`.
2. **Agents:** create the QA agent from the template (`covey QA`) and assign it
   as reviewer of the two merge requests. Its ACCESS.md already has
   `- system: covey scope: services:write`.
3. **Agent secret** for the QA agent: key `demo_api_token`, any value.
4. **For the Flutter test**, a second QA agent (or the same one, switched):
   - workplace `dev-flutter-desktop`;
   - ACCESS.md: `- system: dev scope: exec,processes,agent,mcp`. The `mcp`
     scope is not offered in the interface yet (that is covey-plugin-pack #70,
     not in a release); writing it into ACCESS.md is enough.

## 4. Scenarios

Tick each and note what deviates. The recording of the job (Activity) is
where most of it shows.

### A. Web: set up from the recipe

- [ ] The agent checks out `live-test-web` and calls `covey/env_up`.
- [ ] The answer: `state: ready`, `started: db`, `env.written_to` the `.env`,
      `env.secrets: [demo_api_token]`, `commands`, `start.web.dev_start`,
      `app`, `baseline`, `next`.
- [ ] `.env` in the checkout has `DB_HOST=db` and the token, mode `0600`; the
      token's value appears nowhere in the recording.
- [ ] It runs install, migrate and seed, starts `web` with the `dev_start`
      parameters, and `covey/env_status` reports `app.ready: true`.
- [ ] In the browser it finds the empty-name 500 and reports it with a screenshot.
- [ ] It treats `parses a legacy price without cents` as known red, not as a
      defect of the merge request.

### B. Web: warm database and reset

- [ ] Second merge request on the same project while the sandbox is warm:
      `env_up` answers `kept: [db]` and the seeded data is still there.
- [ ] After switching the branch the agent calls `covey/env_reset`; the
      database is empty, and it migrates and seeds again.

### C. Slow pull

- [ ] Allow an image the runner does not have and that takes longer than 30 s
      to pull (e.g. a large `postgres:*` variant). Remove it from the runner
      first: `docker rmi`.
- [ ] `env_up` or `start_services` answers `state: provisioning` instead of a
      deadline error; `covey/services_status` later says `ready`.

### D. Refusals

- [ ] Delete the agent secret: `env_up` fails naming `demo_api_token`, and
      nothing is written.
- [ ] Remove `services:write` from ACCESS.md: the agent reads nothing about the
      env actions, and a call is refused with the ACCESS.md line it needs.
- [ ] A guard rail with `require_approval` on `covey:env_reset` blocks the reset
      until approved.

### E. Flutter: operate the app natively

- [ ] On `dev-flutter-desktop`: `env_up` on `live-test-flutter`, then install
      (`flutter pub get`, `marionette_mcp` activated into `~/.pub-cache/bin`).
- [ ] It starts `display`, then `desktop`; `covey/env_status` reports
      `control.desktop.uri` and `ws_uri`.
- [ ] **From the next run on**, `marionette` is in the agent's tools, and the
      recording has an `mcp` event with `started: [marionette]`.
- [ ] Connected to the `ws_uri`, the agent adds tasks, finds that an empty
      title is accepted, and takes a screenshot.
- [ ] Without `mcp` in the `dev` scope, the same event lists marionette under
      `left_out`, naming the missing scope.

### F. Private dependencies (if you have a private GitLab project)

- [ ] A fixture with a `git:` dependency on a private project of the GitLab
      the agent has as a target system: the install clones it without anything
      configured, and the recording shows `gitlab:git_credential` with the host.
- [ ] A dependency on a host the agent has no system for: the install fails, and
      the refusal names the hosts that were checked.
- [ ] Optional: a private npm registry via `install_env: { NPM_TOKEN: "{{ secret.<name> }}" }`
      and a committed `.npmrc` with `${NPM_TOKEN}`; `covey/env_install` succeeds,
      and its log shows `[secret]` instead of the token.

### G. No recipe

- [ ] Delete `covey.project.yml` in a copy of the web fixture: `env_up` fails
      naming the file, and the QA agent sets the project up and proposes a recipe
      as a merge request instead of a wiki page.

## 5. Known gaps in this build

- The checkout result does not name the recipe yet (`recipe` field,
  covey-plugin-pack #69), because covey pins the released pack v0.23.0. The agent
  finds `covey.project.yml` at the repository root regardless.
- Mobile targets (Android emulator, iOS) are out of scope by design.

## 6. What to bring back

For each scenario: passed, or what happened instead, with the task or job ID
and the relevant lines of the recording. Problems with the platform go into
issues on `benjaminLedel/covey` (or the pack); with the fixtures, into this
directory.
