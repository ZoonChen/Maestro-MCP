import { useCallback, useEffect, useState } from 'preact/hooks';
import { App } from '../App';
import { createAuthConfig, logout } from './authClient';
import { AuthGateView } from './LoginGate';
import { useAuthSession } from './useAuthSession';
import { SESSION_EXPIRED_EVENT } from '../api/client';

// The config must stay referentially stable: useAuthSession's probe
// effect keys on it, so a per-render config object would re-trigger the
// session probe forever.
const AUTH_CONFIG = createAuthConfig();

// Entry point for the authenticated console (M4-UI-001). The shell owns
// the auth session so the console can render the principal identity,
// offer logout, and fall back to the gate when a rejected session is
// reported by the API layer — a re-probe replaces the page, never a
// blank screen. A mid-session rejection (expiry or server-side
// revocation) additionally arms the gate's degradation notice so the
// operator learns WHY the console swapped to the login view. While the
// backend exposes no /auth routes the probe answers 404 and the console
// keeps its read-only anonymous shape.
export function AppWithAuth() {
  const { session, retry } = useAuthSession(AUTH_CONFIG);
  const [sessionLost, setSessionLost] = useState(false);

  useEffect(() => {
    const onExpired = () => { setSessionLost(true); retry(); };
    window.addEventListener(SESSION_EXPIRED_EVENT, onExpired);
    return () => window.removeEventListener(SESSION_EXPIRED_EVENT, onExpired);
  }, [retry]);

  useEffect(() => {
    if (session.status === 'authenticated' || session.status === 'auth-disabled') {
      setSessionLost(false);
    }
  }, [session.status]);

  const handleLogout = useCallback(async () => {
    try {
      await logout(AUTH_CONFIG);
    } finally {
      // Even a failed logout request re-probes locally: the session view
      // must follow the server truth, not the optimistic button state.
      retry();
    }
  }, [retry]);

  if (session.status === 'authenticated' || session.status === 'auth-disabled') {
    const authenticated = session.status === 'authenticated';
    return (
      <App
        auth={{
          status: session.status,
          principal: authenticated ? session.session.principal : '',
          roles: authenticated ? session.session.roles : [],
          projectScope: authenticated ? session.session.projectScope : [],
          logout: authenticated ? handleLogout : null,
        }}
      />
    );
  }
  return <AuthGateView config={AUTH_CONFIG} session={session} retry={retry} sessionLost={sessionLost} />;
}
