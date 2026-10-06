const $ = (selector) => document.querySelector(selector);
let csrfToken = '';
let polling = null;

function text(selector, value) { $(selector).textContent = value ?? '—'; }
function bytes(value) {
  if (!Number.isFinite(value)) return '—';
  const units = ['B', 'KiB', 'MiB', 'GiB'];
  let size = value, unit = 0;
  while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit += 1; }
  return `${size.toFixed(unit > 1 ? 1 : 0)} ${units[unit]}`;
}
function duration(seconds) {
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  return days ? `${days}d ${hours}h` : `${hours}h ${minutes}m`;
}
async function json(url, options = {}) {
  const response = await fetch(url, options);
  if (response.status === 401) { window.location = '/login'; throw new Error('Authentication required'); }
  const data = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
  return data;
}
function setConnected(connected) {
  text('#connection', connected ? 'Connected' : 'Unavailable');
  $('#connection').classList.toggle('offline', !connected);
}
async function loadSystem() {
  try {
    const data = await json('/api/system');
    text('#hostname', data.hostname);
    text('#os', data.os);
    text('#firmware', data.firmware?.version);
    text('#build-id', `Build ${data.firmware?.build_id || '—'}`);
    text('#slot', data.rauc_slot);
    text('#root-device', `Root ${data.root || '—'}`);
    text('#cpu-load', Number(data.cpu?.load_1m ?? 0).toFixed(2));
    text('#cpu-model', `${data.cpu?.logical_cores || '—'} cores · ${data.cpu?.model || 'Unknown'}`);
    text('#memory', `${Number(data.memory?.usage_percent ?? 0).toFixed(1)}%`);
    text('#memory-detail', `${bytes(data.memory?.used_bytes)} of ${bytes(data.memory?.total_bytes)}`);
    text('#kernel', data.kernel);
    text('#architecture', data.cpu?.architecture);
    text('#uptime', duration(data.uptime_seconds || 0));
    text('#updated-at', `Last update ${data.firmware?.updated_at || '—'}`);
    setConnected(true);
  } catch (error) {
    setConnected(false);
    showMessage(error.message, true);
  }
}
function renderUpdate(data) {
  const phase = data.state || data.phase || 'idle';
  const badge = $('#update-state');
  badge.textContent = phase.charAt(0).toUpperCase() + phase.slice(1);
  badge.className = `state ${phase}`;
  if (data.bytes_received) text('#upload-detail', `${bytes(data.bytes_received)} received by device`);
  if (['uploading', 'installing'].includes(phase)) startPolling();
  if (phase === 'succeeded') showMessage('Firmware installed successfully. The device is ready to reboot.', false);
  if (phase === 'failed') showMessage(`Firmware installation failed${data.exit_code !== undefined ? ` (RAUC exit ${data.exit_code})` : ''}.`, true);
}
async function loadUpdate() {
  try { renderUpdate(await json('/api/update')); } catch (error) { showMessage(error.message, true); }
}
function startPolling() {
  if (polling) return;
  polling = window.setInterval(async () => {
    try {
      const data = await json('/api/update');
      renderUpdate(data);
      const phase = data.state || data.phase;
      if (!['uploading', 'installing'].includes(phase)) { clearInterval(polling); polling = null; loadSystem(); }
    } catch (_) { /* Keep polling through short service interruptions. */ }
  }, 2000);
}
function showMessage(message, error) {
  const box = $('#message');
  box.textContent = message;
  box.className = `alert ${error ? 'error' : 'success'}`;
}
function upload(file) {
  const request = new XMLHttpRequest();
  request.open('POST', '/api/firmware');
  request.setRequestHeader('Content-Type', 'application/octet-stream');
  request.setRequestHeader('X-CSRF-Token', csrfToken);
  request.upload.onprogress = (event) => {
    if (!event.lengthComputable) return;
    const percent = Math.round(event.loaded * 100 / event.total);
    $('#progress').style.width = `${percent}%`;
    text('#upload-detail', `${percent}% · ${bytes(event.loaded)} of ${bytes(event.total)}`);
  };
  request.onload = () => {
    $('#install').disabled = false;
    let data = {};
    try { data = JSON.parse(request.responseText); } catch (_) { /* handled below */ }
    if (request.status >= 200 && request.status < 300) {
      showMessage('Upload complete. RAUC installation has started.', false);
      renderUpdate(data);
      startPolling();
    } else showMessage(data.error || `Upload failed (${request.status}).`, true);
  };
  request.onerror = () => { $('#install').disabled = false; showMessage('The upload connection failed.', true); };
  request.send(file);
}

document.addEventListener('DOMContentLoaded', async () => {
  try { csrfToken = (await json('/api/session')).csrf_token; } catch (_) { return; }
  $('#refresh').addEventListener('click', () => { loadSystem(); loadUpdate(); });
  $('#firmware-file').addEventListener('change', (event) => {
    const file = event.target.files[0];
    text('#file-name', file ? `${file.name} · ${bytes(file.size)}` : 'No file selected');
    $('#install').disabled = !file;
  });
  $('#upload-form').addEventListener('submit', (event) => {
    event.preventDefault();
    const file = $('#firmware-file').files[0];
    if (!file || !window.confirm(`Install ${file.name}?`)) return;
    $('#install').disabled = true;
    $('#progress').style.width = '0';
    upload(file);
  });
  $('#logout-form').addEventListener('submit', (event) => {
    event.preventDefault();
    fetch('/logout', {method: 'POST', headers: {'X-CSRF-Token': csrfToken}}).then(() => { window.location = '/login'; });
  });
  await Promise.all([loadSystem(), loadUpdate()]);
});
