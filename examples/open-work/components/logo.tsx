import * as React from "react";

/**
 * Open Work mark: an "open" ring (a near-closed circle with a gap) around a
 * center dot. Uses `currentColor` so it inherits the surrounding text color
 * and adapts to light/dark themes.
 */
export function Logo({
  size = 22,
  className,
  ...props
}: React.SVGProps<SVGSVGElement> & { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 100 100"
      fill="none"
      aria-hidden="true"
      className={className}
      {...props}
    >
      <g transform="translate(50 50)">
        <circle
          r="36"
          fill="none"
          stroke="currentColor"
          strokeWidth="12"
          strokeLinecap="round"
          strokeDasharray="164 62"
          transform="rotate(-58)"
        />
        <circle r="9" fill="currentColor" />
      </g>
    </svg>
  );
}
