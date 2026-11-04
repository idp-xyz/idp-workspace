import { useEffect, useRef } from 'react';
import { createShellHost } from '@idp-workspace/bridge/shell';
import { windowPort } from '@idp-workspace/bridge';
import type { Capability } from '@idp-workspace/bridge';
import type { Api } from './api.ts';
import { shellHandlers } from './bridgeHandlers.ts';
import type { ShellUI } from './bridgeHandlers.ts';
import type { InstalledApp, Workspace } from './types.ts';

// An app page in a sandboxed frame: scripts, forms and its own origin (so the bridge can check that
// origin and the app keeps its own storage), never top navigation, popups or downloads. An app on
// the shell's own origin is refused: there allow-same-origin would let it reach into the shell. The
// bridge is bound to the frame's window and the manifest entry's origin (ADR-0001 D5).
export function AppFrame({ api, ws, app, ui, contextVersion }: { api: Api; ws: Workspace; app: InstalledApp; ui: ShellUI; contextVersion: number }) {
  const frame = useRef<HTMLIFrameElement>(null);
  const host = useRef<ReturnType<typeof createShellHost> | null>(null);
  useEffect(() => {
    const win = frame.current?.contentWindow;
    if (!win || !app.origin) return;
    host.current = createShellHost({
      port: windowPort(window, win), origin: app.origin, source: win,
      capabilities: app.manifest.capabilities as Capability[],
      handlers: shellHandlers(api, ws, app, ui),
      onRefused: (_req, problem) => console.warn(`bridge: ${app.manifest.name}: ${problem.code} ${problem.message}`),
    });
    return () => { host.current?.close(); host.current = null; };
  }, [api, ws, app, ui]);
  useEffect(() => {
    if (contextVersion > 0) host.current?.emit('context.changed', { version: contextVersion });
  }, [contextVersion]);
  if (!app.manifest.entry) return <p className="muted">{app.manifest.title} has no page; its actions appear on inbox items.</p>;
  if (!app.origin || app.origin === window.location.origin) return <p className="error">{app.manifest.title} is not framed: its page must be on its own origin.</p>;
  return (
    <iframe ref={frame} className="app-frame" title={app.manifest.title} src={app.manifest.entry.url}
      sandbox="allow-scripts allow-forms allow-same-origin" referrerPolicy="no-referrer" allow="" />
  );
}
