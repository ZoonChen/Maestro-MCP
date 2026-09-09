import { SCENARIOS } from '../governance';

// The eight PRD §4 journeys as the console's navigation map (M4-UI-001
// B5). Each card names the scenario, its roles and its journey steps;
// linked scenarios jump to their live view, skeleton ones state the
// honest first-generation boundary instead of a dead link.
export function ScenarioMap({ roles }) {
  return (
    <section class="gov-page" aria-labelledby="scenario-map-title">
      <header class="gov-header">
        <h1 id="scenario-map-title">八场景地图</h1>
        <p class="gov-lead">
          治理控制台按 PRD 的八个端到端场景组织。第一代已接线豁免审批与 MR·Pipeline 视图；
          其余场景保持骨架与旅程说明，待对应后端能力点亮后接入。
          角色只影响可见入口，授权判定一律以服务端为准。
        </p>
        <p class="gov-note" role="note">
          当前身份角色：{roles.length > 0 ? roles.join('、') : '（未启用认证 / 未知角色 —— 只读边界）'}
        </p>
      </header>
      <ul class="scenario-grid">
        {SCENARIOS.map((scenario) => (
          <li key={scenario.id} class="scenario-card" data-scenario={scenario.id}>
            <div class="scenario-card-head">
              <h2>{scenario.title}</h2>
              <span class={`gov-chip ${scenario.status === 'linked' ? 'gov-chip-authoritative' : 'gov-chip-diagnostic'}`}>
                {scenario.status === 'linked' ? '已接线' : '骨架'}
              </span>
            </div>
            <p class="scenario-prd">PRD {scenario.prd} · 角色：{scenario.roles.join('、')}</p>
            <p class="scenario-summary">{scenario.summary}</p>
            <ol class="scenario-steps">
              {scenario.steps.map((step) => <li key={step}>{step}</li>)}
            </ol>
            {scenario.linkTo ? (
              <a class="gov-button gov-button-primary scenario-link" href={scenario.linkTo}>
                打开视图
              </a>
            ) : (
              <p class="scenario-pending">第一代未接线：该场景的写操作面板在后端能力就绪后开放。</p>
            )}
          </li>
        ))}
      </ul>
    </section>
  );
}
