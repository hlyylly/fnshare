// Vanilla SPA. Pulls /v1 endpoints, renders into the static markup.

const fmtBytes = n => {
  if (!Number.isFinite(n)) return '-';
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${u[i]}`;
};

const fmtTime = ts => {
  if (!ts) return '-';
  const d = typeof ts === 'string' ? new Date(ts) : new Date(ts * 1000);
  return d.toLocaleString();
};

const shortPeer = id => id ? id.slice(0, 8) + '…' + id.slice(-6) : '-';
const shortGid  = id => id ? id.slice(0, 8) : '-';

function repBar(rep) {
  const r = Math.max(0, Math.min(100, Number(rep) || 0));
  const cls = r >= 70 ? '' : r >= 30 ? 'warn' : 'bad';
  return `<div class="rep-bar ${cls}" title="${r} / 100"><span style="width:${r}%"></span></div>`;
}
function offlineFor(since) {
  if (!since || since.startsWith('0001-')) return '';
  const ms = Date.now() - new Date(since).getTime();
  if (ms < 60_000)        return Math.round(ms/1000) + 's';
  if (ms < 3_600_000)     return Math.round(ms/60_000) + 'min';
  if (ms < 86_400_000)    return Math.round(ms/3_600_000) + 'h';
  return Math.round(ms/86_400_000) + 'd';
}
const escapeHTML = s => String(s ?? '').replace(/[&<>"']/g, c => ({
  '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
}[c]));

// ---- tabs ----
document.querySelectorAll('nav a').forEach(a => {
  a.addEventListener('click', e => {
    e.preventDefault();
    document.querySelectorAll('nav a').forEach(x => x.classList.remove('active'));
    document.querySelectorAll('.tab').forEach(x => x.classList.remove('active'));
    a.classList.add('active');
    document.getElementById(a.dataset.tab).classList.add('active');
  });
});

// ---- API ----
const api = {
  status:  () => fetch('/v1/status').then(r => r.json()),
  files:   () => fetch('/v1/files').then(r => r.json()),
  ledger:  () => fetch('/v1/ledger').then(r => r.json()),
  invite:  body => fetch('/v1/invite', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body),
  }).then(async r => {
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  }),
  createGroup: body => fetch('/v1/groups', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body),
  }).then(async r => {
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  }),
  joinGroup: body => fetch('/v1/groups/join', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body),
  }).then(async r => {
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  }),
  upload: (file, mode, groupID) => {
    const qs = new URLSearchParams({name: file.name, mode});
    if (groupID) qs.set('group', groupID);
    return fetch('/v1/files?' + qs.toString(), {
      method: 'POST', body: file,
      headers: {'Content-Type': 'application/octet-stream'},
    }).then(async r => {
      if (!r.ok) throw new Error(await r.text());
      return r.json();
    });
  },
};

// ---- state ----
let knownGroups = []; // [{id, name, is_admin, members:[]}]

// ---- renderers ----
async function renderOverview() {
  const s = await api.status();
  knownGroups = s.groups || [];
  document.getElementById('self-nickname').textContent = s.nickname || '-';
  document.getElementById('self-peerid').textContent = s.peer_id;
  document.getElementById('self-quota').textContent = fmtBytes(s.contributed_bytes);
  document.getElementById('self-connected').textContent = (s.connected_peers || []).length;

  const list = document.getElementById('groups-list');
  list.innerHTML = '';
  if (knownGroups.length === 0) {
    list.innerHTML = `<p class="muted">还没有加入任何群组。在容器里跑 <code>fnshare group-create</code> 或 <code>fnshare group-join</code>。</p>`;
  }
  knownGroups.forEach(g => {
    const card = document.createElement('div');
    card.className = 'group-card';
    const role = g.is_admin
      ? '<span class="tag tag-shared">群主</span>'
      : '<span class="tag tag-muted">成员</span>';
    const rows = (g.members || []).map(m => `
      <tr>
        <td><span class="dot ${m.is_online ? 'dot-on' : 'dot-off'}" title="${m.is_online ? '在线' : '离线'}"></span>${escapeHTML(m.nickname || '-')}</td>
        <td class="mono">${m.peer_id}</td>
        <td>${fmtBytes(m.contributed_bytes)}</td>
        <td>${repBar(m.reputation)}</td>
        <td class="muted">${m.is_online ? fmtTime(m.joined_at) : '离线 ' + offlineFor(m.offline_since)}</td>
      </tr>`).join('');
    card.innerHTML = `
      <div class="group-card-head">
        <h3>${escapeHTML(g.name)} ${role}</h3>
        <span class="mono muted">${g.id}</span>
      </div>
      <table>
        <thead><tr><th>状态 / 昵称</th><th>Peer ID</th><th>贡献</th><th>信誉</th><th>状态详情</th></tr></thead>
        <tbody>${rows || '<tr><td colspan="5" class="muted">尚无成员</td></tr>'}</tbody>
      </table>`;
    list.appendChild(card);
  });

  refreshGroupSelectors();
}

function refreshGroupSelectors() {
  const opt = (g) => `<option value="${g.id}">${escapeHTML(g.name)} (${shortGid(g.id)})</option>`;
  const upGroups = knownGroups.map(opt).join('');
  const adminGroups = knownGroups.filter(g => g.is_admin).map(opt).join('');

  const upSel = document.getElementById('upload-group');
  const invSel = document.getElementById('invite-group');
  upSel.innerHTML = upGroups || '<option value="">（先加入一个群）</option>';
  invSel.innerHTML = adminGroups || '<option value="">（你不是任何群的群主）</option>';
}

async function renderFiles() {
  const r = await api.files();
  const tbody = document.querySelector('#files-table tbody');
  tbody.innerHTML = '';
  (r.files || []).sort((a, b) => b.created_at - a.created_at).forEach(f => {
    const tag = f.mode === 'private'
      ? '<span class="tag tag-private">🔒 私有</span>'
      : '<span class="tag tag-shared">共享</span>';
    const fname = f.filename_encrypted ? '<span class="muted">&lt;加密&gt;</span>' : escapeHTML(f.filename || '-');
    const dlName = f.filename_encrypted ? f.file_id + '.bin' : (f.filename || f.file_id);
    const tr = document.createElement('tr');
    tr.innerHTML = `
      <td>${tag}</td>
      <td>${escapeHTML(f.group_name || shortGid(f.group_id))}</td>
      <td>${fname}</td>
      <td>${fmtBytes(f.size)}</td>
      <td class="mono"><a class="copy-id" data-id="${f.file_id}">${f.file_id.slice(0,16)}…</a></td>
      <td class="mono">${shortPeer(f.owner)}</td>
      <td><a href="/v1/files/${encodeURIComponent(f.file_id)}/content" download="${escapeHTML(dlName)}">下载</a></td>
    `;
    tbody.appendChild(tr);
  });
  tbody.querySelectorAll('a.copy-id').forEach(a => a.addEventListener('click', () => {
    navigator.clipboard.writeText(a.dataset.id);
    a.textContent = '已复制';
    setTimeout(() => { a.textContent = a.dataset.id.slice(0,16) + '…'; }, 1200);
  }));
}

async function renderLedger() {
  const r = await api.ledger();
  const tbody = document.querySelector('#ledger-table tbody');
  tbody.innerHTML = '';

  const entries = (r.entries || []).slice();
  const contrib = e => (e.stored_for_them_bytes || 0) + (e.downloaded_from_bytes || 0);
  const consume = e => (e.stored_on_them_bytes  || 0) + (e.served_to_them_bytes || 0);
  entries.sort((a, b) => (contrib(b) + consume(b)) - (contrib(a) + consume(a)));

  if (entries.length === 0) {
    tbody.innerHTML = `<tr><td colspan="5" class="muted">还没有任何流量记录。put / get 一个文件后回来看看。</td></tr>`;
    return;
  }
  entries.forEach(e => {
    const net = consume(e) - contrib(e);
    const cls = net >= 0 ? 'balance-pos' : 'balance-neg';
    const sign = net >= 0 ? '+' : '−';
    const tr = document.createElement('tr');
    tr.innerHTML = `
      <td class="mono">${shortPeer(e.peer_id)}</td>
      <td>${fmtBytes(contrib(e))}</td>
      <td>${fmtBytes(consume(e))}</td>
      <td class="${cls}">${sign}${fmtBytes(Math.abs(net))}</td>
      <td class="muted">${fmtTime(e.updated_at)}</td>
    `;
    tbody.appendChild(tr);
  });
}

// ---- forms ----
document.getElementById('upload-form').addEventListener('submit', async e => {
  e.preventDefault();
  const input = document.getElementById('upload-input');
  const isPrivate = document.getElementById('upload-private').checked;
  const groupID = document.getElementById('upload-group').value;
  const status = document.getElementById('upload-status');
  if (!input.files.length) return;
  if (!groupID) { status.textContent = '✗ 请选择上传到哪个群'; return; }
  status.textContent = `上传中 ${fmtBytes(input.files[0].size)}（${isPrivate ? '私有' : '共享'}）…`;
  try {
    const m = await api.upload(input.files[0], isPrivate ? 'private' : 'shared', groupID);
    status.textContent = `✓ 已上传 → ${m.file_id.slice(0,16)}…  [${m.mode}]`;
    input.value = '';
    document.getElementById('upload-private').checked = false;
    renderFiles();
  } catch (err) {
    status.textContent = '✗ ' + err.message;
  }
});

document.getElementById('create-group-form').addEventListener('submit', async e => {
  e.preventDefault();
  const status = document.getElementById('create-group-status');
  const name = document.getElementById('create-group-name').value.trim();
  if (!name) return;
  status.textContent = '建群中…';
  try {
    const g = await api.createGroup({ name });
    status.textContent = `✓ 已建群「${g.name}」（你是群主） — 去「邀请」页生成邀请链接发给朋友`;
    document.getElementById('create-group-name').value = '';
    refresh();
  } catch (err) { status.textContent = '✗ ' + err.message; }
});

document.getElementById('join-group-form').addEventListener('submit', async e => {
  e.preventDefault();
  const status = document.getElementById('join-group-status');
  const link = document.getElementById('join-group-link').value.trim();
  if (!link) return;
  status.textContent = '加入中…';
  try {
    const r = await api.joinGroup({ invite_link: link });
    status.textContent = `✓ 已加入「${r.group_name}」（${r.members} 个成员）`;
    document.getElementById('join-group-link').value = '';
    refresh();
  } catch (err) { status.textContent = '✗ ' + err.message; }
});

document.getElementById('invite-form').addEventListener('submit', async e => {
  e.preventDefault();
  const out = document.getElementById('invite-output');
  const groupID = document.getElementById('invite-group').value;
  if (!groupID) { out.textContent = '✗ 请先成为某个群的群主才能邀请'; return; }
  out.textContent = '生成中…';
  try {
    const r = await api.invite({
      group_id: groupID,
      bootstrap: [document.getElementById('invite-bootstrap').value],
      ttl_hours: Number(document.getElementById('invite-ttl').value),
    });
    out.textContent = r.link;
  } catch (err) {
    out.textContent = '✗ ' + err.message;
  }
});

// ---- refresh loop ----
function refresh() {
  renderOverview().catch(console.error);
  renderFiles().catch(console.error);
  renderLedger().catch(console.error);
}
refresh();
setInterval(refresh, 5000);
