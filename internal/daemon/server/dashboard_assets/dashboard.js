// dashboard.js — polls /api/dashboard/state every 2 s and re-renders the
// grid + sidebar in place. Deliberately framework-free: the whole page is a
// single CSS grid whose rows mirror the JSON shape one-for-one.

const state = {
  selectedEcosystem: null,
  openWorktree: null,
  lastPayload: null,
};

async function poll() {
  try {
    const res = await fetch('/api/dashboard/state');
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const payload = await res.json();
    state.lastPayload = payload;
    render(payload);
  } catch (e) {
    document.getElementById('status-summary').textContent = `error: ${e.message}`;
  }
}

function render(payload) {
  const ecoSelect = document.getElementById('ecosystem-select');
  const ecos = payload.ecosystems || [];

  // Populate ecosystem dropdown (once, or when size changes).
  if (ecoSelect.options.length !== ecos.length) {
    ecoSelect.innerHTML = '';
    ecos.forEach((e, i) => {
      const opt = document.createElement('option');
      opt.value = e.name;
      opt.textContent = e.name;
      if (i === 0 && !state.selectedEcosystem) state.selectedEcosystem = e.name;
      ecoSelect.appendChild(opt);
    });
    ecoSelect.value = state.selectedEcosystem || (ecos[0] && ecos[0].name);
    ecoSelect.onchange = () => { state.selectedEcosystem = ecoSelect.value; render(state.lastPayload); };
  }

  const eco = ecos.find(e => e.name === state.selectedEcosystem) || ecos[0];
  if (!eco) {
    document.getElementById('grid').innerHTML = '<p class="empty">no ecosystems found</p>';
    document.getElementById('status-summary').textContent = '';
    return;
  }

  // Top bar summary.
  const running = eco.worktrees.filter(w => w.state === 'running').length;
  document.getElementById('status-summary').textContent = `${running}/${eco.worktrees.length} worktrees running`;
  document.getElementById('updated').textContent = formatTime(payload.generated_at);

  renderGrid(eco);
  renderSidebar(eco);
}

function renderGrid(eco) {
  const host = document.getElementById('grid');
  host.innerHTML = '';
  eco.worktrees.forEach(wt => host.appendChild(renderRow(wt)));
}

function renderRow(wt) {
  const row = document.createElement('div');
  row.className = 'wt-row' + (state.openWorktree === wt.name ? ' open' : '');
  row.dataset.name = wt.name;

  row.appendChild(cell('wt-name',
    `${wt.name}${wt.profile ? ' <span class="wt-profile">' + wt.profile + '</span>' : ''}`));
  row.appendChild(cell('', wt.provider ? `<span class="badge">${wt.provider}</span>` : ''));
  row.appendChild(cell('', `<span class="state-dot state-${wt.state}"></span>${wt.state}`));
  row.appendChild(cell('svc-dots', (wt.services || []).map(s =>
    `<span class="svc-dot ${s.status === 'running' ? 'running' : s.status === 'error' ? 'error' : ''}" title="${s.name} · ${s.status}${s.port ? ' · :' + s.port : ''}"></span>`
  ).join('')));
  row.appendChild(cell('endpoint-list', (wt.endpoints || []).map(ep =>
    `<a class="${ep.ok ? 'ok' : 'down'}" href="${ep.url}" target="_blank" rel="noopener">${ep.name}</a>`
  ).join('')));

  row.addEventListener('click', () => {
    state.openWorktree = state.openWorktree === wt.name ? null : wt.name;
    render(state.lastPayload);
  });

  const frag = document.createDocumentFragment();
  frag.appendChild(row);
  if (state.openWorktree === wt.name) frag.appendChild(renderDetail(wt));
  return frag;
}

function cell(cls, html) {
  const d = document.createElement('div');
  if (cls) d.className = cls;
  d.innerHTML = html;
  return d;
}

function renderDetail(wt) {
  const d = document.createElement('div');
  d.className = 'wt-detail';
  const svcRows = (wt.services || []).map(s =>
    `<tr><td>${s.name}</td><td>${s.status}</td><td>${s.port || ''}</td><td>${s.pid || ''}</td></tr>`
  ).join('') || '<tr><td colspan="4" class="empty">no services</td></tr>';
  const epRows = (wt.endpoints || []).map(e =>
    `<tr><td>${e.name}</td><td><a href="${e.url}" target="_blank" rel="noopener">${e.url}</a></td><td>${e.ok ? 'ok' : 'down'}</td></tr>`
  ).join('') || '<tr><td colspan="3" class="empty">no endpoints</td></tr>';
  d.innerHTML = `
    <table><thead><tr><th>service</th><th>status</th><th>port</th><th>pid</th></tr></thead><tbody>${svcRows}</tbody></table>
    <table style="margin-top:8px"><thead><tr><th>endpoint</th><th>url</th><th>ok</th></tr></thead><tbody>${epRows}</tbody></table>
  `;
  return d;
}

function renderSidebar(eco) {
  const shared = document.getElementById('shared');
  if (eco.shared_infra) {
    shared.innerHTML = `
      <div><strong>${eco.shared_infra.profile}</strong> <span class="muted">${eco.shared_infra.provider || ''}</span></div>
      <div class="muted">${eco.shared_infra.drift ? driftLine(eco.shared_infra.drift) : 'no drift cached'}</div>
    `;
  } else {
    shared.innerHTML = '<p class="empty">no shared profile</p>';
  }

  const orphans = eco.orphans || [];
  document.getElementById('orphan-count').textContent = orphans.length ? `(${orphans.length})` : '';
  const ol = document.getElementById('orphans');
  ol.innerHTML = orphans.length
    ? '<ul>' + orphans.map(o => `<li><strong>${o.name}</strong> <span class="muted">${o.category}</span></li>`).join('') + '</ul>'
    : '<p class="empty">none</p>';
}

function driftLine(d) {
  if (!d.has_drift) return 'clean';
  return `+${d.add} ~${d.change} -${d.destroy}`;
}

function formatTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return d.toLocaleTimeString();
}

poll();
setInterval(poll, 2000);
