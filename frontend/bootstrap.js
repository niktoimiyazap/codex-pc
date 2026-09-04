(() => {
  const gate = document.getElementById('bootGate');
  const settingsButton = document.getElementById('settingsButton');
  const savedTheme = localStorage.getItem('codexpc-theme');
  if (savedTheme === 'light') document.body.classList.add('light');

  function loadMonitor() {
    settingsButton?.addEventListener('click', () => { window.location.href = '/setup/'; });
    const script = document.createElement('script');
    script.src = '/monitor.js';
    script.defer = true;
    document.body.appendChild(script);
  }

  function showFailure(message) {
    if (!gate) return;
    gate.classList.add('failed');
    gate.innerHTML = `<div class="boot-failure"><strong>CodexPC could not start</strong><span>${message}</span><button type="button" id="bootRetry">Retry</button></div>`;
    document.getElementById('bootRetry')?.addEventListener('click', () => window.location.reload());
  }

  async function boot() {
    try {
      const response = await fetch('/setup', {cache: 'no-store'});
      if (!response.ok) throw new Error(`Setup status returned HTTP ${response.status}`);
      const status = await response.json();
      if (!status.configured) {
        window.location.replace('/setup/');
        return;
      }
      document.body.classList.remove('booting');
      gate?.classList.add('leaving');
      window.setTimeout(() => gate?.remove(), 180);
      loadMonitor();
    } catch (error) {
      showFailure(error?.message || String(error));
    }
  }

  boot();
})();
