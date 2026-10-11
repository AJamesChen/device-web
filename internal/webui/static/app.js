const $ = (selector) => document.querySelector(selector);
let csrfToken = '';
let polling = null;
let selectedFirmware = null;
let cameraObjectURL = '';
let cameraPlaying = false;
const maxFirmwareSize = 384 * 1024 * 1024;

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
  if (phase === 'uploading') {
    showMessage('Firmware upload is in progress.', false);
    startPolling();
  }
  if (phase === 'installing') {
    showMessage('Upload complete. RAUC is verifying and installing the firmware.', false);
    startPolling();
  }
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
function clearMessage() {
  const box = $('#message');
  box.textContent = '';
  box.className = 'alert hidden';
}
function showCameraMessage(message, error = false) {
  const box = $('#camera-message');
  box.textContent = message;
  box.className = `alert ${error ? 'error' : 'success'}`;
}
function clearCameraMessage() {
  const box = $('#camera-message');
  box.textContent = '';
  box.className = 'alert hidden';
}
function renderCameraState(state) {
  cameraPlaying = state === 'playing';
  const badge = $('#camera-state');
  badge.textContent = cameraPlaying ? 'Playing' : 'Stopped';
  badge.className = `state ${cameraPlaying ? 'succeeded' : 'neutral'}`;
  $('#camera-play').disabled = cameraPlaying;
  $('#camera-stop').disabled = !cameraPlaying;
}
async function playCamera() {
  clearCameraMessage();
  try {
    await stopCameraPlayback({quiet: true});
  } catch (error) {
    showCameraMessage(error.message, true);
    return;
  }
  if (cameraObjectURL) {
    URL.revokeObjectURL(cameraObjectURL);
    cameraObjectURL = '';
  }
  $('#camera-download').classList.add('hidden');
  const feed = $('#camera-feed');
  feed.classList.remove('hidden');
  $('#camera-placeholder').classList.add('hidden');
  renderCameraState('playing');
  feed.src = `/api/camera/stream?cache=${Date.now()}`;
}
async function stopCameraPlayback({quiet = false} = {}) {
  const feed = $('#camera-feed');
  feed.removeAttribute('src');
  feed.classList.add('hidden');
  $('#camera-placeholder').classList.remove('hidden');
  renderCameraState('stopped');
  try {
    await json('/api/camera/stop', {method: 'POST', headers: {'X-CSRF-Token': csrfToken}});
    if (!quiet) showCameraMessage('Camera playback stopped.');
  } catch (error) {
    if (!quiet) showCameraMessage(error.message, true);
    throw error;
  }
}
async function snapCamera() {
  const button = $('#camera-snapshot');
  button.disabled = true;
  clearCameraMessage();
  try {
    await stopCameraPlayback({quiet: true});
    const response = await fetch('/api/camera/snapshot', {
      method: 'POST',
      headers: {'X-CSRF-Token': csrfToken},
    });
    if (response.status === 401) { window.location = '/login'; return; }
    if (!response.ok) {
      const data = await response.json().catch(() => ({}));
      throw new Error(data.error || `Snapshot failed (${response.status})`);
    }
    const image = await response.blob();
    if (cameraObjectURL) URL.revokeObjectURL(cameraObjectURL);
    cameraObjectURL = URL.createObjectURL(image);
    const feed = $('#camera-feed');
    feed.src = cameraObjectURL;
    feed.classList.remove('hidden');
    $('#camera-placeholder').classList.add('hidden');
    const download = $('#camera-download');
    download.href = cameraObjectURL;
    download.classList.remove('hidden');
    showCameraMessage('Picture captured. Use Download picture to save it.');
  } catch (error) {
    showCameraMessage(error.message, true);
  } finally {
    button.disabled = false;
  }
}
function selectTab(tabID) {
  document.querySelectorAll('[role="tab"]').forEach((tab) => {
    const selected = tab.dataset.tab === tabID;
    tab.classList.toggle('active', selected);
    tab.setAttribute('aria-selected', String(selected));
  });
  document.querySelectorAll('.tab-panel').forEach((panel) => panel.classList.toggle('hidden', panel.id !== tabID));
  if (tabID !== 'camera-panel' && cameraPlaying) stopCameraPlayback({quiet: true}).catch(() => {});
}
function selectFirmware(file) {
  let error = '';
  if (!file) error = 'Select a firmware bundle.';
  else if (!file.name.toLowerCase().endsWith('.raucb')) error = 'Firmware must be a .raucb bundle.';
  else if (file.size === 0) error = 'The selected firmware bundle is empty.';
  else if (file.size > maxFirmwareSize) error = `Firmware exceeds the ${bytes(maxFirmwareSize)} upload limit.`;

  selectedFirmware = error ? null : file;
  text('#file-name', selectedFirmware ? `${file.name} · ${bytes(file.size)}` : 'No file selected');
  $('#install').disabled = !selectedFirmware;
  if (error) showMessage(error, true);
  else clearMessage();
  return !error;
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
      renderUpdate(data);
      startPolling();
    } else showMessage(data.error || `Upload failed (${request.status}).`, true);
  };
  request.onerror = () => reconcileUploadStatus(file.size);
  request.send(file);
}

async function reconcileUploadStatus(expectedBytes) {
  $('#install').disabled = false;
  try {
    const data = await json('/api/update');
    const phase = data.state || data.phase;
    const receivedExpectedBundle = Number(data.bytes_received) === expectedBytes;
    if (phase === 'uploading' || (receivedExpectedBundle && ['installing', 'succeeded', 'failed'].includes(phase))) {
      renderUpdate(data);
      return;
    }
  } catch (_) { /* Report the original connection failure below. */ }
  showMessage('The upload connection failed and its installation status could not be confirmed.', true);
}

const delay = (milliseconds) => new Promise((resolve) => window.setTimeout(resolve, milliseconds));
async function waitForReboot() {
  let wentOffline = false;
  for (let attempt = 0; attempt < 90; attempt += 1) {
    await delay(2000);
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 3000);
    try {
      const response = await fetch('/health', {cache: 'no-store', signal: controller.signal});
      if (wentOffline && response.ok) { window.location = '/login'; return; }
    } catch (_) {
      wentOffline = true;
      setConnected(false);
    } finally {
      window.clearTimeout(timeout);
    }
  }
  showMessage('The device has not returned yet. Reload this page after checking its power and network connection.', true);
}
async function rebootDevice() {
  if (!window.confirm('Reboot this device now?')) return;
  const button = $('#reboot');
  button.disabled = true;
  button.textContent = 'Rebooting…';
  try {
    await json('/api/reboot', {method: 'POST', headers: {'X-CSRF-Token': csrfToken}});
    if (polling) { clearInterval(polling); polling = null; }
    showMessage('Reboot requested. Waiting for the device to return…', false);
    waitForReboot();
  } catch (error) {
    button.disabled = false;
    button.textContent = 'Reboot';
    showMessage(error.message, true);
  }
}

document.addEventListener('DOMContentLoaded', async () => {
  try { csrfToken = (await json('/api/session')).csrf_token; } catch (_) { return; }
  $('#refresh').addEventListener('click', () => { loadSystem(); loadUpdate(); });
  document.querySelectorAll('[role="tab"]').forEach((tab) => tab.addEventListener('click', () => selectTab(tab.dataset.tab)));
  $('#camera-play').addEventListener('click', playCamera);
  $('#camera-stop').addEventListener('click', () => stopCameraPlayback().catch(() => {}));
  $('#camera-snapshot').addEventListener('click', snapCamera);
  $('#camera-feed').addEventListener('error', () => {
    if (!cameraPlaying) return;
    renderCameraState('stopped');
    $('#camera-feed').classList.add('hidden');
    $('#camera-placeholder').classList.remove('hidden');
    showCameraMessage('The camera stream could not be opened.', true);
  });
  const fileInput = $('#firmware-file');
  const dropzone = $('#firmware-dropzone');
  let dragDepth = 0;
  fileInput.addEventListener('change', (event) => selectFirmware(event.target.files[0]));
  dropzone.addEventListener('dragenter', (event) => {
    event.preventDefault();
    dragDepth += 1;
    dropzone.classList.add('drag-over');
  });
  dropzone.addEventListener('dragover', (event) => {
    event.preventDefault();
    event.dataTransfer.dropEffect = 'copy';
  });
  dropzone.addEventListener('dragleave', (event) => {
    event.preventDefault();
    dragDepth = Math.max(0, dragDepth - 1);
    if (dragDepth === 0) dropzone.classList.remove('drag-over');
  });
  dropzone.addEventListener('drop', (event) => {
    event.preventDefault();
    dragDepth = 0;
    dropzone.classList.remove('drag-over');
    const files = event.dataTransfer.files;
    if (files.length !== 1) {
      selectFirmware(null);
      showMessage('Drop exactly one .raucb bundle.', true);
      return;
    }
    selectFirmware(files[0]);
  });
  $('#upload-form').addEventListener('submit', (event) => {
    event.preventDefault();
    const file = selectedFirmware;
    if (!file || !window.confirm(`Install ${file.name}?`)) return;
    $('#install').disabled = true;
    $('#progress').style.width = '0';
    upload(file);
  });
  $('#reboot').addEventListener('click', rebootDevice);
  $('#logout-form').addEventListener('submit', (event) => {
    event.preventDefault();
    fetch('/logout', {method: 'POST', headers: {'X-CSRF-Token': csrfToken}}).then(() => { window.location = '/login'; });
  });
  await Promise.all([loadSystem(), loadUpdate()]);
});
