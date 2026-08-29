// Auth contract v0 (working-layer assumption; final shapes freeze with the
// control-plane /auth spec): the backend owns the Authorization Code + PKCE
// exchange, the session cookie is HttpOnly, and the browser only ever holds
// principal metadata plus a one-shot return-state value.

const DEFAULT_AUTH_CONFIG = {
  sessionPath: '/auth/session',
  authorizePath: '/auth/authorize',
  logoutPath: '/auth/logout',
};

const RETURN_STATE_KEY = 'maestro-auth-return-state';

export function createAuthConfig(overrides = {}) {
  const config = { ...DEFAULT_AUTH_CONFIG, ...overrides };
  for (const [key, value] of Object.entries(config)) {
    if (typeof value !== 'string' || !value.startsWith('/')) {
      throw new Error(`auth config ${key} must be an absolute path`);
    }
  }
  return Object.freeze(config);
}

function authDisabled(response) {
  return response.status === 404;
}

export async function fetchSession(config) {
  const response = await fetch(config.sessionPath, {
    method: 'GET',
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
  });
  if (authDisabled(response)) {
    return { kind: 'auth-disabled' };
  }
  if (response.status === 401) {
    return { kind: 'unauthenticated' };
  }
  if (!response.ok) {
    throw new Error(`session probe failed with HTTP ${response.status}`);
  }
  const body = await response.json().catch(() => null);
  if (!body || typeof body.principal !== 'string') {
    throw new Error('session payload missing principal');
  }
  return {
    kind: 'authenticated',
    principal: body.principal,
    roles: Array.isArray(body.roles) ? body.roles : [],
    projectScope: Array.isArray(body.project_scope) ? body.project_scope : [],
  };
}

function createReturnState() {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  let binary = '';
  bytes.forEach((byte) => { binary += String.fromCharCode(byte); });
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// The return-state only proves that a redirect back into the console matches a
// login this tab started. It carries no credential and never leaves
// sessionStorage.
export function buildLoginURL(config) {
  const state = createReturnState();
  sessionStorage.setItem(RETURN_STATE_KEY, state);
  const redirectUri = new URL(window.location.href);
  redirectUri.hash = '';
  const authorize = new URL(config.authorizePath, window.location.origin);
  authorize.searchParams.set('redirect_uri', redirectUri.toString());
  authorize.searchParams.set('state', state);
  return authorize.toString();
}

export function consumeReturnState(state) {
  if (typeof state !== 'string' || state.length === 0) {
    return false;
  }
  const expected = sessionStorage.getItem(RETURN_STATE_KEY);
  sessionStorage.removeItem(RETURN_STATE_KEY);
  return expected !== null && expected === state;
}

export async function logout(config) {
  const response = await fetch(config.logoutPath, {
    method: 'POST',
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
  });
  if (!response.ok && response.status !== 404) {
    throw new Error(`logout failed with HTTP ${response.status}`);
  }
}
