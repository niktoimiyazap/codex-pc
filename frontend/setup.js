(() => {
  const form = document.getElementById('setupForm');
  const page = document.getElementById('setupPage');
  const loader = document.getElementById('setupLoader');
  const exit = document.getElementById('setupExit');
  const cancelButton = document.getElementById('setupCancel');
  const backButton = document.getElementById('setupBack');
  const nextButton = document.getElementById('setupNext');
  const errorBox = document.getElementById('setupError');
  const completeText = document.getElementById('setupCompleteText');
  const workspaceInput = document.getElementById('setupWorkspace');
  const toolProfileInput = document.getElementById('setupToolProfile');
  const toolProfileControl = document.getElementById('setupToolProfileControl');
  const toolProfileButton = document.getElementById('setupToolProfileButton');
  const toolProfileDisplay = document.getElementById('setupToolProfileDisplay');
  const toolProfileMenu = document.getElementById('setupToolProfileMenu');
  const toolProfileOptions = [...document.querySelectorAll('.setup-option')];
  const tunnelIdInput = document.getElementById('setupTunnelId');
  const apiKeyInput = document.getElementById('setupApiKey');
  const tunnelProfileInput = document.getElementById('setupTunnelProfile');
  const organizationInput = document.getElementById('setupOrganization');
  const keyNote = document.getElementById('setupKeyNote');
  const steps = [...document.querySelectorAll('[data-setup-step]')];
  const dots = [...document.querySelectorAll('[data-setup-dot]')];
  if (!form || !page || !loader) return;

  let currentStep = 0;
  let status = null;
  let saving = false;
  let stepAnimating = false;
  let optionIndex = 0;
  const params = new URLSearchParams(window.location.search);
  const previewMode = params.get('preview') === '1' || params.get('setup') === 'preview';

  if (localStorage.getItem('codexpc-theme') === 'light') document.body.classList.add('light');

  function showError(message = '') {
    errorBox.textContent = message;
    errorBox.classList.toggle('visible', Boolean(message));
  }

  function closeDropdown() {
    toolProfileControl?.classList.remove('open');
    toolProfileMenu?.classList.add('hidden');
    toolProfileButton?.setAttribute('aria-expanded', 'false');
    toolProfileOptions.forEach(option => option.classList.remove('focused'));
  }

  function selectToolProfile(value, focusButton = false) {
    const option = toolProfileOptions.find(item => item.dataset.value === value) || toolProfileOptions[0];
    if (!option || !toolProfileInput) return;
    toolProfileInput.value = option.dataset.value || 'core';
    if (toolProfileDisplay) toolProfileDisplay.textContent = option.dataset.label || option.textContent.trim();
    toolProfileOptions.forEach(item => {
      const selected = item === option;
      item.classList.toggle('selected', selected);
      item.setAttribute('aria-selected', selected ? 'true' : 'false');
    });
    optionIndex = Math.max(0, toolProfileOptions.indexOf(option));
    closeDropdown();
    if (focusButton) toolProfileButton?.focus();
  }

  function openDropdown() {
    if (!toolProfileMenu || !toolProfileButton) return;
    toolProfileControl?.classList.add('open');
    toolProfileMenu.classList.remove('hidden');
    toolProfileButton.setAttribute('aria-expanded', 'true');
    optionIndex = Math.max(0, toolProfileOptions.findIndex(item => item.dataset.value === toolProfileInput?.value));
    toolProfileOptions[optionIndex]?.classList.add('focused');
  }

  function updateStepChrome() {
    dots.forEach(node => {
      const n = Number(node.dataset.setupDot);
      node.classList.toggle('active', n === Math.min(currentStep, 2));
      node.classList.toggle('complete', currentStep > n);
    });
    backButton.hidden = currentStep === 0 || currentStep === 3;
    cancelButton.hidden = !status?.configured || currentStep === 3;
    nextButton.textContent = currentStep === 2 ? (previewMode ? 'Finish preview' : 'Save setup') : currentStep === 3 ? 'Done' : 'Continue';
    showError('');
  }

  function setStep(index) {
    currentStep = Math.max(0, Math.min(3, Number(index) || 0));
    closeDropdown();
    steps.forEach(node => node.classList.toggle('hidden', Number(node.dataset.setupStep) !== currentStep));
    updateStepChrome();
  }

  async function navigateStep(index) {
    const targetStep = Math.max(0, Math.min(3, Number(index) || 0));
    if (targetStep === currentStep || stepAnimating) return;
    closeDropdown();
    showError('');

    const currentNode = steps.find(node => Number(node.dataset.setupStep) === currentStep);
    const nextNode = steps.find(node => Number(node.dataset.setupStep) === targetStep);
    const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    if (!currentNode || !nextNode || reduceMotion) {
      setStep(targetStep);
      return;
    }

    stepAnimating = true;
    const direction = targetStep > currentStep ? 1 : -1;
    const distance = Math.min(54, Math.max(34, Math.round(window.innerWidth * 0.045)));
    const out = currentNode.animate([
      {opacity: 1, transform: 'translateX(0)'},
      {opacity: 0, transform: `translateX(${-direction * distance}px)`},
    ], {duration: 135, easing: 'cubic-bezier(.4,0,1,1)', fill: 'forwards'});

    try { await out.finished; } catch {}
    out.cancel();
    currentNode.classList.add('hidden');
    currentStep = targetStep;
    nextNode.classList.remove('hidden');
    updateStepChrome();

    const incoming = nextNode.animate([
      {opacity: 0, transform: `translateX(${direction * distance}px)`},
      {opacity: 1, transform: 'translateX(0)'},
    ], {duration: 245, easing: 'cubic-bezier(.16,1,.3,1)'});
    try { await incoming.finished; } catch {}
    stepAnimating = false;
  }

  function populate(data = {}) {
    status = data;
    workspaceInput.value = data.workspace || '';
    selectToolProfile(data.tool_profile === 'full' ? 'full' : 'core');
    tunnelIdInput.value = data.tunnel_id || '';
    tunnelProfileInput.value = data.tunnel_profile || 'codex';
    organizationInput.value = data.organization || '';
    apiKeyInput.value = '';
    apiKeyInput.required = !data.api_key_saved;
    apiKeyInput.placeholder = data.api_key_saved ? 'Saved securely — leave blank to keep' : 'Paste runtime key';
    keyNote.textContent = data.api_key_saved ? 'Saved' : 'Required';
    keyNote.classList.toggle('saved', Boolean(data.api_key_saved));
    exit.hidden = !data.configured;
  }

  function fieldValid(field) {
    if (!field || field.checkValidity()) return true;
    field.reportValidity();
    return false;
  }

  async function saveSetup() {
    if (saving) return;
    if (!fieldValid(tunnelIdInput) || (!status?.api_key_saved && !fieldValid(apiKeyInput))) return;
    saving = true;
    nextButton.disabled = true;
    backButton.disabled = true;
    cancelButton.disabled = true;
    nextButton.textContent = 'Saving…';
    showError('');
    try {
      const response = await fetch('/setup', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({
          workspace: workspaceInput.value.trim(),
          tool_profile: toolProfileInput.value || 'core',
          tunnel_id: tunnelIdInput.value.trim(),
          api_key: apiKeyInput.value || '',
          tunnel_profile: tunnelProfileInput.value.trim() || 'codex',
          organization: organizationInput.value.trim(),
        }),
      });
      const data = await response.json().catch(() => ({}));
      if (!response.ok || data.ok === false) throw new Error(data.error || `HTTP ${response.status}`);
      if (data.setup) populate(data.setup);
      if (completeText) completeText.textContent = 'Configuration and tunnel profile are validated and ready. No manual config editing is needed.';
      await navigateStep(3);
    } catch (error) {
      showError(error?.message || String(error));
      nextButton.textContent = 'Save setup';
    } finally {
      saving = false;
      nextButton.disabled = false;
      backButton.disabled = false;
      cancelButton.disabled = false;
    }
  }

  toolProfileButton?.addEventListener('click', () => {
    if (toolProfileControl?.classList.contains('open')) closeDropdown();
    else openDropdown();
  });
  toolProfileOptions.forEach(option => option.addEventListener('click', () => selectToolProfile(option.dataset.value, true)));
  toolProfileButton?.addEventListener('keydown', event => {
    if (!['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(event.key)) return;
    event.preventDefault();
    if (!toolProfileControl?.classList.contains('open')) return openDropdown();
    if (event.key === 'ArrowDown') optionIndex = (optionIndex + 1) % toolProfileOptions.length;
    else if (event.key === 'ArrowUp') optionIndex = (optionIndex - 1 + toolProfileOptions.length) % toolProfileOptions.length;
    else return selectToolProfile(toolProfileOptions[optionIndex]?.dataset.value, true);
    toolProfileOptions.forEach((item, index) => item.classList.toggle('focused', index === optionIndex));
  });
  document.addEventListener('mousedown', event => {
    if (!toolProfileControl?.contains(event.target)) closeDropdown();
  });
  document.addEventListener('keydown', event => {
    if (event.key === 'Escape' && toolProfileControl?.classList.contains('open')) {
      closeDropdown();
      toolProfileButton?.focus();
    }
  });

  cancelButton.addEventListener('click', () => {
    if (status?.configured) window.location.href = '/';
  });
  backButton.addEventListener('click', () => navigateStep(currentStep - 1));
  nextButton.addEventListener('click', () => {
    if (currentStep === 0) return navigateStep(1);
    if (currentStep === 1) {
      if (fieldValid(workspaceInput)) navigateStep(2);
      return;
    }
    if (currentStep === 2) return previewMode ? navigateStep(3) : saveSetup();
    window.location.href = '/';
  });
  form.addEventListener('submit', event => {
    event.preventDefault();
    if (currentStep === 2) previewMode ? navigateStep(3) : saveSetup();
  });

  (async () => {
    try {
      const response = await fetch('/setup', {cache: 'no-store'});
      if (!response.ok) throw new Error(`Setup status returned HTTP ${response.status}`);
      populate(await response.json());
      setStep(status?.configured && !previewMode ? 1 : 0);
      if (window.feather) feather.replace({'stroke-width': 1.7});
      page.hidden = false;
      document.body.classList.remove('setup-loading');
      loader.classList.add('leaving');
      window.setTimeout(() => loader.remove(), 180);
    } catch (error) {
      loader.innerHTML = `<div class="setup-load-error"><strong>Could not load setup</strong><span>${String(error?.message || error)}</span><button type="button" id="setupRetry">Retry</button></div>`;
      document.getElementById('setupRetry')?.addEventListener('click', () => window.location.reload());
    }
  })();
})();
