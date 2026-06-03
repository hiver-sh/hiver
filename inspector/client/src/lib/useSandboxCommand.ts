import { useState } from "react";

export function useSandboxCommand(id: string): [string, (value: string) => void, () => void] {
  const [command, setCommand] = useState("");

  function persist() {
    if (id) localStorage.setItem(`sandbox.command.${id}`, command);
  }

  return [command, setCommand, persist];
}
