import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import { fetchSession } from './authClient';

function toSessionState(result) {
  if (result.kind === 'authenticated') {
    return { status: 'authenticated', session: result };
  }
  if (result.kind === 'auth-disabled') {
    return { status: 'auth-disabled' };
  }
  return { status: 'unauthenticated' };
}

// probing → authenticated | unauthenticated | auth-disabled | error
export function useAuthSession(config) {
  const [session, setSession] = useState({ status: 'probing' });
  const aliveRef = useRef(true);

  useEffect(() => {
    aliveRef.current = true;
    return () => { aliveRef.current = false; };
  }, []);

  const probe = useCallback(async () => {
    setSession({ status: 'probing' });
    try {
      const result = await fetchSession(config);
      if (aliveRef.current) setSession(toSessionState(result));
    } catch (error) {
      if (!aliveRef.current) return;
      const message = error && error.message ? error.message : String(error);
      setSession({ status: 'error', message });
    }
  }, [config]);

  useEffect(() => { probe(); }, [probe]);

  return { session, retry: probe };
}
