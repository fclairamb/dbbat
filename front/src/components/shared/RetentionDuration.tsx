import { useState } from "react";
import { AlertTriangle } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

/** A window as the API carries it: null inherits, 0 never, else seconds. */
export type RetentionSeconds = number | null;

export const RETENTION_UNITS = [
  { value: "seconds", label: "seconds", factor: 1 },
  { value: "minutes", label: "minutes", factor: 60 },
  { value: "hours", label: "hours", factor: 3600 },
  { value: "days", label: "days", factor: 86400 },
] as const;

/** The largest unit that divides the value exactly. */
export function splitSeconds(seconds: number): { amount: number; unit: string } {
  for (const u of [...RETENTION_UNITS].reverse()) {
    if (seconds % u.factor === 0) {
      return { amount: seconds / u.factor, unit: u.value };
    }
  }
  return { amount: seconds, unit: "seconds" };
}

/** "36 hours", "never", "inherited". */
export function formatSeconds(seconds: RetentionSeconds | undefined): string {
  if (seconds === null || seconds === undefined) return "inherited";
  if (seconds === 0) return "never";
  const { amount, unit } = splitSeconds(seconds);
  return `${amount} ${unit}`;
}

type Mode = "inherit" | "never" | "custom";

function modeOf(value: RetentionSeconds): Mode {
  if (value === null) return "inherit";
  return value === 0 ? "never" : "custom";
}

/**
 * One retention window: inherit, never, or a number and a unit. Three states,
 * as the API has them. The unit shown is the largest one dividing the value.
 */
export function RetentionDuration({
  label,
  value,
  onChange,
  testId,
  hint,
}: {
  label: string;
  value: RetentionSeconds;
  onChange: (next: RetentionSeconds) => void;
  testId: string;
  hint?: string;
}) {
  const initial = value && value > 0 ? splitSeconds(value) : { amount: 1, unit: "days" };
  const [mode, setMode] = useState<Mode>(modeOf(value));
  const [amount, setAmount] = useState(String(initial.amount));
  const [unit, setUnit] = useState(initial.unit);

  const emit = (m: Mode, a: string, u: string) => {
    if (m === "inherit") return onChange(null);
    if (m === "never") return onChange(0);
    const factor = RETENTION_UNITS.find((x) => x.value === u)?.factor ?? 1;
    const n = parseInt(a, 10);
    onChange(Number.isFinite(n) && n > 0 ? n * factor : 0);
  };

  return (
    <div className="space-y-2">
      <Label htmlFor={`${testId}-mode`}>{label}</Label>
      <div className="flex gap-2">
        <Select
          value={mode}
          onValueChange={(v) => {
            setMode(v as Mode);
            emit(v as Mode, amount, unit);
          }}
        >
          <SelectTrigger
            id={`${testId}-mode`}
            className="w-36"
            data-testid={`${testId}-mode`}
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="inherit" data-testid={`${testId}-mode-inherit`}>
              Inherit
            </SelectItem>
            <SelectItem value="never" data-testid={`${testId}-mode-never`}>
              Never
            </SelectItem>
            <SelectItem value="custom" data-testid={`${testId}-mode-custom`}>
              After…
            </SelectItem>
          </SelectContent>
        </Select>
        {mode === "custom" && (
          <>
            <Input
              type="number"
              min={1}
              className="w-24"
              value={amount}
              data-testid={`${testId}-amount`}
              onChange={(e) => {
                setAmount(e.target.value);
                emit(mode, e.target.value, unit);
              }}
            />
            <Select
              value={unit}
              onValueChange={(u) => {
                setUnit(u);
                emit(mode, amount, u);
              }}
            >
              <SelectTrigger className="w-32" data-testid={`${testId}-unit`}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {RETENTION_UNITS.map((u) => (
                  <SelectItem
                    key={u.value}
                    value={u.value}
                    data-testid={`${testId}-unit-${u.value}`}
                  >
                    {u.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </>
        )}
      </div>
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  );
}

/** True when saving `next` over `current` destroys data at the next sweep. */
export function deleteWindowShrinks(
  current: RetentionSeconds | undefined,
  next: RetentionSeconds,
): boolean {
  if (next === null || next === 0) return false;
  const cur = current ?? null;
  // From inherit or never to a positive value, or to a shorter one.
  if (cur === null || cur === 0) return cur !== next;
  return next < cur;
}

export function DeleteWindowWarning({ testId }: { testId: string }) {
  return (
    <div
      className="flex gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-xs"
      data-testid={testId}
    >
      <AlertTriangle className="h-4 w-4 shrink-0 text-amber-600" />
      <p>
        <strong>This destroys data.</strong> Statements and archives older than
        the new delete window are removed for good at the next sweep, including
        data already stored. The change is audited.
      </p>
    </div>
  );
}

/** The pair of windows, with the destructive-edit warning. */
export function RetentionFields({
  archive,
  onArchiveChange,
  del,
  onDeleteChange,
  originalDelete,
  archiveEnabled,
  testIdPrefix,
  inheritNote,
}: {
  archive: RetentionSeconds;
  onArchiveChange: (v: RetentionSeconds) => void;
  del: RetentionSeconds;
  onDeleteChange: (v: RetentionSeconds) => void;
  originalDelete: RetentionSeconds | undefined;
  archiveEnabled: boolean;
  testIdPrefix: string;
  inheritNote?: string;
}) {
  return (
    <div className="space-y-4 rounded-lg border p-3" data-testid={`${testIdPrefix}-retention`}>
      <div>
        <p className="text-sm font-medium">Retention</p>
        <p className="text-xs text-muted-foreground">
          Inherit follows the server groups, then the instance. Never overrides
          what is above it.
          {inheritNote ? ` ${inheritNote}` : ""}
        </p>
      </div>
      {archiveEnabled && (
        <RetentionDuration
          label="Archive after"
          value={archive}
          onChange={onArchiveChange}
          testId={`${testIdPrefix}-archive-after`}
          hint="Captured result rows move to the archive bucket this long after the session closes."
        />
      )}
      <RetentionDuration
        label="Delete after"
        value={del}
        onChange={onDeleteChange}
        testId={`${testIdPrefix}-delete-after`}
        hint="Statements, rows and archives are deleted for good after this."
      />
      {deleteWindowShrinks(originalDelete, del) && (
        <DeleteWindowWarning testId={`${testIdPrefix}-delete-warning`} />
      )}
    </div>
  );
}
