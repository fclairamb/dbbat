import { useState } from "react";
import { useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";

import {
  useInstance,
  useServerGroups,
  useUserGroups,
  useCreateGrantDefinition,
  useUpdateGrantDefinition,
  useValidateGrantDefinitionPatterns,
  type GrantDefinition,
  type CreateGrantDefinitionRequest,
  type UpdateGrantDefinitionRequest,
  type PatternValidationResult,
  type QueryValidationResult,
} from "@/api";
import { FormPage } from "@/components/shared/FormPage";
import { MultiSelect } from "@/components/shared/MultiSelect";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Textarea } from "@/components/ui/textarea";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { autoPriority } from "@/lib/grant-priority";

const GIB = 1024 * 1024 * 1024;

// statementTimeoutPlaceholder shows what an empty field will actually do, so
// "inherit" is never a guess.
function statementTimeoutPlaceholder(resolvedSeconds: number): string {
  return resolvedSeconds > 0
    ? `Inherit (${resolvedSeconds}s)`
    : "Inherit (no limit)";
}

function describeResolvedStatementTimeout(resolvedSeconds: number): string {
  return resolvedSeconds > 0
    ? `currently ${resolvedSeconds}s`
    : "currently no limit";
}

const CONTROLS = [
  { value: "read_only", label: "Read Only" },
  { value: "block_copy", label: "Block COPY" },
  { value: "block_ddl", label: "Block DDL" },
];

// slugify turns free text into a valid definition slug: lowercase,
// alphanumeric segments separated by single hyphens, no leading/trailing
// hyphen. Mirrors the server's ^[a-z0-9]+(-[a-z0-9]+)*$ validation.
function slugify(input: string): string {
  return input
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

/**
 * Create/edit form for a grant definition, shared by `new` and `$uid`.
 * Definitions are immutably versioned: saving an edit creates a successor with
 * a new uid, so both paths return to the list rather than to the old uid.
 */
export function GrantDefinitionForm({
  editing,
  onDone,
}: {
  editing: GrantDefinition | null;
  /** Create mode only: close the dialog the form is rendered in. */
  onDone?: () => void;
}) {
  const navigate = useNavigate();
  const onClose = () =>
    onDone ? onDone() : navigate({ to: "/grant-definitions" });
  const [name, setName] = useState(editing?.name ?? "");
  const [slug, setSlug] = useState(editing?.slug ?? "");
  // "Linked until touched": the slug auto-derives from the name as the
  // operator types, right up until they edit the slug field themselves —
  // at which point it's theirs to manage. An existing definition's slug is
  // already touched from the start, so editing never silently re-derives
  // it from a renamed title.
  const [slugTouched, setSlugTouched] = useState(!!editing);
  const [description, setDescription] = useState(editing?.description ?? "");
  const [durationValue, setDurationValue] = useState<string>(() => {
    if (!editing) return "1";
    const s = editing.duration_seconds;
    if (s % 86400 === 0) return String(s / 86400);
    if (s % 3600 === 0) return String(s / 3600);
    return String(Math.max(1, Math.floor(s / 60)));
  });
  const [durationUnit, setDurationUnit] = useState<"m" | "h" | "d">(() => {
    if (!editing) return "h";
    const s = editing.duration_seconds;
    if (s % 86400 === 0) return "d";
    if (s % 3600 === 0) return "h";
    return "m";
  });
  const [controls, setControls] = useState<string[]>(editing?.controls ?? []);
  // Optional on a definition: empty means "let each materialized grant take
  // the tier its controls earn". Prefilled from the controls until the
  // operator edits it, same "linked until touched" rule as the slug.
  const [priority, setPriority] = useState<string>(() =>
    editing?.priority != null
      ? String(editing.priority)
      : String(autoPriority(editing?.controls ?? []))
  );
  const [priorityTouched, setPriorityTouched] = useState(
    editing?.priority != null
  );
  const [userGroupUids, setUserGroupUids] = useState<string[]>(
    editing?.user_group_uids ?? []
  );
  const [serverGroupUids, setServerGroupUids] = useState<string[]>(
    editing?.server_group_uids ?? []
  );
  // One RE2 pattern per line — the shape operators actually think in.
  const [approvalPatterns, setApprovalPatterns] = useState<string>(
    (editing?.approval_patterns ?? []).join("\n")
  );
  // Sample queries — the pattern-authoring test bench, one statement per
  // line, saved alongside approval_patterns and versioned with it (see
  // GrantDefinition.sample_queries). Not a first-class matcher: these are
  // fixtures to validate the RE2 patterns above against, nothing more.
  const [sampleQueries, setSampleQueries] = useState<string>(
    (editing?.sample_queries ?? []).join("\n")
  );
  const [approverUserGroupUids, setApproverUserGroupUids] = useState<string[]>(
    editing?.approver_user_group_uids ?? []
  );
  // Results of the last "Test patterns" run (manual, or the implicit one on
  // submit). null = never run yet, so the panel stays hidden until the
  // operator asks for it or tries to save.
  const [patternResults, setPatternResults] = useState<
    PatternValidationResult[] | null
  >(null);
  const [queryResults, setQueryResults] = useState<
    QueryValidationResult[] | null
  >(null);
  const { data: instance } = useInstance();
  const resolvedStatementTimeout =
    instance?.resolved_limits?.statement_timeout_seconds ?? 0;
  const { data: groups = [] } = useUserGroups();
  const { data: serverGroups = [] } = useServerGroups();
  const [autoApprove, setAutoApprove] = useState(
    editing?.auto_approve ?? false
  );
  const [maxQueries, setMaxQueries] = useState<string>(
    editing?.max_query_counts != null ? String(editing.max_query_counts) : ""
  );
  const [maxBytesValue, setMaxBytesValue] = useState<string>(() => {
    if (editing?.max_bytes_transferred == null) return "";
    const v = editing.max_bytes_transferred;
    if (v >= 1024 * 1024 * 1024) return String(v / (1024 * 1024 * 1024));
    if (v >= 1024 * 1024) return String(v / (1024 * 1024));
    return String(v / 1024);
  });
  // Three states, and "" is not one of the two obvious ones: "" means
  // *inherit* the instance-wide default, "0" means explicitly no limit, and a
  // positive value is the limit in seconds. That is why the input is a string
  // rather than a number — a number could not tell "empty" from "zero".
  const [statementTimeout, setStatementTimeout] = useState<string>(
    editing?.statement_timeout_seconds != null
      ? String(editing.statement_timeout_seconds)
      : ""
  );
  // Byte soft limit, entered in GiB. Same three states as the statement
  // timeout: "" inherits DBB_SLACK_NOTIFY_BYTES_STEP, "0" turns the notice off
  // for this definition, a positive value is the step.
  const [notifyBytesStep, setNotifyBytesStep] = useState<string>(
    editing?.notify_bytes_step != null
      ? String(editing.notify_bytes_step / GIB)
      : ""
  );
  // Slack channel key: "default" (stored as null), "none", or a configured key.
  const [notifyChannel, setNotifyChannel] = useState<string>(
    editing?.notify_channel ?? "default"
  );
  const notifyChannelKeys = instance?.notify_channels ?? [];
  const [bytesUnit, setBytesUnit] = useState<"KB" | "MB" | "GB">(() => {
    if (editing?.max_bytes_transferred == null) return "MB";
    const v = editing.max_bytes_transferred;
    if (v >= 1024 * 1024 * 1024) return "GB";
    if (v >= 1024 * 1024) return "MB";
    return "KB";
  });

  const create = useCreateGrantDefinition({
    onSuccess: () => {
      toast.success("Definition created");
      onClose();
    },
    onError: (e) => toast.error(e.message),
  });
  const update = useUpdateGrantDefinition({
    onSuccess: () => {
      toast.success("Definition updated");
      onClose();
    },
    onError: (e) => toast.error(e.message),
  });
  const validatePatterns = useValidateGrantDefinitionPatterns({
    onError: (e) => toast.error(e.message),
  });

  // One pattern/query per non-blank line — the shape the two textareas
  // present to the operator. Shared by the manual "Test patterns" button and
  // the implicit check onSubmit runs, so both send the API the exact same
  // arrays.
  const parsedPatterns = () =>
    approvalPatterns
      .split("\n")
      .map((p) => p.trim())
      .filter((p) => p.length > 0);
  const parsedSampleQueries = () =>
    sampleQueries
      .split("\n")
      .map((q) => q.trim())
      .filter((q) => q.length > 0);

  // Runs the validate-patterns preview against whatever is currently typed
  // into the two textareas — not necessarily anything saved yet — and stores
  // the result for the "Test patterns" panel. Returns the pattern results so
  // callers (onSubmit) can decide whether a compile error should block the
  // save without waiting on a second render.
  const runPatternValidation = async (): Promise<
    PatternValidationResult[]
  > => {
    const patterns = parsedPatterns();
    const queries = parsedSampleQueries();

    try {
      const result = await validatePatterns.mutateAsync({
        patterns,
        queries,
      });
      setPatternResults(result.patterns ?? []);
      setQueryResults(result.queries ?? []);

      return result.patterns ?? [];
    } catch {
      // The hook's onError already toasted. Treat as "nothing to report"
      // rather than blocking the save on an infra hiccup unrelated to the
      // patterns themselves — the server still re-validates compile errors
      // at save time regardless.
      return [];
    }
  };

  const toggleControl = (v: string) =>
    setControls((prev) => {
      const next = prev.includes(v)
        ? prev.filter((c) => c !== v)
        : [...prev, v];

      if (!priorityTouched) {
        setPriority(String(autoPriority(next)));
      }

      return next;
    });

  const computedPriority = autoPriority(controls);

  const handleNameChange = (value: string) => {
    setName(value);
    if (!slugTouched) {
      setSlug(slugify(value));
    }
  };

  const handleSlugChange = (value: string) => {
    setSlug(value);
    setSlugTouched(true);
  };

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();

    // Validate patterns against the saved sample bench before saving. A
    // pattern that fails to *compile* blocks the save (the inline error
    // appears in the panel below); a sample that simply doesn't match one
    // does not — see the resolved open questions in
    // specs/todos/2026-08-06-06-sql-control-pattern-query-templates.md. This
    // is also what the API would reject with a generic 400 regardless, but
    // running it here surfaces exactly which pattern is broken instead of a
    // single opaque error banner.
    if (parsedPatterns().length > 0) {
      const results = await runPatternValidation();
      if (results.some((p) => p.error)) {
        toast.error(
          "Fix the invalid approval pattern(s) below before saving."
        );

        return;
      }
    }

    const durationSeconds =
      parseInt(durationValue || "0") *
      (durationUnit === "d" ? 86400 : durationUnit === "h" ? 3600 : 60);

    const unitMult =
      bytesUnit === "GB"
        ? 1024 * 1024 * 1024
        : bytesUnit === "MB"
          ? 1024 * 1024
          : 1024;

    const controlsValue = controls as ("read_only" | "block_copy" | "block_ddl")[];
    const maxQueryCounts = maxQueries ? parseInt(maxQueries) : null;
    const maxBytesTransferred = maxBytesValue
      ? parseInt(maxBytesValue) * unitMult
      : null;
    // Only an explicitly-edited value is pinned on the definition; an
    // untouched field stays null so each materialized grant re-derives the
    // tier from its own controls.
    const priorityValue =
      priorityTouched && priority !== "" ? parseInt(priority) : null;
    // Trimmed before the emptiness test: a field holding only spaces means
    // "inherit", not "parse this as NaN seconds".
    const statementTimeoutValue =
      statementTimeout.trim() === "" ? null : parseInt(statementTimeout, 10);
    const notifyBytesStepValue =
      notifyBytesStep.trim() === ""
        ? null
        : Math.round(parseFloat(notifyBytesStep) * GIB);
    const approvalPatternsValue = parsedPatterns();
    const sampleQueriesValue = parsedSampleQueries();

    if (editing) {
      // PATCH is a partial update: max_query_counts/max_bytes_transferred/
      // priority are themselves nullable, so a bare `null` can't be told
      // apart from an omitted field — the matching clear_* flag is what
      // actually resets them.
      const body: UpdateGrantDefinitionRequest = {
        name,
        slug,
        description,
        duration_seconds: durationSeconds,
        controls: controlsValue,
        max_query_counts: maxQueryCounts ?? undefined,
        clear_max_query_counts: maxQueryCounts === null,
        max_bytes_transferred: maxBytesTransferred ?? undefined,
        clear_max_bytes_transferred: maxBytesTransferred === null,
        // Unlike the quotas above, null and 0 mean different things here
        // ("inherit the global" vs "no limit at all"), so the clear flag is
        // the only way to restore inheritance.
        statement_timeout_seconds: statementTimeoutValue ?? undefined,
        clear_statement_timeout_seconds: statementTimeoutValue === null,
        // Same tri-state as the statement timeout.
        notify_bytes_step: notifyBytesStepValue ?? undefined,
        clear_notify_bytes_step: notifyBytesStepValue === null,
        notify_channel: notifyChannel === "default" ? undefined : notifyChannel,
        clear_notify_channel: notifyChannel === "default",
        priority: priorityValue ?? undefined,
        clear_priority: priorityValue === null,
        auto_approve: autoApprove,
        user_group_uids: userGroupUids,
        server_group_uids: serverGroupUids,
        approval_patterns: approvalPatternsValue,
        sample_queries: sampleQueriesValue,
        approver_user_group_uids: approverUserGroupUids,
      };
      update.mutate({ uid: editing.uid, body });
    } else {
      const body: CreateGrantDefinitionRequest = {
        name,
        slug,
        description,
        duration_seconds: durationSeconds,
        controls: controlsValue,
        max_query_counts: maxQueryCounts,
        max_bytes_transferred: maxBytesTransferred,
        statement_timeout_seconds: statementTimeoutValue,
        notify_bytes_step: notifyBytesStepValue,
        notify_channel: notifyChannel === "default" ? null : notifyChannel,
        priority: priorityValue,
        auto_approve: autoApprove,
        user_group_uids: userGroupUids,
        server_group_uids: serverGroupUids,
        approval_patterns: approvalPatternsValue,
        sample_queries: sampleQueriesValue,
        approver_user_group_uids: approverUserGroupUids,
      };
      create.mutate(body);
    }
  };

  return (
    <FormPage
      dialog={!editing}
      testId={editing ? "edit-definition-page" : undefined}
      title={editing ? editing.name : "New Grant Definition"}
      description={
        <span data-testid="definition-page-description">
          A definition is the shape of a grant, and the only source of one:
          users request access by picking a definition, and admins assign it
          directly.
          {editing && (
            <>
              {" "}
              Saving creates a <strong>new version</strong> — grants already
              issued keep the version they were issued from, so their access
              does not change.
            </>
          )}
        </span>
      }
      backTo="/grant-definitions"
      backLabel="Grant Definitions"
      onSubmit={onSubmit}
      onCancel={onClose}
      submitLabel={editing ? "Save" : "Create"}
      submitTestId="grant-definition-submit"
      submitting={create.isPending || update.isPending}
    >
      <div className="max-w-3xl space-y-4">
          <div className="space-y-2">
            <Label htmlFor="def-name">Name</Label>
            <Input
              id="def-name"
              value={name}
              onChange={(e) => handleNameChange(e.target.value)}
              placeholder="Read-only 1h"
              maxLength={64}
              required
              data-testid="grant-definition-name"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="def-slug">Slug</Label>
            <Input
              id="def-slug"
              value={slug}
              onChange={(e) => handleSlugChange(e.target.value)}
              placeholder="read-only-1h"
              maxLength={64}
              pattern="^[a-z0-9]+(-[a-z0-9]+)*$"
              required
              data-testid="grant-definition-slug-input"
            />
            <p className="text-xs text-muted-foreground">
              Lowercase letters, numbers, and hyphens. Auto-filled from the
              name until you edit it — used to reference this definition
              from the CLI, agents, or scripted API calls instead of its
              uid.
            </p>
          </div>
          <div className="space-y-2">
            <Label htmlFor="def-desc">Description</Label>
            <Input
              id="def-desc"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Standard read access for an hour"
              data-testid="grant-definition-description"
            />
          </div>
          <div className="space-y-2">
            <Label>Duration</Label>
            <div className="flex gap-2">
              <Input
                type="number"
                min="1"
                value={durationValue}
                onChange={(e) => setDurationValue(e.target.value)}
                className="flex-1"
                required
                data-testid="grant-definition-duration-value"
              />
              <Select
                value={durationUnit}
                onValueChange={(v) => setDurationUnit(v as "m" | "h" | "d")}
              >
                <SelectTrigger
                  className="w-28"
                  data-testid="grant-definition-duration-unit"
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="m">Minutes</SelectItem>
                  <SelectItem value="h">Hours</SelectItem>
                  <SelectItem value="d">Days</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
          <div className="space-y-2">
            <Label>Access Controls</Label>
            <div className="space-y-2">
              {CONTROLS.map((c) => (
                <div key={c.value} className="flex items-center gap-2">
                  <Checkbox
                    id={`def-${c.value}`}
                    checked={controls.includes(c.value)}
                    onCheckedChange={() => toggleControl(c.value)}
                  />
                  <Label htmlFor={`def-${c.value}`} className="cursor-pointer">
                    {c.label}
                  </Label>
                </div>
              ))}
            </div>
          </div>
          <div className="space-y-2">
            <Label htmlFor="def-priority">Priority</Label>
            <p className="text-xs text-muted-foreground">
              When a user holds several active grants on the same database, a
              new session is admitted under the highest priority one. Derived
              from the controls above until you edit it — leave it linked and
              every grant from this definition computes its own.
            </p>
            <div className="flex items-center gap-2">
              <Input
                id="def-priority"
                type="number"
                min="-32768"
                max="32767"
                value={priority}
                onChange={(e) => {
                  setPriority(e.target.value);
                  setPriorityTouched(true);
                }}
                className="flex-1"
                data-testid="grant-definition-priority"
              />
              {priorityTouched && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    setPriority(String(computedPriority));
                    setPriorityTouched(false);
                  }}
                  data-testid="grant-definition-priority-reset"
                >
                  Reset
                </Button>
              )}
            </div>
            <p
              className="text-xs text-muted-foreground"
              data-testid="grant-definition-priority-auto-hint"
            >
              auto: {computedPriority}
              {priorityTouched ? " (overridden)" : ""}
            </p>
          </div>
          <div className="space-y-2">
            <Label>Restrict to user groups</Label>
            <p className="text-xs text-muted-foreground">
              Leave empty to let every user request this definition.
            </p>
            <MultiSelect
              options={groups.map((g) => ({ value: g.uid, label: g.name }))}
              selected={userGroupUids}
              onChange={setUserGroupUids}
              placeholder="Every user"
              emptyMessage="No user groups defined yet — this definition applies to every user."
              testId="grant-definition-user-groups"
            />
          </div>
          <div className="space-y-2">
            <Label>Restrict to server groups</Label>
            <p className="text-xs text-muted-foreground">
              Leave empty to allow this definition against every database.
              Membership is live: a server added to one of these groups is
              covered immediately, without editing this definition.
            </p>
            <MultiSelect
              options={serverGroups.map((g) => ({
                value: g.uid,
                label: g.name,
              }))}
              selected={serverGroupUids}
              onChange={setServerGroupUids}
              placeholder="Every database"
              emptyMessage="No server groups defined yet — this definition applies to every database."
              testId="grant-definition-server-groups"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="def-approval-patterns">
              Approval patterns (one per line)
            </Label>
            <p className="text-xs text-muted-foreground">
              Statements matching any of these Go RE2 patterns are suspended
              mid-flight until a second human approves them. Nothing runs until
              then, and a hold has no timeout — it ends on approve, deny, or the
              client giving up. Leave empty for no approval gating.
            </p>
            <Textarea
              id="def-approval-patterns"
              data-testid="grant-definition-approval-patterns"
              rows={3}
              spellCheck={false}
              className="font-mono text-xs"
              placeholder={"(?i)^\\s*DELETE\\s+FROM\n(?i)^\\s*(GRANT|REVOKE)\\b"}
              value={approvalPatterns}
              onChange={(e) => {
                setApprovalPatterns(e.target.value);
                // Stale results would misreport patterns that no longer
                // exist (or exist differently) once the operator keeps
                // typing — hide the panel until "Test patterns" runs again.
                setPatternResults(null);
                setQueryResults(null);
              }}
            />
            {patternResults?.some((p) => p.error) && (
              <ul
                data-testid="grant-definition-pattern-errors"
                className="space-y-1 text-xs text-destructive"
              >
                {patternResults
                  .filter((p) => p.error)
                  .map((p, i) => (
                    <li key={i}>
                      <code className="break-all">{p.pattern}</code>:{" "}
                      {p.error}
                    </li>
                  ))}
              </ul>
            )}
          </div>
          <div className="space-y-2">
            <Label htmlFor="def-sample-queries">
              Sample queries — test bench (one per line)
            </Label>
            <p className="text-xs text-muted-foreground">
              Representative statements saved alongside the patterns above so
              a typo'd pattern is caught here at authoring time instead of
              degrading silently to "no hold" on the proxy. Saved as part of
              this definition and versioned with it. A sample that doesn't
              match doesn't block saving — it's fine to save a bench that's
              currently red while you iterate.
            </p>
            <Textarea
              id="def-sample-queries"
              data-testid="grant-definition-sample-queries"
              rows={3}
              spellCheck={false}
              className="font-mono text-xs"
              placeholder={
                "DELETE FROM users WHERE id = 1\nSELECT * FROM accounts"
              }
              value={sampleQueries}
              onChange={(e) => {
                setSampleQueries(e.target.value);
                setPatternResults(null);
                setQueryResults(null);
              }}
            />
            <Button
              type="button"
              variant="secondary"
              size="sm"
              onClick={() => void runPatternValidation()}
              disabled={validatePatterns.isPending}
              data-testid="grant-definition-test-patterns"
            >
              Test patterns
            </Button>
            {queryResults !== null && (
              <div
                data-testid="grant-definition-test-results"
                className="space-y-2 rounded-md border p-2"
              >
                {queryResults.length === 0 ? (
                  <p className="text-xs text-muted-foreground">
                    No sample queries to test — add one per line above.
                  </p>
                ) : (
                  queryResults.map((r, i) => (
                    <div
                      key={i}
                      data-testid={`grant-definition-test-result-${i}`}
                      className="text-xs"
                    >
                      <div className="flex items-center gap-2">
                        <span
                          className={
                            r.matched
                              ? "font-medium text-amber-600 dark:text-amber-400"
                              : "font-medium text-muted-foreground"
                          }
                          data-testid={`grant-definition-test-result-status-${i}`}
                        >
                          {r.matched ? "HOLD" : "pass-through"}
                        </span>
                        <code className="break-all">{r.normalized}</code>
                      </div>
                      {r.matched && (
                        <p className="pl-14 text-muted-foreground">
                          matched: <code>{r.matched_pattern}</code>
                        </p>
                      )}
                    </div>
                  ))
                )}
              </div>
            )}
          </div>
          <div className="space-y-2">
            <Label>Approver user groups</Label>
            <p className="text-xs text-muted-foreground">
              Members of these user groups may resolve holds on grants built
              from this definition, in addition to admins. Self-approval is
              always rejected.
            </p>
            <MultiSelect
              options={groups.map((g) => ({ value: g.uid, label: g.name }))}
              selected={approverUserGroupUids}
              onChange={setApproverUserGroupUids}
              placeholder="Admins only"
              emptyMessage="No user groups defined yet — admins only."
              testId="grant-definition-approver-user-groups"
            />
          </div>
          <div className="flex items-center gap-2">
            <Checkbox
              id="def-auto-approve"
              checked={autoApprove}
              onCheckedChange={(v) => setAutoApprove(!!v)}
              data-testid="grant-definition-auto-approve"
            />
            <Label htmlFor="def-auto-approve" className="cursor-pointer">
              Auto-approve requests
            </Label>
          </div>
          {autoApprove && (
            <p className="text-xs text-muted-foreground">
              Requests against this definition skip admin review and are
              approved instantly. A justification will be required from
              requesters.
            </p>
          )}
          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-2">
              <Label htmlFor="def-max-q">Max Queries</Label>
              <Input
                id="def-max-q"
                type="number"
                min="1"
                placeholder="Unlimited"
                value={maxQueries}
                onChange={(e) => setMaxQueries(e.target.value)}
                data-testid="grant-definition-max-queries"
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="def-max-b">Max Data Transfer</Label>
              <div className="flex gap-2">
                <Input
                  id="def-max-b"
                  type="number"
                  min="1"
                  placeholder="Unlimited"
                  value={maxBytesValue}
                  onChange={(e) => setMaxBytesValue(e.target.value)}
                  className="flex-1"
                  data-testid="grant-definition-max-bytes"
                />
                <Select
                  value={bytesUnit}
                  onValueChange={(v) =>
                    setBytesUnit(v as "KB" | "MB" | "GB")
                  }
                >
                  <SelectTrigger
                    className="w-20"
                    data-testid="grant-definition-bytes-unit"
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="KB">KB</SelectItem>
                    <SelectItem value="MB">MB</SelectItem>
                    <SelectItem value="GB">GB</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>
          </div>
          <p
            className="text-xs text-muted-foreground"
            data-testid="grant-definition-quota-help"
          >
            Each grant gets one budget, shared by all its sessions at once on
            every database it covers: parallel connections draw on the same
            quota. Sessions on other dbbat instances are seen within about
            five seconds.
          </p>
          <div className="space-y-2">
            <Label htmlFor="def-notify-channel">Slack channel</Label>
            <Select value={notifyChannel} onValueChange={setNotifyChannel}>
              <SelectTrigger
                id="def-notify-channel"
                data-testid="grant-definition-notify-channel"
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="default">Default channel</SelectItem>
                <SelectItem value="none">None (post nothing)</SelectItem>
                {notifyChannelKeys.map((key) => (
                  <SelectItem key={key} value={key}>
                    {key}
                  </SelectItem>
                ))}
                {notifyChannel !== "default" &&
                  notifyChannel !== "none" &&
                  !notifyChannelKeys.includes(notifyChannel) && (
                    <SelectItem value={notifyChannel}>
                      {notifyChannel} (no longer configured)
                    </SelectItem>
                  )}
              </SelectContent>
            </Select>
            <p className="text-sm text-muted-foreground">
              Where this definition's Slack notifications go. Channels are
              named in <code className="text-xs">DBB_SLACK_NOTIFY_CHANNELS</code>.
              Approval holds on a "none" definition still use the default
              channel.
            </p>
          </div>
          <div className="space-y-2">
            <Label htmlFor="def-notify-bytes">Slack notice every (GiB)</Label>
            <Input
              id="def-notify-bytes"
              type="number"
              min="0"
              step="0.1"
              placeholder="Instance default"
              value={notifyBytesStep}
              onChange={(e) => setNotifyBytesStep(e.target.value)}
              data-testid="grant-definition-notify-bytes-step"
            />
            <p className="text-sm text-muted-foreground">
              Posts in the grant request's Slack thread each time a grant's
              total transfer crosses another multiple of this size. Traffic is
              never blocked. Leave empty to inherit{" "}
              <code className="text-xs">DBB_SLACK_NOTIFY_BYTES_STEP</code>, enter{" "}
              <code className="text-xs">0</code> to turn it off for this
              definition.
            </p>
          </div>
          <div className="space-y-2">
            <Label htmlFor="def-stmt-timeout">Statement timeout (seconds)</Label>
            <Input
              id="def-stmt-timeout"
              type="number"
              min="0"
              max="86400"
              placeholder={statementTimeoutPlaceholder(resolvedStatementTimeout)}
              value={statementTimeout}
              onChange={(e) => setStatementTimeout(e.target.value)}
              data-testid="grant-definition-statement-timeout"
            />
            <p className="text-sm text-muted-foreground">
              How long a single statement may run before dbbat cancels it and
              ends the session. Leave empty to inherit the instance-wide
              setting ({describeResolvedStatementTimeout(resolvedStatementTimeout)}).
              Enter <code className="text-xs">0</code> for{" "}
              <strong>no limit (overrides the global timeout)</strong> — the
              escape hatch for a dump or ETL definition.
            </p>
          </div>
      </div>
    </FormPage>
  );
}
