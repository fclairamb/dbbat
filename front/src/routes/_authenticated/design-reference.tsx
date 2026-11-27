import { useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import {
  Ban,
  Check,
  Copy,
  Download,
  Ellipsis,
  Pencil,
  Plus,
  RefreshCw,
  Search,
  Trash2,
  Users,
} from "lucide-react";
import { toast } from "sonner";

import { DataTable, type Column } from "@/components/shared/DataTable";
import { DetailHeaderView } from "@/components/shared/DetailHeader";
import { FormSection } from "@/components/shared/FormPage";
import { LoadingSpinner } from "@/components/shared/LoadingSpinner";
import { PageHeader } from "@/components/shared/PageHeader";
import { StatCard } from "@/components/shared/StatCard";
import { UsageMeter } from "@/components/shared/UsageMeter";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { designReferenceNavItem } from "@/lib/navigation";

export const Route = createFileRoute("/_authenticated/design-reference")({
  component: DesignReferencePage,
});

/**
 * The design reference: the rules every page follows, each next to a live
 * example and the code to copy. Read it before any frontend change; when a
 * page needs a pattern that is not here, add it here in the same change.
 */
const SECTIONS = [
  { id: "conventions", label: "Conventions" },
  { id: "app-chrome", label: "App chrome" },
  { id: "page-header", label: "Page header" },
  { id: "detail-header", label: "Detail header & back" },
  { id: "button-placement", label: "Button placement" },
  { id: "buttons-badges", label: "Buttons & badges" },
  { id: "forms", label: "Forms" },
  { id: "data-display", label: "Data display" },
  { id: "feedback", label: "Feedback" },
  { id: "colors", label: "Colors" },
];

function DesignReferencePage() {
  return (
    <div className="space-y-10 pb-24" data-testid="design-reference-page">
      <PageHeader
        icon={designReferenceNavItem.icon}
        title="Design Reference"
        description="The rules every dbbat page follows, each with a live example and the code to copy. Source: front/src/routes/_authenticated/design-reference.tsx."
      />
      <SubNav />
      <ConventionsSection />
      <AppChromeSection />
      <PageHeaderSection />
      <DetailHeaderSection />
      <ButtonPlacementSection />
      <ButtonsBadgesSection />
      <FormsSection />
      <DataDisplaySection />
      <FeedbackSection />
      <ColorsSection />
    </div>
  );
}

/* ------------------------------------------------------------------------ */
/* Page scaffolding                                                          */
/* ------------------------------------------------------------------------ */

function SubNav() {
  return (
    <nav
      aria-label="Design reference sections"
      className="sticky top-0 z-10 -mx-4 border-b bg-background/80 px-4 py-2 backdrop-blur sm:-mx-6 sm:px-6"
    >
      <ul className="flex flex-wrap gap-2">
        {SECTIONS.map((s) => (
          <li key={s.id}>
            <a
              href={`#${s.id}`}
              className="inline-flex items-center rounded-full border bg-card px-3 py-1 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
              data-testid={`design-reference-nav-${s.id}`}
            >
              {s.label}
            </a>
          </li>
        ))}
      </ul>
    </nav>
  );
}

function Section({
  id,
  title,
  description,
  children,
}: {
  id: string;
  title: string;
  description?: string;
  children: React.ReactNode;
}) {
  return (
    <section
      id={id}
      className="scroll-mt-16 space-y-4"
      data-testid={`design-reference-section-${id}`}
    >
      <header>
        <h2 className="text-lg font-semibold tracking-tight">{title}</h2>
        {description && (
          <p className="mt-1 text-sm text-muted-foreground">{description}</p>
        )}
      </header>
      {children}
    </section>
  );
}

/** A rule: a short title and the prose that states it. */
function Rule({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="space-y-2 rounded-md border bg-card p-4">
      <h3 className="text-sm font-semibold">{title}</h3>
      <div className="space-y-2 text-sm text-muted-foreground">{children}</div>
    </div>
  );
}

function C({ children }: { children: React.ReactNode }) {
  return (
    <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs text-foreground">
      {children}
    </code>
  );
}

function CodeSnippet({ code }: { code: string }) {
  const [copied, setCopied] = useState(false);
  const onCopy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // The clipboard needs a secure context; the code stays selectable.
    }
  };
  return (
    <div className="relative">
      <pre className="overflow-x-auto rounded-md border bg-muted/40 px-3 py-2 pr-12 text-xs leading-relaxed">
        <code>{code}</code>
      </pre>
      <button
        type="button"
        onClick={onCopy}
        aria-label={copied ? "Copied" : "Copy to clipboard"}
        className="absolute right-1.5 top-1.5 inline-flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
      >
        {copied ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
      </button>
    </div>
  );
}

/** A live example on the left, the code that produces it on the right. */
function Example({
  preview,
  code,
}: {
  preview: React.ReactNode;
  code: string;
}) {
  // minmax(0,…) and min-w-0 on both children: a grid item defaults to
  // min-width:auto, so a wide preview would widen its track and overflow the
  // page on a phone instead of wrapping.
  return (
    <div className="grid gap-3 rounded-md border bg-card p-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)] lg:items-start">
      <div className="flex min-w-0 flex-wrap items-center gap-2">{preview}</div>
      <div className="min-w-0">
        <CodeSnippet code={code} />
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------------ */
/* Sections                                                                  */
/* ------------------------------------------------------------------------ */

function ConventionsSection() {
  return (
    <Section
      id="conventions"
      title="Conventions"
      description="Project-wide rules. Reach for them whenever you scaffold a page."
    >
      <Rule title="A title is a name, never a uid">
        <p>
          A detail or edit page is titled with the record&apos;s name: the
          username, the server name, the definition name, the SQL preview of a
          statement. Never &quot;Edit user&quot;, &quot;Query details&quot; or
          a uid, in the page title or in the top-bar breadcrumb.{" "}
          <C>DetailHeader</C> publishes the name to the top bar for you; until
          it loads, the crumb shows an ellipsis. When the name cannot be
          resolved (a non-admin cannot list other users), say so in words
          (&quot;Unknown user&quot;).
        </p>
      </Rule>
      <Rule title="Edit is a page, create is a dialog">
        <p>
          Editing an existing record navigates to its own route (
          <C>/users/$uid</C>, <C>/servers/$uid</C>), so it can be linked,
          reloaded and left with the browser&apos;s back button. The list&apos;s
          Edit action is a <C>Link</C>. Creating stays a <C>Dialog</C> opened
          from the list. Where both share fields, one form component in{" "}
          <C>src/components/&lt;entity&gt;/</C> renders as a page, or as a
          dialog with <C>FormPage</C>&apos;s <C>dialog</C> prop. Dialogs also
          stay for confirmations (delete, revoke, rename) and one-shot reveals
          (a new API key).
        </p>
      </Rule>
      <Rule title="Row actions: ghost icons, not menus">
        <p>
          In a table row, use ghost icon buttons: <C>Pencil</C> links to the
          edit route, <C>Trash2</C> opens an <C>AlertDialog</C>. Give each an{" "}
          <C>aria-label</C> naming the record. Other actions live on the
          record&apos;s page, not in the row.
        </p>
        <div className="flex items-center gap-1">
          <Button variant="ghost" size="icon" aria-label="Edit alice">
            <Pencil className="h-4 w-4" />
          </Button>
          <Button
            variant="ghost"
            size="icon"
            className="text-destructive hover:text-destructive"
            aria-label="Delete alice"
          >
            <Trash2 className="h-4 w-4" />
          </Button>
        </div>
      </Rule>
      <Rule title="Delete is always red, always a trash bin">
        <p>
          Every delete or irreversible action (delete, revoke, deactivate,
          terminate) is red: <C>variant=&quot;destructive&quot;</C> for a
          standalone button, <C>text-destructive hover:text-destructive</C> on a
          ghost icon button, <C>text-destructive focus:text-destructive</C> on a
          menu item. Red is reserved for that: never for a neutral or primary
          action.
        </p>
      </Rule>
      <Rule title="Test ids on everything that matters">
        <p>
          Buttons, inputs, forms, links, dialogs, list rows and status badges
          carry a kebab-case <C>data-testid</C> (<C>save-button</C>,{" "}
          <C>user-list-item</C>). E2E tests select on them, never on classes or
          DOM structure. Edit pages use <C>*-page</C> and <C>*-section</C>.
        </p>
      </Rule>
      <Rule title="Mobile works">
        <p>
          Every page is usable at 390px: no fixed widths, headers and toolbars
          wrap (<C>flex-wrap</C>), wide tables scroll inside their card, never
          the page.
        </p>
      </Rule>
    </Section>
  );
}

function AppChromeSection() {
  return (
    <Section
      id="app-chrome"
      title="App chrome"
      description="The frame every authenticated page sits in. It belongs to _authenticated.tsx: pages never repeat it."
    >
      <Rule title="Sidebar">
        <p>
          Sections and their icons are declared once in{" "}
          <C>src/lib/navigation.ts</C>: the sidebar, the page-header tile and the
          top-bar breadcrumb all read them, so a section has one name and one
          icon everywhere. An entry stays highlighted on its detail pages (
          <C>isInSection</C>).
        </p>
      </Rule>
      <Rule title="Top bar breadcrumb">
        <p>
          Route-driven, built from the pathname by <C>Header.tsx</C>. A section
          segment shows the section&apos;s title; an entity segment shows the
          name its page published. The trail truncates its last crumb instead
          of wrapping.
        </p>
      </Rule>
      <Rule title="Content column">
        <p>
          The layout owns the gutter (16px on a phone, 24px from <C>sm</C>) and
          a centered <C>max-w-[1400px]</C> column. A page&apos;s root is a plain{" "}
          <C>{'<div className="space-y-6">'}</C>: no <C>container</C>, no{" "}
          <C>mx-auto</C>, no outer padding. Forms cap their own fields at{" "}
          <C>max-w-2xl</C> (or <C>max-w-3xl</C> for wide ones) inside that
          column.
        </p>
      </Rule>
      <Example
        preview={
          <div className="w-full overflow-hidden rounded-md border bg-background">
            <div className="flex h-10 items-center border-b px-3 text-xs text-muted-foreground">
              Users › alice
            </div>
            <div className="px-4 py-4 text-sm sm:px-6 sm:py-6">
              <div className="mx-auto w-full max-w-[1400px] rounded-md border border-dashed p-3 text-muted-foreground">
                Page content, inside the 1400px column.
              </div>
            </div>
          </div>
        }
        code={`// Already in routes/_authenticated.tsx: don't repeat it.
<main className="flex-1 overflow-auto px-4 py-4 sm:px-6 sm:py-6">
  <div className="mx-auto w-full max-w-[1400px]">
    <Outlet />
  </div>
</main>

// A page:
function UsersPage() {
  return <div className="space-y-6">…</div>;
}`}
      />
    </Section>
  );
}

function PageHeaderSection() {
  return (
    <Section
      id="page-header"
      title="Page header"
      description="Every list and section page opens with PageHeader: the section icon in a tile, the title, a one-line description and the page-level actions on the right (below the title on a phone)."
    >
      <Example
        preview={
          <PageHeader
            icon={Users}
            title="Users"
            description="Manage user accounts"
            actions={
              <Button data-testid="example-create-button">
                <Plus className="h-4 w-4" />
                New User
              </Button>
            }
            className="w-full"
          />
        }
        code={`import { PageHeader } from "@/components/shared/PageHeader";
import { navItem } from "@/lib/navigation";

<PageHeader
  icon={navItem("/users")!.icon}
  title="Users"
  description="Manage user accounts"
  actions={<Button><Plus className="h-4 w-4" />New User</Button>}
/>`}
      />
      <p className="text-sm text-muted-foreground">
        The title is the sidebar&apos;s name for the section, word for word.
        The icon comes from <C>navItem(href)</C>, never picked per page. Detail
        and edit pages do not use <C>PageHeader</C>: see the next section.
      </p>
    </Section>
  );
}

function DetailHeaderSection() {
  return (
    <Section
      id="detail-header"
      title="Detail header & back navigation"
      description="Detail and edit pages open with DetailHeader: a breadcrumb line that is the way back, then the record's name with its badges and actions."
    >
      <Example
        preview={
          <DetailHeaderView
            className="w-full"
            backTo="/connections"
            backLabel="Connections"
            title="alice @ analytics"
            badges={<Badge variant="default">Active</Badge>}
            meta={
              <>
                <span>Connected Nov 27, 2026, 10:42</span>
                <span>12 queries</span>
              </>
            }
            actions={
              <>
                <Button variant="outline" size="sm">
                  <Download className="h-4 w-4" />
                  <span className="hidden md:inline">Download capture</span>
                </Button>
                <Button variant="destructive" size="sm">
                  <Ban className="h-4 w-4" />
                  <span className="hidden md:inline">Terminate</span>
                </Button>
              </>
            }
          />
        }
        code={`import { DetailHeader } from "@/components/shared/DetailHeader";

<DetailHeader
  backTo="/connections"
  backLabel="Connections"
  title={\`\${username} @ \${serverName}\`}   // a name, never a uid
  badges={<Badge>Active</Badge>}
  meta={\`Connected \${format(connectedAt, "PPpp")}\`}
  actions={<>…at most two buttons…</>}
/>`}
      />
      <Rule title="Going back">
        <ul className="list-disc space-y-1 pl-5">
          <li>
            The way back is the breadcrumb line: <C>← Parent</C> as a link, then
            the record&apos;s name. Its link carries{" "}
            <C>data-testid=&quot;back-to-list&quot;</C>.
          </li>
          <li>
            No &quot;Back to …&quot; buttons in footers or toolbars, no{" "}
            <C>history.back()</C>: the target is always the parent route, so the
            link works from a bookmark or a fresh tab.
          </li>
          <li>
            A record reached through another one shows that chain with{" "}
            <C>parents</C>: a query reads{" "}
            <C>← Queries › alice @ analytics › SELECT …</C>. The middle crumbs
            hide on a phone; the back link and the name stay.
          </li>
          <li>
            An edit form&apos;s Cancel returns to the list too. It is the
            form&apos;s action, not a second back button.
          </li>
          <li>
            A uid that resolves to nothing renders <C>EntityNotFound</C>, which
            keeps a link back to the list.
          </li>
        </ul>
      </Rule>
      <Rule title="Actions on a detail page">
        <p>
          At most two visible buttons beside the title: the primary one{" "}
          <C>default</C>, the secondary one <C>outline</C> (or{" "}
          <C>destructive</C> when it is the irreversible one). Anything more
          goes in a ⋯ menu (<C>Button variant=&quot;outline&quot; size=&quot;icon&quot;</C>{" "}
          with an <C>Ellipsis</C> icon), Delete last after a separator. Labels
          show from <C>md</C> up; below, the buttons are icon-only.
        </p>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm">
            <Pencil className="h-4 w-4" />
            Edit
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="outline" size="icon" aria-label="More actions">
                <Ellipsis className="h-4 w-4" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem>
                <Copy className="mr-2 h-4 w-4" />
                Copy connection URL
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem className="text-destructive focus:text-destructive">
                <Trash2 className="mr-2 h-4 w-4" />
                Delete server
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </Rule>
      <Rule title="Edit pages">
        <p>
          <C>FormPage</C> renders the <C>DetailHeader</C> (title = the
          record&apos;s name, description in the meta line), the form body and
          a sticky Save/Cancel bar that spans the content column. A page made of
          independently saved parts (the server page) stacks{" "}
          <C>FormSection</C>s under its own <C>DetailHeader</C> instead.
        </p>
      </Rule>
      <CodeSnippet
        code={`<FormPage
  testId="edit-user-page"
  title={user.username}                 // the name, not "Edit user"
  description="Roles and user groups of this account."
  backTo="/users"
  backLabel="Users"
  onSubmit={handleSubmit}
  onCancel={() => navigate({ to: "/users" })}
  submitLabel="Save"
  submitTestId="edit-user-submit"
>
  <div className="max-w-2xl space-y-4">…fields…</div>
</FormPage>`}
      />
    </Section>
  );
}

function ButtonPlacementSection() {
  return (
    <Section
      id="button-placement"
      title="Button placement"
      description="Where a button lives depends on what it does."
    >
      <Rule title="Header actions change what exists">
        <p>
          <C>PageHeader</C>&apos;s <C>actions</C> hold the page&apos;s primary
          action, usually one &quot;New &lt;Resource&gt;&quot; button (
          <C>Plus</C> icon, the resource named as in the sidebar, singular:
          &quot;New Server Group&quot;), plus at most one secondary one. Its
          create dialog carries the same title. Page-wide switches that change the whole listing
          (&quot;Show all users&quot;) may sit to its left.
        </p>
      </Rule>
      <Rule title="The toolbar row changes what you look at">
        <p>
          Search and filters go in their own <C>flex flex-wrap items-center gap-2</C>{" "}
          row under the header: filters on the left, view controls (count,
          refresh) on the right. A page with no filter row keeps its refresh in
          the header, left of the primary button.
        </p>
      </Rule>
      <Example
        preview={
          <div className="w-full space-y-3">
            <PageHeader
              icon={Users}
              title="Users"
              actions={
                <Button size="sm">
                  <Plus className="h-4 w-4" />
                  New User
                </Button>
              }
            />
            <div className="flex flex-wrap items-center gap-2">
              <div className="relative min-w-[200px] max-w-sm flex-1">
                <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
                <Input placeholder="Search…" className="pl-9" />
              </div>
              <Button variant="outline" size="icon" aria-label="Refresh" className="ml-auto">
                <RefreshCw className="h-4 w-4" />
              </Button>
            </div>
          </div>
        }
        code={`<PageHeader icon={…} title="Users" actions={<Button>New User</Button>} />
<div className="flex flex-wrap items-center gap-2">
  <div className="relative min-w-[200px] max-w-sm flex-1">
    <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
    <Input placeholder="Search…" className="pl-9" />
  </div>
  <Button variant="outline" size="icon" aria-label="Refresh" className="ml-auto">
    <RefreshCw className="h-4 w-4" />
  </Button>
</div>`}
      />
      <p className="text-sm text-muted-foreground">
        A button disabled for lack of a role is a <C>PermissionButton</C>: its
        tooltip says which role is missing, so the button is never silently
        dead.
      </p>
    </Section>
  );
}

function ButtonsBadgesSection() {
  return (
    <Section
      id="buttons-badges"
      title="Buttons & badges"
      description="Variants of the shadcn primitives in src/components/ui. Use them as they are; don't restyle per page."
    >
      <Example
        preview={
          <>
            <Button>Primary</Button>
            <Button variant="outline">Outline</Button>
            <Button variant="secondary">Secondary</Button>
            <Button variant="ghost">Ghost</Button>
            <Button variant="link">Link</Button>
            <Button variant="destructive">
              <Trash2 className="h-4 w-4" />
              Delete
            </Button>
            <Button size="sm">Small</Button>
            <Button disabled>Disabled</Button>
          </>
        }
        code={`import { Button } from "@/components/ui/button";

<Button>Primary</Button>
<Button variant="outline">Outline</Button>
<Button variant="destructive"><Trash2 className="h-4 w-4" />Delete</Button>
<Button size="sm">Small</Button>`}
      />
      <Example
        preview={
          <>
            <Badge>Active</Badge>
            <Badge variant="secondary">Disconnected</Badge>
            <Badge variant="destructive">Terminated</Badge>
            <Badge variant="outline">Archived</Badge>
          </>
        }
        code={`import { Badge } from "@/components/ui/badge";

<Badge>Active</Badge>                       // live, on
<Badge variant="secondary">Disconnected</Badge>  // ended, neutral
<Badge variant="destructive">Terminated</Badge>  // ended by force, failed
<Badge variant="outline">Archived</Badge>        // a fact, no state`}
      />
    </Section>
  );
}

function FormsSection() {
  const [listable, setListable] = useState(true);
  return (
    <Section
      id="forms"
      title="Forms"
      description="Labels above fields, help text under them, fields capped at max-w-2xl. Save is the primary button, at the end."
    >
      <Example
        preview={
          <div className="w-full max-w-md space-y-4">
            <div className="space-y-2">
              <Label htmlFor="dr-name">Name</Label>
              <Input id="dr-name" placeholder="analytics_replica" />
              <p className="text-xs text-muted-foreground">
                Lowercase letters, numbers, underscores and hyphens.
              </p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="dr-ssl">SSL mode</Label>
              <Select defaultValue="require">
                <SelectTrigger id="dr-ssl">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="disable">Disable</SelectItem>
                  <SelectItem value="require">Require</SelectItem>
                  <SelectItem value="verify-full">Verify full</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label htmlFor="dr-desc">Description</Label>
              <Textarea id="dr-desc" placeholder="What this server is for" />
            </div>
            <div className="flex items-center justify-between rounded-md border p-3">
              <div>
                <Label htmlFor="dr-listable">Listable</Label>
                <p className="text-xs text-muted-foreground">
                  Show in the access-request dropdown
                </p>
              </div>
              <Switch
                id="dr-listable"
                checked={listable}
                onCheckedChange={setListable}
              />
            </div>
            <div className="flex items-center gap-2">
              <Checkbox id="dr-test" />
              <Label htmlFor="dr-test">Test the connection after saving</Label>
            </div>
          </div>
        }
        code={`<div className="space-y-2">
  <Label htmlFor="name">Name</Label>
  <Input id="name" data-testid="server-name-input" />
  <p className="text-xs text-muted-foreground">Help text.</p>
</div>

// A boolean setting: a bordered row, label left, Switch right.
<div className="flex items-center justify-between rounded-md border p-3">
  <div><Label htmlFor="listable">Listable</Label><p …>…</p></div>
  <Switch id="listable" checked={v} onCheckedChange={setV} />
</div>`}
      />
      <Example
        preview={
          <FormSection
            testId="example-section"
            title="Rename server"
            description="Saved on its own, independently of the other sections."
            onSubmit={(e) => e.preventDefault()}
            submitLabel="Rename"
            submitTestId="example-section-submit"
          >
            <div className="space-y-2">
              <Label htmlFor="dr-rename">Name</Label>
              <Input id="dr-rename" defaultValue="proxy_target" />
            </div>
          </FormSection>
        }
        code={`import { FormSection } from "@/components/shared/FormPage";

// One independently-saved block of a multi-part edit page.
<FormSection
  testId="rename-server-section"
  title="Rename server"
  onSubmit={onSubmit}
  submitLabel="Rename"
  submitTestId="rename-server-submit"
>
  …fields…
</FormSection>`}
      />
    </Section>
  );
}

type ExampleRow = { uid: string; name: string; role: string; queries: number };

const EXAMPLE_ROWS: ExampleRow[] = [
  { uid: "1", name: "alice", role: "admin", queries: 1243 },
  { uid: "2", name: "bob", role: "connector", queries: 87 },
];

function DataDisplaySection() {
  const columns: Column<ExampleRow>[] = [
    { key: "name", header: "Name", cell: (r) => <span className="font-medium">{r.name}</span> },
    { key: "role", header: "Role", cell: (r) => <Badge variant="secondary">{r.role}</Badge> },
    { key: "queries", header: "Queries", cell: (r) => r.queries.toLocaleString() },
    {
      key: "actions",
      header: "",
      className: "w-24 text-right",
      cell: (r) => (
        <div className="flex justify-end gap-1">
          <Button variant="ghost" size="icon" aria-label={`Edit ${r.name}`}>
            <Pencil className="h-4 w-4" />
          </Button>
          <Button
            variant="ghost"
            size="icon"
            className="text-destructive hover:text-destructive"
            aria-label={`Delete ${r.name}`}
          >
            <Trash2 className="h-4 w-4" />
          </Button>
        </div>
      ),
    },
  ];
  return (
    <Section
      id="data-display"
      title="Data display"
      description="Lists are DataTable; a row that has a page links to it with rowHref. Key figures are StatCards."
    >
      <Example
        preview={
          <DataTable
            className="w-full"
            columns={columns}
            data={EXAMPLE_ROWS}
            rowKey={(r) => r.uid}
            emptyMessage="No users yet."
          />
        }
        code={`import { DataTable, type Column } from "@/components/shared/DataTable";

<DataTable
  columns={columns}
  data={users}
  isLoading={isLoading}
  rowKey={(u) => u.uid}
  rowHref={(u) => \`/users/\${u.uid}\`}
  emptyMessage="No users yet."
/>`}
      />
      <Example
        preview={
          <div className="grid w-full gap-4 sm:grid-cols-2">
            <StatCard title="Active connections" value={12} description="Across all servers" />
            <StatCard title="Queries today" value="4,210" isLoading />
          </div>
        }
        code={`import { StatCard } from "@/components/shared/StatCard";

<StatCard title="Active connections" value={12} description="Across all servers" />
<StatCard title="Queries today" value={n} isLoading={isLoading} />`}
      />
      <Example
        preview={
          <div className="w-full max-w-xs space-y-3">
            <UsageMeter used={42} limit={100} unit="queries" />
            <UsageMeter used={95} limit={100} unit="queries" />
            <UsageMeter used={3} limit={null} unit="queries" />
          </div>
        }
        code={`import { UsageMeter } from "@/components/shared/UsageMeter";

// Warning at 80%, destructive at the limit, "unlimited" when none.
<UsageMeter used={used} limit={grant.max_query_counts} unit="queries" />`}
      />
    </Section>
  );
}

function FeedbackSection() {
  return (
    <Section
      id="feedback"
      title="Feedback"
      description="How a page says it is loading, failed, empty, or done."
    >
      <Example
        preview={
          <div className="w-full space-y-3">
            <Alert>
              <AlertTitle>Retention is off</AlertTitle>
              <AlertDescription>
                Statements are kept forever until a window is set.
              </AlertDescription>
            </Alert>
            <Alert variant="destructive">
              <AlertTitle>Connection failed</AlertTitle>
              <AlertDescription>password authentication failed</AlertDescription>
            </Alert>
          </div>
        }
        code={`import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";

<Alert data-testid="retention-off-alert">…</Alert>
<Alert variant="destructive" data-testid="connection-error">…</Alert>`}
      />
      <Example
        preview={
          <>
            <Button variant="outline" onClick={() => toast.success("Server updated")}>
              Success toast
            </Button>
            <Button variant="outline" onClick={() => toast.error("Save failed")}>
              Error toast
            </Button>
          </>
        }
        code={`import { toast } from "sonner";

// After a mutation: a toast, not an alert. Name what changed.
toast.success(\`Server "\${name}" updated\`);
toast.error(error.message);`}
      />
      <Example
        preview={
          <div className="flex w-full items-center gap-4">
            <LoadingSpinner />
            <div className="flex-1 space-y-2">
              <Skeleton className="h-4 w-1/2" />
              <Skeleton className="h-4 w-1/3" />
            </div>
          </div>
        }
        code={`import { PageLoader } from "@/components/shared/LoadingSpinner";

if (isLoading) return <PageLoader />;           // a whole page
<Skeleton className="h-4 w-24" />                // a value inside a card`}
      />
      <Rule title="Not found, not allowed">
        <p>
          A uid that resolves to nothing renders <C>EntityNotFound</C> (it
          names the record and links back to the list). A page the role cannot
          see renders <C>AccessDenied</C> with the role it needs. Neither
          redirects.
        </p>
        <CodeSnippet
          code={`if (!canUpdate) return <AccessDenied requiredRole="admin" />;
if (!user) return <EntityNotFound what="User" backTo="/users" backLabel="Back to users" />;`}
        />
      </Rule>
    </Section>
  );
}

const COLOR_TOKENS: { name: string; use: string }[] = [
  { name: "primary", use: "The product pink: primary buttons, links, focus rings, the page-header tile." },
  { name: "primary-foreground", use: "Text and icons on a primary fill." },
  { name: "secondary", use: "Neutral fills: secondary buttons and badges." },
  { name: "accent", use: "Hover and highlight surfaces." },
  { name: "muted", use: "Quiet surfaces: code, skeletons, table headers." },
  { name: "muted-foreground", use: "Secondary text: descriptions, meta lines, help." },
  { name: "card", use: "Cards, sections, table frames." },
  { name: "border", use: "Every border and separator." },
  { name: "destructive", use: "Delete and irreversible actions, errors. Nothing else." },
];

function ColorsSection() {
  return (
    <Section
      id="colors"
      title="Colors"
      description="Tokens from src/index.css, redefined for dark mode. Use the Tailwind classes (bg-primary, text-muted-foreground), never a raw color."
    >
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        {COLOR_TOKENS.map((t) => (
          <div key={t.name} className="flex items-center gap-3 rounded-md border bg-card p-3">
            <div
              className="h-10 w-10 shrink-0 rounded-md border"
              style={{ backgroundColor: `var(--${t.name})` }}
            />
            <div className="min-w-0">
              <div className="font-mono text-xs font-medium">--{t.name}</div>
              <div className="text-xs text-muted-foreground">{t.use}</div>
            </div>
          </div>
        ))}
      </div>
    </Section>
  );
}
