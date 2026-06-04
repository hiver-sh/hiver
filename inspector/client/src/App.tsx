import { Check, ChevronLeft, ChevronRight, Monitor, Moon, Pencil, RefreshCw, ServerCrash, Settings, Sun } from "lucide-react";
import React, { useCallback, useEffect, useState } from "react";
import { Route, Routes, useLocation, useMatch, useNavigate, useParams } from "react-router-dom";
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
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Separator } from "@/components/ui/separator";
import { DEFAULT_GATEWAY_URL, DEFAULT_INSPECTOR_SERVER, type SandboxRef } from "@/types";
import { purgeOrphanEvents } from "@/lib/eventStore";
import { useScrollbarVisibility } from "@/lib/useScrollbarVisibility";
import { TransportProvider, useTransport } from "@/lib/transport";
import { UserPreferencesProvider, useUserPreferences, type Theme } from "@/lib/userPreferences";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";

const THEME_OPTIONS: { value: Theme; label: string; icon: React.ReactNode }[] = [
  { value: "light", label: "Light", icon: <Sun className="h-3.5 w-3.5" /> },
  { value: "dark", label: "Dark", icon: <Moon className="h-3.5 w-3.5" /> },
  { value: "system", label: "System", icon: <Monitor className="h-3.5 w-3.5" /> },
];

function ThemeToggle() {
  const { prefs, setPref } = useUserPreferences();
  const theme = prefs.theme;
  const setTheme = (t: Theme) => setPref("theme", t);
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button className="text-muted-foreground/50 hover:text-muted-foreground transition-colors" title="Settings">
          <Settings className="h-4 w-4" />
        </button>
      </PopoverTrigger>
      <PopoverContent side="right" align="end" className="w-48 p-1">
        <p className="px-2 py-1 text-[10px] font-medium text-muted-foreground uppercase tracking-wider">Color theme</p>
        {THEME_OPTIONS.map(({ value, label, icon }) => (
          <button
            key={value}
            onClick={() => setTheme(value)}
            className={cn(
              "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-sm transition-colors hover:bg-accent",
              theme === value && "bg-accent text-accent-foreground font-medium",
            )}
          >
            {icon}
            {label}
          </button>
        ))}
      </PopoverContent>
    </Popover>
  );
}

function ControllerUnreachable({ message, loading, onRetry, onOpenSettings }: { message: string; loading: boolean; onRetry: () => void; onOpenSettings: () => void }) {
  return (
    <div className="flex h-screen flex-col items-center justify-center gap-4 bg-background text-foreground">
      <ServerCrash className="h-10 w-10 text-muted-foreground/40" />
      <p className="font-mono text-sm text-muted-foreground max-w-lg text-center">{message}</p>
      <div className="flex items-center gap-2">
        <Button variant="outline" size="sm" onClick={onOpenSettings}>
          <Pencil className="mr-2 h-3.5 w-3.5" />
          Change URL
        </Button>
        <Button variant="outline" size="sm" onClick={onRetry} disabled={loading}>
          <RefreshCw className={cn("mr-2 h-3.5 w-3.5", loading && "animate-spin")} />
          Retry
        </Button>
      </div>
    </div>
  );
}

// --- shared state passed down from the layout ---
interface LayoutProps {
  gatewayUrl: string;
  serverUrl: string;
  sandboxes: SandboxRef[];
  loading: boolean;
  fetchError: string | null;
  fetchSandboxes: () => void;
  setGatewayUrl: (url: string) => void;
  onConnectedChange: (id: string, connected: boolean) => void;
}

function SandboxDetailRoute({ serverUrl, gatewayUrl, sandboxes, fetchSandboxes, onConnectedChange }: LayoutProps) {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const location = useLocation();
  const initCommand = (location.state as { initCommand?: string } | null)?.initCommand;
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
      gatewayUrl={gatewayUrl}
      initCommand={initCommand}
      onShutdown={() => {
        fetchSandboxes();
        navigate("/");
      }}
      onConnectedChange={(c) => onConnectedChange(sandbox.id, c)}
    />
  );
}

function AppContent() {
  const { transport } = useTransport();
  const { prefs, setPref } = useUserPreferences();
  const [gatewayUrl, setGatewayUrl] = useState(DEFAULT_GATEWAY_URL);
  const [serverUrl] = useState(DEFAULT_INSPECTOR_SERVER);
  const [gatewayInput, setGatewayInput] = useState(DEFAULT_GATEWAY_URL);
  const [settingsOpen, setSettingsOpen] = useState(false);

  const sidebarCollapsed = prefs.sidebarCollapsed;
  const setSidebarCollapsed = (v: boolean) => setPref("sidebarCollapsed", v);
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
      url.searchParams.set("gateway", gatewayUrl);
      const res = await transport.fetch(url);
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
  }, [serverUrl, gatewayUrl, transport]);

  useEffect(() => {
    fetchSandboxes();
  }, [fetchSandboxes]);

  useScrollbarVisibility();

  const layoutProps: LayoutProps = {
    gatewayUrl,
    serverUrl,
    sandboxes,
    loading,
    fetchError,
    fetchSandboxes,
    setGatewayUrl,
    onConnectedChange: (id, c) => setConnectedId(c ? id : null),
  };

  const controllerDialog = (
    <Dialog open={settingsOpen} onOpenChange={setSettingsOpen}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>Gateway URL</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-2">
          <div className="grid gap-1.5">
            <Label htmlFor="ctrl-url">Gateway URL</Label>
            <Input
              id="ctrl-url"
              value={gatewayInput}
              onChange={(e) => setGatewayInput(e.target.value)}
              placeholder={DEFAULT_GATEWAY_URL}
            />
          </div>
          <Button
            onClick={() => {
              setGatewayUrl(gatewayInput.trim() || DEFAULT_GATEWAY_URL);
              setSettingsOpen(false);
            }}
          >
            Apply
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );

  if (fetchError) {
    return (
      <>
        {controllerDialog}
        <ControllerUnreachable message={fetchError} loading={loading} onRetry={fetchSandboxes} onOpenSettings={() => setSettingsOpen(true)} />
      </>
    );
  }

  return (
    <div className="flex h-screen bg-background text-foreground">
      {controllerDialog}
      {/* Sidebar */}
      {sidebarCollapsed ? (
        <aside className="flex w-10 shrink-0 flex-col items-center gap-2 py-3 sidebar">
          <button
            onClick={() => setSidebarCollapsed(false)}
            className="text-muted-foreground/50 hover:text-muted-foreground transition-colors"
          >
            <ChevronRight className="h-4 w-4" />
          </button>
          <CreateSandboxDialog compact serverUrl={serverUrl} gatewayUrl={gatewayUrl} onCreated={(id, command) => { fetchSandboxes(); navigate(`/sandboxes/${id}`, { state: { initCommand: command } }); }} />
          <div className="flex flex-col items-center gap-1 mt-1">
            {sandboxes.map((sb) => (
              <button
                key={sb.id}
                onClick={() => navigate(`/sandboxes/${sb.id}`)}
                title={sb.id}
                className={cn(
                  "flex h-7 w-7 items-center justify-center rounded-md transition-colors hover:bg-sidebar-accent",
                  selectedId === sb.id && "bg-sidebar-accent",
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
          <div className="mt-auto pb-3">
            <ThemeToggle />
          </div>
        </aside>
      ) : (
        <aside className="flex w-72 flex-none flex-col sidebar">
          {/* Branding + controller URL */}
          <div className="h-[70px] flex flex-col justify-center">
            <div className="flex items-center justify-between px-5 pt-4 pb-2">
              <button onClick={() => navigate("/")} className="flex items-center gap-1.5 text-sm font-semibold tracking-tight hover:opacity-80 transition-opacity">
                <img src="/favicon.svg" alt="" className="h-4 w-4 invert dark:invert-0" />
                Hive Inspector
              </button>
              <button
                onClick={() => setSidebarCollapsed(true)}
                className="text-muted-foreground/50 hover:text-muted-foreground transition-colors"
              >
                <ChevronLeft className="h-5 w-5" />
              </button>
            </div>
            <div
              onClick={() => setSettingsOpen(true)}
              className="flex items-center gap-2 px-1.5 py-1 ml-4 mb-2 cursor-pointer group w-fit rounded-md hover:bg-foreground/10 transition-colors"
            >
              <span className="font-mono text-[11px] text-muted-foreground leading-none group-hover:text-foreground transition-colors">
                {gatewayUrl}
              </span>
              <Pencil style={{ width: 12, height: 12 }} className="shrink-0 text-muted-foreground group-hover:text-foreground transition-colors" />
            </div>
          </div>

          <Separator />

          <SandboxList
            sandboxes={sandboxes}
            selectedId={selectedId ?? null}
            connectedId={connectedId}
            loading={loading}
            onSelect={(id) => navigate(`/sandboxes/${id}`)}
            onRefresh={fetchSandboxes}
            onCreated={(id, command) => { fetchSandboxes(); navigate(`/sandboxes/${id}`, { state: { initCommand: command } }); }}
            serverUrl={serverUrl}
            gatewayUrl={gatewayUrl}
          />
          <div className="flex items-center px-4 py-3 border-t border-border/50">
            <ThemeToggle />
          </div>
        </aside>
      )}

      {/* Main */}
      <main className="min-w-0 flex-1">
        <Routes>
          <Route path="/" element={<GettingStarted gatewayUrl={gatewayUrl} />} />
          <Route
            path="/sandboxes/:id"
            element={<SandboxDetailRoute {...layoutProps} />}
          />
        </Routes>
      </main>
    </div>
  );
}

export default function App() {
  return (
    <UserPreferencesProvider>
      <TransportProvider>
        <AppContent />
      </TransportProvider>
    </UserPreferencesProvider>
  );
}
