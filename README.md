# Speculate

Spec-Driven Autonomous Verification and Development Orchestrator.

`speculate` bridges human intent, formal specifications, and autonomous development agents. It monitors target repositories, evaluates implementation alignment against specifications, synthesizes integration tests (Red phase), and orchestrates autonomous agent environments to implement matching code (Green phase).

---

## The `speculate init` Tool

The `speculate init` command is the repository bootstrapping tool. It automates configuring any target repository (such as [speculate-kv](https://github.com/brotherlogic/speculate-kv)) with the directory structure, GitHub Actions workflows, CODEOWNERS rules, collaborator access, and branch protection rulesets required by Speculate.

```mermaid
flowchart TD
    A["speculate init"] --> B["1. Scaffold Directories<br/>(specs/, proto/, tests/, internal/)"]
    B --> C["2. Provision Workflows & CODEOWNERS<br/>(review-gate, auto-merge, tests, create-pr)"]
    C --> D{"User has Admin Access?"}
    D -- Yes --> E["3. Configure GitHub Repo Settings<br/>(auto-merge, branch deletion)"]
    E --> F["4. Configure Collaborator<br/>(invite @brotherlogic-speculate)"]
    F --> G["5. Git Commit & Push<br/>(chore: initialize speculate project scaffolding)"]
    D -- No --> H["Log notice: admin permissions needed for repo settings/rulesets"]
    H --> G
    G --> I{"User has Admin Access?"}
    I -- Yes --> J["6. Apply Ruleset 'main'<br/>(CODEOWNERS approval, required checks, squash)"]
    I -- No --> K["Target Repository Ready for Autonomous Spec Development"]
    J --> K
```

---

### Prerequisites

1. **Go Toolchain**: Go `>= 1.24` (or using pre-built binary `/bin/speculate`).
2. **Git**: Installed and configured on your system.
3. **GitHub CLI (`gh`) or API Token**:
   - Authenticated via `gh auth login` with `repo`, `admin:repo_hook`, and `admin:org` / ruleset permissions, OR
   - Set the `GH_TOKEN` or `GITHUB_TOKEN` environment variable.

---

### Installation

To use `speculate` from any target repository or machine without cloning the `speculate` codebase, choose one of the following methods:

#### Option 1: Install with `go install` (Recommended)

Install the `speculate` binary directly into your `$GOPATH/bin`:

```bash
go install github.com/brotherlogic/speculate/cmd/speculate@latest
```

Ensure `$(go env GOPATH)/bin` is in your `PATH` (e.g. `export PATH="$PATH:$(go env GOPATH)/bin"`). You can then run `speculate init` from any repository:

```bash
speculate init --help
```

#### Option 2: Run On-The-Fly with `go run`

Execute `speculate init` directly without installing any binary:

```bash
cd /path/to/my-target-repo
go run github.com/brotherlogic/speculate/cmd/speculate@latest init [flags]
```

#### Option 3: Run via Docker (Zero local Go installation required)

Run the official container image from GitHub Container Registry:

```bash
cd /path/to/my-target-repo

docker run --rm \
  -v "$(pwd):/repo" \
  -w /repo \
  -e GH_TOKEN="${GH_TOKEN:-$(gh auth token)}" \
  ghcr.io/brotherlogic/speculate:latest \
  ./speculate init [flags]
```

#### Option 4: Build from Source

```bash
git clone https://github.com/brotherlogic/speculate.git /tmp/speculate
cd /tmp/speculate
go build -o /usr/local/bin/speculate ./cmd/speculate
```

---

### Usage & CLI Flags

Run `speculate init` from within the root of your target repository:

```bash
speculate init [flags]
```

#### Available Options

| Flag | Env Var | Default | Description |
| :--- | :--- | :--- | :--- |
| `--repo` | `TARGET_REPO` | Auto-detected | Target GitHub repository (`owner/name`). Inferred from `git remote get-url origin` if empty. |
| `--dir` | `PROBER_TARGET_DIR` | `.` | Root directory of the target project to initialize. |
| `--token` | `GH_TOKEN`, `GITHUB_TOKEN` | Inferred from `gh` | GitHub Personal Access Token used for API configuration and rulesets. |
| `--collaborator` | - | `brotherlogic-speculate` | Collaborator account to invite with push permissions for automatic tracking. |
| `--force` | - | `false` | Overwrite existing template files without prompting. |
| `--skip-push` | - | `false` | Scaffold files and configure GitHub without creating a Git commit or pushing to remote. |

---

### What `speculate init` Does (Step-by-Step)

When executed, `speculate init` executes the following sequence:

#### 1. Directory Structure Scaffolding
Creates the canonical directory layout expected by the Speculate evaluator and synthesizer:
- `specs/`: Holds Markdown specification files (e.g. `specs/kv.md`) defining stages and requirements.
- `proto/`: Holds Protocol Buffer schemas and service contracts.
- `tests/`: Holds integration tests synthesized by Speculate.
- `internal/`: Application and service implementation code.
- `.github/workflows/`: GitHub Actions workflows.

*(Empty directories are created with `.gitkeep` so Git tracks them immediately.)*

#### 2. Workflow & Template Provisioning
Renders and writes repository governance templates:
- **`.github/CODEOWNERS`**: Assigns the repository `@owner` as the code owner for `/specs/`, `/proto/`, `/api/`, `/tests/`, and `/.github/`.
- **`.github/workflows/review-gate.yml`**:
  - Automatically identifies whether pull requests touch protected contracts or tests.
  - Skips human review for non-protected files (e.g. agent implementation code).
  - Enforces mandatory review approval from `@owner` when specs, contracts, or tests are modified.
  - Automatically re-runs failed review-gate checks when an approving review is submitted.
- **`.github/workflows/auto-merge.yml`**:
  - Automatically enables squash auto-merge on pull requests once required status checks pass.
- **`.github/workflows/tests.yml`**:
  - Runs unit/integration tests (`go test -v ./...`), builds all packages (`go build ./...`), and verifies linter (`go vet ./...`).
- **`.github/workflows/create-pr.yml`**:
  - Automatically opens a pull request against `main` when any non-`main` branch is pushed (if an open PR does not already exist for that branch).

*If existing files differ from the templates, `speculate init` prompts interactively before overwriting (or overwrites immediately if `--force` is passed).*

#### 3. Repository Settings (Admin)
Configures repository behavior via the GitHub API:
- Enables GitHub **Auto-merge** (`--enable-auto-merge`).
- Enables **Automatic branch deletion** upon merge (`--delete-branch-on-merge`).

#### 4. Collaborator Access & Automatic Tracking (Admin)
Invites `@brotherlogic-speculate` (or a custom collaborator specified via `--collaborator`) with `push` permission, allowing the Speculate daemon to automatically accept the invitation, track the repository's lifecycle stage, and coordinate automated workflows.

#### 5. Git Commit & Push
- Stages `.github`, `specs`, `proto`, `tests`, and `internal`.
- Creates a clean Git commit: `chore: initialize speculate project scaffolding`.
- Pushes the commit to `origin HEAD` (skipped if `--skip-push` is set). Executing this step before branch ruleset enforcement ensures that initial repository scaffolding can be pushed directly without triggering pull request or status check blocks.

#### 6. Ruleset Enforcement (Admin)
Creates or updates the `main` branch ruleset on GitHub to enforce:
- **Branch Protection**: Disables branch deletion and non-fast-forward pushes (prevents force-pushing).
- **Linear History**: Requires linear commit history.
- **Pull Request Requirements**: Requires CODEOWNERS review for protected paths; allows squash merging.
- **Required Status Checks**: Requires `test` and `review-gate` checks to pass before merging.
*(If an active ruleset named `main` already exists, `speculate init` temporarily pauses enforcement during the push and re-enforces it here).*

---

### Examples

#### Example 1: Standard Initialization
Navigate to a freshly cloned target repository and initialize it:

```bash
cd /path/to/speculate-kv
speculate init
```

#### Example 2: Non-Interactive / CI Initialization
Initialize a project without interactive prompts, specifying a token explicitly:

```bash
speculate init \
  --dir=/path/to/my-repo \
  --repo=brotherlogic/my-repo \
  --token="$ADMIN_GITHUB_TOKEN" \
  --force
```

#### Example 3: Scaffolding Only (Skip Git Push)
Generate all directories and workflows locally without making a remote commit:

```bash
speculate init --skip-push
```

---

### What to Do After `speculate init`

Once initialized, your repository is ready for autonomous spec-driven development:

1. **Add Your Initial Specification**:
   Create `specs/<feature>.md` defining the service requirements and stages using standard GIVEN / WHEN / THEN structure.
2. **Add Proto Contracts**:
   Create your gRPC schemas under `proto/<service>/v1/<service>.proto`.
3. **Run the Speculate Prober**:
   Verify alignment and synthesize the first test:
   ```bash
   # Via go run (without cloning speculate)
   go run github.com/brotherlogic/speculate/cmd/prober@latest \
     --mode=evaluator \
     --target-repo=https://github.com/<owner>/<repo>

   # Or via Docker
   docker run --rm ghcr.io/brotherlogic/speculate:latest \
     ./prober --mode=evaluator --target-repo=https://github.com/<owner>/<repo>
   ```

---

## Speculate Daemon, Collaborator Tracking & Web UI

Running `speculate daemon` (the default command when invoked with no subcommand) starts both the gRPC server (`SpeculateService` and `ghwebhook.v1.WebhookHandler`) and the HTTP observability and dashboard server.

### Automatic Collaborator Tracking & Real-Time Webhooks (`brotherlogic-speculate`)

When `GH_TOKEN` or `GITHUB_TOKEN` is configured for the `brotherlogic-speculate` account, the daemon runs a background collaborator `Syncer` on startup and on every `--sync-interval` (`SYNC_INTERVAL`, default `5m`):

1. Automatically accepts pending GitHub collaboration invitations sent by `speculate init`.
2. Discovers all active collaborator repositories and prunes repositories where collaborator access was removed.
3. When `--ghwebhook-addr` (`GHWEBHOOK_ADDR`) is configured, automatically registers each active collaborator repository with `ghwebhook` (advertising `--self-grpc-addr`) and unregisters pruned repositories so incoming `push`, `pull_request`, `pull_request_review`, `check_suite`, and `issues` events trigger immediate asynchronous reconciliation via `ghwebhook.v1.WebhookHandler`.
4. Evaluates each tracked repository across the six Speculate lifecycle stages (`Awaiting Seed`, `Evaluating`, `Awaiting Test Review`, `Aligning`, `Stalled`, and `Aligned`), preserving the last known stage alongside `last_error` if a transient check error occurs.
5. Persists enrollment records to `pstore` (`PSTORE_ADDR`) wrapped with an in-memory `FallbackStore` so tracking and UI reads remain available during `pstore` outages.

### Phase 4 Red-to-Green Orchestrator (`pkg/orchestrator`)

The `pkg/orchestrator.Reconciler` (integrated with `pkg/enroll.Pipeline`) drives an idempotent, single-flight single-PR Red-to-Green lifecycle per scenario (`feat/<scenario-slug> -> main`):

1. **Seed & Stalled Checks**: Verifies `proto/*.proto` and `specs/*.md` on `main` (auto-closing `speculate-seed` once present), honors `speculate-stalled` by tearing down active containers (`DCM.Down`), and resets strike counters to `0` when the stalled label is removed or the stalled issue is closed.
2. **PR-Based Alignment Badges**: Writes updated alignment badges onto `feat/<scenario-slug>` and opens an idempotent `chore/readme-badge` PR when `100%` aligned with a stale badge on `main` (never committing directly to protected `main`).
3. **Red Phase (`EVALUATING` -> `AWAITING_TEST_REVIEW`)**: Synthesizes a Scenario Card (`[Speculate Loop]`), provisions a `DCM.Up` container (`HARNESS_ANTIGRAVITY`) to author a compiling-but-RED test (`compile=success`, `test=failure`), tears down the container on verification, and retries up to 3 strikes (`Down` + `WaitForGone` + `Up`) on failures or 30m inactivity timeouts.
4. **Green Phase (`AWAITING_TEST_REVIEW` -> `ALIGNING` -> `ALIGNED`)**: On `@<owner>` `APPROVED` review (`approved_test_sha`), closes `[Speculate Loop]`, opens `[Speculate Align]`, launches a fresh `DCM.Up` session to implement `internal/**`, resets the branch on any post-approval edits to protected paths, and squash-merges + cleans up the branch once `compile`, `test`, and `review-gate` all succeed.

### Daemon Options

| Flag | Env Var | Default | Description |
| :--- | :--- | :--- | :--- |
| `--http-port` | `PORT`, `HTTP_PORT` | `8080` | HTTP server port (serves `GET /`, `/healthz`, `/status`, `/metrics`). |
| `--grpc-port` | `GRPC_PORT` | `50051` | gRPC port serving `SpeculateService` (`Enroll`, `ListEnrollments`) and `ghwebhook.v1.WebhookHandler` (`ReceiveWebhook`). |
| `--ollama-endpoint` | `OLLAMA_ENDPOINT` | `http://192.168.68.112:11434/v1` | Ollama LLM API endpoint. |
| `--target-repo` | `TARGET_REPO` | `brotherlogic/speculate-kv` | Default target repository reported in `/status`. |
| `--sync-interval` | `SYNC_INTERVAL` | `5m` | Interval between automatic collaborator discovery and evaluation cycles. |
| `--dcm-addr` | `DCM_ADDR` | `""` | Optional `devcontainer-manager` (DCM) gRPC address for autonomous Red/Green sessions. |
| `--dcm-harness` | `DCM_HARNESS` | `antigravity` | DCM agent harness (`antigravity` or `pi`). |
| `--dcm-model` | `DCM_MODEL` | `""` | Optional LLM model override passed to DCM sessions. |
| `--ghwebhook-addr` | `GHWEBHOOK_ADDR` | `""` | Optional `ghwebhook` gRPC address for automatic repository registration/unregistration. |
| `--self-grpc-addr` | `SELF_GRPC_ADDR` | `speculate.speculate.svc:50051` | Speculate gRPC address advertised to `ghwebhook` during registration. |

### Read-Only Web UI Dashboard (`GET /`) & On-Demand `speculate enroll`

- **Web Dashboard (`GET /`)**: Visit `http://localhost:8080/` to view a read-only HTML dashboard listing all tracked repositories, their current lifecycle stage pill (alongside `Attempt <n>/3` during active `Evaluating` or `Aligning` strikes), alignment progress bar, active GitHub issue and PR links, last checked timestamp (UTC and relative), and any inline check error callout.
- **On-Demand Check (`speculate enroll`)**: Trigger an immediate evaluation of a repository via gRPC without waiting for the next background sync tick:
  ```bash
  speculate enroll --daemon-addr=localhost:50051 <owner/repo>
  ```

