import { Button, type ButtonProps } from "@/components/ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

interface PermissionButtonProps extends ButtonProps {
  /**
   * Tooltip message to show when button is disabled
   */
  disabledReason?: string;
  /**
   * Tooltip message to show when button is enabled
   */
  enabledTooltip?: string;
  /**
   * Children to render inside the button
   */
  children: React.ReactNode;
}

/**
 * A button with tooltip support for showing permission messages.
 *
 * - Shows `disabledReason` immediately when disabled (0ms delay)
 * - Shows `enabledTooltip` after 300ms delay when enabled
 */
export function PermissionButton({
  disabledReason,
  enabledTooltip,
  disabled,
  children,
  ...buttonProps
}: PermissionButtonProps) {
  const tooltipContent = disabled ? disabledReason : enabledTooltip;

  // If no tooltip is provided, render button without tooltip wrapper
  if (!tooltipContent) {
    return (
      <Button disabled={disabled} {...buttonProps}>
        {children}
      </Button>
    );
  }

  return (
    <Tooltip delayDuration={disabled ? 0 : 300}>
      <TooltipTrigger asChild>
        <span>
          <Button disabled={disabled} {...buttonProps}>
            {children}
          </Button>
        </span>
      </TooltipTrigger>
      <TooltipContent>
        <p>{tooltipContent}</p>
      </TooltipContent>
    </Tooltip>
  );
}
