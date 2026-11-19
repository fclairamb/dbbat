import { Badge } from "@/components/ui/badge";

interface UnverifiedBadgeProps {
  chained?: boolean;
  testId: string;
}

// Flags a row that carries no chain position: nothing vouches for it. Renders
// nothing for chained rows.
export function UnverifiedBadge({ chained, testId }: UnverifiedBadgeProps) {
  if (chained !== false) return null;

  return (
    <Badge
      variant="outline"
      className="border-amber-500 text-amber-600"
      data-testid={testId}
      title="This row carries no chain position, so nothing vouches for it. Run `dbbat audit verify` to check the integrity of the trail."
    >
      Unverified
    </Badge>
  );
}
