// DOM helpers and display formatting.
const $ = (id) => document.getElementById(id);
const escapeHTML = (value) =>
  String(value ?? '').replace(
    /[&<>"']/g,
    (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c],
  );
const bytes = (n) => {
  if (!n) return '0 B';
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), 4);
  return `${(n / 1024 ** i).toLocaleString(undefined, { maximumFractionDigits: i ? 1 : 0 })} ${['B', 'KiB', 'MiB', 'GiB', 'TiB'][i]}`;
};
const date = (value) => {
  const d = new Date(value);
  return Number.isNaN(d.valueOf())
    ? String(value)
    : d.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
};

// Session identity and shared dashboard state.
const client = `console-${globalThis.crypto?.randomUUID?.() || Math.random().toString(36).slice(2)}`;
let volumes = [];
let volume = '';
let dashboard = null;
let busy = false;
let generation = 0;
let submitModal = null;
const base = (name = volume) => `/v1/volumes/${encodeURIComponent(name)}`;
const fileURL = (path, name = volume) =>
  `${base(name)}/files/${path.split('/').map(encodeURIComponent).join('/')}`;

// HTTP requests, error messages, and mutation controls.
async function api(url, { method = 'GET', json, body, headers = {}, signal } = {}) {
  const response = await fetch(url, {
    method,
    body: json === undefined ? body : JSON.stringify(json),
    headers: { ...(json === undefined ? {} : { 'Content-Type': 'application/json' }), ...headers },
    signal,
    cache: 'no-store',
  });
  if (!response.ok) {
    const text = await response.text();
    let message = `Request failed (${response.status}).`;
    try {
      message = JSON.parse(text).error || message;
    } catch {
      if (!text.trim().startsWith('<')) message = text.slice(0, 500) || message;
    }
    throw new Error(message || `Request failed (${response.status})`);
  }
  return response.status === 204 ? null : response.json();
}

function notice(message, error = false) {
  $('notice').textContent = message;
  $('notice').className = `notice${error ? ' danger' : ''}`;
  $('notice').hidden = false;
}

function controls() {
  for (const id of ['volume-select', 'create-volume', 'refresh']) $(id).disabled = busy;
  $('upload-button').disabled = busy || !volume || !!dashboard?.volume.alert_open;
  $('snapshot-button').disabled = busy || !volume;
  $('file-rows')
    .querySelectorAll('button')
    .forEach(
      (b) =>
        (b.disabled = busy || (!!dashboard?.volume.alert_open && b.dataset.action === 'delete')),
    );
  $('snapshot-rows')
    .querySelectorAll('button')
    .forEach((b) => (b.disabled = busy));
}

async function operation(action) {
  if (busy) return;
  busy = true;
  controls();
  try {
    await action();
  } catch (error) {
    notice(error.message, true);
  } finally {
    busy = false;
    controls();
  }
}

// Shared dialog lifecycle. Keep dialogs open while an action is pending.
function modal(title, content, confirm, handler) {
  $('modal-title').textContent = title;
  $('modal-content').innerHTML = content;
  $('modal-error').hidden = true;
  $('confirm-modal').textContent = confirm || 'Confirm';
  $('confirm-modal').hidden = !handler;
  $('cancel-modal').textContent = handler ? 'Cancel' : 'Close';
  submitModal = handler;
  $('modal').showModal();
}

function closeModal() {
  if (!$('confirm-modal').disabled) $('modal').close();
}
$('close-modal').onclick = closeModal;
$('cancel-modal').onclick = closeModal;
$('modal').addEventListener('cancel', (e) => {
  if ($('confirm-modal').disabled) e.preventDefault();
});
$('modal-form').onsubmit = async (e) => {
  e.preventDefault();
  if (!submitModal || $('confirm-modal').disabled) return;
  $('confirm-modal').disabled = true;
  $('modal-error').hidden = true;
  try {
    await submitModal(new FormData(e.target));
    $('modal').close();
  } catch (error) {
    $('modal-error').textContent = error.message;
    $('modal-error').hidden = false;
  } finally {
    $('confirm-modal').disabled = false;
  }
};

// Render files, storage totals, and snapshots from the API response.
function render() {
  const data = dashboard?.storage;
  $('stat-files').textContent = data
    ? data.files.filter((f) => !f.deleted).length.toLocaleString()
    : '—';
  $('stat-current').textContent = data ? bytes(data.current_bytes) : '—';
  $('stat-stored').textContent = data ? bytes(data.stored_bytes) : '—';
  $('stat-versions').textContent = data ? data.version_count.toLocaleString() : '—';
  $('volume-caption').textContent = volume
    ? `${volume} /`
    : 'Select or create a volume to get started.';
  $('alert').hidden = !dashboard?.volume.alert_open;
  renderFiles();
  renderSnapshots();
  controls();
}

function renderFiles() {
  const query = $('search').value.toLowerCase();
  const files = (dashboard?.storage.files || []).filter(
    (f) => (!f.deleted || $('show-deleted').checked) && f.path.toLowerCase().includes(query),
  );
  $('file-count').textContent = files.length;
  $('file-rows').innerHTML = files
    .map(
      (f) => /* HTML */ `
        <tr>
          <td>
            <div class="file-name">
              <span class="file-icon" aria-hidden="true">▤</span>
              <span class="path-text" title="${escapeHTML(f.path)}">${escapeHTML(f.path)}</span>
            </div>
          </td>
          <td>${f.deleted ? '—' : bytes(f.size)}</td>
          <td>
            <span class="badge${f.deleted ? ' deleted' : ''}">
              ${f.deleted ? 'Deleted · ' : ''}v${f.current_version}
            </span>
          </td>
          <td>${escapeHTML(date(f.updated_at))}</td>
          <td>
            <div class="row-actions">
              ${
                !f.deleted
                  ? /* HTML */ `
                      <button
                        class="text-button"
                        data-action="download"
                        data-path="${escapeHTML(f.path)}"
                        aria-label="Download ${escapeHTML(f.path)}"
                      >
                        Download
                      </button>
                    `
                  : ''
              }
              <button
                class="text-button"
                data-action="history"
                data-path="${escapeHTML(f.path)}"
                aria-label="Version history for ${escapeHTML(f.path)}"
              >
                History
              </button>
              ${
                !f.deleted
                  ? /* HTML */ `
                      <button
                        class="text-button danger"
                        data-action="delete"
                        data-path="${escapeHTML(f.path)}"
                        aria-label="Delete ${escapeHTML(f.path)}"
                      >
                        Delete
                      </button>
                    `
                  : ''
              }
            </div>
          </td>
        </tr>
      `,
    )
    .join('');
  $('file-empty').hidden = files.length > 0;
  $('file-empty').querySelector('h3').textContent = !dashboard
    ? 'Choose your volume'
    : query
      ? 'No matching files'
      : 'A home for your files';
  $('file-empty').querySelector('p').textContent = !dashboard
    ? 'Select or create a volume to get started.'
    : query
      ? 'Try another search or show deleted files.'
      : 'Upload files or drop them here. Enable “Show deleted files” to find older history.';
  $('file-footer').textContent = volume
    ? `${files.length} shown · ${dashboard?.storage.chunk_count ?? 0} stored chunks`
    : 'No volume selected';
  controls();
}

function renderSnapshots() {
  const snapshots = dashboard?.storage.snapshots || [];
  $('snapshot-rows').innerHTML = snapshots
    .map(
      (s) => /* HTML */ `
        <tr>
          <td>
            <strong>#${s.id}</strong>
            · ${escapeHTML(s.reason)}
          </td>
          <td>${s.commit_seq}</td>
          <td><span class="badge">${s.protected ? 'Object Lock requested' : 'Standard'}</span></td>
          <td>${escapeHTML(date(s.created_at))}</td>
          <td class="right">
            <button class="text-button" data-snapshot="${s.id}">Restore</button>
          </td>
        </tr>
      `,
    )
    .join('');
  $('snapshot-empty').hidden = snapshots.length > 0;
}

// Load volume data and discard stale responses after a volume change.
async function loadVolumes(preferred = volume) {
  const result = await api('/v1/volumes');
  volumes = result.volumes;
  volume = volumes.some((v) => v.name === preferred) ? preferred : volumes[0]?.name || '';
  $('volume-select').innerHTML = volumes.length
    ? volumes
        .map(
          (v) => /* HTML */ `
            <option value="${escapeHTML(v.name)}">${escapeHTML(v.name)}</option>
          `,
        )
        .join('')
    : '<option value="">No volumes yet</option>';
  $('volume-select').value = volume;
  if (volume) localStorage.setItem('edgevault-volume', volume);
}

async function refresh() {
  const request = ++generation,
    name = volume;
  dashboard = null;
  render();
  if (!name) return;
  try {
    const result = await api(`${base(name)}/dashboard`);
    if (request !== generation || name !== volume) return;
    dashboard = result;
    render();
  } catch (error) {
    if (request === generation) notice(error.message, true);
  }
}

// Service readiness and process-wide metrics.
async function health() {
  const results = await Promise.allSettled([
    fetch('/healthz', { cache: 'no-store' }),
    fetch('/readyz', { cache: 'no-store' }),
    fetch('/metrics', { cache: 'no-store' }),
  ]);
  const alive = results[0].status === 'fulfilled' && results[0].value.ok;
  const ready = results[1].status === 'fulfilled' && results[1].value.ok;
  $('service-label').textContent = ready
    ? 'Service ready'
    : alive
      ? 'Database unavailable'
      : 'Service unreachable';
  $('service-dot').className = `dot ${ready ? 'online' : 'offline'}`;
  let measurements = {};
  if (results[2].status === 'fulfilled' && results[2].value.ok) {
    const text = await results[2].value.text();
    for (const line of text.split('\n')) {
      const m = line.match(/^(\w+)\s+([\d.e+-]+)$/);
      if (m) measurements[m[1]] = Number(m[2]);
    }
  }
  const hits = measurements.edge_cache_hits_total,
    misses = measurements.edge_cache_misses_total;
  const values = [
    ['Go API', alive ? 'Reachable' : 'Unreachable'],
    ['Database readiness', ready ? 'Ready' : 'Unavailable'],
    ['API origin', location.origin],
    ['Stored chunk backend', 'Configured by the Go service (Silo or S3)'],
    [
      'Process chunk reuse',
      measurements.edge_dedup_ratio === undefined
        ? 'Unavailable'
        : `${(measurements.edge_dedup_ratio * 100).toFixed(1)}%`,
    ],
    ['Cache hits / misses', hits === undefined ? 'Unavailable' : `${hits} / ${misses}`],
    ['Upload queue', measurements.edge_upload_queue_depth ?? 'Unavailable'],
    ['Ransomware alerts since startup', measurements.edge_ransomware_alerts_total ?? 'Unavailable'],
  ];
  $('system-details').innerHTML = values
    .map(
      ([k, v]) => /* HTML */ `
        <dt>${escapeHTML(k)}</dt>
        <dd>${escapeHTML(v)}</dd>
      `,
    )
    .join('');
}

// Navigation, filtering, and volume selection.
$('volume-select').onchange = async () => {
  volume = $('volume-select').value;
  localStorage.setItem('edgevault-volume', volume);
  $('notice').hidden = true;
  await refresh();
};
$('search').oninput = renderFiles;
$('show-deleted').onchange = renderFiles;
$('refresh').onclick = () =>
  operation(async () => {
    await loadVolumes();
    await refresh();
    await health();
  });
for (const button of document.querySelectorAll('[data-view]'))
  button.onclick = () => {
    for (const b of document.querySelectorAll('[data-view]'))
      b.classList.toggle('active', b === button);
    for (const view of document.querySelectorAll('.view'))
      view.hidden = view.id !== `${button.dataset.view}-view`;
    const titles = {
      files: ['File explorer', 'A clear view of your files. Every version within reach.'],
      recovery: ['Snapshots & recovery', 'Preserve a recovery point. Return to a cleaner state.'],
      system: ['System status', 'Check the service behind your storage.'],
    };
    $('page-title').textContent = $('breadcrumb').textContent = titles[button.dataset.view][0];
    $('page-description').textContent = titles[button.dataset.view][1];
    if (button.dataset.view === 'system') health();
  };
$('create-volume').onclick = () =>
  modal(
    'Create a volume',
    '<p class="dialog-description">A volume groups files, encryption keys, and recovery history.</p><label class="field">Volume name<input name="name" required maxlength="80" pattern="[A-Za-z0-9][A-Za-z0-9_-]*" placeholder="e.g. personal-documents" autocomplete="off"><small>Use letters, numbers, hyphens, or underscores.</small></label>',
    'Create volume',
    async (form) => {
      const name = form.get('name').trim();
      if (!/^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$/.test(name))
        throw new Error('Enter a valid volume name.');
      if (volumes.some((v) => v.name === name))
        throw new Error('A volume with that name already exists.');
      await api('/v1/volumes', { method: 'POST', json: { name } });
      await loadVolumes(name);
      await refresh();
      notice(`Created volume “${name}”.`);
    },
  );

// File mutations must acquire a lease and release it even after failure.
async function withLease(path, action) {
  const url = fileURL(path),
    headers = { 'X-Edge-Node': client };
  const lease = await api(`${url}/lease`, { method: 'POST', json: { ttl_seconds: 300 }, headers });
  try {
    return await action(url, { ...headers, 'X-Edge-Lease-Token': String(lease.token) });
  } finally {
    try {
      await api(`${url}/lease`, {
        method: 'DELETE',
        headers: { ...headers, 'X-Edge-Lease-Token': String(lease.token) },
      });
    } catch {
      notice(
        'Operation finished, but its lease could not be released. It will expire automatically.',
        true,
      );
    }
  }
}

async function download(path, version) {
  const response = await fetch(`${fileURL(path)}${version ? `?version=${version}` : ''}`);
  if (!response.ok) throw new Error(`Download failed (${response.status}).`);
  const url = URL.createObjectURL(await response.blob());
  const a = document.createElement('a');
  a.href = url;
  a.download = path.split('/').pop();
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 60000);
}
$('file-rows').onclick = (event) => {
  const button = event.target.closest('[data-action]');
  if (!button || busy) return;
  const path = button.dataset.path;
  if (button.dataset.action === 'download') return operation(() => download(path));
  if (button.dataset.action === 'delete')
    return modal(
      'Delete file?',
      /* HTML */ `
        <p class="dialog-description">
          Delete
          <strong>${escapeHTML(path)}</strong>
          from
          <strong>${escapeHTML(volume)}</strong>
          ? Retained versions remain available in History.
        </p>
      `,
      'Delete file',
      async () => {
        await withLease(path, (url, headers) => api(url, { method: 'DELETE', headers }));
        await refresh();
        notice(`Deleted “${path}”.`);
      },
    );
  operation(async () => {
    const result = await api(`${fileURL(path)}/versions`);
    modal(
      `History · ${path}`,
      result.versions
        .map(
          (v) => /* HTML */ `
            <div class="history-item">
              <div>
                <strong>Version ${v.version}</strong>
                <span class="badge${v.is_deleted ? ' deleted' : ''}">
                  ${v.is_deleted ? 'Deleted' : bytes(v.size)}
                </span>
                <small>${escapeHTML(v.created_at)} · Commit ${v.commit_seq}</small>
              </div>
              ${
                !v.is_deleted
                  ? /* HTML */ `
                      <button type="button" class="button small" data-version="${v.version}">
                        Download
                      </button>
                    `
                  : ''
              }
            </div>
          `,
        )
        .join('') || '<p>No retained versions.</p>',
      null,
      null,
    );
    $('modal-content')
      .querySelectorAll('[data-version]')
      .forEach((b) => (b.onclick = () => operation(() => download(path, b.dataset.version))));
  });
};

// Validate destinations against the existing API path rules.
function validatePath(path) {
  const parts = path.split('/');
  if (
    !path ||
    path.startsWith('/') ||
    parts.some((p) => !p || p === '.' || p === '..') ||
    /[\\\x00-\x1f?#]/.test(path)
  )
    throw new Error(
      'Use a relative file path without empty segments, dot segments, backslashes, ?, or #.',
    );
  if (
    [
      'versions',
      'lease',
      'state',
      'snapshots',
      'restore',
      'retention',
      'dashboard',
      'files',
    ].includes(parts.at(-1))
  )
    throw new Error('That final path segment is reserved by the API. Rename the file.');
}

// Upload sequentially so partial completion can be reported accurately.
function uploadFiles(list) {
  const files = Array.from(list);
  if (!files.length || busy || !dashboard || dashboard.volume.alert_open) return;
  const single = files.length === 1;
  modal(
    single ? 'Upload file' : 'Upload files',
    /* HTML */ `
      <p class="dialog-description">
        ${single ? escapeHTML(files[0].name) : `${files.length} files`} ·
        ${bytes(files.reduce((n, f) => n + f.size, 0))}
      </p>
      <label class="field">
        ${single ? 'Destination path' : 'Destination folder (optional)'}
        <input
          name="destination"
          ${single ? 'required' : ''}
          value="${single ? escapeHTML(files[0].name) : ''}"
          placeholder="documents/${single ? 'report.pdf' : ''}"
        />
        <small>
          Existing paths create new versions. Unchanged content reuses the current version.
        </small>
      </label>
      <p id="upload-status" class="dialog-description"></p>
      <progress id="upload-progress" max="${files.length}" value="0" hidden></progress>
    `,
    'Upload',
    async (form) => {
      const destination = form.get('destination').trim();
      const paths = files.map((f) =>
        single
          ? destination
          : `${destination ? destination.replace(/\/$/, '') + '/' : ''}${f.name}`,
      );
      paths.forEach(validatePath);
      if (new Set(paths).size !== paths.length)
        throw new Error(
          'Multiple selected files have the same destination path. Upload them separately.',
        );
      const existing = paths.filter((path) =>
        dashboard.storage.files.some((f) => f.path === path && !f.deleted),
      );
      if (
        existing.length &&
        !window.confirm(`Create new versions of ${existing.length} existing file(s)?`)
      )
        return Promise.reject(new Error('Upload cancelled.'));
      busy = true;
      controls();
      let completed = 0;
      try {
        $('upload-progress').hidden = false;
        for (let i = 0; i < files.length; i++) {
          $('upload-status').textContent = `Uploading ${i + 1} of ${files.length}: ${paths[i]}`;
          const prior = dashboard.storage.files.find((f) => f.path === paths[i]);
          await withLease(paths[i], (url, headers) =>
            api(url, {
              method: 'PUT',
              body: files[i],
              headers: {
                ...headers,
                'Content-Type': 'application/octet-stream',
                'If-Match': String(prior?.current_version || 0),
              },
            }),
          );
          completed++;
          $('upload-progress').value = completed;
        }
        notice(`Uploaded ${completed} file${completed === 1 ? '' : 's'} to “${volume}”.`);
      } catch (error) {
        throw new Error(`${completed} of ${files.length} uploaded. ${error.message}`);
      } finally {
        await refresh();
        await health();
        busy = false;
        controls();
      }
    },
  );
}
$('upload-button').onclick = () => $('file-input').click();
$('file-input').onchange = () => {
  uploadFiles($('file-input').files);
  $('file-input').value = '';
};
const drop = $('drop-zone');
drop.ondragover = (e) => {
  e.preventDefault();
  if (!busy && dashboard && !dashboard.volume.alert_open) drop.classList.add('dragging');
};
drop.ondragleave = () => drop.classList.remove('dragging');
drop.ondrop = (e) => {
  e.preventDefault();
  drop.classList.remove('dragging');
  uploadFiles(e.dataTransfer.files);
};

// Snapshot creation and full-volume recovery.
$('snapshot-button').onclick = () =>
  modal(
    'Create a snapshot',
    '<p class="dialog-description">Capture the current file state for this volume.</p><label class="field">Reason<input name="reason" required value="Manual recovery point" maxlength="200"></label><label class="field check"><input type="checkbox" name="protected" checked> Request protected snapshot (seven-day Object Lock)</label><p class="dialog-description">Protected snapshots require an Object Lock capable bucket. Standard snapshots record a recovery point without Object Lock protection.</p>',
    'Create snapshot',
    async (form) => {
      const result = await api(`${base()}/snapshots`, {
        method: 'POST',
        json: { reason: form.get('reason'), protected: form.has('protected') },
      });
      await refresh();
      notice(`Created snapshot #${result.id} at commit ${result.commit_seq}.`);
    },
  );
$('snapshot-rows').onclick = (event) => {
  const button = event.target.closest('[data-snapshot]');
  if (!button || busy) return;
  const snapshot = dashboard.storage.snapshots.find(
    (s) => String(s.id) === button.dataset.snapshot,
  );
  modal(
    `Restore snapshot #${snapshot.id}?`,
    /* HTML */ `
      <p class="dialog-description">
        Restore
        <strong>${escapeHTML(volume)}</strong>
        to commit
        <strong>${snapshot.commit_seq}</strong>
        . This creates new versions and removes current files that did not exist at that point.
      </p>
      <label class="field">
        Type the volume name to confirm
        <input name="confirmation" required autocomplete="off" />
      </label>
    `,
    'Restore volume',
    async (form) => {
      if (form.get('confirmation') !== volume) throw new Error('The volume name does not match.');
      const result = await api(`${base()}/restore`, {
        method: 'POST',
        json: { snapshot_id: snapshot.id },
      });
      await refresh();
      await health();
      notice(`Restore complete. ${result.files_changed} file(s) changed.`);
    },
  );
};

// Initial render and first connection to the Go API.
render();
(async () => {
  try {
    await loadVolumes(localStorage.getItem('edgevault-volume'));
    await refresh();
  } catch (error) {
    notice(`Could not connect to the storage service: ${error.message}`, true);
  }
  await health();
})();
