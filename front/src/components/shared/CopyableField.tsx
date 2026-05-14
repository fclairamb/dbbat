import { Copy } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

interface CopyableFieldProps {
  label?: string;
  value: string;
  mask?: boolean;
  monospace?: boolean;
  toastMessage?: string;
  testId?: string;
}

export function CopyableField({
  label,
  value,
  mask = false,
  monospace = true,
  toastMessage = "Copied to clipboard",
  testId,
}: CopyableFieldProps) {
  const handleCopy = () => {
    navigator.clipboard.writeText(value).then(() => {
      toast.success(toastMessage);
    });
  };

  return (
    <div className="space-y-1">
      {label && (
        <label className="text-sm font-medium">{label}</label>
      )}
      <div className="flex gap-2">
        <Input
          readOnly
          value={mask ? "•".repeat(Math.min(value.length, 20)) : value}
          data-testid={testId}
          className={monospace ? "font-mono text-sm" : ""}
        />
        <Button
          variant="outline"
          size="icon"
          onClick={handleCopy}
          title="Copy to clipboard"
        >
          <Copy className="h-4 w-4" />
        </Button>
      </div>
    </div>
  );
}
