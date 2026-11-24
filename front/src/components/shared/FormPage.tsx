import { Link } from "@tanstack/react-router";
import { ArrowLeft, SearchX } from "lucide-react";
import { Button } from "@/components/ui/button";

interface FormPageProps {
  /** data-testid of the page container, e.g. "edit-user-page". */
  testId: string;
  title: React.ReactNode;
  description?: React.ReactNode;
  /** List route the back link and Cancel return to. */
  backTo: string;
  backLabel: string;
  onSubmit: (e: React.FormEvent) => void;
  submitLabel: string;
  submitTestId: string;
  submitting?: boolean;
  submitDisabled?: boolean;
  cancelTestId?: string;
  onCancel: () => void;
  children: React.ReactNode;
}

/**
 * Page chrome shared by every create/edit form: back link, header, the form
 * body at page width, and a sticky Save/Cancel footer so the actions are always
 * reachable however long the form grows.
 */
export function FormPage({
  testId,
  title,
  description,
  backTo,
  backLabel,
  onSubmit,
  submitLabel,
  submitTestId,
  submitting,
  submitDisabled,
  cancelTestId,
  onCancel,
  children,
}: FormPageProps) {
  return (
    <div className="container mx-auto py-6" data-testid={testId}>
      <Link
        to={backTo}
        className="mb-4 inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
        data-testid="back-to-list"
      >
        <ArrowLeft className="h-4 w-4" />
        {backLabel}
      </Link>
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
        {description && (
          <p className="text-sm text-muted-foreground">{description}</p>
        )}
      </div>
      <form onSubmit={onSubmit} className="mt-6">
        <div className="space-y-6">{children}</div>
        <div className="sticky bottom-0 -mx-4 mt-8 flex justify-end gap-2 border-t bg-background px-4 py-3">
          <Button
            type="button"
            variant="outline"
            onClick={onCancel}
            data-testid={cancelTestId ?? "form-cancel"}
          >
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={submitting || submitDisabled}
            data-testid={submitTestId}
          >
            {submitLabel}
          </Button>
        </div>
      </form>
    </div>
  );
}

/** Shown when the uid in the URL does not resolve to an editable entity. */
export function EntityNotFound({
  what,
  backTo,
  backLabel,
  detail,
}: {
  what: string;
  backTo: string;
  backLabel: string;
  detail?: string;
}) {
  return (
    <div
      className="container mx-auto flex flex-col items-center gap-3 py-16 text-center"
      data-testid="entity-not-found"
    >
      <SearchX className="h-10 w-10 text-muted-foreground" />
      <h1 className="text-xl font-semibold">{what} not found</h1>
      <p className="text-sm text-muted-foreground">
        {detail ?? "It may have been deleted, or the link is wrong."}
      </p>
      <Button asChild variant="outline">
        <Link to={backTo}>{backLabel}</Link>
      </Button>
    </div>
  );
}

/**
 * One independently-saved block of a multi-part edit page (the server page):
 * its own form, its own Save, because the API calls behind the sections are
 * separate and sending one section's fields with another's would break the
 * "send only what moved" logic.
 */
export function FormSection({
  testId,
  title,
  description,
  onSubmit,
  submitLabel,
  submitTestId,
  submitting,
  submitDisabled,
  children,
}: {
  testId: string;
  title: React.ReactNode;
  description?: React.ReactNode;
  onSubmit: (e: React.FormEvent) => void;
  submitLabel: string;
  submitTestId: string;
  submitting?: boolean;
  submitDisabled?: boolean;
  children: React.ReactNode;
}) {
  return (
    <section
      className="rounded-lg border bg-card p-6"
      data-testid={testId}
    >
      <form onSubmit={onSubmit} className="max-w-2xl space-y-4">
        <div>
          <h2 className="text-lg font-semibold">{title}</h2>
          {description && (
            <p className="text-sm text-muted-foreground">{description}</p>
          )}
        </div>
        <div className="space-y-4">{children}</div>
        <div className="flex justify-end">
          <Button
            type="submit"
            disabled={submitting || submitDisabled}
            data-testid={submitTestId}
          >
            {submitLabel}
          </Button>
        </div>
      </form>
    </section>
  );
}
