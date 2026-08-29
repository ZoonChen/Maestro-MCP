import { useState } from 'preact/hooks';
import { buildLoginURL, consumeReturnState } from './authClient';
import { useAuthSession } from './useAuthSession';

// Read once per page load: consuming the one-shot return-state on the first
// render keeps the callback error stable across later re-renders.
function readCallbackError() {
  const params = new URLSearchParams(window.location.search);
  const error = params.get('auth_error');
  const state = params.get('state');
  if (!error || !state) return null;
  // A callback error is only trusted when the return-state still matches the
  // login this tab started; otherwise fall through to the plain login view
  // instead of echoing an unverifiable failure.
  return consumeReturnState(state) ? error : null;
}

export function LoginGate({ config, children }) {
  const { session, retry } = useAuthSession(config);
  const [callbackError] = useState(readCallbackError);

  if (session.status === 'authenticated' || session.status === 'auth-disabled') {
    return children;
  }

  if (session.status === 'probing') {
    return (
      <main class="auth-gate" aria-busy="true">
        <p role="status">正在检查登录状态…</p>
      </main>
    );
  }

  if (session.status === 'error') {
    return (
      <main class="auth-gate">
        <h1>无法确认登录状态</h1>
        <p role="alert">{session.message}</p>
        <button type="button" onClick={retry}>重试</button>
      </main>
    );
  }

  const startLogin = () => { window.location.assign(buildLoginURL(config)); };

  return (
    <main class="auth-gate">
      <h1>Maestro 控制台</h1>
      <p>需要通过公司身份认证后访问治理控制台。</p>
      {callbackError ? <p role="alert">登录未完成（{callbackError}），请重新登录。</p> : null}
      <button type="button" onClick={startLogin}>使用公司账号登录</button>
    </main>
  );
}
