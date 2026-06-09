import * as ScrollAreaPrimitive from "@radix-ui/react-scroll-area";
import * as React from "react";
import { cn } from "@/lib/utils";

// @radix-ui/react-scroll-area@1.2.x was typed against React 19 types, which
// causes HTML attributes (className, children) to be absent from its prop types
// when used with @types/react@18.3.x. Each wrapper uses intrinsic HTML element
// props and casts the inner Radix call to bridge the incompatibility.
/* eslint-disable @typescript-eslint/no-explicit-any */

const ScrollArea = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement>
>(({ className, children, ...props }, ref) => (
  <ScrollAreaPrimitive.Root
    ref={ref}
    className={cn("relative overflow-hidden", className)}
    {...(props as any)}
  >
    <ScrollAreaPrimitive.Viewport
      {...({ className: "h-full w-full rounded-[inherit]", children } as any)}
    />
    <ScrollBar />
    <ScrollAreaPrimitive.Corner />
  </ScrollAreaPrimitive.Root>
));
ScrollArea.displayName = ScrollAreaPrimitive.Root.displayName;

const ScrollBar = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement> & {
    orientation?: "vertical" | "horizontal";
  }
>(({ className, orientation = "vertical", ...props }, ref) => (
  <ScrollAreaPrimitive.ScrollAreaScrollbar
    ref={ref}
    orientation={orientation}
    {...({
      className: cn(
        "flex touch-none select-none transition-colors",
        orientation === "vertical" &&
          "h-full w-2.5 border-l border-l-transparent p-[1px]",
        orientation === "horizontal" &&
          "h-2.5 flex-col border-t border-t-transparent p-[1px]",
        className,
      ),
      children: (
        <ScrollAreaPrimitive.ScrollAreaThumb
          {...({ className: "relative flex-1 rounded-full bg-border" } as any)}
        />
      ),
      ...props,
    } as any)}
  />
));
ScrollBar.displayName = ScrollAreaPrimitive.ScrollAreaScrollbar.displayName;

export { ScrollArea, ScrollBar };
