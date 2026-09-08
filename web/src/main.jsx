import { render } from 'preact';
import { AppWithAuth } from './auth/AppWithAuth';
import './app.css';

// M4-UI-001: the console entry goes through the auth shell. Deployments
// without the /auth routes probe 404 (auth-disabled) and keep the exact
// read-only anonymous behavior, so the M0 gates and e2e stay untouched.
render(<AppWithAuth />, document.getElementById('app'));
