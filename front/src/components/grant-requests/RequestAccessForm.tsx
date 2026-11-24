import { useState } from "react";
import { toast } from "sonner";
import {
  useGrantDefinitions,
  useDatabases,
  useCreateGrantRequest,
} from "@/api";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useNavigate } from "@tanstack/react-router";
import { FormPage } from "@/components/shared/FormPage";

/** Request-access form (the `new` route). */
export function RequestAccessForm() {
  const navigate = useNavigate();
  const onClose = () => navigate({ to: "/grant-requests" });
  const { data: definitions = [] } = useGrantDefinitions({ active_only: true });
  const { data: databases = [] } = useDatabases();

  const [definitionId, setDefinitionId] = useState("");
  const [databaseId, setDatabaseId] = useState("");
  const [justification, setJustification] = useState("");

  const selectedDefinition = definitions.find((d) => d.uid === definitionId);
  const justificationRequired = selectedDefinition?.auto_approve ?? false;

  // The API resolves a definition's server-group scope to the concrete
  // databases currently in scope (scoped_database_uids), so the picker
  // narrows for every requester — not just admins, who are the only ones who
  // could previously read /server-groups to compute this themselves.
  //
  // null/omitted means the definition is unscoped: every database is in
  // scope. A present array — which may be empty — is the resolved set;
  // treating null and [] the same here would be wrong in both directions
  // (hiding every database, or offering every database when none are
  // actually in scope). The server still enforces the scope on submit (403
  // on an out-of-scope pick) — this is a convenience, never the control.
  const scopedDatabaseUids = selectedDefinition?.scoped_database_uids ?? null;
  const selectableDatabases =
    scopedDatabaseUids === null
      ? databases
      : databases.filter((d) => scopedDatabaseUids.includes(d.uid));

  // Switching to a definition that excludes the currently picked database
  // must clear the stale selection rather than submit it.
  if (
    databaseId &&
    !selectableDatabases.some((d) => d.uid === databaseId)
  ) {
    setDatabaseId("");
  }

  const create = useCreateGrantRequest({
    onSuccess: (req) => {
      toast.success(
        req.status === "approved"
          ? "Request submitted and auto-approved — access is active now."
          : "Request submitted"
      );
      onClose();
    },
    onError: (e) => toast.error(e.message),
  });

  const onSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    create.mutate({
      grant_definition_id: definitionId,
      database_id: databaseId,
      justification,
    });
  };

  return (
    <FormPage
      testId="new-grant-request-page"
      title={<>Request access</>}
      description={<>{justificationRequired
              ? "This definition auto-approves requests — access will be active immediately after you submit."
              : "Pick a grant definition and a database. An admin will approve or deny."}</>}
      backTo="/servers"
      backLabel="Servers"
      onCancel={onClose}
      onSubmit={onSubmit}
      submitLabel="Submit"
      submitTestId="grant-request-submit"
      submitDisabled={
              create.isPending ||
              !definitionId ||
              !databaseId ||
              (justificationRequired && !justification.trim())
            }
    >
      <div className="max-w-2xl space-y-4">
          <div className="space-y-2">
            <Label>Definition</Label>
            <Select
              value={definitionId}
              onValueChange={setDefinitionId}
              required
            >
              <SelectTrigger data-testid="grant-request-definition">
                <SelectValue placeholder="Select a definition" />
              </SelectTrigger>
              <SelectContent>
                {definitions.map((d) => (
                  <SelectItem key={d.uid} value={d.uid}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label>Database</Label>
            <Select
              value={databaseId}
              onValueChange={setDatabaseId}
              required
            >
              <SelectTrigger data-testid="grant-request-database">
                <SelectValue
                  placeholder={
                    selectableDatabases.length === 0 && definitionId
                      ? "No database in this definition's scope"
                      : "Select a database"
                  }
                />
              </SelectTrigger>
              <SelectContent>
                {selectableDatabases.map((d) => (
                  <SelectItem key={d.uid} value={d.uid}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="justification">
              Justification {justificationRequired ? "" : "(optional)"}
            </Label>
            <Textarea
              id="justification"
              value={justification}
              onChange={(e) => setJustification(e.target.value)}
              maxLength={1000}
              required={justificationRequired}
              placeholder="Investigating bug X, need read-only access for an hour."
              data-testid="grant-request-justification"
            />
          </div>
      </div>
    </FormPage>
  );
}
