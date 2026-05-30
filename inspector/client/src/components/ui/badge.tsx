import { cva, type VariantProps } from "class-variance-authority";
import * as React from "react";
import { cn } from "@/lib/utils";

const badgeVariants = cva(
  "inline-flex items-center rounded-full border px-2.5 py-0.5 text-xs font-semibold transition-colors focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2",
  {
    variants: {
      variant: {
        default: "border-transparent bg-primary text-primary-foreground hover:bg-primary/80",
        secondary: "border-transparent bg-secondary text-secondary-foreground hover:bg-secondary/80",
        destructive: "border-transparent bg-destructive text-destructive-foreground hover:bg-destructive/80",
        outline: "text-foreground",
        blue: "border-transparent bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200",
        green: "border-transparent bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200",
        red: "border-transparent bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200",
        purple: "border-transparent bg-purple-100 text-purple-800 dark:bg-purple-900 dark:text-purple-200",
        orange: "border-transparent bg-orange-900 text-orange-200",
        zinc: "border-transparent bg-zinc-800 text-zinc-300",
        cyan: "border-transparent bg-cyan-900 text-cyan-200",
        indigo: "border-transparent bg-indigo-900 text-indigo-200",
      },
    },
    defaultVariants: { variant: "default" },
  },
);

export interface BadgeProps
  extends React.HTMLAttributes<HTMLDivElement>,
    VariantProps<typeof badgeVariants> {}

function Badge({ className, variant, ...props }: BadgeProps) {
  return (
    <div className={cn(badgeVariants({ variant }), className)} {...props} />
  );
}

export { Badge, badgeVariants };
