// Identity strip for the console: who is signed in, which roles the
// server reports, and the logout action. In auth-disabled deployments it
// states the read-only boundary instead of pretending an identity.
export function IdentityBar({ auth }) {
  if (auth && auth.status === 'authenticated') {
    return (
      <div class="identity-bar" data-auth="authenticated">
        <span class="identity-principal" title={auth.principal}>{auth.principal}</span>
        {(auth.roles || []).length > 0 ? (
          <ul class="identity-roles" aria-label="角色">
            {auth.roles.map((role) => <li key={role} class="gov-chip">{role}</li>)}
          </ul>
        ) : (
          <span class="identity-roles">无项目成员角色（只读边界）</span>
        )}
        <button type="button" class="gov-button" onClick={auth.logout}>退出登录</button>
      </div>
    );
  }
  return (
    <div class="identity-bar" data-auth="anonymous">
      <span>只读模式：认证未启用（/auth 未挂载），写操作不可用。</span>
    </div>
  );
}
