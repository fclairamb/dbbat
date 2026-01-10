.PHONY: dev build serve install clean typecheck rebuild deploy

# Start development server
dev:
	bun run start

# Build for production
build:
	bun run build

# Serve built site locally
serve:
	bun run serve

# Install dependencies
install:
	bun install

# Clean build artifacts
clean:
	rm -rf build .docusaurus node_modules

# Run TypeScript type checking
typecheck:
	bun run typecheck

# Clean and rebuild
rebuild: clean install build

# Deploy to GitHub Pages (manual)
deploy:
	bun run deploy
