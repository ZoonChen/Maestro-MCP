import { App } from '../App';
import { createAuthConfig } from './authClient';
import { LoginGate } from './LoginGate';

// Entry point for the authenticated console. main.jsx deliberately keeps the
// read-only anonymous entry until M4-UI-001 wires the frozen /auth contract,
// so existing gates and e2e stay untouched.
export function AppWithAuth() {
  return (
    <LoginGate config={createAuthConfig()}>
      <App />
    </LoginGate>
  );
}
