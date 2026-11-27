import { Link } from "@tanstack/react-router";
import { SearchX } from "lucide-react";
import { DetailHeader, useCurrentPathname } from "@/components/shared/DetailHeader";
import { Button } from "@/components/ui/button";
import { useBreadcrumbTitle } from "@/contexts/BreadcrumbContext";
import {
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

interface FormPageProps {
  /** data-testid of the page container, e.g. "edit-user-page". */
  testId?: string;
  /**
   * Render as the content of a create dialog instead of a page: same form
   * body, wrapped in a DialogContent. Edit forms are pages, create forms are
   * dialogs, and both share one form component.
   */
  dialog?: boolean;
  /** Extra classes for the DialogContent in dialog mode. */
  dialogClassName?: string;
  /**
   * Edit page: the entity's name, never "Edit X" nor a uid. Create dialog: the
   * action ("New server group").
   */
  title: string;
  description?: React.ReactNode;
  /** List route the breadcrumb and Cancel return to. */
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
 * Page chrome shared by every create/edit form: the detail header (breadcrumb
 * back to the list, the entity's name), the form body, and a sticky
 * Save/Cancel footer so the actions are always reachable however long the form
 * grows.
 */
export function FormPage({
  testId,
  dialog,
  dialogClassName,
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
  if (dialog) {
    return (
      <DialogContent data-testid={testId} className={dialogClassName}>
        <form onSubmit={onSubmit}>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            {description && <DialogDescription>{description}</DialogDescription>}
          </DialogHeader>
          <div className="space-y-4 py-4 max-h-[60vh] overflow-y-auto">
            {children}
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={onCancel}
              data-testid={cancelTestId}
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
          </DialogFooter>
        </form>
      </DialogContent>
    );
  }
  return (
    <div data-testid={testId}>
      <DetailHeader
        backTo={backTo}
        backLabel={backLabel}
        title={title}
        meta={description}
      />
      <form onSubmit={onSubmit} className="mt-6">
        <div className="space-y-6">{children}</div>
        {/* Bleeds over the layout gutter (16px, 24px from sm) so the bar
            spans the content column edge to edge. */}
        <div className="sticky bottom-0 -mx-4 mt-8 flex justify-end gap-2 border-t bg-background px-4 py-3 sm:-mx-6 sm:px-6">
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
  useBreadcrumbTitle(useCurrentPathname(), "Not found");
  return (
    <div
      className="flex flex-col items-center gap-3 py-16 text-center"
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
