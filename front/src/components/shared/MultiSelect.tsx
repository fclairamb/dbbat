import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";

export interface MultiSelectOption {
  value: string;
  label: string;
}

/**
 * A scrollable checkbox list for picking several values at once.
 *
 * Deliberately a plain checkbox list rather than a combobox: the lists it
 * backs (users, groups, databases) are small, every option stays visible
 * without extra clicks, and it is trivial to drive from Playwright.
 */
export function MultiSelect({
  options,
  selected,
  onChange,
  placeholder = "Nothing selected",
  emptyMessage = "Nothing to pick from",
  testId,
}: {
  options: MultiSelectOption[];
  selected: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  emptyMessage?: string;
  testId?: string;
}) {
  const toggle = (value: string) =>
    onChange(
      selected.includes(value)
        ? selected.filter((v) => v !== value)
        : [...selected, value]
    );

  if (options.length === 0) {
    return (
      <p className="text-xs text-muted-foreground italic" data-testid={testId}>
        {emptyMessage}
      </p>
    );
  }

  return (
    <div className="space-y-2" data-testid={testId}>
      <div className="max-h-40 overflow-y-auto rounded-md border p-2 space-y-1.5">
        {options.map((option) => (
          <div key={option.value} className="flex items-center gap-2">
            <Checkbox
              id={`${testId ?? "multiselect"}-${option.value}`}
              checked={selected.includes(option.value)}
              onCheckedChange={() => toggle(option.value)}
              data-testid={`${testId ?? "multiselect"}-option-${option.value}`}
            />
            <Label
              htmlFor={`${testId ?? "multiselect"}-${option.value}`}
              className="cursor-pointer font-normal"
            >
              {option.label}
            </Label>
          </div>
        ))}
      </div>
      {selected.length === 0 && (
        <p className="text-xs text-muted-foreground italic">{placeholder}</p>
      )}
    </div>
  );
}
