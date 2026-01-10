# Semantic Release

## Overview

This specification introduces [semantic-release](https://github.com/semantic-release/semantic-release) to automate version management and release publishing based on commit message conventions.

## Current State

### Existing Release Process

The current release workflow (`release.yml`) is triggered manually by pushing a git tag:

```bash
git tag v1.2.3
git push origin v1.2.3
```

This triggers:
1. GoReleaser builds binaries for all platforms
2. Docker images are built and pushed to GHCR
3. Helm chart is packaged and published

### Problems with Manual Tagging

1. **Human error**: Easy to forget, mislabel, or skip versions
2. **Inconsistent changelogs**: Changelog quality depends on who creates the release
3. **No enforcement**: Nothing ensures version bumps follow semantic versioning rules
4. **Coordination overhead**: Requires manual decision-making for each release

## Proposed Solution

Adopt **semantic-release** to automatically:
- Analyze commit messages to determine version bumps
- Generate changelogs from commit history
- Create git tags and GitHub releases
- Trigger the existing release workflow

### How It Works

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         Automated Release Flow                           │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  Developer pushes to main                                                │
│         │                                                                │
│         ▼                                                                │
│  ┌─────────────────┐                                                     │
│  │  CI runs tests  │←── All tests must pass                              │
│  └────────┬────────┘                                                     │
│           │                                                              │
│           ▼                                                              │
│  ┌─────────────────────────┐                                             │
│  │  semantic-release runs  │                                             │
│  │  - Analyze commits      │                                             │
│  │  - Determine version    │                                             │
│  │  - Generate changelog   │                                             │
│  │  - Create git tag       │                                             │
│  │  - Create GH release    │                                             │
│  └────────┬────────────────┘                                             │
│           │                                                              │
│           ▼ (tag created)                                                │
│  ┌─────────────────────────┐                                             │
│  │  release.yml triggers   │←── Existing workflow                        │
│  │  - GoReleaser binaries  │                                             │
│  │  - Docker images        │                                             │
│  │  - Helm chart           │                                             │
│  └─────────────────────────┘                                             │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘
```

## Commit Message Convention

Semantic-release uses [Conventional Commits](https://www.conventionalcommits.org/) to determine version bumps:

### Format

```
<type>(<scope>): <description>

[optional body]

[optional footer(s)]
```

### Types and Version Impact

| Type | Description | Version Bump |
|------|-------------|--------------|
| `fix` | Bug fix | PATCH (0.0.X) |
| `feat` | New feature | MINOR (0.X.0) |
| `feat!` or `BREAKING CHANGE:` | Breaking change | MAJOR (X.0.0) |
| `docs` | Documentation only | No release |
| `style` | Code style (formatting) | No release |
| `refactor` | Code refactoring | No release |
| `perf` | Performance improvement | PATCH |
| `test` | Adding/updating tests | No release |
| `build` | Build system changes | No release |
| `ci` | CI configuration | No release |
| `chore` | Maintenance tasks | No release |

### Examples

```bash
# Patch release (1.0.0 → 1.0.1)
git commit -m "fix(proxy): handle connection timeout gracefully"

# Minor release (1.0.1 → 1.1.0)
git commit -m "feat(api): add endpoint to export audit logs"

# Major release (1.1.0 → 2.0.0)
git commit -m "feat(auth)!: replace JWT with session tokens

BREAKING CHANGE: API authentication now requires session tokens instead of JWTs.
All existing tokens will be invalidated."

# No release (documentation)
git commit -m "docs: update API documentation for grants endpoint"
```

## Configuration

### Package Configuration (package.json)

Create a minimal `package.json` at the repository root for semantic-release:

```json
{
  "name": "dbbat",
  "private": true,
  "devDependencies": {
    "@commitlint/cli": "^19.0.0",
    "@commitlint/config-conventional": "^19.0.0",
    "semantic-release": "^24.0.0",
    "@semantic-release/changelog": "^6.0.0",
    "@semantic-release/git": "^10.0.0"
  }
}
```

### Semantic Release Configuration (.releaserc.yml)

```yaml
branches:
  - main

plugins:
  # Analyze commits to determine version bump
  - "@semantic-release/commit-analyzer"

  # Generate release notes from commits
  - "@semantic-release/release-notes-generator"

  # Update CHANGELOG.md
  - - "@semantic-release/changelog"
    - changelogFile: CHANGELOG.md

  # Commit updated CHANGELOG.md and package.json
  - - "@semantic-release/git"
    - assets:
        - CHANGELOG.md
      message: "chore(release): ${nextRelease.version} [skip ci]\n\n${nextRelease.notes}"

  # Create GitHub release
  - "@semantic-release/github"
```

### Commitlint Configuration (commitlint.config.js)

```javascript
module.exports = {
  extends: ['@commitlint/config-conventional'],
  rules: {
    'scope-enum': [
      2,
      'always',
      [
        'api',
        'auth',
        'config',
        'crypto',
        'db',
        'deps',
        'docs',
        'grants',
        'migrations',
        'proxy',
        'store',
        'ui',
        'release'
      ]
    ],
    'scope-empty': [1, 'never'],  // Warning if scope is missing
    'body-max-line-length': [0],  // Disable body line length limit
  }
};
```

## Workflow Changes

### New Semantic Release Workflow (.github/workflows/semantic-release.yml)

```yaml
name: Semantic Release

on:
  push:
    branches: [main]

permissions:
  contents: write
  issues: write
  pull-requests: write

jobs:
  release:
    runs-on: ubuntu-latest
    # Only run after CI passes
    needs: []  # Add CI job dependencies if in same workflow

    steps:
      - uses: actions/checkout@v6.0.2
        with:
          fetch-depth: 0
          persist-credentials: false

      - uses: oven-sh/setup-bun@v2.1.0

      - name: Install dependencies
        run: bun install

      - name: Run semantic-release
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: bunx semantic-release
```

### Modified CI Workflow

Add commitlint to the CI workflow to validate commit messages on PRs:

```yaml
# In .github/workflows/ci.yml
commitlint:
  runs-on: ubuntu-latest
  if: github.event_name == 'pull_request'
  steps:
    - uses: actions/checkout@v6.0.2
      with:
        fetch-depth: 0

    - uses: oven-sh/setup-bun@v2.1.0

    - name: Install dependencies
      run: bun install

    - name: Validate commit messages
      run: bunx commitlint --from ${{ github.event.pull_request.base.sha }} --to ${{ github.event.pull_request.head.sha }} --verbose
```

### Release Workflow (Unchanged)

The existing `release.yml` continues to work unchanged - it triggers on `v*` tags which semantic-release creates automatically.

## Git Hooks (Optional)

Add a commit-msg hook to validate commits locally before push:

### Husky Configuration

```bash
# Install husky
bun add -D husky

# Initialize husky
bunx husky init

# Add commit-msg hook
echo 'bunx --no -- commitlint --edit "$1"' > .husky/commit-msg
```

This provides immediate feedback when developers write non-conforming commit messages.

## Migration Strategy

### Phase 1: Add Infrastructure (Non-Breaking)

1. Add `package.json` with semantic-release dependencies
2. Add `.releaserc.yml` configuration
3. Add `commitlint.config.js`
4. Add semantic-release workflow (disabled initially)
5. Add commitlint to CI for PRs

### Phase 2: Enable Commit Validation

1. Enable commitlint in CI (warning only initially)
2. Document commit message conventions in CONTRIBUTING.md
3. Optionally add husky for local validation

### Phase 3: Enable Automated Releases

1. Enable semantic-release workflow
2. Remove manual tagging from release process
3. Update documentation

## Branch Strategy

### Supported Branches

| Branch | Release Channel | Example Version |
|--------|-----------------|-----------------|
| `main` | Latest | `1.2.3` |
| `next` (optional) | Next | `1.2.3-next.1` |
| `beta` (optional) | Beta | `1.2.3-beta.1` |

Start with `main` only. Add pre-release branches later if needed.

### Protected Branches

Configure branch protection for `main`:
- Require PR reviews
- Require status checks (CI, commitlint)
- Require linear history (recommended for cleaner changelog)

## Changelog Generation

Semantic-release generates a `CHANGELOG.md` with entries grouped by type:

```markdown
# Changelog

## [1.2.0](https://github.com/fclairamb/dbbat/compare/v1.1.0...v1.2.0) (2026-01-15)

### Features

* **api:** add endpoint to export audit logs ([abc1234](https://github.com/fclairamb/dbbat/commit/abc1234))
* **ui:** add dark mode toggle ([def5678](https://github.com/fclairamb/dbbat/commit/def5678))

### Bug Fixes

* **proxy:** handle connection timeout gracefully ([ghi9012](https://github.com/fclairamb/dbbat/commit/ghi9012))
```

## Integration with GoReleaser

GoReleaser's changelog generation should be disabled since semantic-release handles it:

```yaml
# .goreleaser.yml
changelog:
  disable: true  # Changed from filters
```

The GitHub release body is set by semantic-release with the generated notes.

## Alternatives Considered

### 1. Keep Manual Tagging

**Pros:**
- No new tooling
- Full control over release timing

**Cons:**
- Prone to human error
- No enforced commit conventions
- Manual changelog maintenance

### 2. Release Please (Google)

**Pros:**
- Creates release PRs for review
- Simpler configuration

**Cons:**
- Requires merging release PRs
- Less flexible plugin system
- Golang-centric, less Node.js ecosystem integration

### 3. Standard Version

**Pros:**
- Simpler, npm-only
- No CI integration needed

**Cons:**
- Still requires manual trigger
- No GitHub release creation
- Deprecated in favor of semantic-release

## Files to Create

1. `package.json` - Node.js package configuration
2. `.releaserc.yml` - Semantic release configuration
3. `commitlint.config.js` - Commit message linting rules
4. `.github/workflows/semantic-release.yml` - Automated release workflow
5. `CHANGELOG.md` - Generated changelog (initial empty file)
6. `.husky/commit-msg` (optional) - Git hook for local validation

## Files to Modify

1. `.github/workflows/ci.yml` - Add commitlint job for PRs
2. `.goreleaser.yml` - Disable changelog generation
3. `CLAUDE.md` - Document commit message conventions

## Open Questions

1. **Pre-release branches**: Should we configure `beta` or `next` branches for pre-releases?
2. **Squash merging**: Should PRs be squash-merged to ensure clean commit history?
3. **Release frequency**: Should we add a delay/batching mechanism, or release on every qualifying commit?
4. **Dry run period**: How long should we run semantic-release in dry-run mode before enabling actual releases?

## Implementation Order

1. Create `package.json` and install dependencies
2. Add `.releaserc.yml` and `commitlint.config.js`
3. Add commitlint to CI workflow (for PR validation)
4. Create initial `CHANGELOG.md`
5. Add semantic-release workflow (with `--dry-run` initially)
6. Update `.goreleaser.yml` to disable changelog
7. Test with a few commits to main
8. Remove `--dry-run` to enable automated releases
9. (Optional) Add husky for local commit validation
