# 30 — Projects & environments (live testing)

> **Status: specification, nothing built.** This document takes up an earlier draft that was never merged. The idea stands; the placement does not. Since the draft, services beside the sandbox ([`16-runner.md`](16-runner.md)) and registered runners have landed on `main`, and the draft's own "known expiry date" — an environment on the control plane's host — has arrived. What follows is the draft's design restated against the platform as it is now, with the measurements kept and the parts that no longer fit replaced. Where the draft's code proved something, it is said; where only the draft's text claimed it, that is said too.

An agent that works on code can run that code, but not the application around it. Since [`16-runner.md`](16-runner.md) a sandbox can have services beside it — a `postgres:16` reachable as `db:5432`, taken from the agent's declaration or from the project's own `docker-compose.yml` through `covey/start_services`. That closed the hardest gap: there is somewhere to put a database. It left four others open, and the traces of them are where they were before:

- **Every agent pays the full setup, on every wake.** Services are per agent and scratch by definition: they end with the sandbox, keep no volume, and come up empty. A QA agent accepting a merge request on a project with a few hundred migrations starts a MySQL, migrates and seeds it, tests for ten minutes, sleeps — and does all of it again on the next merge request of the same project. A second agent on the same repository does the same thing beside it.
- **There is no project.** Covey knows agents, an org chart, target systems and a backlog. A project exists only as an unpacked archive under `~/repos/<dir>` in one agent's home (`covey-plugin-pack`, `gitlab/checkout.go`), and how to build, start and test it lives as prose on that agent's wiki page — the QA agent's `testumgebung` skill says so in as many words. Two agents on one repository keep two recipes that drift apart, and every new agent rediscovers `npm ci` at its own expense.
- **Nobody hands the application its configuration.** A database the platform starts is one the application still has to be told about. The services prompt tells the agent to read the README for the variables the project expects (`internal/agents/compile.go`); a value the platform cannot invent — an API token, a signing key — has no route at all, so the only one left is a human pasting it somewhere the agent reads — a wiki page, in clear text.
- **Some applications have no door the agent can use.** A Flutter application renders into a canvas: the browser plugin addresses CSS selectors and finds nothing in it. The way in is the application's own VM service, and the tools that speak it run as processes beside the application — which no runtime configuration in covey can start today.

None of that is the agents being careless. It is what a competent colleague does when the workplace is missing a tool: build the tool badly, on the spot, again on every run.

[D11](07-open-decisions.md) answered "which compiler" — the toolchain in the image, the version in the home. Services answered "where does a database run". What remains is the axis neither covers: **what belongs to the project** — its recipe, its shared and slow state, the configuration its application needs — and how much of it agents on the same project can share without stepping on each other.

## The governing rule

> **Share what is slow and immutable. Isolate what is stateful and cheap.**

The instinct is to share the whole environment per project, and then the objection lands immediately: two agents on two feature branches apply two sets of migrations to one database, and the database is neither. The conclusion drawn from that — "so nothing can be shared", which is where services beside the sandbox stand today — rests on a wrong price.

**Isolating a database is cheap enough, and it gets cheaper the more realistic the data is.** `CREATE DATABASE … TEMPLATE` is a file-level copy of an existing database, not a migration run: its cost follows the template's size rather than the number of migrations, while re-seeding follows both. Measured for the draft on a `postgres:16`, warm, `STRATEGY = FILE_COPY`, median of three, against a synthetic schema of two tables and three indexes:

| Template | `migrate` + `seed` | Clone | Factor |
|---|---|---|---|
| 15 MB (10k users, 20k orders) | 228 ms | 203 ms | 1× |
| 85 MB (100k / 200k) | 1.4 s | 241 ms | 5× |
| 775 MB (1M / 2M) | 18.4 s | 972 ms | 18× |

**The clone is sub-second to about a second** for data volumes worth testing against. Dropping the working database afterwards costs about 90 ms.

**The measurement understates the gain.** It was taken on Docker Desktop for macOS, where storage goes through a VM; and the seed it is compared against is raw `INSERT`s, whereas a real project's `migrate` plus `seed` is a chain of framework migrations and ORM-driven seeders that is slower by a wide margin. Both push the real factor up.

**Below roughly 50 MB the template is not worth having.** At 15 MB cloning and re-seeding cost the same, and a template is then pure apparatus. `template: true` in the recipe is a choice a project makes, not a default the platform imposes.

What costs real time either way is starting the server and filling it the first time. So the server is shared, and the database is cloned.

| Layer | Lifetime | Cost to create | Shared |
|---|---|---|---|
| Service image (`postgres:16`) | until pruned | pulled once per runner | org-wide |
| Project service **container** (the server) | per project and runner, idle-stopped | seconds | every agent on the project on that runner |
| **Template database** (migrated + seeded) | per project × base ref × runner | minutes, once | every agent on the project on that runner |
| **Working database** | per agent and component | a file copy, sub-second to ~1 s | never |
| Dependency caches (`~/.m2`, `~/.gradle`, `~/.npm`, pub cache) | the home's | slow, once | per agent (see limits) |
| Checkout | per branch, bounded by `COVEY_CHECKOUT_KEEP` | seconds | never |
| **The application process** | per waking phase | seconds | **never** |

The branch collision dissolves by construction rather than by coordination: an agent clones the template of **its base ref** and then runs its *own* new migrations on the clone. Two agents adding conflicting migrations never see each other's.

The last row carries as much weight as the rest. "One central setup per project" must not extend to the running application: two agents testing two branches against one process is worse than what exists today. The application is the thing under test; it is per agent, in the agent's own sandbox, as it is now.

**Per agent, not per task — and why that is the honest line.** The draft first said "per task"; its implementation keyed the environment by agent, project and component instead, and it was right to. Two agents on two branches get two databases and cannot collide, which is the property the design exists for. One agent moving from one task to the next keeps its database, and with it the migrations of the task before. That is defensible because an agent works serially ([`03-lifecycle-scheduling.md`](03-lifecycle-scheduling.md)) and because `reset` makes the working database anew in about a second — but it is not automatic: an agent that switches branch has to say so, and the prompt documentation names `reset` as the step after a branch change. A database per task stays open, with a cost attached: somebody would have to collect it afterwards.

## The project

A **project** is a repository the organisation has agents work on: a slug, a target system with the reference into it (a GitLab project id, a GitHub `owner/repo`), a default branch, and the agents assigned to it. It is what a recipe, an environment and a project service stack all hang off, and it is the object the platform is missing. It is an organisation object like an agent or a workflow ([`29-workflows.md`](29-workflows.md)), created by a human in the interface or the API. An agent does not create projects; it is assigned to them.

A checkout is linked to a project by its reference: the agent checks out GitLab project 42, and project 42 is `acme-portal`. Nothing else about checkouts changes — the archive, the synthetic baseline commit, the size limit and the pruning stay as the pack has them.

### The recipe: `covey.project.yml`

How a project is installed, migrated, started and tested belongs **in the project**, versioned with its code and reviewed like any other change (design principle 5). Not in an agent's memory, where it is invisible, unreviewed, and true for one agent only.

```yaml
version: 1
name: acme-portal

# Either the project's own stack …
compose: docker-compose.test.yml
# … or a short declaration covey brings up itself:
services:
  - name: db
    image: postgres:16
    template: true          # keep a migrated + seeded golden database, clone it per agent
    migrations: database/migrations   # what the template is keyed by (below)
  - name: redis
    image: redis:7

secrets:                    # NAMES of values the platform cannot invent, never
  - acme_api_token          #   the values themselves — those live in the
                            #   organisation's secret store under these keys
env:                        # the variable names THIS stack wants; covey fills the values
  write_to: .env            # a dotenv file in the checkout — or `environment`,
  base: .env.example        #   exported into the processes under `start`
  vars:                     # `base` is the project's OWN file, copied first
    DB_CONNECTION: pgsql
    DB_HOST: "{{ service.db.host }}"
    DB_DATABASE: "{{ env.database }}"
    DB_USERNAME: "{{ env.user }}"
    DB_PASSWORD: "{{ env.password }}"
    ACME_API_TOKEN: "{{ secret.acme_api_token }}"

# A monorepo declares `components:` instead, each with a `path` of its own and
# its own commands/start/services block. The two forms are mutually exclusive.

commands:                   # one-shot, each run to completion
  install: composer install --no-interaction && npm ci
  migrate: php artisan migrate --force
  seed:    php artisan db:seed --class=DemoSeeder
  build:   npm run build
  test:    php artisan test
  e2e:     npx playwright test

start:                      # long-running, and there is usually more than one
  api:
    cmd:  php artisan serve --host 0.0.0.0 --port 8000
    port: 8000
  web:
    cmd:  npm run dev -- --host 0.0.0.0 --port 5173
    port: 5173

app:
  url:   http://localhost:5173   # what a person would open
  ready: "GET / 200"             # probed before the agent starts clicking

mcp:                        # stdio servers beside the application (see "Seeing the application")
  marionette:
    command: marionette_mcp

baseline:                   # the known-red initial state
  ref: main
  known_failing:
    - "tests/Feature/LegacyImportTest.php::testOldFormat"
```

Six parts of this earn their place.

**`env.vars` is the reason this stays technology-agnostic** without a covey-invented configuration language. Laravel wants `DB_*`, Spring wants `SPRING_DATASOURCE_URL`, a Flutter client wants an `API_BASE_URL`. The recipe names the variables the stack expects; the platform supplies the values it alone knows — which database was cloned, under which role, on which host. A technology covey has never heard of is a new file in a repository, not a new release of covey. `write_to` covers both ways a stack takes them: a dotenv file written into the checkout, or the process environment of what `start` brings up — Laravel reads `.env`, Spring Boot wants the variables exported.

**`secrets:` closes the last gap between a recipe and a running application.** A database the platform creates itself; an API token, a signing key or a licence it cannot. Without it a recipe ends where a typical Spring application ends: services up, Flyway migrated, Hibernate started — and then "`app.api.token` is not configured", process exited. Names, never values. They resolve through the same broker as every other access, with the same rules (`internal/secrets`): the agent's own secret first, an org-wide one only on explicit assignment. A name the store does not answer makes `up` **fail**, naming it — the alternative is an application that comes up, serves its health check and refuses every real request, a failure diagnosed three layers from its cause. The value reaches the sandbox only as part of the rendered environment, which is the same exposure the brokered LLM key already has; it is never written into the recipe, the recording or the wiki.

**`start` is a map rather than a command.** The common case is not one process: a Laravel backend with a Vite dev server in front of it, a Spring service with a React application beside it. Each entry gets a name, and the agent starts, reads and stops them by that name through the `dev` plugin's existing `start`/`logs`/`stop` — the recipe names them, the supervisor runs them.

**`baseline`** answers the question every acceptance asks first and today rediscovers every time: is this test red because of this merge request, or was it red already? Today that knowledge lives on a wiki page as a sentence; here it is a list, at a named ref, versioned alongside the tests it describes.

**`app.url` and `app.ready`** give the browser somewhere reliable to point. Screenshots as evidence only work if something knows which port the application came up on, and readiness is the one place where the platform's stance on services ("readiness is not answered here", [`16-runner.md`](16-runner.md)) does not carry: for a service the agent is the one that wants to talk to it; for the application under test, the agent is the one that needs to be told it may start clicking.

**`mcp`** is the way in where the browser has none — its own section below.

Where a project already carries a `.devcontainer/devcontainer.json`, it is read for what it already says — image, `dockerComposeFile`, `forwardPorts` — and `covey.project.yml` supplies only the rest. Resolution is `covey.project.yml` → `devcontainer.json` → **nothing**. There is deliberately no fourth step that guesses: an environment for a project without a recipe fails with the name of the file it wants, because a guessed setup that half works is what produces the hand-assembled Laravel project in the first place.

### Declare what the platform must act on, script what only the project understands

A recipe is not an attempt to describe every project as data. The platform has to **understand** a small number of things in order to act on them at all: which service is a database (so that it can build a template and clone it), which command is the test suite, where the application will answer. A shell script is opaque to the control plane — nothing can read `setup.sh` and work out that a template database should be built from it.

Everything else is better as a script, and the recipe says so by pointing at one:

```yaml
commands:
  install: ./scripts/covey-setup.sh
```

### Monorepos are the normal case, not the exception

Real repositories are often monorepos, and a flat schema cannot express them. Two shapes of the kind the draft was checked against:

| Repository | Components | Services in play |
|---|---|---|
| A | Flutter app, a React SDK in npm workspaces, a Python service, a JVM backend | MariaDB, MongoDB, Redis, an object store, a vector database |
| B | Laravel application with hundreds of migrations and dozens of seeders, a PHP package, a second Laravel portal, a Flutter app, an Electron desktop app | MySQL at runtime, **SQLite in the suite** |

Their CI is typically already cut this way — one job per component, each with its own `cd` and its own install. So the recipe follows: a `components:` map, each entry with a `path` and its own commands, processes and services. The flat form stays as the shorthand for a project with one stack; a file that mixes the two is refused.

Two consequences fall out of the table.

**The suite and the running application do not use the same database.** In shape B the tests run on SQLite while the application runs on MySQL. So the environment's variables apply to what `start` brings up — **`commands.test` runs with whatever the project configures for tests, and the platform does not touch it.**

**A project usually already owns its env file.** A common CI step is `mv ./test.env ./.env`. A recipe that rendered only the variables covey knows would destroy every other setting — so `env.base` names the project's own file, which is copied first and then overlaid.

### Who writes the recipe

An agent, once, as a merge request.

The discovery work is what the QA agent's `testumgebung` skill does today — install, build, run the suite once in its unchanged state, note what is red. What changes is where the result goes: into `covey.project.yml` in a merge request instead of onto one agent's wiki page. It is then reviewed by a human like any change, applies to every agent, and lives in the history of the repository it describes.

The agent's first job is to find the answers the project already has rather than to derive them again, in this order:

| Source | What it gives, and why it is trustworthy |
|---|---|
| `.gitlab-ci.yml`, `.github/workflows/` | install, build and test commands **that are known to run green** — and, in GitLab CI, a `services:` block that is very nearly this file's own |
| `docker-compose.yml` / `compose.yaml` | the service stack the team already uses locally |
| `.devcontainer/devcontainer.json` | image, compose file, forwarded ports — a standard, already parsed |
| `Makefile`, `Taskfile`, `package.json` scripts | the commands humans on the project actually type |
| the project's own `CLAUDE.md`, `.claude/` | conventions and skills written for exactly this repository |
| `README`, `CONTRIBUTING` | prose, last and least |

The CI configuration comes first because **it is the only one of these that breaks loudly when it goes stale**. Where it and the README disagree, the pipeline is right.

**What setup must not do is add things.** If the project has no end-to-end test runner, that is a finding and a sentence in the merge request, not a Playwright dependency introduced as a side effect. Recording what a project *is* and changing what it *has* are two different merge requests, and only the first one is setup.

### The seed is a strategy, not a command

`commands.seed` looks like a single line, and for a small project it is. For a real one it is a **decision made once and written down** — a project with dozens of seeders has some that build a tenant, some grant one permission, some backfill a migration. The ladder, in order of preference:

1. **The project's own seeders**, or a named subset of them.
2. **A seeder written for the purpose**, committed to the project — it improves the project rather than only the agents.
3. **Direct database writes** as the last rung: a SQL fixture applied after the seeders, for the state no seeder produces.

Deliberately not on the ladder: production data, sanitised or otherwise (see *Rejected*).

## The environment

An **environment** is what an agent gets in order to run one project (or one component of it): a share of the project's service stack, a database of its own, a role scoped to that database, and the rendered configuration. It belongs to (project, component, agent), and it is cheap, because almost everything in it is already there.

### Two kinds of service, side by side

Services beside the sandbox ([`16-runner.md`](16-runner.md)) stay exactly as they are: per agent, scratch, ending with the sandbox, on a network that hangs off the sandbox. They remain the right answer for "this agent always wants a Redis" and for a compose file sent without a recipe.

A **project service** is the second kind, and it differs in the two places where 16 is deliberately strict:

| | Agent service (16) | Project service (this document) |
|---|---|---|
| Declared by | the agent, or a compose file it sends | the project's recipe |
| Lifetime | the sandbox's | outlives sandboxes; idle-stopped, then removed |
| Volume | none | one named volume, kept across an idle stop |
| Network | per sandbox | per project **on one runner** |
| Shared | never | by every agent on the project on that runner — the server, not the data |
| Image allowlist | the organisation's | the same list, the same two checks |

The arguments 16 gives for its strictness still hold, and the project service meets each rather than waiving it:

- **"Two agents both asking for `db` would put two aliases of the same name into one network."** On a project network there is one `db`, the project's, and an agent holds one environment at a time (serial before parallel). The collision 16 guards against cannot arise; a second environment per agent at once is *not in the first pass* for exactly this reason.
- **"Whatever they hold is scratch by definition."** The template is rebuildable from the recipe and the base ref, and the working database from the template — so nothing a project service holds is something nobody wrote down. It survives an idle stop for speed, not for memory, and it may be removed at any time without loss beyond the minutes to rebuild it. What an agent *keeps* still lives in its home.
- **The image allowlist** is one list for every path; a recipe is a third path onto it, checked at `up` like a compose file is at `start_services`.

### Where it runs: on the runner that holds the sandbox

The draft ran the project stack on the control plane's host, as sibling containers through the same Docker CLI the sandbox provider used. It said itself that this had an expiry date — "as soon as sandboxes run on registered runners, an environment on the control plane's host is on the wrong machine" — and that date has passed.

So a project stack is a **runner-local resource**, brought up through the runner protocol like agent services are, by the same arm (`internal/runner/dockerservices.go`). The consequence is stated rather than hidden: sharing is per project **per runner**. Two agents on the same project on two runners get two servers and two templates. That is the price of keeping the data plane where it is, and it is softened by placement:

**The project joins the affinity ranking.** Today a sandbox is placed by tags, then by `last_runner_id`, then by whether the host holds the image, then by load ([`16-runner.md`](16-runner.md), *Scheduling*). A host that already holds a warm stack for a project of the agent becomes one more preference in that order — after the agent's own last runner, before the image. A preference, never a binding, for the reason 16 gives for every affinity: a binding turns a busy host into a queue.

What does **not** change: the sandbox never gets the Docker socket. It is a general code-execution channel onto the host, out of the egress network and around every guard rail — rejected for the same reason D11 rejected installing packages at runtime.

### The pieces, in the order they are needed

```
   runner host
   ┌─ covey-proj-<project> (docker network, --internal) ───────────┐
   │                                                                │
   │   ┌──────────┐   tmpl_a91c   ← migrated + seeded, sealed       │
   │   │ postgres ├── covey_<agent A>                               │
   │   │          ├── covey_<agent B>                               │
   │   └──────────┘                                                 │
   │   ┌──────────┐                                                 │
   │   │  redis   │                                                 │
   │   └──────────┘                                                 │
   └────────▲───────────────────────▲───────────────────────────────┘
            │                       │
   ┌────────┴──────┐       ┌────────┴──────┐
   │  sandbox A    │       │  sandbox B    │   ← the application runs HERE,
   │  branch fix/9 │       │  branch feat/3│     one process per agent
   └───────────────┘       └───────────────┘
```

**The stack** is per project and runner. It brings the declared services up — or the project's compose file, through the same subset `sandbox.ParseCompose` reads today — on a network of its own, and stops them once nothing has used them for a while. The volume survives that; stopping a stack is a pause, not a reset.

**The working database** is created per agent: on Postgres a clone of the template where there is one, otherwise an empty database the agent migrates and seeds itself with `commands.migrate` and `commands.seed`. The draft's code reached exactly this state — shared server, two agents, two working databases, and the database's own grant table showing one privilege per user — and never built the template. That is the honest starting point, and the build order below keeps it as the first slice.

**The credential** is a database role created with the environment, granted on its own database and nothing else, dropped with the environment. Short-lived and scoped, brokered rather than deposited ([`04-identity-secrets.md`](04-identity-secrets.md)). Two agents on one project share a server and a network; the separation between them is enforced by the database's own grants rather than by the platform remembering to check.

**The template**, where the recipe asks for one, is the last piece and the one with the most open edges.

- It is built by running `commands.migrate` and `commands.seed` against a template database — which needs the project's toolchain, and the toolchain lives in a sandbox, not on the runner. So the template is built **in the sandbox of the first agent that needs it**, by the daemon on the control plane's instruction, not by the agent: the control plane holds an advisory lock for (project, runner, key), tells the daemon to run the two commands in the checkout with the template's connection values, and seals the result. An agent asked to do it would do it differently each time. The commands, their output and the duration go into the recording of the run they happened in.
- It is **keyed by the migration directory at the base ref** (`services.*.migrations` in the recipe), so it is rebuilt exactly when the base branch's migrations move. The checkout is an archive with a synthetic baseline commit (`target.BaselineRef`), so the key cannot come from local git history. The target system has it: the tree id of that path at the base ref is one API call on GitLab and GitHub. Who makes that call — the checkout action reporting it, or the control plane asking the target system — is open; the preference is the checkout, which already holds the brokered credential for that system.
- Not every stack has a migration command. Flyway and Liquibase commonly run inside the application at startup; leaving `commands.migrate` out means "the application migrates itself", and the template is then built by starting the application once against the template database and waiting for `app.ready` before the seed.
- **A finished template is sealed.** Postgres refuses to clone a database that anything is connected to — *"source database is being accessed by other users"* — so a single forgotten `psql` turns every clone on that project into a failure, for everyone. The remedy is the mechanism Postgres provides for exactly this, applied the moment the seed finishes:

  ```sql
  ALTER DATABASE tmpl_a91c WITH is_template = true ALLOW_CONNECTIONS = false;
  ```

  After that nothing can connect to it, cloning still works, and an accidental `DROP` is refused into the bargain. Several agents may clone the same template at the same time; only the template itself has to stay idle.

### The actions

The environment is control-plane work handed to a runner — the stack, the roles, the secrets, the lock — so its actions are **`covey/` actions**, beside `covey/start_services`, and not a plugin. That is not a matter of taste. A plugin's ACCESS.md scopes today only trim what its prompt documentation shows (`PromptDocForScopes`); the broker asks whether an agent may use a *system*, not which of its actions. The `covey/` actions are the ones whose scope is checked on the control plane (`internal/orchestrator/hiring.go`), and a grant that is only a sentence in the prompt is the capability by suggestion this platform argues against everywhere else.

| Action | Scope | What it does |
|---|---|---|
| `covey/env_up` | `env:write` | read the recipe from the agent's checkout, ensure the stack and (if asked for) the template on this runner, create the working database and its role, resolve `secrets:`, render the environment into the checkout, attach the sandbox to the project network — returns the ready environment or `state: provisioning` |
| `covey/env_status` | `env:write` | what is up, since when, which database, which template — and, while provisioning, which step is running |
| `covey/env_reset` | `env:write` | drop the working database and create it again — "make it clean" as one call, and the step after a branch change |
| `covey/env_down` | `env:admin` | drop the database and the role, detach the network; the stack itself is the idle timer's |

**The recipe is read from the checkout, not sent by the agent.** The compose route sends content because "a path would have to be resolved by a control plane that does not reach into sandboxes" ([`16-runner.md`](16-runner.md)). The control plane does reach into **homes**, though — the file tree reads them through the runner link (`home_op`, *File access* in 16), with or without a running sandbox — and that is the door used here: the agent names the checkout, the control plane opens `covey.project.yml` in it at the ref the agent is working on. The difference matters — the recipe is what gets secrets resolved and a template built, and it should be the file that is in the repository, not a text an agent composed.

**`env_up` is asynchronous, and it has to be.** Every daemon request to the control plane runs against a 30-second deadline (`internal/daemon/daemon.go`), and the control plane answers `covey/` requests inline in the job's message loop. A cold `env_up` — pull an image, start a server, build a template — takes minutes. So `env_up` starts the provisioning outside the message loop and returns what it has: a ready environment when everything was warm, otherwise `state: provisioning` with the step in progress. The agent carries on and asks `env_status` again — the pattern it already uses for anything long-running (`dev start` and `dev logs`). The warm case, which is the common one, answers in one call. `covey/start_services` has the same problem today on a slow image pull and is its own issue; the fix for both is the same construction.

`env_reset` deserves its own line. Today an agent that has wrecked its data has no way back except rebuilding the project, which is why it does not try things. A clean slate that costs about a second changes what an agent is willing to do: destructive test cases, migrations run forwards and backwards, a seeded state deliberately corrupted to see the error page.

### Network and trust boundary

Each project gets an `--internal` Docker network on the runner, without a gateway. The sandbox joins it for as long as it holds an environment, as a second network beside the egress one and after its own start — the construction 16 uses for agent services, for the same reason: an internal network brings no default route, so joining it opens no way out. A service container is reachable by name inside the network and by nothing at all from outside it.

Colleagues on one project are not a trust boundary; a stray `DROP TABLE` in the wrong database is a bad afternoon, and the cheapest place to prevent it is the database's own grants.

**What the application's processes inherit is a precondition, not a detail.** `commands.install` runs the postinstall scripts of a foreign repository, and the rendered environment holds a database password and possibly a brokered secret. Processes started by the `dev` plugin are to get the same filtered environment as the runtime's own children (`childEnv`, `internal/daemon/runtime.go`). That is made sure before any of this is built, as its own issue.

## Who may use it

The capability is granted **per agent**, through the gates that already exist. There is no role-specific code and no "QA only" special case.

1. **Organisation** — project services need at least one allowed image, and a project exists.
2. **Agent** — assigned to the project, and `- system: covey scope: env:write` (or `env:admin`) in `ACCESS.md`. Without the line the agent gets neither the actions nor the description of them.
3. **Guard rails** — `covey:env_up`, `covey:env_reset` and the rest are subjects like any other: denied or made approval-bound, org-wide or for one agent, outside the runtime and fail-closed.

| Agent | `covey` scopes | Why |
|---|---|---|
| Developer | `env:write` | verify its own change before handing it over |
| QA | `env:write`, `env:admin` | the acceptance owns the environment's lifetime |
| Everyone else | *(none)* | never learns the capability exists |

**The developer has it deliberately**, and the argument is a cost one. The expensive part of an environment — image, server, template, seed — is paid per project and shared; a second agent on a warm project pays a clone measured in hundreds of milliseconds. Withholding it saves very little, while the loop it closes is the expensive one: without it a developer hands over code it has never seen run. Separation of duties is preserved where it actually lives — `approve_mr` and `merge_mr` are in the QA agent's access and not in the developer's. Running the application is not grading it.

**A budget rather than a prohibition.** The number of environments an agent holds at once is one in the first pass, and the number of project stacks a runner holds is a limit the runner's configuration sets, the same shape as `max_sandboxes`. Project services count against it; agent services today do not count against anything, which is noted in 16 and not repeated here.

## Seeing the application

An environment is only worth its complexity if the agent can look at what it started.

**Web** works today: headless Chromium through the `browser` plugin, screenshots in the recording, the important ones attached to the merge request. What the environment adds is a reliable target — `app.url` and a passed `app.ready` instead of a port guessed from a log line. What a live tester needs beyond clicking (console errors, failed requests, performance figures) belongs to the browser plugin and is its own work in the pack.

**Flutter is not the same shape.** A Flutter application renders into a canvas, so the browser plugin finds no element to address in it — and the `html` renderer that used to produce real DOM nodes is gone as of Flutter 3.29. What remains is the application's own VM service, which `marionette` and the Dart MCP server speak. Measured for the draft, and worth keeping because it decides the route:

- **On the web target marionette connects and can do nothing.** `connect` reaches the VM service through DWDS, the handshake succeeds, and the isolate reports `extensionRPCs: []`. For a Flutter web UI the route the framework itself tests web with is `integration_test` driven by chromedriver, and that belongs in `commands.e2e`. The browser still sees the page if the application enables its semantics tree, which is a line in the project's test entry point, not a platform feature.
- **On a native Linux target the same application reports 99 service extensions, 17 of them marionette**, and `ext.flutter.marionette.getVersion` answers. Headless: Xvfb plus Mesa's software rasteriser, no GPU. Marionette needs its binding in the application's debug entry point; that is the project's decision, recorded in its recipe.

### Servers out of the recipe: `mcp:`

The tools that drive a running application are **stdio** servers: processes beside the application, not endpoints. That is deliberately not the MCP **target system** (`internal/target/mcp`), which speaks streamable HTTP to a reachable endpoint and leaves stdio out on purpose. A tool that remote-controls a running application cannot sit on the other side of the sandbox wall.

Today the runtime gets exactly one MCP server, covey's own action proxy, and only when `COVEY_ACTION_MCP` is switched on (`internal/daemon/actionmcp.go`, [`12-claude-code-adapter.md`](12-claude-code-adapter.md)). Project servers need that document to hold more than one entry, independent of whether covey's own route is on. Four consequences, all load-bearing:

- **The grant is per agent** — a scope of its own on `dev` (`mcp`). And since plugin scopes are not enforced per action, this one is enforced where it can be: the control plane builds the run's configuration and leaves the servers out for an agent without it. Not part of `exec`: with `exec` the agent runs one command and the recording shows it; here the platform starts a server before the first turn and its tools appear without anyone having named them.
- **Which servers were started goes into the recording**, with the project they came from. The grant is per agent, but the list is decided by a file in a repository.
- **The list is fixed when the run starts.** The runtime takes its servers at launch, so a recipe that only arrives during a run counts from the next one. The first run on a new project does not have its MCP tools, and the same applies to a server that `commands.install` installs.
- **A server is spawned without a shell.** Everything it needs has to be on the image `PATH`; a path exported in `.bashrc` reaches `dev exec` and not the runtime. A command that cannot be resolved is left out of the configuration and named in the recording, because the alternative failure is an absence, and nobody investigates an absence.

`start.<name>.control` belongs with this: Flutter prints its VM service URI at startup and it differs on every run, so a `start` entry may declare a pattern that fishes it out of that process's log, and the server is handed the URI instead of an agent being asked to scrape it.

### The desktop workplace

The native route needs an image: GTK, X11, Mesa, Xvfb and the C++ toolchain, measured for the draft at **910 MB** on top of the developer image, plus whatever system libraries a project's native plugins pull in. Since the draft, workplaces are per agent and role-cut ([`16-runner.md`](16-runner.md), *Sandbox images per agent*), so this is one more profile, `dev-flutter-desktop`, built **on `dev-flutter`** — it is that role plus a display, not the union `dev` plus a display. It is not part of `make sandbox-images`: an installation without Flutter projects has nothing from it. The workplace tests (`internal/sandbox/profiles_test.go`) already require that every tool its description promises is installed in its Dockerfile chain, which is the right check for an image whose whole point is a list of system packages.

**Native mobile is out of scope.** An Android emulator needs `/dev/kvm`; the sandbox gets no devices, and a service that asks for `devices` or `privileged` is refused by design (`internal/sandbox/compose.go`). iOS needs macOS and the Simulator, which no Linux container runs. A Flutter project is tested here on the web target, on the Linux desktop target, and through its widget and integration tests. A real device is a case for a human or for a device farm the organisation reaches through egress, and the platform says so rather than pretending.

## Rejected

**The stack on the control plane's host.** The draft's own placement. It worked on one machine and is on the wrong machine as soon as a sandbox runs on a registered runner — the database reached across the network instead of across a bridge, and two copies of the service mechanism, one per host type, to keep correct.

**Services in the sandbox image.** Every agent would carry servers it mostly does not use, without version control per project, and it does nothing about the part that actually costs time, which is seeding.

**The Docker socket in the sandbox.** Maximum flexibility, and the end of the security model. See above.

**A shared application process.** The one thing that genuinely must not be shared.

**Production data as the seed.** It puts personal data into agent sandboxes and session recordings, and makes a sanitisation pipeline a prerequisite of testing. Where fixtures are too thin, the fix is better fixtures.

**A covey-specific environment description language.** Projects already describe themselves — compose files, devcontainers, lock files, CI configuration. The recipe adds what none of them says and reuses the rest.

## The honest limits

**`TEMPLATE` is a Postgres feature.** MySQL and MariaDB have no equivalent, and the projects this is built for run `mysql:8.0` and `mariadb:10.6`. The MySQL path was measured for the draft too, on `mysql:8.0` with the same 100k/200k rows (99 MB of tables, a 51 MB dump):

| Path | Per agent | Note |
|---|---|---|
| Postgres `TEMPLATE` clone | 241 ms | for comparison, same row counts |
| MySQL dump restore into the shared server | **3.0 s** | the dump itself is 385 ms and happens once per template |
| MySQL fresh container from a seeded image | **1.1–1.6 s** | stronger isolation, ~390 MB of writable layer per concurrent agent |

**The decision survives the difference.** Three seconds is an order of magnitude off Postgres and entirely affordable against a run measured in minutes, so MySQL projects get a database per agent like everyone else; the shared artefact is a dump rather than a sealed template, and there is nothing to seal. The figures were taken at 99 MB on Docker Desktop for macOS; a project with ten times the seed data should measure rather than extrapolate.

**A recipe that lies is worse than none.** An environment that cannot carry out a declared command fails with that command and its output, and never falls back to guessing.

**Sharing is per runner.** Two runners, two stacks, two templates. Placement prefers the warm host; it does not guarantee it.

**Not all state is a relational database.** Templates and clones cover Postgres and, through a dump, MySQL. MongoDB, MinIO and Milvus hold state a task can dirty and none of which this mechanism touches. The honest scope: the relational database is isolated per agent; everything else is either reset by `commands.seed` against the whole stack or is knowingly shared, and the environment's status says which.

**A project's own compose file is usually not reusable as it stands.** Real compose files commonly pin `container_name:` and host ports. A host port binding is absolute and `container_name:` overrides the project prefix, so a second project on the same runner collides on both. The platform refuses such a file and names what is in the way — today's compose parser already drops host bind mounts and privileged keys service by service; `container_name:` and published `ports:` join that list for project services.

**Private registries: half solved in the draft, unsolved here.** Projects commonly need credentials before a dependency resolves — an `~/.npmrc` token, a `COMPOSER_AUTH` block, `git:` dependencies into a company GitLab. The draft solved the git half with `GIT_ASKPASS`: git asks a program, the program asks the daemon, the daemon brokers per call among the systems the agent may use anyway. That construction is right and is taken up again, as its own issue. The npm and Composer half wants a credential in a **file**, which everything the agent runs can read — a file written for the length of an install and deleted afterwards is the likely answer, and weaker than the askpass route; it should be labelled as such.

**A shared dependency cache is a channel between agents.** Package managers verify integrity by hash, which is most of the guarantee but not the whole of it. Caches therefore stay in each agent's home, where they are today; a per-project cache is not in the first pass.

**A project cannot depend on another project.** A Flutter client is useless without the backend it talks to, and `services` describes containers, not sibling repositories. For now the client's `env.vars` points at a backend the agent started itself or at a deployed staging one.

## Not in the first pass

- **The template**, sealing and clone — the first slice is a shared server with a working database per agent that the agent migrates and seeds itself.
- **Several environments per agent at once** — serial before parallel ([`03-lifecycle-scheduling.md`](03-lifecycle-scheduling.md)).
- **Checkouts as git worktrees off a shared mirror** — the bigger saving, and a real `.git`, which the archive checkout deliberately lacks. Its own change in the pack.
- **Environments a person can reach** — obviously valuable and obviously a public-exposure question.
- **Snapshots of a working database** ("show me the state that broke").
- **Mobile devices and emulators** — see above.

## Build order

| Stage | Contents | Value on its own |
|---|---|---|
| 0 | Preconditions, each its own issue: `dev` processes get a filtered environment; `covey/` requests that outlive the 30-second deadline (fixes `start_services` on a slow pull as well) | correct on its own; nothing below is safe without the first |
| 1 | The project object, `covey.project.yml` parsed and validated, the recipe read from the checkout through the file tree, a `recipe` note in the checkout answer, the project view in the interface | the setup recipe stops being one agent's private prose — before a single service runs |
| 2 | Project services on the runner: the stack per project and runner, the project network, a working database and role per agent, `secrets:`, the rendered environment, `covey/env_*` asynchronous, placement preference | an agent runs the real application against a real database, and the second agent on a project does not start a second server |
| 3 | The template: the daemon-run build, the key from the base ref, sealing, clone, the MySQL dump path; idle stop and removal with a preview | the second agent on a project starts warm; a reset costs a second |
| 4 | `mcp:` servers beside the runtime, `start.*.control`, `app.ready`; the `dev-flutter-desktop` workplace; askpass for git dependencies; the recipe written by an agent as a merge request | native Flutter joins the web stack; new projects open themselves up |

Stage 1 is worth having on its own, and that is the point of cutting it there: it converts the most expensive recurring waste — every agent rediscovering how a project is built — into a reviewed file, without any of the machinery below it. Stage 2 is where the unknowns are, and stage 0 is where the draft found two of them the hard way.
