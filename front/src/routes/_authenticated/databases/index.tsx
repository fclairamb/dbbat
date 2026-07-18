import { createFileRoute, redirect } from "@tanstack/react-router";

// The Databases page moved to /servers (it now also lists SSH bastions).
// Keep this route around so old links/bookmarks still resolve.
export const Route = createFileRoute("/_authenticated/databases/")({
  beforeLoad: () => {
    throw redirect({ to: "/servers" });
  },
});
