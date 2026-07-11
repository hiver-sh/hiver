/** Metadata the client sends alongside each uploaded file, in file order. */
export type ManifestEntry = {
  /** Basename the file is uploaded as inside the sandbox (`/workspace/<name>`). */
  name: string;
  /** Original path: the folder-relative path for references, or the file name. */
  relPath: string;
  kind: "attachment" | "reference";
  size: number;
};

/** A file that made it into the sandbox, echoed back to the Input panel. */
export type UploadedFile = ManifestEntry & { path: string };

/** A file the user attached in the composer, before it's uploaded. */
export type Attachment = { id: string; file: File };

/** One message in a restored conversation. */
export type ConversationMessage = {
  role: "user" | "assistant";
  content: string;
};

/** A file observed in the sandbox (input or output). Size may be unknown. */
export type OutputFile = { name: string; path: string; size?: number };

/** SSE payloads streamed from the task route to the browser. */
export type TaskStreamEvent =
  | { type: "status"; message: string; uploaded?: UploadedFile[] }
  | { type: "output"; text: string }
  | { type: "file"; role: "input" | "output"; file: OutputFile }
  | { type: "file-removed"; role: "input" | "output"; path: string }
  | { type: "egress-denied"; hosts: string[] }
  | { type: "browser"; ready: boolean }
  | { type: "error"; message: string }
  | { type: "done" };
