// New-run form. Structured around a start-mode chooser (my forges / custom)
// so a first-time user has one obvious entry point, with workload tuning and
// per-target advanced fields tucked behind disclosures. No credential ever
// enters the browser: a target only carries a secret_source (gh | glab |
// entire) and the server reads the token from that CLI at run start. A forge
// without a supported CLI login is benchmarked from the command line
// (forgemark -token-file …), not this form.

import { h } from '../dom.js';
import { api } from '../api.js';
import { targetColor } from '../charts.js';

const strategies = [
  ['branch', 'branch — one repo, per-agent branches (headline write number)'],
  ['repo', 'repo — agents spread across N repos (horizontal scale)'],
  ['clone', 'clone — shallow-clone loop (read-side ceiling)'],
  ['session', 'session — clone + push checkpoints per session (agent lifecycle)'],
];

const DEMO_PAIR = [
  { name: 'demo-fast', remote: 'demo://fast?p50=60ms&spread=3&err=0.01&cap=400' },
  { name: 'demo-slow', remote: 'demo://slow?p50=180ms&spread=4&err=0.03&cas=0.02&cap=120' },
];

const isDemo = (remote) => /^demo:\/\//i.test((remote || '').trim());

let targetSeq = 0;

function blankTarget(extra = {}) {
  return {
    key: ++targetSeq, name: '', remote: '', repos: '', user: '', secret_source: '',
    insecure: false, object_format: 'auto', token_url: '', jurisdiction: '', client_id: '', ...extra,
  };
}

export function renderNewRun(app) {
  let targets = [blankTarget()];
  let mode = null; // null (chooser) | 'forges' | 'custom'
  let raceView = false;
  let errorBanner = null;

  const field = (label, input, cls = '') => h('div', { class: `field ${cls}` }, h('label', {}, label), input);
  const inp = (attrs) => h('input', { type: 'text', ...attrs });

  // --- workload inputs (persist across mode switches) ---
  const w = {
    strategy: h('select', { onchange: () => { syncStrategy(); updateWorkloadSummary(); } }, strategies.map(([v, t]) => h('option', { value: v }, t))),
    concurrency: inp({ value: '1,8,32', placeholder: 'e.g. 1,8,32,128', oninput: updateWorkloadSummary }),
    duration_sec: inp({ value: '60', oninput: updateWorkloadSummary }),
    warmup_sec: inp({ value: '10', oninput: updateWorkloadSummary }),
    files_min: inp({ value: '1' }),
    files_max: inp({ value: '10' }),
    file_size: inp({ value: '2048' }),
    branch_prefix: inp({ value: '', placeholder: 'e.g. bench/' }),
    session_commits: inp({ value: '5' }),
    clone_depth: inp({ value: '1' }),
    base_ref: inp({ value: '', placeholder: 'default branch' }),
  };
  const commitFields = h('div', { class: 'grid c3' },
    field('Files per commit (min)', w.files_min),
    field('Files per commit (max)', w.files_max),
    field('Bytes per file', w.file_size));
  const cloneFields = h('div', { class: 'grid c3' },
    field('Session commits', w.session_commits),
    field('Clone depth (0 = full)', w.clone_depth),
    field('Base ref', w.base_ref));

  function syncStrategy() {
    const s = w.strategy.value;
    commitFields.style.display = s === 'clone' ? 'none' : '';
    cloneFields.style.display = (s === 'clone' || s === 'session') ? '' : 'none';
    w.session_commits.parentElement.style.display = s === 'session' ? '' : 'none';
  }

  const workloadSummary = h('span', { class: 'dsum' });
  function updateWorkloadSummary() {
    const warm = parseFloat(w.warmup_sec.value || '0');
    workloadSummary.textContent =
      `${w.strategy.value} · sweep ${w.concurrency.value || '—'} · ${w.duration_sec.value || '—'}s/level` +
      (warm > 0 ? ` · ${warm}s warm-up` : '');
  }

  function applyRacePreset() {
    w.strategy.value = 'branch';
    w.concurrency.value = '16';
    w.duration_sec.value = '60';
    w.warmup_sec.value = '0';
    w.files_min.value = '1'; w.files_max.value = '3'; w.file_size.value = '1024';
    w.branch_prefix.value = 'bench/';
    raceView = true;
    syncStrategy();
    updateWorkloadSummary();
  }

  // --- targets ---
  const targetsBox = h('div');
  function renderTargets() {
    targetsBox.innerHTML = '';
    targets.forEach((t, i) => targetsBox.append(targetCard(t, i)));
    updateGate();
  }

  function targetCard(t, i) {
    const bind = (key, input) => {
      input.value = t[key];
      input.addEventListener('input', () => { t[key] = input.value; });
      return input;
    };
    const insecure = h('input', { type: 'checkbox', style: { width: 'auto' }, onchange: (e) => { t.insecure = e.target.checked; } });
    insecure.checked = t.insecure;

    const remoteInput = bind('remote', inp({ placeholder: 'https://gitlab.example — or demo://fast?p50=80ms' }));

    // The credential comes from an authenticated CLI the server reads at run
    // start, so the token never enters the browser or the request body. Demo
    // targets need none. The credential control and the forge-only advanced
    // knobs are (re)synced whenever the remote changes: typing a demo:// URL
    // into a custom target must drop the credential source and hide the advanced
    // fields, or the run would post secret_source and the server would reject it.
    const credBox = h('div');
    const advanced = h('details', { class: 'disclose', style: { marginTop: '12px', marginBottom: '0' } },
      h('summary', {}, 'Advanced'),
      h('div', { class: 'dbody' },
        h('div', { class: 'grid c3' },
          field('Username (token forges ignore it)', bind('user', inp({ placeholder: 'x-access-token' }))),
          field('Object format', bind('object_format', h('select', {},
            h('option', { value: 'auto' }, 'auto'), h('option', { value: 'sha1' }, 'sha1'), h('option', { value: 'sha256' }, 'sha256')))),
          h('div', { class: 'field' }, h('label', {}, 'TLS'),
            h('label', { style: { color: 'var(--ink-2)', fontSize: '13px' } }, insecure, ' skip verification (dev hosts)'))),
        h('p', { class: 'eyebrow', style: { marginTop: '14px' } }, 'entiredb'),
        h('div', { class: 'grid c3' },
          field('Token URL', bind('token_url', inp({ placeholder: 'https://…/oauth/token' }))),
          field('Jurisdiction', bind('jurisdiction', inp({ placeholder: 'https://us.example.com' }))),
          field('Client ID', bind('client_id', inp({ placeholder: 'entire-cli' }))))));

    function syncCred() {
      const demoish = isDemo(t.remote);
      credBox.innerHTML = '';
      if (demoish) {
        t.secret_source = '';
        credBox.append(h('div', { class: 'cred-note' }, 'none needed (demo target)'));
      } else {
        if (!t.secret_source) t.secret_source = 'gh';
        const srcSel = h('select', { onchange: () => { t.secret_source = srcSel.value; } },
          h('option', { value: 'gh' }, 'gh CLI login'),
          h('option', { value: 'glab' }, 'glab CLI login'),
          h('option', { value: 'entire' }, 'entire CLI login'));
        srcSel.value = t.secret_source;
        credBox.append(srcSel);
      }
      advanced.style.display = demoish ? 'none' : '';
    }
    // Swap only credBox/advanced on input, never the remote field itself, so
    // focus and caret stay put while typing the demo:// URL.
    remoteInput.addEventListener('input', () => { updateGate(); syncCred(); });
    syncCred();

    return h('div', { class: 'target-card', style: { '--tcolor': targetColor(i) } },
      h('div', { class: 'thead' },
        h('span', { class: 'tname' }, h('span', { class: 'dot', style: { '--tcolor': targetColor(i), marginRight: '8px' } }), `Target ${i + 1}`),
        targets.length > 1 ? h('button', { class: 'small danger', type: 'button', onclick: () => { targets.splice(i, 1); renderTargets(); } }, 'Remove') : null),
      h('div', { class: 'grid c3' },
        field('Name (chart label)', bind('name', inp({ placeholder: 'e.g. gitlab-prod' }))),
        field('Remote base URL', remoteInput, 'wide')),
      h('div', { class: 'grid c2', style: { marginTop: '12px' } },
        field('Repos (comma-separated)', bind('repos', inp({ placeholder: 'group/bench.git' }))),
        field('Credential source', credBox)),
      advanced);
  }

  const cliNotes = h('div');
  async function addFromCLI() {
    cliNotes.innerHTML = '';
    cliNotes.append(h('div', { class: 'banner' }, 'Detecting gh / glab / entire CLI logins…'));
    try {
      const sug = await api.localSuggest();
      cliNotes.innerHTML = '';
      for (const key of ['github', 'gitlab', 'entire']) {
        const e = sug[key] || {};
        if (e.target) {
          const s = e.target;
          if (targets.some((t) => t.remote === s.remote && t.repos === (s.repos || []).join(','))) continue;
          targets.push(blankTarget({
            name: s.name || '', remote: s.remote || '', repos: (s.repos || []).join(','),
            user: s.user || '', secret_source: s.secret_source || '',
            token_url: s.token_url || '', jurisdiction: s.jurisdiction || '', client_id: s.client_id || '',
          }));
          if (e.note) cliNotes.append(h('div', { class: 'banner' }, `${s.name}: ${e.note}`));
        } else if (e.error) {
          cliNotes.append(h('div', { class: 'banner warn' }, `${key}: ${e.error}`));
        }
      }
      if (targets.length > 1 && !targets[0].remote && !targets[0].name) targets.shift();
      if (!targets.length) {
        cliNotes.append(h('div', { class: 'banner warn' },
          'No CLI logins found. Run ', h('code', {}, 'scripts/login.sh'), ' to authenticate gh and entire, then try again.'));
        targets = [blankTarget()];
      }
      renderTargets();
    } catch (err) {
      cliNotes.innerHTML = '';
      cliNotes.append(h('div', { class: 'banner error' }, String(err.message || err)));
    }
  }

  // --- authorization + start (stable elements, re-appended per mode) ---
  const startBtn = h('button', { class: 'primary', type: 'submit', disabled: true }, 'Start benchmark');
  const confirm = h('input', { type: 'checkbox', id: 'confirm', onchange: updateGate });
  const confirmRow = h('div', { class: 'confirm-row' },
    confirm,
    h('label', { for: 'confirm' }, 'I own this infrastructure or am explicitly authorized to load-test it. A benchmark run is sustained, abusive-looking traffic to any host that has not agreed to it.'));
  const raceChk = h('input', { type: 'checkbox', style: { width: 'auto' }, onchange: (e) => { raceView = e.target.checked; } });

  // updateGate keeps the authorization step honest: an all-demo run touches no
  // real host, so it needs no confirmation and no checkbox; any real target
  // brings the checkbox back and gates Start on it.
  function updateGate() {
    const withRemote = targets.filter((t) => t.remote.trim());
    const allDemo = withRemote.length > 0 && withRemote.every((t) => isDemo(t.remote));
    confirmRow.style.display = allDemo ? 'none' : '';
    startBtn.disabled = withRemote.length === 0 || (!allDemo && !confirm.checked);
    startBtn.textContent = raceView ? 'Start race' : 'Start benchmark';
  }

  async function submit(ev) {
    ev.preventDefault();
    if (errorBanner) { errorBanner.remove(); errorBanner = null; }
    const showError = (msg) => {
      errorBanner = h('div', { class: 'banner error' }, msg);
      form.prepend(errorBanner);
      window.scrollTo(0, 0);
    };
    // Reject malformed concurrency instead of silently dropping tokens: an
    // empty list would make the server substitute its default [1,8,32,128]
    // sweep — a 128-agent surprise the user never asked for.
    const concTokens = w.concurrency.value.split(',').map((s) => s.trim()).filter(Boolean);
    const concurrency = concTokens.map((s) => parseInt(s, 10));
    if (!concurrency.length || concurrency.some((n, i) => !Number.isInteger(n) || n < 1 || String(n) !== concTokens[i])) {
      showError(`Concurrency must be comma-separated positive integers (e.g. 1,8,32) — got "${w.concurrency.value}"`);
      return;
    }

    // Validate every numeric field to a finite number before posting. A blank
    // or non-numeric entry would parse to NaN, which JSON.stringify emits as
    // null; the server reads null as "absent" and quietly substitutes the flag
    // default — so a typo'd "6o" duration would silently run for 60s instead of
    // erroring. Integer fields must also be whole. Ranges (e.g. min <= max) stay
    // the server's job via Validate.
    const numSpecs = [
      { el: w.duration_sec, label: 'Duration per level', int: false, min: 0, gt0: true },
      { el: w.warmup_sec, label: 'Warm-up', int: false, min: 0 },
      { el: w.files_min, label: 'Files per commit (min)', int: true, min: 1 },
      { el: w.files_max, label: 'Files per commit (max)', int: true, min: 1 },
      { el: w.file_size, label: 'Bytes per file', int: true, min: 0 },
      { el: w.session_commits, label: 'Session commits', int: true, min: 1 },
      { el: w.clone_depth, label: 'Clone depth', int: true, min: 0 },
    ];
    const nums = new Map();
    for (const f of numSpecs) {
      const raw = (f.el.value || '').trim();
      const v = Number(raw);
      if (raw === '' || !Number.isFinite(v) || (f.int && !Number.isInteger(v)) || v < f.min || (f.gt0 && v <= 0)) {
        const want = f.int ? 'a whole number' : 'a number';
        showError(`${f.label} must be ${want}${f.gt0 ? ' greater than 0' : ` >= ${f.min}`} — got "${f.el.value}"`);
        return;
      }
      nums.set(f.el, v);
    }
    const num = (el) => nums.get(el);

    // Only remote-bearing rows are real targets — a blank card is not posted.
    // Compute allDemo over the same set updateGate() uses, so the confirm-gate
    // the UI showed and the confirm_authorized we send can't disagree.
    const active = targets.filter((t) => t.remote.trim());
    if (!active.length) {
      showError('Add at least one target with a remote.');
      return;
    }
    const allDemo = active.every((t) => isDemo(t.remote));
    const spec = {
      confirm_authorized: confirm.checked || allDemo,
      workload: {
        strategy: w.strategy.value,
        concurrency,
        duration_sec: num(w.duration_sec),
        warmup_sec: num(w.warmup_sec),
        files_min: num(w.files_min), files_max: num(w.files_max), file_size: num(w.file_size),
        branch_prefix: w.branch_prefix.value,
        session_commits: num(w.session_commits), clone_depth: num(w.clone_depth), base_ref: w.base_ref.value,
      },
      targets: active.map((t) => ({
        name: t.name, remote: t.remote,
        repos: t.repos.split(',').map((s) => s.trim()).filter(Boolean),
        user: t.user, secret_source: t.secret_source,
        insecure: t.insecure, object_format: t.object_format,
        token_url: t.token_url, jurisdiction: t.jurisdiction, client_id: t.client_id,
      })),
    };
    startBtn.disabled = true;
    try {
      const res = await api.startRun(spec);
      if (res.warnings && res.warnings.length) {
        sessionStorage.setItem(`fm-warn-${res.id}`, JSON.stringify(res.warnings));
      }
      location.hash = raceView ? `#race/${res.id}` : `#run/${res.id}`;
    } catch (err) {
      updateGate();
      showError(String(err.message || err));
    }
  }

  // --- mode chooser & body assembly ---
  const body = h('div');

  function chooseMode(next) {
    mode = next;
    errorBanner = null;
    if (mode === 'forges') {
      targets = [];
      applyRacePreset();
      addFromCLI();
    } else if (mode === 'custom') {
      targets = [blankTarget()];
      raceView = false;
    }
    renderBody();
  }

  function modeCard(ico, title, desc, onclick) {
    return h('button', { type: 'button', class: 'mode-card', onclick },
      h('span', { class: 'ico' }, ico),
      h('span', { class: 'mtitle' }, title),
      h('span', { class: 'mdesc' }, desc));
  }

  const MODE_LABELS = { forges: 'Compare my forges', custom: 'Custom run' };

  function renderBody() {
    body.innerHTML = '';
    if (mode === null) {
      body.append(h('div', { class: 'modes' },
        modeCard('🏁', 'Compare my forges',
          'GitHub, GitLab, and Entire, authenticated from your gh / glab / entire CLI logins. Nothing to paste.',
          () => chooseMode('forges')),
        modeCard('⚙️', 'Custom run',
          'Point at any smart-HTTP forge. Configure targets and the workload by hand.',
          () => chooseMode('custom'))));
      return;
    }

    const addButtons = h('div', { class: 'add-row' },
      targets.length < 8 ? h('button', { type: 'button', class: 'small', onclick: () => { targets.push(blankTarget()); renderTargets(); } }, '+ Add target') : null,
      mode === 'custom' ? h('button', { type: 'button', class: 'small', onclick: addFromCLI }, 'Add from CLI logins') : null,
      mode === 'custom' ? h('button', { type: 'button', class: 'small', onclick: () => {
        for (const d of DEMO_PAIR) targets.push(blankTarget(d));
        if (targets.length > DEMO_PAIR.length && !targets[0].remote && !targets[0].name) targets.shift();
        renderTargets();
      } }, 'Add demo pair') : null);

    body.append(
      h('div', { class: 'mode-bar' },
        h('span', { class: 'mlabel' }, MODE_LABELS[mode]),
        h('button', { type: 'button', class: 'change', onclick: () => { mode = null; renderBody(); } }, 'change')),
      h('p', { class: 'eyebrow' }, 'Targets'),
      targetsBox,
      cliNotes,
      addButtons,
      h('details', { class: 'disclose', style: { marginTop: '16px' } },
        h('summary', {}, 'Workload tuning', workloadSummary),
        h('div', { class: 'dbody' },
          h('div', { class: 'grid c4' },
            field('Strategy', w.strategy, 'wide'),
            field('Concurrency sweep', w.concurrency),
            field('Duration per level (s)', w.duration_sec),
            field('Warm-up (s)', w.warmup_sec),
            field('Branch prefix', w.branch_prefix)),
          h('div', { style: { marginTop: '12px' } }, commitFields),
          h('div', { style: { marginTop: '12px' } }, cloneFields))),
      confirmRow,
      h('div', { class: 'start-row' },
        startBtn,
        h('label', { class: 'race-toggle' }, raceChk, 'Open the live race view')));

    raceChk.checked = raceView;
    renderTargets();
    syncStrategy();
    updateWorkloadSummary();
  }

  const form = h('form', { onsubmit: submit },
    h('p', { class: 'eyebrow' }, 'ForgeMark'),
    h('h1', {}, 'New benchmark run'),
    h('p', { class: 'sub' }, 'Drive real git push/clone load against one or more forges and watch them race, live. Pick how you want to start:'),
    body);

  app.append(form);
  renderBody();
}
