import * as React from "react";

const MOBILE_BREAKPOINT = 768;

export function useIsMobile() {
  // Seeded from the current viewport in a lazy initializer rather than by a
  // setState inside the effect: this is a browser-only SPA, so the width is
  // readable during the first render and the extra render pass (plus the
  // one-frame "not mobile" flash it caused) is avoidable. The effect is left
  // to track *subsequent* changes only.
  const [isMobile, setIsMobile] = React.useState(
    () => window.innerWidth < MOBILE_BREAKPOINT
  );

  React.useEffect(() => {
    const mql = window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT - 1}px)`);
    const onChange = () => {
      setIsMobile(window.innerWidth < MOBILE_BREAKPOINT);
    };
    mql.addEventListener("change", onChange);
    return () => mql.removeEventListener("change", onChange);
  }, []);

  return isMobile;
}
