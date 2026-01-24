# Documentation Improvements

## Overview

Improve the public documentation website (https://dbbat.com) to include the changelog, reference the demo website (https://demo.dbbat.com), and add screenshots showcasing the application.

## Goals

1. **Discoverability**: Make it easy for users to see what's new and track project progress
2. **Try before install**: Let users experience DBBat without any setup via the demo website
3. **Visual appeal**: Add screenshots to help users understand the UI before installing

## Requirements

### 1. Changelog Page

**Requirement**: Display the project changelog on the website.

#### 1.1 Automatic Sync via Docusaurus Plugin

Use the `@docusaurus/plugin-content-docs` feature to include files from outside the docs directory. This avoids custom scripts and keeps everything declarative.

**File**: `website/docusaurus.config.ts`

Add to the `docs` preset configuration:

```typescript
docs: {
  sidebarPath: "./sidebars.ts",
  editUrl: "https://github.com/fclairamb/dbbat/tree/main/website/",
  // Include CHANGELOG.md from repo root
  include: ["**/*.md", "**/*.mdx"],
},
```

**File**: `website/docs/changelog.md`

Create a wrapper file that imports the root changelog:

```mdx
---
sidebar_position: 100
title: Changelog
description: Release history and version changes
---

import Changelog from '../../CHANGELOG.md';

<Changelog />
```

This approach:
- Keeps the changelog in sync automatically (no build scripts)
- Works with hot reload during development
- Maintains a single source of truth

#### 1.2 Navigation

**Navbar**: Add changelog link in `docusaurus.config.ts`:

```typescript
{
  to: "/docs/changelog",
  label: "Changelog",
  position: "left",
}
```

**Footer**: Add to the "More" section:

```typescript
{
  label: "Changelog",
  to: "/docs/changelog",
}
```

### 2. Demo Website Reference

**Demo URL**: https://demo.dbbat.com

#### 2.1 Homepage Call-to-Action

Add a "Try Demo" button in the homepage header, next to "Get Started" and "View on GitHub".

**Design**:
```
┌────────────────────────────────────────────────────────────┐
│                        DBBat                               │
│   Give (temporary) accesses to prod databases...           │
│                                                            │
│   [Get Started]  [Try Demo]  [View on GitHub]              │
│                                                            │
└────────────────────────────────────────────────────────────┘
```

**Button styling**: Primary style with distinguishing color (e.g., outline variant or accent color).

#### 2.2 Demo Credentials Banner

Add a small info text below the "Try Demo" button or as tooltip:

**Text**: "Login: admin / admin"

#### 2.3 Introduction Page

Add a "Try the Demo" section to `docs/intro.md`:

```markdown
## Try the Demo

Experience DBBat without any setup. Our demo instance is available at:

**[demo.dbbat.com](https://demo.dbbat.com)**

- Login: `admin` / `admin`
- Data resets periodically
- Explore all features freely
```

#### 2.4 Footer Link

Add demo link to the footer under a new "Resources" section or the existing "More" section.

### 3. Screenshots

#### 3.1 Screenshots to Capture

Capture screenshots of the main UI components in both light and dark modes.

| Screen | Filename | Description |
|--------|----------|-------------|
| Login | `screenshot-login.png` | Login page with demo mode badge |
| Dashboard | `screenshot-dashboard.png` | Main dashboard view |
| Users List | `screenshot-users.png` | User management page |
| User Detail | `screenshot-user-detail.png` | Single user view with grants |
| Databases | `screenshot-databases.png` | Database configuration list |
| Database Detail | `screenshot-database-detail.png` | Database connection details |
| Connections | `screenshot-connections.png` | Active/historical connections |
| Queries | `screenshot-queries.png` | Query log with SQL details |
| Grants | `screenshot-grants.png` | Grant management view |

**Resolution**: 1280x800 or similar standard resolution.

**Format**: PNG with reasonable compression.

**Location**: `website/static/img/screenshots/`

#### 3.2 Screenshot Display on Homepage

Add a "Screenshots" or "See it in Action" section to the homepage below features.

**Design options**:

| Option | Description |
|--------|-------------|
| Carousel | Rotating gallery of screenshots |
| Grid | Static grid of 2-3 key screenshots |
| Single hero | One large screenshot with caption |

**Recommended**: Grid layout with 3 key screenshots (Dashboard, Queries, Grants) that link to full-size versions.

#### 3.3 Screenshots in Documentation

Add relevant screenshots to documentation pages:

| Doc Page | Screenshots |
|----------|-------------|
| `docs/intro.md` | Dashboard screenshot |
| `docs/features/user-management.md` | Users list, User detail |
| `docs/features/access-control.md` | Grants screenshot |
| `docs/features/query-logging.md` | Queries screenshot |

**Image markup**:
```markdown
![Dashboard showing recent connections](/img/screenshots/screenshot-dashboard.png)
```

#### 3.4 Screenshot Automation (Optional)

Consider adding Playwright scripts to automatically capture screenshots during E2E tests for consistency and easy updates.

**Location**: `e2e/screenshot-capture.spec.ts`

**Trigger**: Manual or CI job after UI changes.

### 4. Implementation Changes

#### 4.1 Website Configuration Updates

**File**: `website/docusaurus.config.ts`

```typescript
// Navbar items - add Changelog and Demo
items: [
  // ... existing items
  {
    to: "/docs/changelog",
    label: "Changelog",
    position: "left",
  },
  {
    href: "https://demo.dbbat.com",
    label: "Demo",
    position: "right",
  },
]

// Footer - add Resources section
links: [
  // ... existing sections
  {
    title: "Resources",
    items: [
      {
        label: "Changelog",
        to: "/docs/changelog",
      },
      {
        label: "Live Demo",
        href: "https://demo.dbbat.com",
      },
    ],
  },
]
```

#### 4.2 Homepage Updates

**File**: `website/src/pages/index.tsx`

Add:
1. "Try Demo" button in header
2. Screenshots section component
3. Demo credentials tooltip/text

#### 4.3 Changelog Wrapper File

**File**: `website/docs/changelog.md`

```mdx
---
sidebar_position: 100
title: Changelog
description: Release history and version changes
---

import Changelog from '../../CHANGELOG.md';

<Changelog />
```

No build scripts needed - Docusaurus handles the MDX import natively.

#### 4.4 Directory Structure

```
website/
├── static/
│   └── img/
│       └── screenshots/
│           ├── screenshot-dashboard.png
│           ├── screenshot-queries.png
│           ├── screenshot-grants.png
│           └── ...
└── docs/
    └── changelog.md (MDX wrapper importing ../CHANGELOG.md)
```

## Implementation Plan

### Phase 1: Changelog Integration

1. Create `website/docs/changelog.md` MDX wrapper file
2. Update `docusaurus.config.ts` with changelog link in navbar and footer
3. Test build and verify changelog renders correctly

### Phase 2: Demo Integration

1. Update `docusaurus.config.ts` with demo link in navbar and footer
2. Update `website/src/pages/index.tsx` with "Try Demo" button
3. Update `docs/intro.md` with demo section

### Phase 3: Screenshots

1. Create `website/static/img/screenshots/` directory
2. Capture screenshots from the demo instance
3. Add screenshots section to homepage
4. Embed screenshots in relevant documentation pages

### Phase 4: Polish

1. Review responsive behavior of screenshots
2. Add alt text and captions to all images
3. Optimize image file sizes
4. Test dark mode appearance

## Future Enhancements

- **Video walkthroughs**: Short demo videos embedded in docs
- **Interactive demos**: Embedded iframe of demo instance
- **Version-specific screenshots**: Screenshots tagged by version for historical docs
- **Animated GIFs**: Short animations showing key workflows
