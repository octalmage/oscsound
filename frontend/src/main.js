import './style.css';
import {
    GetConfig, SaveConfig, GetStatus,
    PickFile, ImportPack, ExportPack,
    TestStart, TestStop, SetPreviewVolume, SetLoopVolume,
} from '../wailsjs/go/main/App';
import { EventsOn } from '../wailsjs/runtime/runtime';

const root = document.querySelector('#app');

// Logarithmic volume curve so the slider feels perceptually even.
// The slider (0–100) maps to amplitude (0–1) through a 40 dB range.
// Thank you https://www.dr-lex.be/info-stuff/volumecontrols.html
const DB_RANGE = 40;
function sliderToVolume(slider) {
    if (slider === 0) return 0;
    return Math.pow(10, (slider - 100) / (DB_RANGE / 2));
}
function volumeToSlider(volume) {
    if (!volume) return 0;
    return Math.round(Math.max(0, 100 + (DB_RANGE / 2) * Math.log10(volume)));
}

let cfg = { sounds: [] };
let status = { port: 0, oscQuery: false };

const previews = new Map();    // row idx -> preview token
const activeLoops = new Set(); // params currently looping (OSC-driven)

let saveTimer;
function queueSave() {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(() => SaveConfig(cfg), 250);
}

function setCfg(c) {
    cfg = c || { sounds: [] };
    if (!cfg.sounds) cfg.sounds = [];
}

function basename(p) {
    if (!p) return '';
    const i = Math.max(p.lastIndexOf('/'), p.lastIndexOf('\\'));
    return i >= 0 ? p.slice(i + 1) : p;
}

function portLabel() {
    if (status.oscQuery) return `OSCQuery · UDP ${status.port}`;
    if (status.port) return `listening on :${status.port}`;
    return 'starting…';
}

function el(tag, props = {}, ...children) {
    const e = document.createElement(tag);
    Object.assign(e, props);
    for (const c of children) if (c != null) e.append(c);
    return e;
}

function render() {
    root.replaceChildren(renderHeader(), ...renderBody());
}

function renderHeader() {
    const title = el('h1', { textContent: 'oscsound' });
    const port = el('span', { className: 'port', textContent: portLabel() });
    const left = el('div', { className: 'header-left' }, title, port);

    const imp = el('button', { textContent: 'import', onclick: onImport });
    const exp = el('button', { textContent: 'export', onclick: () => ExportPack().catch(console.error) });
    const actions = el('div', { className: 'header-actions' }, imp, exp);

    return el('header', {}, left, actions);
}

function renderBody() {
    const out = [];
    if (cfg.sounds.length === 0) {
        out.push(el('div', { className: 'empty-state', textContent: 'No sounds yet. Add one below.' }));
    }
    cfg.sounds.forEach((s, i) => out.push(renderRow(s, i)));
    out.push(el('button', { className: 'add', textContent: '+ add sound', onclick: onAdd }));
    return out;
}

function renderRow(s, i) {
    const row = el('div', { className: 'row' });
    row.dataset.param = s.param;
    if (activeLoops.has(s.param) || (s.type === 'loop' && previews.has(i))) {
        row.classList.add('playing');
    }

    const name = el('input', {
        placeholder: 'name',
        value: s.name,
        oninput: () => { cfg.sounds[i].name = name.value; queueSave(); },
    });

    const param = el('input', {
        className: 'param',
        placeholder: 'AvatarParam',
        value: s.param,
        oninput: () => {
            cfg.sounds[i].param = param.value;
            row.dataset.param = param.value;
            queueSave();
        },
    });

    const type = el('select', {
        innerHTML: '<option value="oneshot">one-shot</option><option value="loop">loop</option>',
        value: s.type || 'oneshot',
        onchange: () => { cfg.sounds[i].type = type.value; queueSave(); },
    });

    const path = el('div', {
        className: s.path ? 'path' : 'path empty',
        textContent: s.path ? basename(s.path) : 'click to choose sound',
        title: s.path,
        onclick: async () => {
            const p = await PickFile();
            if (p) { cfg.sounds[i].path = p; queueSave(); render(); }
        },
    });

    const sliderPct = volumeToSlider(s.volume ?? 1);
    const vol = el('input', { type: 'range', min: 0, max: 100, value: sliderPct, title: `${sliderPct}%` });
    vol.oninput = () => {
        const v = sliderToVolume(+vol.value);
        cfg.sounds[i].volume = v;
        vol.title = `${vol.value}%`;
        if (previews.has(i)) SetPreviewVolume(previews.get(i), v);
        if (s.param) SetLoopVolume(s.param, v);
        queueSave();
    };

    const playing = previews.has(i);
    const test = el('button', {
        textContent: playing ? 'stop' : 'play',
        onclick: () => onTest(i, s),
    });
    if (playing) test.classList.add('active');

    const del = el('button', {
        className: 'del',
        textContent: '×',
        onclick: () => onDelete(i),
    });

    row.append(name, param, type, path, vol, test, del);
    return row;
}

async function onTest(i, s) {
    if (previews.has(i)) {
        const tok = previews.get(i);
        previews.delete(i);
        await TestStop(tok);
        render();
        return;
    }
    if (!s.path) return;
    const tok = await TestStart(s.path, s.type === 'loop', s.volume ?? 1);
    previews.set(i, tok);
    render();
}

async function onDelete(i) {
    for (const tok of previews.values()) await TestStop(tok);
    previews.clear();
    cfg.sounds.splice(i, 1);
    queueSave();
    render();
}

function onAdd() {
    cfg.sounds.push({ name: '', param: '', path: '', type: 'oneshot', volume: 1 });
    queueSave();
    render();
}

async function onImport() {
    try {
        await ImportPack();
        setCfg(await GetConfig());
        render();
    } catch (e) {
        console.error(e);
    }
}

function flash(param) {
    const row = root.querySelector(`.row[data-param="${CSS.escape(param)}"]`);
    if (!row) return;
    row.classList.add('flash');
    setTimeout(() => row.classList.remove('flash'), 200);
}

Promise.all([GetConfig(), GetStatus()]).then(([c, s]) => {
    setCfg(c);
    status = s;
    render();
});

EventsOn('trigger', flash);
EventsOn('loop-on', (p) => { activeLoops.add(p); render(); });
EventsOn('loop-off', (p) => { activeLoops.delete(p); render(); });
EventsOn('preview-end', (tok) => {
    for (const [idx, t] of previews) {
        if (t === tok) {
            previews.delete(idx);
            render();
            return;
        }
    }
});
