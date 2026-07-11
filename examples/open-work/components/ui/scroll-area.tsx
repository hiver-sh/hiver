import * as React from "react";

import { cn } from "@/lib/utils";

/**
 * A lightweight scroll container. We avoid the Radix ScrollArea dependency
 * here and lean on native overflow with the `.scroll-slim` styling from
 * globals.css so the example stays small.
 */
const ScrollArea = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement>
>(({ className, children, ...props }, ref) => (
  <div
    ref={ref}
    className={cn("overflow-y-auto scroll-slim", className)}
    {...props}
  >
    {children}
  </div>
));
ScrollArea.displayName = "ScrollArea";

export { ScrollArea };
