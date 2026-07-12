"use client";

import * as React from "react";
import { MoreHorizontal, PanelLeftClose, SquarePen, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Logo } from "@/components/logo";
import { ScrollArea } from "@/components/ui/scroll-area";
import { cn } from "@/lib/utils";
import type { Task } from "@/lib/task";

export function TaskSidebar({
  tasks,
  activeId,
  onSelect,
  onNew,
  onDelete,
  onCollapse,
}: {
  tasks: Task[];
  activeId: string | null;
  onSelect: (id: string) => void;
  onNew: () => void;
  onDelete: (id: string) => void;
  onCollapse: () => void;
}) {
  const [menuId, setMenuId] = React.useState<string | null>(null);
  return (
    <aside className="fixed inset-y-0 left-0 z-40 flex h-full w-64 shrink-0 flex-col border-r bg-card shadow-xl lg:static lg:z-auto lg:bg-card/40 lg:shadow-none">
      <div className="flex items-center justify-between px-4 py-3.5">
        <button
          onClick={onNew}
          className="flex cursor-pointer items-center gap-2 text-base font-semibold tracking-tight transition-opacity hover:opacity-70"
          style={{ fontFamily: "var(--font-display)" }}
          aria-label="Go to home"
        >
          <Logo size={20} />
          Open Work
        </button>
        <Button
          variant="ghost"
          size="icon-sm"
          onClick={onCollapse}
          aria-label="Collapse sidebar"
        >
          <PanelLeftClose />
        </Button>
      </div>

      <div className="px-2">
        <button
          onClick={onNew}
          className="flex w-full items-center gap-2 rounded-md px-2 py-2 text-left text-sm font-medium transition-colors hover:bg-accent/50"
        >
          <SquarePen className="size-4 shrink-0 text-muted-foreground" />
          New task
        </button>
      </div>

      <ScrollArea className="flex-1 px-2 py-1">
        {/* Click-away layer for the open task menu. */}
        {menuId && (
          <div className="fixed inset-0 z-10" onClick={() => setMenuId(null)} />
        )}
        {tasks.length > 0 && (
          <ul className="space-y-0.5">
            {tasks.map((task) => (
              <li key={task.id} className="group relative">
                <button
                  onClick={() => onSelect(task.id)}
                  className={cn(
                    "flex w-full items-center rounded-md py-2 pl-2 pr-8 text-left text-sm transition-colors",
                    task.id === activeId
                      ? "bg-accent text-accent-foreground"
                      : "hover:bg-accent/50",
                  )}
                >
                  <span className="truncate">{task.title}</span>
                </button>

                <button
                  onClick={(e) => {
                    e.stopPropagation();
                    setMenuId(menuId === task.id ? null : task.id);
                  }}
                  aria-label="Task options"
                  className={cn(
                    "absolute right-1 top-1/2 -translate-y-1/2 rounded p-1 text-muted-foreground transition-opacity hover:bg-background/80 hover:text-foreground",
                    menuId === task.id
                      ? "opacity-100"
                      : "opacity-0 group-hover:opacity-100",
                  )}
                >
                  <MoreHorizontal className="size-4" />
                </button>

                {menuId === task.id && (
                  <div className="absolute right-1 top-9 z-20 w-32 overflow-hidden rounded-md border bg-card py-1 shadow-lg">
                    <button
                      onClick={() => {
                        setMenuId(null);
                        onDelete(task.id);
                      }}
                      className="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm text-destructive hover:bg-accent"
                    >
                      <Trash2 className="size-3.5" />
                      Delete
                    </button>
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
      </ScrollArea>
    </aside>
  );
}
