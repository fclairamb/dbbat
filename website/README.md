# DBBat Website

This website is built using [Docusaurus](https://docusaurus.io/), a modern static website generator.

## Installation

```bash
bun install
```

## Local Development

```bash
bun run start
```

This command starts a local development server and opens up a browser window. Most changes are reflected live without having to restart the server.

## Build

```bash
bun run build
```

This command generates static content into the `build` directory and can be served using any static contents hosting service.

## Deployment

The site is automatically deployed to GitHub Pages when changes are pushed to the `main` branch via GitHub Actions.

Manual deployment:

```bash
bun run deploy
```

## Makefile Commands

```bash
make dev          # Start dev server
make build        # Build for production
make serve        # Serve built site locally
make install      # Install dependencies
make clean        # Remove build artifacts
make typecheck    # Run TypeScript type checking
make rebuild      # Clean + build
make deploy       # Deploy to GitHub Pages (manual)
```
