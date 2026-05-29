import { ChevronLeft, ChevronRight, Pencil, RefreshCw, ServerCrash } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { Route, Routes, useMatch, useNavigate, useParams } from "react-router-dom";
import { CreateSandboxDialog } from "@/components/CreateSandboxDialog";
import { GettingStarted } from "@/components/GettingStarted";
import { SandboxDetail } from "@/components/SandboxDetail";
import { SandboxList } from "@/components/SandboxList";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Separator } from "@/components/ui/separator";
import { DEFAULT_CONTROLLER_URL, DEFAULT_INSPECTOR_SERVER, type SandboxRef } from "@/types";
import { purgeOrphanEvents } from "@/lib/eventStore";

function ControllerUnreachable({ message, loading, onRetry }: { message: string; loading: boolean; onRetry: () => void }) {
  return (
    <div className="flex h-screen flex-col items-center justify-center gap-4 bg-background text-foreground">
      <ServerCrash className="h-10 w-10 text-muted-foreground/40" />
      <p className="font-mono text-sm text-muted-foreground max-w-lg text-center">{message}</p>
      <Button variant="outline" size="sm" onClick={onRetry} disabled={loading}>
        <RefreshCw className={cn("mr-2 h-3.5 w-3.5", loading && "animate-spin")} />
        Retry
      </Button>
    </div>
  );
}

// --- shared state passed down from the layout ---
interface LayoutProps {
  controllerUrl: string;
  serverUrl: string;
  sandboxes: SandboxRef[];
  loading: boolean;
  fetchError: string | null;
  fetchSandboxes: () => void;
  setControllerUrl: (url: string) => void;
  onConnectedChange: (id: string, connected: boolean) => void;
}

function SandboxDetailRoute({ serverUrl, controllerUrl, sandboxes, fetchSandboxes, onConnectedChange }: LayoutProps) {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const sandbox = sandboxes.find((s) => s.id === id);

  if (!sandbox) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
        Sandbox not found — select one from the sidebar.
      </div>
    );
  }

  return (
    <SandboxDetail
      key={sandbox.id}
      sandbox={sandbox}
      serverUrl={serverUrl}
      controllerUrl={controllerUrl}
      onShutdown={() => {
        fetchSandboxes();
        navigate("/");
      }}
      onConnectedChange={(c) => onConnectedChange(sandbox.id, c)}
    />
  );
}

export default function App() {
  const [controllerUrl, setControllerUrl] = useState(DEFAULT_CONTROLLER_URL);
  const [serverUrl] = useState(DEFAULT_INSPECTOR_SERVER);
  const [controllerInput, setControllerInput] = useState(DEFAULT_CONTROLLER_URL);
  const [settingsOpen, setSettingsOpen] = useState(false);

  const [sidebarCollapsed, setSidebarCollapsed] = useState(
    () => localStorage.getItem("app:sidebarCollapsed") === "true",
  );

  useEffect(() => {
    localStorage.setItem("app:sidebarCollapsed", String(sidebarCollapsed));
  }, [sidebarCollapsed]);
  const [sandboxes, setSandboxes] = useState<SandboxRef[]>([]);
  const [loading, setLoading] = useState(false);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [connectedId, setConnectedId] = useState<string | null>(null);

  const navigate = useNavigate();
  const match = useMatch("/sandboxes/:id");
  const selectedId = match?.params.id ?? null;

  const fetchSandboxes = useCallback(async () => {
    setLoading(true);
    setFetchError(null);
    try {
      const url = new URL(`${serverUrl}/api/sandboxes`);
      url.searchParams.set("controller", controllerUrl);
      const res = await fetch(url);
      if (!res.ok) {
        const body = await res.json().catch(() => ({ error: res.statusText }));
        setFetchError((body as { error?: string }).error ?? res.statusText);
        return;
      }
      const list = (await res.json()) as SandboxRef[];
      setSandboxes(list);
      void purgeOrphanEvents(list.map((s) => s.id));
    } catch (err) {
      setFetchError(String(err));
    } finally {
      setLoading(false);
    }
  }, [serverUrl, controllerUrl]);

  useEffect(() => {
    fetchSandboxes();
  }, [fetchSandboxes]);

  const layoutProps: LayoutProps = {
    controllerUrl,
    serverUrl,
    sandboxes,
    loading,
    fetchError,
    fetchSandboxes,
    setControllerUrl,
    onConnectedChange: (id, c) => setConnectedId(c ? id : null),
  };

  if (fetchError) {
    return <ControllerUnreachable message={fetchError} loading={loading} onRetry={fetchSandboxes} />;
  }

  return (
    <div className="flex h-screen bg-background text-foreground">
      {/* Sidebar */}
      {sidebarCollapsed ? (
        <aside className="flex w-10 shrink-0 flex-col items-center gap-2 py-3 sidebar">
          <button
            onClick={() => setSidebarCollapsed(false)}
            className="text-muted-foreground/50 hover:text-muted-foreground transition-colors"
          >
            <ChevronRight className="h-4 w-4" />
          </button>
          <CreateSandboxDialog compact serverUrl={serverUrl} controllerUrl={controllerUrl} onCreated={(id) => { fetchSandboxes(); navigate(`/sandboxes/${id}`); }} />
          <div className="flex flex-col items-center gap-1 mt-1">
            {sandboxes.map((sb) => (
              <button
                key={sb.id}
                onClick={() => navigate(`/sandboxes/${sb.id}`)}
                title={sb.id}
                className={cn(
                  "flex h-7 w-7 items-center justify-center rounded-md transition-colors hover:bg-accent",
                  selectedId === sb.id && "bg-accent",
                )}
              >
                <span className={cn(
                  "block rounded-full transition-all",
                  selectedId === sb.id ? "h-2.5 w-2.5" : "h-2 w-2",
                  connectedId === sb.id ? "bg-green-400" : "bg-muted-foreground/40",
                )} />
              </button>
            ))}
          </div>
        </aside>
      ) : (
        <aside className="flex w-72 flex-none flex-col sidebar">
          {/* Branding + controller URL */}
          <div className="h-[70px] flex flex-col justify-center">
            <div className="flex items-center justify-between px-5 pt-4 pb-2">
              <button onClick={() => navigate("/")} className="flex items-center gap-1.5 text-sm font-semibold tracking-tight hover:opacity-80 transition-opacity">
                <img src="/favicon.svg" alt="" className="h-4 w-4" />
                Hive Inspector
              </button>
              <button
                onClick={() => setSidebarCollapsed(true)}
                className="text-muted-foreground/50 hover:text-muted-foreground transition-colors"
              >
                <ChevronLeft className="h-5 w-5" />
              </button>
            </div>
            <Dialog open={settingsOpen} onOpenChange={setSettingsOpen}>
              <DialogTrigger asChild>
                <div className="flex items-center gap-2 px-1.5 py-1 ml-4 mb-2 cursor-pointer group w-fit rounded-md hover:bg-white/10 transition-colors">
                  <span className="font-mono text-[11px] text-muted-foreground leading-none group-hover:text-foreground transition-colors">
                    {controllerUrl}
                  </span>
                  <Pencil style={{ width: 12, height: 12 }} className="shrink-0 text-muted-foreground group-hover:text-foreground transition-colors" />
                </div>
              </DialogTrigger>
              <DialogContent className="sm:max-w-sm">
                <DialogHeader>
                  <DialogTitle>Controller URL</DialogTitle>
                </DialogHeader>
                <div className="grid gap-4 py-2">
                  <div className="grid gap-1.5">
                    <Label htmlFor="ctrl-url">Controller URL</Label>
                    <Input
                      id="ctrl-url"
                      value={controllerInput}
                      onChange={(e) => setControllerInput(e.target.value)}
                      placeholder={DEFAULT_CONTROLLER_URL}
                    />
                  </div>
                  <Button
                    onClick={() => {
                      setControllerUrl(controllerInput.trim() || DEFAULT_CONTROLLER_URL);
                      setSettingsOpen(false);
                    }}
                  >
                    Apply
                  </Button>
                </div>
              </DialogContent>
            </Dialog>
          </div>

          <Separator />

          <SandboxList
            sandboxes={sandboxes}
            selectedId={selectedId ?? null}
            connectedId={connectedId}
            loading={loading}
            onSelect={(id) => navigate(`/sandboxes/${id}`)}
            onRefresh={fetchSandboxes}
            onCreated={(id) => { fetchSandboxes(); navigate(`/sandboxes/${id}`); }}
            serverUrl={serverUrl}
            controllerUrl={controllerUrl}
          />
        </aside>
      )}

      {/* Main */}
      <main className="min-w-0 flex-1">
        <Routes>
          <Route path="/" element={<GettingStarted />} />
          <Route
            path="/sandboxes/:id"
            element={<SandboxDetailRoute {...layoutProps} />}
          />
        </Routes>
      </main>
    </div>
  );
}
