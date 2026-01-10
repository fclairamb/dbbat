import { createFileRoute } from "@tanstack/react-router";
import { useAuditEvents, useUsers, type AuditEvent } from "@/api";
import { DataTable, type Column } from "@/components/shared/DataTable";
import { PageHeader } from "@/components/shared/PageHeader";
import { AdaptiveRefresh } from "@/components/shared/AdaptiveRefresh";
import { Badge } from "@/components/ui/badge";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import { Button } from "@/components/ui/button";
import { ChevronDown } from "lucide-react";
import { formatDistanceToNow } from "date-fns";
import { useState, useRef, useCallback } from "react";
import { useAuth } from "@/contexts/AuthContext";
import { canViewAudit } from "@/lib/permissions";
import { AccessDenied } from "@/components/shared/AccessDenied";

export const Route = createFileRoute("/_authenticated/audit/")({
  component: AuditPage,
});

function AuditPage() {
  const { user } = useAuth();
  const { data: events, isLoading, refetch } = useAuditEvents({ limit: 100 });
  const { data: users } = useUsers();

  const previousFirstUid = useRef<string | null>(null);

  const handleRefresh = useCallback(async () => {
    const result = await refetch();
    const newData = result.data;

    let hasNewData = false;
    if (newData && newData.length > 0) {
      const firstUid = newData[0].uid;
      hasNewData = previousFirstUid.current !== null &&
                   firstUid !== previousFirstUid.current;
      previousFirstUid.current = firstUid;
    }

    return { hasNewData };
  }, [refetch]);

  // Check if user has viewer role
  if (!canViewAudit(user?.roles)) {
    return <AccessDenied requiredRole="viewer" />;
  }

  const getUserName = (uid: string | null | undefined) => {
    if (!uid) return "-";
    return users?.find((u) => u.uid === uid)?.username ?? uid;
  };

  const getEventBadgeVariant = (
    eventType: string
  ): "default" | "secondary" | "destructive" | "outline" => {
    if (eventType.includes("created")) return "default";
    if (eventType.includes("deleted") || eventType.includes("revoked"))
      return "destructive";
    if (eventType.includes("updated")) return "secondary";
    return "outline";
  };

  const columns: Column<AuditEvent>[] = [
    {
      key: "event_type",
      header: "Event",
      cell: (e) => (
        <Badge variant={getEventBadgeVariant(e.event_type)}>
          {e.event_type}
        </Badge>
      ),
    },
    {
      key: "user_id",
      header: "Target User",
      cell: (e) => (
        <span className="text-sm">{getUserName(e.user_id)}</span>
      ),
    },
    {
      key: "performed_by",
      header: "Performed By",
      cell: (e) => (
        <span className="text-sm">{getUserName(e.performed_by)}</span>
      ),
    },
    {
      key: "created_at",
      header: "Time",
      cell: (e) => (
        <span className="text-sm text-muted-foreground">
          {formatDistanceToNow(new Date(e.created_at), { addSuffix: true })}
        </span>
      ),
    },
    {
      key: "details",
      header: "Details",
      cell: (e) =>
        e.details && Object.keys(e.details).length > 0 ? (
          <DetailsCell details={e.details} />
        ) : (
          <span className="text-muted-foreground">-</span>
        ),
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Audit Log"
        description="View system activity and changes"
        actions={
          <AdaptiveRefresh
            onRefresh={handleRefresh}
            storageKey="dbbat.autoRefresh.audit"
          />
        }
      />

      <DataTable
        columns={columns}
        data={events ?? []}
        isLoading={isLoading}
        rowKey={(e) => e.uid}
        emptyMessage="No audit events found"
      />
    </div>
  );
}

function DetailsCell({ details }: { details: Record<string, unknown> }) {
  const [isOpen, setIsOpen] = useState(false);

  return (
    <Collapsible open={isOpen} onOpenChange={setIsOpen}>
      <CollapsibleTrigger asChild>
        <Button variant="ghost" size="sm" className="h-6 px-2">
          <ChevronDown
            className={`h-3 w-3 transition-transform ${isOpen ? "rotate-180" : ""}`}
          />
          <span className="ml-1">View</span>
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent className="mt-2">
        <pre className="bg-muted p-2 rounded text-xs overflow-x-auto max-w-xs">
          {JSON.stringify(details, null, 2)}
        </pre>
      </CollapsibleContent>
    </Collapsible>
  );
}
