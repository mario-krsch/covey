# 30 — Project recipes and the running application (live testing)

> **Status: specification, nothing built** (#600). This takes up an earlier, unmerged draft. That draft's central idea was a service stack shared per project, with a template database cloned per agent, and it is not taken up: warm sandboxes already keep an agent's services, database included, between wakes, so most of what sharing would save is not being spent. The parts of the draft that warm sandboxes do not touch are what this document specifies. Sharing is kept at the end as a later option, with the measurement that would justify it.

An agent that tests a project can already run it. Its workplace carries the toolchain ([`16-runner.md`](16-runner.md), *Sandbox images per agent*). The database it needs runs beside the sandbox, from its declaration or from the project's `docker-compose.yml` through `covey/start_services` (*Services beside the sandbox*, 16). With `warm_sandbox` on, which is the case for both the QA and the coding bundle, the container and its services stay up between wakes. They are torn down only after half an hour without work (`warmIdleTTL`, `internal/orchestrator/orchestrator.go`), and caches and the checkout tree survive in the home regardless. A QA agent on a fifteen-minute heartbeat therefore keeps its database across merge requests.

What it does not have is four things, and they are the subject here:

1. **The recipe is one agent's prose.** How a project is installed, started and tested lives on that agent's wiki page: the QA bundle's `testumgebung` skill tells it to `wiki_search` for the page and `wiki_append` to it. Two agents on one repository keep two versions. Neither is reviewed, and neither is in the repository whose build it describes.
2. **Nothing hands the application its configuration.** The services prompt tells the agent to read the README for the variables the project expects (`internal/agents/compile.go`). A value the platform cannot invent, such as an API token or a signing key, has no route at all. The only one left is a human pasting it somewhere the agent reads, a wiki page, in clear text.
3. **A warm database carries the last branch into the next one.** The database that survives between merge requests also keeps the migrations and data of the merge request before. The agent has no way back to a clean state except throwing the project setup away. That is also why it does not try destructive cases.
4. **Some applications have no door.** A Flutter application renders into a canvas, so the browser plugin finds no element to address in it. The way in is the application's own VM service, and the tools that speak it are stdio MCP servers, which run as processes beside the application. The runtime gets exactly one MCP server today, covey's own (`internal/daemon/actionmcp.go`).

## The recipe: `covey.project.yml`

How a project is installed, migrated, started and tested belongs **in the project**, versioned with its code and reviewed like any other change (design principle 5). Not in an agent's memory, where it is invisible, unreviewed, and true for one agent only.

```yaml
version: 1
name: acme-portal

# Either the project's own stack …
compose: docker-compose.test.yml
# … or a short declaration, in the shape services already have (16):
services:
  - name: db
    image: postgres:16
    env: { POSTGRES_PASSWORD: test }
  - name: redis
    image: redis:7

secrets:                    # NAMES of values the platform cannot invent, never
  - acme_api_token          #   the values themselves — those live in the
                            #   organisation's secret store under these keys
env:                        # what the application is told
  write_to: .env            # a dotenv file in the checkout — or `environment`,
  base: .env.example        #   exported into the processes under `start`
  vars:                     # `base` is the project's OWN file, copied first
    DB_HOST: db
    DB_PASSWORD: test
    ACME_API_TOKEN: "{{ secret.acme_api_token }}"

# A monorepo declares `components:` instead, each with a `path` of its own and
# its own commands/start/services/env block. The two forms are mutually exclusive.

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

mcp:                        # stdio servers beside the runtime (see "Seeing the application")
  marionette:
    command: marionette_mcp

baseline:                   # the known-red initial state
  ref: main
  known_failing:
    - "tests/Feature/LegacyImportTest.php::testOldFormat"
```

What each part is for:

**`services` and `compose`** are not a new mechanism. They are a third source for the services 16 already runs, beside the agent's declaration and the compose content an agent sends. The runner protocol was written expecting it: `StartSandbox.Services` notes that "today the agent declares it, later a project may" (`internal/runner/protocol.go`). Everything 16 says stays true for them unchanged:
- they run per agent, on a network that belongs to the sandbox;
- they are scratch, and live as long as the sandbox;
- they are checked against the same image allowlist;
- the same compose subset is read.

**`env`** is what keeps this technology-agnostic without a covey-invented configuration language. Laravel wants `DB_*`, Spring wants `SPRING_DATASOURCE_URL`, a Flutter client wants an `API_BASE_URL`. The recipe names the variables the stack expects, and the platform writes them where the stack reads them. `write_to` covers both ways a stack takes them: a dotenv file in the checkout, or the process environment of what `start` brings up. `base` names the project's own env file, which is copied first and then overlaid. A common CI step is `mv ./test.env ./.env`, and a recipe that wrote only its own variables would destroy every other setting the project has.

**`secrets:`** closes the last gap between a recipe and a running application. A database the platform starts itself; an API token, a signing key or a licence it cannot. Without this block a recipe ends where a typical Spring application ends: services up, Flyway migrated, Hibernate started, and then "`app.api.token` is not configured", process exited.
- **Names, never values.** They resolve through the route `{{secret:<key>}}` placeholders already take: the daemon asks, and the control plane answers from the store, with the agent's own secret first and an org-wide one only on explicit assignment (`internal/secrets`, `brokerSecret`). Both grant and refusal are recorded.
- **A name the store does not answer makes `env_up` fail**, naming it. The alternative is an application that comes up, serves its health check and refuses every real request: a failure diagnosed three layers from its cause.
- **Where the value ends up.** It reaches the sandbox only inside the rendered environment. That is the exposure the brokered LLM key already has. It is never written into the recipe, the recording or the wiki.

**`commands`** name what CI and humans already type, and **`start` is a map rather than a command**, because the common case is more than one process: a backend with a dev server in front of it. Each entry has a name. The agent starts, reads and stops it under that name through the `dev` plugin's existing `start`/`logs`/`stop`: the recipe names them, the supervisor runs them.

**`app.url` and `app.ready`** give the browser somewhere reliable to point. 16 deliberately leaves readiness to the agent for services, because the agent is the one that wants to talk to them. The application under test is the opposite case: the agent needs to be told it may start clicking.

**`baseline`** answers the question every acceptance asks first and today rediscovers every time: is this test red because of this merge request, or was it red already? Today it is a sentence on a wiki page. Here it is a list at a named ref, versioned with the tests it describes.

**Resolution** is `covey.project.yml`, then `.devcontainer/devcontainer.json` (read for the image, `dockerComposeFile` and `forwardPorts` it already declares), then **nothing**. There is no fourth step that guesses. An `env_up` without a recipe fails with the name of the file it wants, because a guessed setup that half works is what produces the hand-assembled replica of a project the QA bundle's `SOUL.md` forbids.

### Declare what the platform must act on, script what only the project understands

A recipe is not an attempt to describe every project as data. The platform has to understand a small number of things in order to act on them: which services to start, which variables to write, which command is the suite, where the application answers. Everything else is better as a script, and the recipe says so by pointing at one:

```yaml
commands:
  install: ./scripts/covey-setup.sh
```

### Monorepos are the normal case

Real repositories are often monorepos: a Flutter app, an SDK in npm workspaces, a Python service and a JVM backend side by side, or two Laravel applications, a package and an Electron app in one tree. Their CI is typically already cut per component, each job with its own `cd` and its own install. So the recipe follows: a `components:` map, each entry with a `path` and its own commands, processes, services and variables. The flat form stays as the shorthand for a project with one stack. A file that mixes the two is refused.

**The suite and the running application do not have to use the same database.** A Laravel suite on SQLite beside an application on MySQL is common. So `env` applies to what `start` brings up. **`commands.test` runs with whatever the project configures for its tests, and the platform does not touch it.**

### Who writes the recipe

An agent, once, as a merge request.

The discovery work is what the `testumgebung` skill does today: install, build, run the suite once in its unchanged state, note what is red. What changes is where the result goes: into `covey.project.yml` in a merge request, instead of onto one agent's wiki page. A human reviews it like any change. It then applies to every agent, and it lives in the history of the repository it describes.

The agent's first job is to find the answers the project already has rather than derive them again, in this order:

| Source | What it gives, and why it is trustworthy |
|---|---|
| `.gitlab-ci.yml`, `.github/workflows/` | install, build and test commands **that are known to run green** — and, in GitLab CI, a `services:` block that is very nearly this file's own |
| `docker-compose.yml` / `compose.yaml` | the service stack the team already uses locally |
| `.devcontainer/devcontainer.json` | image, compose file, forwarded ports — a standard, already parsed |
| `Makefile`, `Taskfile`, `package.json` scripts | the commands humans on the project actually type |
| the project's own `CLAUDE.md`, `.claude/` | conventions and skills written for exactly this repository |
| `README`, `CONTRIBUTING` | prose, last and least |

The CI configuration comes first because **it is the only one of these that breaks loudly when it goes stale.** Where it and the README disagree, the pipeline is right.

**What setup must not do is add things.** If the project has no end-to-end test runner, that is a finding and a sentence in the merge request, not a Playwright dependency introduced as a side effect. Recording what a project *is* and changing what it *has* are two different merge requests, and only the first one is setup.

**The seed is a decision made once.** For a real project, "run the seeders" is rarely a complete answer: with dozens of them, some build a tenant, some grant one permission, some backfill a migration. The preference runs from the project's own seeders (or a named subset), to a seeder written for the purpose and committed to the project, to a SQL fixture applied afterwards for the state no seeder produces. Production data, sanitised or otherwise, is not on the list: it would put personal data into sandboxes and recordings.

## The actions

Three `covey/` actions, beside `covey/start_services`. They are not a plugin, and that is not a matter of taste. A plugin's ACCESS.md scopes only trim what its prompt documentation shows (`PromptDocForScopes`); the broker asks whether an agent may use a *system*, not which of its actions. The `covey/` actions are the ones whose scope the control plane checks (`internal/orchestrator/hiring.go`). They need the scope that already gates services, `services:write`, because what they do beyond `start_services` writes into the agent's own checkout and its own services and nothing else.

| Action | What it does |
|---|---|
| `covey/env_up {checkout, component?}` | read the recipe in the checkout, start its services through the path `start_services` takes, resolve `secrets:`, write the environment, and report the `start` entries, `app` and `baseline` back |
| `covey/env_status {checkout, component?}` | which services run and since when, what was written where, which secrets were resolved (by name), and the state of a provisioning that has not finished |
| `covey/env_reset {checkout, component?}` | recreate the recipe's services **empty** and write the environment again — the clean state, as one call, and the step after a branch change; the agent then runs `commands.migrate` and `commands.seed` |

**The daemon reads the recipe.** The checkout is in the sandbox, and the daemon is already where `{{secret:…}}` placeholders are resolved before an action runs (`internal/daemon/actionproxy.go`). It parses `covey.project.yml`, sends the service list to the control plane the way the compose route sends its content, asks for the named secrets, and writes the environment into the checkout itself. The control plane learns the service list and the secret names; it never needs the file. The trust is exactly the compose route's: a recipe can name only images the organisation allows, and only secrets the agent is assigned.

**`env_reset` is cheap because the services are scratch.** Recreating a service container under its name is what the runner does already when a name is asked for again (`internal/runner/dockerservices.go`). A reset therefore costs one container start plus the project's own migrate and seed: seconds for a small project, longer for a large seed. Being able to get back costs that much, and it changes what an agent is willing to try: destructive test cases, migrations run forwards and backwards, a seeded state deliberately corrupted to see the error page. The prompt documentation names `env_reset` as the step after switching branch, because a warm database does not know that the branch changed.

**`env_up` has to survive a slow start.** Every daemon request to the control plane runs against a 30-second deadline (`internal/daemon/daemon.go`), and the control plane answers `covey/` requests inline in the job's message loop. A cold image pull exceeds that. `start_services` already has the problem today, so it is fixed there first, as its own issue, and `env_up` inherits the fix: the work starts outside the message loop, the answer is `state: provisioning` naming the step, and `env_status` reports the result. That is the pattern the agent already uses for long-running work with `dev start` and `dev logs`. A warm sandbox, the common case, answers in one call.

**What the application's processes inherit is a precondition.** `commands.install` runs the postinstall scripts of a repository nobody at covey has read, and the environment it writes holds a brokered secret. Processes started by the `dev` plugin are to get the same filtered environment as the runtime's own children (`childEnv`, `internal/daemon/runtime.go`). That is made sure before any of this is built, as its own issue.

## Who may use it

Granted per agent, through the gates that already exist. There is no role-specific code.

1. **Organisation:** the images the recipe names are on the allowlist, and the secrets it names exist and are assigned.
2. **Agent:** `- system: covey scope: services:write` in `ACCESS.md`. Without it the agent gets neither the actions nor their description.
3. **Guard rails:** `covey:env_up` and `covey:env_reset` are subjects like any other. They can be made approval-bound, org-wide or for one agent, outside the runtime and fail-closed.

The QA bundle holds the scope today. The coding bundle gets it too, deliberately: a developer that can bring the application up verifies its change before handing it over, instead of the acceptance finding out a round trip later. Separation of duties stays where it lives: `approve_mr` and `merge_mr` are in the QA agent's access, not the developer's. Running the application is not grading it.

## Seeing the application

**Web** works today: headless Chromium through the `browser` plugin, screenshots in the recording, the important ones attached to the merge request. The recipe adds a reliable target, `app.url` and a passed `app.ready`, instead of a port guessed from a log line. What a live tester needs beyond clicking (console errors, failed requests, performance figures) belongs to the browser plugin and is its own work in the pack.

**Flutter is not the same shape.** A Flutter application renders into a canvas, so the browser plugin finds no element in it. The `html` renderer that used to produce real DOM nodes is gone as of Flutter 3.29. What remains is the application's VM service, which `marionette` and the Dart MCP server speak. Measured for the draft, and worth keeping because it decides the route:

- **On the web target, marionette connects and can do nothing.** The handshake through DWDS succeeds, and then the isolate reports `extensionRPCs: []`. For a Flutter web UI, the route the framework itself tests web with is `integration_test` driven by chromedriver, and that belongs in `commands.e2e`. The browser still sees the page if the application enables its semantics tree. That is a line in the project's test entry point, not a platform feature.
- **On a native Linux target, the same application reports 99 service extensions, 17 of them marionette**, and `ext.flutter.marionette.getVersion` answers. This runs headless, on Xvfb plus Mesa's software rasteriser, with no GPU. Marionette needs its binding in the application's debug entry point; that is the project's decision, recorded in its recipe.

### Servers out of the recipe: `mcp:`

The tools that drive a running application are stdio servers: processes beside the application, not endpoints. That is deliberately not the MCP target system (`internal/target/mcp`), which speaks HTTP to a reachable endpoint and leaves stdio out on purpose. A tool that remote-controls an application inside the sandbox cannot sit on the other side of the sandbox wall.

Today the runtime's MCP document holds one entry, covey's own action proxy, and only when `COVEY_ACTION_MCP` is on (`mcpConfig`, `internal/daemon/actionmcp.go`; [`12-claude-code-adapter.md`](12-claude-code-adapter.md)). Recipe servers need that document to hold more than one entry, whether or not covey's own route is on. Four consequences, all load-bearing:

- **The grant is per agent**, a scope of its own on `dev` (`mcp`). Plugin scopes are not enforced per action, so this one is enforced where it can be: the control plane builds the run's configuration and leaves the servers out for an agent without the scope. It is not folded into `exec`. With `exec` the agent runs one command and the recording shows it. Here the platform starts a server before the first turn, and its tools appear without anyone having named them.
- **Which servers were started goes into the recording**, with the recipe they came from. The grant is per agent, but the list is decided by a file in a repository.
- **The list is fixed when the run starts.** The runtime takes its servers at launch, so a recipe that only arrives during a run counts from the next one. The same holds for a server that `commands.install` installs.
- **A server is spawned without a shell.** Everything it needs has to be on the image `PATH`. A path exported in `.bashrc` reaches `dev exec`, not the runtime. A command that cannot be resolved is left out of the configuration and named in the recording, because the alternative failure is an absence, and nobody investigates an absence.

`start.<name>.control` belongs with this. Flutter prints its VM service URI at startup, and it differs on every run. So a `start` entry may declare a pattern that picks it out of that process's log, and the server is handed the URI instead of an agent being asked to scrape it.

### The desktop workplace

The native route needs an image: GTK, X11, Mesa, Xvfb and the C++ toolchain. The draft measured them at 910 MB on top of the developer image, plus whatever system libraries a project's native plugins pull in. Workplaces are per agent and cut by role (16), so this is one more profile, `dev-flutter-desktop`, built on `dev-flutter`: that role plus a display. It is not part of `make sandbox-images`, because an installation without Flutter projects gets nothing from it. The workplace tests (`internal/sandbox/profiles_test.go`) already require that every tool its description promises is installed in its Dockerfile chain. That is the right check for an image whose whole point is a list of system packages.

**Native mobile is out of scope.** An Android emulator needs `/dev/kvm`. The sandbox gets no devices, and a service that asks for `devices` or `privileged` is refused by design (`internal/sandbox/compose.go`). iOS needs macOS and the Simulator, which no Linux container runs. A Flutter project is tested here on the web target, on the Linux desktop target, and through its widget and integration tests. A real device is a case for a human, or for a device farm the organisation reaches through egress, and the platform says so rather than pretending.

## The honest limits

**A recipe that lies is worse than none.** A declared command that fails is reported with its output, and nothing falls back to guessing.

**A warm database is per agent and lives as long as the sandbox.** After half an hour without work, an image change or a crash, the agent migrates and seeds again. That is the cold path, and this document does not shorten it (see *Later*).

**Not all state is a database.** `env_reset` recreates every service the recipe names. An object store or a search index comes back empty too, and is seeded by `commands.seed` against the whole stack or not at all.

**A project's own compose file is usually not reusable as it stands.** The compose subset 16 reads drops `build:`, host bind mounts and privileged keys service by service. That stays the rule for recipes: a `compose:` file is read the same way, and what it cannot express is reported per service, not guessed.

**Private registries are not solved here.** Projects commonly need credentials before a dependency resolves: an `~/.npmrc` token, a `COMPOSER_AUTH` block, `git:` dependencies into a company GitLab. The draft solved the git half with `GIT_ASKPASS`: git asks a program, the program asks the daemon, and the daemon brokers per call among the systems the agent may use anyway. That construction is right and is its own issue. The npm and Composer half wants a credential in a file, which everything the agent runs can read. A file written for the length of an install and deleted afterwards is the likely answer. It is weaker than the askpass route and should be labelled as such.

**A project cannot depend on another project.** A Flutter client is useless without the backend it talks to, and `services` describes containers, not sibling repositories. For now the client's `env.vars` points at a backend the agent started itself or at a deployed staging one.

## Later: sharing across agents

The draft's main proposal was a service stack per project, shared by every agent on it, with a migrated and seeded template database that each agent clones. The rule behind it was "share what is slow and immutable, isolate what is stateful and cheap". Its measurement, on `postgres:16` against a synthetic schema, is kept here because it is the argument that would bring it back:

| Template | `migrate` + `seed` | `CREATE DATABASE … TEMPLATE` clone |
|---|---|---|
| 15 MB | 228 ms | 203 ms |
| 85 MB | 1.4 s | 241 ms |
| 775 MB | 18.4 s | 972 ms |

For MySQL, which has no `TEMPLATE`, a dump restore into a shared server measured 3.0 s at 99 MB.

It is not taken up now, for three reasons:
- **Warm sandboxes already keep the database between an agent's wakes.**
- **It saves only on the cold path and for a second agent on the same project.** Neither has been measured to matter.
- **It would cost a second kind of service.** That kind would outlive sandboxes, be shared across agents and live on a network per project and runner, which undoes two of the decisions 16 makes deliberately for services.

It comes back if the cold path is measured to cost real time on real projects: how often agents go cold, and what a migrate plus seed costs then. The draft's design is the starting point then: a sealed template (`ALTER DATABASE … WITH is_template = true ALLOW_CONNECTIONS = false`), keyed by the migration directory at the base ref, built by the daemon in the first agent's sandbox. It would not run on the control plane's host, where the draft put it, because sandboxes run on runners now.

A **project object** (a repository as an organisation object, with agents assigned to it and a project view) belongs to the same later step. The recipe in the repository already removes the per-agent prose. What a project object would add is a place to show it and a key to share by, and the second is the part that is not needed yet.

## Build order

| Stage | Contents | Value on its own |
|---|---|---|
| 0 | Preconditions, each its own issue: `dev` processes get a filtered environment; `covey/` requests that outlive the 30-second deadline (fixes `start_services` on a slow pull) | correct on its own; nothing below is safe without the first |
| 1 | `covey.project.yml` parsed and validated in the daemon; `covey/env_up`, `env_status`, `env_reset`; `secrets:` through the existing broker; the checkout answer naming a recipe when there is one (pack); the QA and coding bundles reading the recipe instead of the wiki page, and the QA skill writing it as a merge request | the recipe is one reviewed file instead of per-agent prose; the application gets its configuration; a clean state is one call |
| 2 | `app.ready`; `mcp:` servers beside the runtime and `start.*.control`; the `dev-flutter-desktop` workplace | native Flutter can be operated, not just built |
| 3 | askpass for `git:` dependencies; the file route for npm and Composer credentials, labelled as weaker | private dependencies install without a human |

Stage 1 is the thinnest slice that changes what an agent does: it reads a file instead of a wiki page, and it can get back to a clean state. Stage 0 comes first because stage 1 writes a brokered secret into a tree whose install scripts run with the `dev` plugin's environment.
