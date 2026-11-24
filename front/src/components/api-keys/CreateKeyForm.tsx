import { useState } from "react";
import { useCreateAPIKey } from "@/api";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { toast } from "sonner";
import { stashCreatedKey } from "@/lib/created-key";
import { useNavigate } from "@tanstack/react-router";
import { FormPage } from "@/components/shared/FormPage";

export function CreateKeyForm() {
  const navigate = useNavigate();
  const onClose = () => navigate({ to: "/api-keys" });
  const [name, setName] = useState("");
  const [expiresAt, setExpiresAt] = useState("");

  const createKey = useCreateAPIKey({
    onSuccess: (data) => {
      // The secret is shown once, by the list the user lands on: it travels in
      // memory rather than in the URL or in history state.
      stashCreatedKey(data);
      onClose();
    },
    onError: (error) => {
      toast.error(error.message);
    },
  });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    createKey.mutate({
      name,
      expires_at: expiresAt ? new Date(expiresAt).toISOString() : undefined,
    });
  };

  return (
    <FormPage
      testId="new-api-key-page"
      title={<>Create API Key</>}
      description={<>Create a new API key for programmatic access.</>}
      backTo="/servers"
      backLabel="Servers"
      onCancel={onClose}
      onSubmit={handleSubmit}
      submitLabel="Create"
      submitTestId="api-key-submit"
      submitDisabled={createKey.isPending || !name}
    >
      <div className="max-w-2xl space-y-4">
          <div className="space-y-2">
            <Label htmlFor="name">Name</Label>
            <Input
              id="name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="My API Key"
              required
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="expiresAt">Expires At (optional)</Label>
            <Input
              id="expiresAt"
              type="date"
              value={expiresAt}
              onChange={(e) => setExpiresAt(e.target.value)}
            />
          </div>
      </div>
    </FormPage>
  );
}
