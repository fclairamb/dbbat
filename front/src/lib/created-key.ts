import type { CreateAPIKeyResponse } from "@/api";

// A freshly minted API key is shown exactly once. The create page hands it to
// the list page through this in-memory slot, so the secret never lands in the
// URL, the router's history state or storage.
let pending: CreateAPIKeyResponse | null = null;

export function stashCreatedKey(key: CreateAPIKeyResponse): void {
  pending = key;
}

export function peekCreatedKey(): CreateAPIKeyResponse | null {
  return pending;
}

export function clearCreatedKey(): void {
  pending = null;
}
