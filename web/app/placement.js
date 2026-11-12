/*
 * Where a model lives: one stacked bar per memory tier (GPU, RAM, NVMe)
 * with the model's weights, its KV cache, the compute buffers, what other
 * programs use and what is free, each with exact figures. Used by the
 * active model on the Models page (measured, live) and by the preview of
 * a model's settings (estimated by the planner).
 */
import { esc, icon, mem, size, num, pct, plural } from './ui.js';

/* A part never vanishes below a sliver, so a small figure in the legend still has its mark. */
const MIN_SHARE = 0.006;

function bar({ key, label, total, totalText, parts, note }) {
  const shown = parts.filter((p) => p.value > 0);
  let shares = shown.map((p) => Math.max(MIN_SHARE, p.value / total));
  const sum = shares.reduce((a, b) => a + b, 0);
  if (sum > 1) shares = shares.map((s) => s / sum);
  const words = shown.map((p) => `${p.label} ${p.text || mem(p.value)}`).join(', ');
  return `<div class="so-tier" data-ds-key="${key}"><figure class="ds-category-bar ds-category-bar--rest">` +
    `<figcaption class="ds-category-bar__head"><span class="ds-category-bar__label">${label}</span><span class="ds-category-bar__total">${esc(totalText)}</span></figcaption>` +
    `<div class="ds-category-bar__track" role="img" aria-label="${esc(`${words || 'Nothing of the model'}; ${totalText}`)}">` +
      shown.map((p, i) => `<span class="ds-category-bar__part${partClass(p.s)}" data-ds-key="${key}-${p.id}" style="--ds-share: ${shares[i].toFixed(4)}"></span>`).join('') +
    '</div>' +
    `<ul class="ds-legend" aria-hidden="true">${parts.filter((p) => p.value > 0 || p.always).map((p) =>
      `<li data-ds-key="${key}-l-${p.id}"><span class="ds-legend__key${keyClass(p.s)}"></span>${esc(p.label)} <span class="ds-category-bar__value">${esc(p.text || mem(p.value))}</span></li>`).join('')}</ul>` +
    '</figure>' +
    (note ? `<p class="ds-meta">${note}</p>` : '') + '</div>';
}

/**
 * What other programs use right now, apart from the engine: the GPU's and
 * the RAM's. Taken from the live status, so a preview can lay a planned
 * model over what the machine holds besides the running engine.
 */
export function othersOf(status) {
  const sys = status && status.system;
  if (!sys) return { gpu: 0, ram: 0 };
  const p = status.placement;
  const running = status.engine && ['ready', 'loading', 'starting'].includes(status.engine.state) && p;
  const gpu = sys.gpus && sys.gpus[0];
  let engineGpu = 0;
  let engineAnon = 0;
  if (running) {
    const loaded = status.engine.state === 'ready' ? 1 : status.engine.state === 'loading' ? status.engine.progress || 0 : 0;
    engineGpu = (p.gpu.weights + p.gpu.kv + p.gpu.compute) * loaded;
    engineAnon = Math.max(0, (sys.ram.engineRss || 0) - (p.ram.weightsResident || 0));
  }
  return {
    gpu: gpu ? Math.max(0, gpu.used - engineGpu) : 0,
    ram: Math.max(0, (sys.ram.used || 0) - engineAnon),
  };
}

/**
 * The three bars. `placement` is status.placement or a plan's; `system`
 * the live status.system; `others` what othersOf() measured.
 */
export function placementHtml(placement, system, others, { live = false } = {}) {
  const p = placement;
  const gpu = system && system.gpus && system.gpus[0];
  const out = [];
  const L = p.layers || { total: 0, gpu: 0, expertLayersOnCpu: 0, moe: false };

  if (gpu) {
    const engine = p.gpu.weights + p.gpu.kv + p.gpu.compute;
    const other = others.gpu;
    const used = engine + other;
    const free = Math.max(0, gpu.total - used);
    const layerText = L.moe
      ? (L.expertLayersOnCpu
        ? `${num(L.gpu)} of ${num(L.total)} layers on the GPU, the experts of ${plural(L.expertLayersOnCpu, 'layer')} in RAM`
        : `All ${num(L.total)} layers on the GPU, experts included`)
      : `${num(L.gpu)}/${num(L.total)} layers on the GPU`;
    const liveText = live ? ` · ${gpu.util} % busy · ${gpu.tempC} °C · ${gpu.powerW} W` : '';
    out.push(bar({
      key: 'gpu',
      label: `${icon('cpu', 16)} GPU · ${esc(gpu.name.replace(/^NVIDIA (GeForce )?/, ''))}`,
      total: gpu.total,
      totalText: `${mem(used)} of ${mem(gpu.total)} · ${mem(free)} free`,
      parts: [
        { id: 'w', label: 'Weights', value: p.gpu.weights, s: 1, always: true },
        { id: 'kv', label: 'KV cache', value: p.gpu.kv, s: 2 },
        { id: 'c', label: 'Compute', value: p.gpu.compute, s: 3 },
        { id: 'o', label: 'Other programs', value: other, s: 'n' },
      ],
      note: `${icon('layers', 16)}${esc(layerText)}${esc(liveText)}`,
    }));
  } else {
    out.push(`<p class="ds-meta" data-ds-key="gpu-none">${icon('info', 16)}No GPU found: the model runs on the CPU.</p>`);
  }

  if (system && system.ram) {
    const ram = system.ram;
    const resident = p.ram.weightsResident ?? p.ram.weights;
    const other = others.ram;
    const used = resident + p.ram.kv + p.ram.compute + other;
    const free = Math.max(0, ram.total - used);
    const missing = Math.max(0, p.ram.weights - resident);
    let note = p.ram.weights
      ? `${esc(mem(p.ram.weights))} of weights belong in RAM${missing ? `, ${esc(mem(missing))} of them are not read in yet` : ', all of them read in'}`
      : 'No weights in RAM';
    if (live) note += ` · ${esc(mem(ram.cached))} file cache`;
    out.push(bar({
      key: 'ram',
      label: `${icon('memory', 16)} RAM`,
      total: ram.total,
      totalText: `${mem(used)} of ${mem(ram.total)} · ${mem(free)} free`,
      parts: [
        { id: 'w', label: 'Weights', value: resident, s: 1, always: true },
        { id: 'kv', label: 'KV cache', value: p.ram.kv, s: 2 },
        { id: 'c', label: 'Compute', value: p.ram.compute, s: 3 },
        { id: 'o', label: 'Other programs', value: other, s: 'n' },
      ],
      note: `${icon('memory', 16)}${note}`,
    }));
  }

  if (system && system.disk) {
    const disk = system.disk;
    const weights = p.disk ? p.disk.weights : 0;
    const usedOther = Math.max(0, disk.total - disk.free - weights);
    out.push(bar({
      key: 'disk',
      label: `${icon('hardDrive', 16)} NVMe · ${esc(disk.path)}`,
      total: disk.total,
      totalText: `${size(disk.free)} free of ${size(disk.total)}`,
      parts: [
        { id: 'w', label: 'Weights read from here', value: weights, text: weights ? size(weights) : 'none', s: 1, always: true },
        { id: 'o', label: 'Other data', value: usedOther, text: size(usedOther), s: 'n' },
      ],
      note: weights
        ? `${icon('alert', 16)}${esc(size(weights))} of weights are read from here while the model answers: the slowest tier.`
        : `${icon('checkCircle', 16)}Nothing is read from the NVMe while the model answers.`,
    }));
  }
  return `<div class="so-tiers">${out.join('')}</div>`;
}

/* A part and its legend key take the same series, or the neutral tone for what is not the model's. */
const partClass = (s) => (s === 'n' ? ' ds-tone-neutral' : s > 1 ? ` ds-category-bar__part--s${s}` : '');
const keyClass = (s) => (s === 'n' ? ' ds-tone-neutral' : s > 1 ? ` ds-legend__key--s${s}` : '');

/** The plan's words for how well it fits. */
export const FITS = {
  gpu: ['success', 'Fits on the GPU', 'checkCircle'],
  hybrid: ['', 'GPU + RAM', 'layers'],
  disk: ['warning', 'Spills to NVMe', 'hardDrive'],
  no: ['error', 'Too large', 'alertCircle'],
};

export const SPEED = { fast: 'Fast', medium: 'Medium speed', slow: 'Slow' };

/** The share of a model's file that sits on the GPU, in words. */
export function gpuShareText(p) {
  if (!p || !p.fileBytes) return '';
  return `${pct(Math.min(1, p.gpu.weights / p.fileBytes))} of the weights on the GPU`;
}
