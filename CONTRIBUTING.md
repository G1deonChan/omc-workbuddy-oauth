# Contributing to OMC WorkBuddy OAuth

`omc-workbuddy-oauth` is part of the [Oh-My-CPA](https://github.com/WizisCool/oh-my-cpa)
ecosystem. It provides native WorkBuddy and WorkBuddy AI OAuth sign-in, model
discovery, OpenAI-compatible execution, and normalized quota for CLIProxyAPI (CPA).

We welcome issues, feedback, and pull requests. Please adhere to the ecosystem
standards described below.

---

## Local Development

### Prerequisites

- Go 1.27.1+ with `CGO_ENABLED=1`
- `gcc` (or an equivalent C toolchain for c-shared builds)
- Docker (optional, for end-to-end testing with a local CPA container)

### Building and Testing

All dependencies are pre-cached after `go mod download`; tests run without network
access:

```bash
cd go
go mod download

# Run unit tests and static checks offline
GOPROXY=off GOSUMDB=off go test ./...
GOPROXY=off GOSUMDB=off go vet ./...

# Build the native shared library for your current platform
CGO_ENABLED=1 go build -buildmode=c-shared -o ../build/omc-workbuddy-oauth.so .
```

Use `.dylib` on macOS and `.dll` on Windows. `build/` is ignored by git.

---

## Commit Guidelines

This project follows the **Oh-My-CPA Conventional Commits** standard. Commits must
be atomic, focused, and independently verified.

### Subject Format

```text
<type>(<scope>): <imperative subject>
```

- **Types**:
  - `feat`: A new user-facing feature or configuration option.
  - `fix`: A bug fix or behavioral correction.
  - `refactor`: Code change that neither fixes a bug nor adds a feature.
  - `perf`: A code change that improves performance.
  - `test`: Adding missing tests or correcting existing tests.
  - `docs`: Documentation-only changes.
  - `chore`: Maintenance tasks, build tooling, or release preparations.

- **Scopes**:
  - `oauth`: Sign-in flows, build selection, token exchange, and refresh.
  - `models`: Model config discovery, fallback lists, and model info mapping.
  - `executor`: Request shaping, upstream headers, stream pumps, and SSE framing.
  - `quota`: Billing reads and quota normalization.
  - `config`: Plugin configuration fields, parsing, and validation.
  - `registry`: Release packaging and plugin store publication.
  - `readme`: Project overview, documentation links, and branding.
  - `tooling`: CI workflows, build scripts, and release packaging.
  - `release`: Release tags and version bumps.

### Commit Body Structure

Each non-trivial commit body should contain:

1. **Why**: The problem, bug symptom, or user need that prompted the change.
   Reference upstream issues or error messages where applicable.
2. **What changed**: The concrete architectural or behavioral changes made across
   the affected subsystems.
3. **Boundaries / Exclusions**: What was intentionally not changed or left out of
   scope.
4. **Verification**: A clear record of tests run, edge cases exercised, and live
   verification results. Say plainly when something was only tested offline.

#### Example

```text
fix(executor): keep a refusal inside an OK stream on its own status

WorkBuddy can answer HTTP 200 and then carry a refusal in a stream frame, for
example an unapproved-channel code 11128. The pump opened the client stream
first, so that refusal arrived as a 200 that ended in an error frame.

Read up to the first answer event before handing the stream to CPA, and
classify any refusal found there from its own frame, so the client receives
the real status.

Verification:
- Go test suite: TestExecuteStreamRejectsARefusalInsideAnOKStream,
  TestExecuteMapsRefusals.
- Live verification: not run for this change; the channel check needs a real
  account and a refused prompt, so the offline tests stand in for it.
```

---

## Testing & Quality Standards

- **Lowest layer first**: Write regression tests at the lowest possible layer
  (`go/*_test.go`). A bug fix must include a test that reproduces the failure
  without the fix.
- **Offline & hermetic**: Unit tests must pass with `GOPROXY=off GOSUMDB=off`.
  Never make live network calls in unit tests; use `fakeSender` for HTTP and inject
  the stream seams (`readStreamChunk`, `emitStreamFrame`, `openHTTPStream`). A test
  that replaces the seams waits on `pumpFinished` before restoring them, so a pump
  that is still running never races the restore.
- **No flaky sleeps**: Use synchronous channels or dependency injection instead of
  `time.Sleep`.
- **No real accounts**: A live WorkBuddy or WorkBuddy AI login is operator
  verification, not a unit test. Record it in the commit body when you run one, and
  never commit the tokens, enterprise ids or auth files it produces.
- **Two builds, one path**: A change to sign-in, model listing or request building
  should cover both entries, including what differs between them: the international
  build's text-only GLM models, and the China build's Messages-only Claude models.
- **CPA ABI safety**:
  - Catch panics and return `pluginabi.Error` with machine codes and HTTP status.
  - Do not expose raw token bytes, auth directory paths, or upstream network errors
    to client-facing responses.
  - Model IDs for CPA routing must always be lowercase (`strings.ToLower(key)`),
    because CPA registry lookups are case-sensitive.
  - WorkBuddy's own model key goes upstream untouched: the lowercase id is the
    routing name, the catalogue key is what the endpoint expects.

---

## Documentation & Language Rules

- **Code & technical prose**: Code comments, commit messages, and primary technical
  documentation (`README.md`, `docs/USAGE.md`, `CONTRIBUTING.md`, `NOTICE.md`) are
  written in English.
- **Chinese translations**: User-facing documentation provides a Chinese mirror
  (`README.zh-CN.md`). Keep the English and Chinese versions in sync.
- **Domain vocabulary**: Preserve exact upstream and CPA terminology:
  - Provider identifier: `workbuddy`
  - Plugin identifier: `omc-workbuddy-oauth`
  - Build names: `WorkBuddy` (`workbuddy`, the China build), `WorkBuddy AI`
    (`workbuddy-ai`, the international build)
  - Credential fields: `site` (the signing build), `oauth_provider` (the same value)
  - Token names: access token, refresh token
  - Billing terms: credits, extra credits, prepaid balance, plan cycle
- **No unverified claims**: State which artifacts and behaviours were observed, and
  mark anything that was only exercised offline as such.

---

## Security & Secrets

- **Zero credential leaks**: Never commit access or refresh tokens, enterprise ids,
  or `.json` auth files.
- `.workbuddy-oauth-state.key` and auth directories are gitignored. Test fixtures
  must use synthetic ids or an ephemeral `t.TempDir()`.
- The install state key seals login state; do not log it, shorten it, or replace it
  with a value derived from public input.
- Report security issues privately through GitHub Security Advisories or by
  contacting the maintainer directly.
