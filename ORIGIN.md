# Engineering RFC: Spec-Driven Autonomous Verification (SDAV)

**Author:** Simon Tucker
**Status:** Draft / Proposal
**Target Environment:** Homelab & Personal Systems

## Executive Summary
Traditional code review models (pull requests, static diff inspections, human approvals) introduce disproportionate friction in solo engineering environments, frequently collapsing into rubber-stamped self-merges.

This RFC proposes Spec-Driven Autonomous Verification (SDAV)—a paradigm shift where plain-text behavioral specifications and strict interface definitions replace implementation diffs as the primary reviewable artifact. Code is treated as ephemeral scaffolding synthesized to satisfy these contracts, while an adversarial test agent translates human intent into executable integration suites through phased evolution.

```text
+-------------------------------------------------------------+
|                     Human Focus Zone                        |
|  1. Plain-Text Spec (.md)  +  2. Strict API Schemas (Proto) |
+-------------------------------------------------------------+
                              │
                              ▼
+-------------------------------------------------------------+
|                Test Synthesizer Agent                       |
|   - Slices frontier dependencies                            |
|   - Produces Human-Readable Scenario Cards                  |
|   - Generates executable integration harness                |
|   - Injects counterexamples to prove failure paths          |
+-------------------------------------------------------------+
                              │
                              ▼
+-------------------------------------------------------------+
|                    Implementation Engine                    |
|        (Human or Coding Agent writes implementation)        |
+-------------------------------------------------------------+
                              │
                              ▼
+-------------------------------------------------------------+
|                     Deterministic Gate                      |
|                `go test -v ./tests/...`                     |
|            Green = Direct Merge to `main`                   |
+-------------------------------------------------------------+
```

## 1. System Architecture & Core Concepts

### 1.1 Separation of Concerns
* **The Spec is the Product:** Plain-text Markdown (`specs/*.md`) capturing intent, stages, constraints, and system invariants.
* **The Contract is the Law:** Interface Definition Languages (Protocol Buffers, OpenAPI) defining data layouts, RPC contracts, and error domains.
* **The Code is Scaffolding:** Written by either a human or an agent, implementation code is disposable. If it passes the generated integration tests and preserves invariants, its internal structure requires zero human peer review.

### 1.2 Progressive Frontier Slicing
To prevent test explosion and agent hallucination when tackling complex systems, specs are structured as an acyclic dependency graph across stages. The system only compiles tests for the active frontier stage.

```markdown
# specs/storage.md

## [Stage: Core] Basic Chunk Storage
- Accepts binary chunk stream over gRPC.
- Flushes to append-only local journal.
- Returns sequence offset ACK.

## [Stage: Lifecycle] Compaction (Depends on: Core)
- Triggers compaction when journal exceeds 64MB.
- Merges chunks into immutable block format.
- Evicts journal segments cleanly.
```

### 1.3 Reviewing Behavior Instead of Boilerplate
Humans never review generated test code (`*_test.go`). Instead, the synthesizer produces two high-density artifacts for approval:

**Scenario Cards:**
```text
SCENARIO: Compaction under storage pressure
GIVEN: Journal at 68MB, 3 active read streams
WHEN: Compaction routine executes
THEN: 
  - Block created within 2.0s
  - Active reader latency does not spike > 20%
  - Journal segment deleted
```

**Counterexample Proofs (Mutation Verification):**
The agent verifies its own assertions by running a mutant probe (e.g., omitting the journal deletion logic) and demonstrating that the test suite actually turns red with an expected invariant violation.

## 2. Repository Layout
```text
├── .sdav/
│   ├── config.yaml            # Runner configurations and model endpoints
│   └── prompts/               # System prompts for test synthesis & auditing
├── specs/
│   ├── storage.md             # Markdown behavioral specs (Human owned)
│   └── ingestion.md
├── api/
│   └── v1/
│       └── storage.proto      # IDL interfaces and status contracts (Human owned)
├── tests/
│   └── generated/             # Ephemeral / Agent-managed integration suites
│       └── storage_test.go
├── cmd/
└── internal/                  # Implementation code (Human/Agent owned)
```

## 3. Implementation Plan

### Phase 1: The Contract Foundation (Week 1)
**Objective:** Establish the declarative boundaries.
**Deliverables:**
- Adopt a strict IDL standard (e.g., Protocol Buffers with `buf` linting and breaking-change detection).
- Build a standardized integration harness container setup (e.g., local ephemeral test harnesses via Docker/Podman or local test fixtures).
- Formalize the Markdown specification schema (defining standard headers: `[Stage: Name]`, `Depends on:`, `Invariants:`).

### Phase 2: The Spec-to-Scenario Compiler (Weeks 2–3)
**Objective:** Automate translation of plain-text specs into reviewable Scenario Cards and executable tests.
**Deliverables:**
- Build a CLI tool (`sdav`) with subcommands:
  - `sdav plan <spec.md>`: Parses spec stages, inspects current repo state, and outputs the target frontier along with high-level Scenario Cards.
  - `sdav synth <spec.md>`: Generates Go integration test code targeting the service endpoints for the active stage.
  - `sdav verify-tests`: Executes mutation probes against generated tests to guarantee assertions are non-trivial.

### Phase 3: The Evolutionary Engine & Gatekeeper (Week 4)
**Objective:** Connect the loop into an automated, review-free Git flow.
**Deliverables:**
- Pre-push or CI gate:
  - Ensure API contracts pass compatibility checks (`buf breaking`).
  - Check that active spec stages have matching generated tests.
  - Run the integration test suite.
  - Auto-commit passing code directly to `main`.
- Test Distillation Mechanism: Implement a cleanup routine that prunes ephemeral intermediate tests once a downstream stage has established broader regression coverage.

## 4. Operational Day-in-the-Life Workflow

| Step | Action | Actor | Time Spent |
| ---- | ------ | ----- | ---------- |
| 1 | Define/tweak `storage.proto` and write 15 lines in `specs/storage.md`. | Human | 5–10 min |
| 2 | Run `sdav plan specs/storage.md`. Inspect the generated Scenario Cards. | Human / Tool | 1–2 min |
| 3 | Approve scenario; run `sdav synth`. Harness tests are compiled. | Agent | Automated |
| 4 | Code is implemented to fulfill the contracts (Red -> Green). | Human or Agent | Variable |
| 5 | Deterministic runner confirms green; branch merges to `main`. | CI Engine | Zero-touch |

## 5. Risk Assessment & Mitigations
* **Risk:** Verifier/Synthesizer collusion (test agent writes tests that pass trivially).
  **Mitigation:** Mandatory counterexample probing (mutation injection) before any generated test is admitted to the test directory.
* **Risk:** Test suite latency bloat over time.
  **Mitigation:** Automated test distillation that consolidates micro-checks into end-to-end stage contracts once an evolutionary frontier is cleared.
* **Risk:** Under-specified edge cases.
  **Mitigation:** The `sdav plan` agent acts adversarially, flagging ambiguous boundary conditions (timeouts, concurrency, disconnections) directly into the Scenario Cards for human resolution.