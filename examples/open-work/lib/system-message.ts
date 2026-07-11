// Hints we inject into the agent conversation (e.g. "egress approved") — Claude
// sees them and may act or stay silent. They're prefixed so the UI and the
// transcript restore can hide them from the user, who never typed them.
export const SYSTEM_HINT_PREFIX = "[system] ";

export const systemHint = (text: string): string => SYSTEM_HINT_PREFIX + text;

export const isSystemHint = (content: string): boolean =>
  content.startsWith(SYSTEM_HINT_PREFIX);
