#!/usr/bin/env node
/*
 * A dependency-free development server for the Socrates API interface: it
 * serves web/ and mocks the whole internal /api/* contract of
 * docs/internal-api.md with data that moves (the engine loads and becomes
 * ready, a download progresses, chats stream with prompt progress, thinking
 * and timings, API traffic comes in, the engine log grows).
 *
 *   node scripts/mock-server.mjs            http://localhost:8090
 *   PORT=9000 node scripts/mock-server.mjs
 *
 * The machine it pretends to be: an RTX 3090 (24 GB), 64 GB of RAM and a
 * 2 TB NVMe. State lives in memory; a restart starts at the first-start
 * screen again (MOCK_PASSWORD=... skips it).
 */
import http from 'node:http';
import { readFile, stat } from 'node:fs/promises';
import { dirname, extname, join, normalize, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { randomBytes } from 'node:crypto';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', 'web');
const PORT = Number(process.env.PORT) || 8090;
const GiB = 1024 ** 3;
const MiB = 1024 ** 2;
const now = () => Date.now();
const rand = (a, b) => a + Math.random() * (b - a);
const clamp = (v, a, b) => Math.max(a, Math.min(b, v));
const pick = (list) => list[Math.floor(Math.random() * list.length)];

/* ── The machine ──────────────────────────────────────────────────── */

const SYS = {
  gpu: { name: 'NVIDIA GeForce RTX 3090', total: 24 * GiB, other: 0.55 * GiB },
  ram: { total: 64 * GiB, other: 9.4 * GiB, cache: 6.1 * GiB },
  disk: { path: '/data', total: 2000398934016, used: 241e9 },
  cpu: { name: 'Intel Core i7-11700KF', threads: 16 },
};

/* ── Models ───────────────────────────────────────────────────────── */

const DEFAULTS = { context: 0, gpuLayers: 'auto', cpuMoe: 'auto', kvType: 'q8_0', flashAttn: 'auto', loadMode: 'mmap', parallel: 1,
  threads: 0, batch: 2048, ubatch: 512, reasoning: 'auto', reasoningBudget: -1, gpuReserveMb: 1024, preload: true, extraArgs: '' };

const models = [
  {
    id: 'huihui-qwen3-8-27b-ud-q4-k-xl', name: 'Huihui Qwen3.8 27B · UD-Q4_K_XL',
    repo: 'huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF', quant: 'UD-Q4_K_XL',
    files: [{ path: 'Huihui-Qwen3.8-27B-abliterated.UD-Q4_K_XL.gguf', size: 17363000000 }],
    mmproj: 'mmproj-model-bf16.gguf', mmprojSize: 930000000, size: 18293000000,
    state: 'ready', error: '', progress: 1, active: true,
    meta: { arch: 'qwen35', params: 27800000000, ctxTrain: 262144, layers: 65, experts: 0, expertsUsed: 0, moe: false, vision: true, mtp: true, chatTemplate: true },
    settings: {},
    createdAt: now() - 9 * 86400e3,
    // Measured on the real RTX 3090: 15.5 GiB of layers, 1.84 GiB for the projector and the MTP head, 0.67 GiB of token embeddings in RAM.
    sim: { kvPerToken: 49152, promptTps: 1500, genTps: 68, compute: 0.26 * GiB, extra: 1.84 * GiB, embedShare: 0.0414 },
  },
  {
    id: 'qwen3-8-flash-next-ud-iq3-xxs', name: 'Qwen3.8 Flash Next · UD-IQ3_XXS',
    repo: 'unsloth/Qwen3.8-Flash-Next-GGUF', quant: 'UD-IQ3_XXS',
    files: [
      { path: 'UD-IQ3_XXS/Qwen3.8-Flash-Next-UD-IQ3_XXS-00001-of-00002.gguf', size: 49900000000 },
      { path: 'UD-IQ3_XXS/Qwen3.8-Flash-Next-UD-IQ3_XXS-00002-of-00002.gguf', size: 33700000000 },
    ],
    mmproj: '', mmprojSize: 0, size: 83600000000,
    state: 'ready', error: '', progress: 1, active: false,
    meta: { arch: 'qwen3moe', params: 235000000000, ctxTrain: 262144, layers: 36, experts: 128, expertsUsed: 8, moe: true, vision: false, mtp: false, chatTemplate: true },
    settings: { context: 32768, kvType: 'q8_0' },
    createdAt: now() - 3 * 86400e3,
    sim: { kvPerToken: 49152, promptTps: 210, genTps: 7.8, sharedShare: 0.075 },
  },
  {
    id: 'gemma-4-12b-it-q5-k-m', name: 'Gemma 4 12B it · Q5_K_M',
    repo: 'bartowski/google_gemma-4-12b-it-GGUF', quant: 'Q5_K_M',
    files: [{ path: 'google_gemma-4-12b-it-Q5_K_M.gguf', size: 8440000000 }],
    mmproj: 'mmproj-google_gemma-4-12b-it-f16.gguf', mmprojSize: 854000000, size: 9294000000,
    state: 'downloading', error: '', progress: 0.38, active: false,
    meta: { arch: 'gemma4', params: 12200000000, ctxTrain: 131072, layers: 48, experts: 0, expertsUsed: 0, moe: false, vision: true, mtp: false, chatTemplate: true },
    settings: {},
    createdAt: now() - 120e3,
    sim: { kvPerToken: 28672, promptTps: 2900, genTps: 71 },
  },
];

const effective = (m, s = m.settings) => ({ ...DEFAULTS, speculative: m.meta.mtp ? 'mtp' : 'off', vision: !!m.mmproj, ...s });

/* What goes where for a model and its settings: a coarse copy of the server's planner, good enough for the numbers to move. */
function planFor(m, raw) {
  const s = effective(m, raw);
  const reserve = s.gpuReserveMb * MiB;
  const weights = m.files.reduce((sum, f) => sum + f.size, 0);
  const mmproj = (s.vision && m.mmproj ? m.mmprojSize : 0) + (s.speculative === 'mtp' && m.sim.extra ? m.sim.extra - m.mmprojSize : 0);
  const kvScale = { f16: 2, q8_0: 1, q4_0: 0.56 }[s.kvType] || 1;
  const perTok = m.sim.kvPerToken * kvScale;
  const compute = (m.sim.compute || (m.meta.moe ? 1.1 * GiB : 0.48 * GiB)) * clamp(s.ubatch / 512, 0.5, 4);
  const room = SYS.gpu.total - SYS.gpu.other - reserve - compute - mmproj;
  let ctx = s.context || 0;
  // Automatic: as large as fits on the GPU next to all layers, between 32k and 128k (never below 32k: layers move to RAM first).
  if (!ctx) ctx = clamp(Math.floor(((room - weights * (m.meta.moe ? m.sim.sharedShare : 1)) / perTok) / 8192) * 8192, 32768, 131072);
  ctx = Math.min(ctx, m.meta.ctxTrain);
  const kv = ctx * perTok;
  const budget = room - kv;
  const L = m.meta.layers;
  let gpuLayers = L;
  let cpuMoe = 0;
  let gpuWeights;
  let cpuWeights;
  const embed = weights * (m.sim.embedShare || 0.05);
  if (m.meta.moe) {
    const shared = weights * m.sim.sharedShare;
    const perLayer = (weights - shared) / L;
    cpuMoe = s.cpuMoe === 'auto' ? clamp(Math.ceil((weights - budget) / perLayer), 0, L) : clamp(Number(s.cpuMoe), 0, L);
    if (s.gpuLayers !== 'auto') gpuLayers = clamp(Number(s.gpuLayers), 0, L);
    gpuWeights = shared + perLayer * (L - cpuMoe);
    cpuWeights = perLayer * cpuMoe;
  } else {
    const perLayer = (weights - embed) / L;
    gpuLayers = s.gpuLayers === 'auto' ? clamp(Math.floor(budget / perLayer), 0, L) : clamp(Number(s.gpuLayers), 0, L);
    gpuWeights = perLayer * gpuLayers;
    cpuWeights = weights - gpuWeights;
  }
  const kvGpu = m.meta.moe || gpuLayers === L ? kv : kv * (gpuLayers / L);
  const kvRam = kv - kvGpu;
  const ramCompute = 0.06 * GiB + (gpuLayers < L ? 0.3 * GiB : 0);
  const ramRoom = SYS.ram.total - SYS.ram.other - kvRam - ramCompute - 2.5 * GiB;
  const resident = s.loadMode === 'mmap' ? Math.min(cpuWeights, ramRoom) : cpuWeights;
  const disk = Math.max(0, cpuWeights - resident);
  const gpuTotal = SYS.gpu.other + gpuWeights + mmproj + kvGpu + compute;
  let fits = 'gpu';
  if (cpuWeights > embed * 1.2 || kvRam > 0) fits = 'hybrid';
  if (disk > 0) fits = 'disk';
  let ok = true;
  let error = '';
  if (gpuTotal > SYS.gpu.total) {
    ok = false;
    error = `This does not fit: the GPU would need ${(gpuTotal / GiB).toFixed(1)} GB of its ${(SYS.gpu.total / GiB).toFixed(0)} GB. Lower the context or the GPU layers.`;
    fits = 'no';
  } else if (s.loadMode !== 'mmap' && cpuWeights > ramRoom) {
    ok = false;
    error = `Load mode "${s.loadMode}" keeps ${(cpuWeights / GiB).toFixed(1)} GB of weights in RAM, but only ${(ramRoom / GiB).toFixed(1)} GB are free. Use mmap to let the rest stay on the NVMe.`;
    fits = 'no';
  }
  const notes = [];
  if (fits === 'gpu') notes.push('Everything fits on the GPU.');
  if (m.meta.moe && cpuMoe) notes.push(`The experts of ${cpuMoe} of ${L} layers stay in RAM; attention and the shared weights run on the GPU.`);
  if (!m.meta.moe && gpuLayers < L) notes.push(`${L - gpuLayers} of ${L} layers run on the CPU from RAM.`);
  if (disk > 0) notes.push(`${(disk / 1e9).toFixed(1)} GB of weights do not fit into RAM and are read from the NVMe as needed: generation slows down.`);
  if (kvRam > 0) notes.push('Part of the KV cache lives in RAM.');
  const summary = fits === 'gpu'
    ? `Everything on the GPU: ${(gpuTotal / GiB).toFixed(1)} GB of ${(SYS.gpu.total / GiB).toFixed(0)} GB.`
    : fits === 'hybrid'
      ? `GPU and RAM: ${(gpuTotal / GiB).toFixed(1)} GB on the GPU, ${((resident + kvRam) / GiB).toFixed(1)} GB in RAM.`
      : fits === 'disk'
        ? `GPU, RAM and NVMe: ${(gpuTotal / GiB).toFixed(1)} GB on the GPU, ${(resident / GiB).toFixed(1)} GB in RAM, ${(disk / 1e9).toFixed(1)} GB read from the NVMe.`
        : error;
  const placement = {
    source: 'estimated', fileBytes: weights,
    layers: { total: L, gpu: gpuLayers, expertLayersOnCpu: cpuMoe, moe: m.meta.moe },
    gpu: { weights: Math.round(gpuWeights + mmproj), kv: Math.round(kvGpu), compute: Math.round(compute) },
    ram: { weights: Math.round(cpuWeights), weightsResident: Math.round(resident), kv: Math.round(kvRam), compute: Math.round(ramCompute) },
    disk: { weights: Math.round(disk) },
    notes,
  };
  return { ok, error, ctx, gpuLayers, cpuMoe, args: argsFor(m, s, ctx, gpuLayers, cpuMoe), placement, fits, summary,
    tokensPerSecondHint: fits === 'gpu' ? 'fast' : fits === 'hybrid' ? 'medium' : 'slow' };
}

function argsFor(m, s, ctx, gpuLayers, cpuMoe) {
  const args = ['--model', `/data/models/${m.id}/${m.files[0].path}`, '--ctx-size', String(ctx), '--n-gpu-layers', String(gpuLayers),
    '--cache-type-k', s.kvType, '--cache-type-v', s.kvType, '--flash-attn', s.flashAttn, '--parallel', String(s.parallel),
    '--batch-size', String(s.batch), '--ubatch-size', String(s.ubatch), '--jinja', '--host', '127.0.0.1', '--port', '18080', '--metrics'];
  if (cpuMoe) args.push('--n-cpu-moe', String(cpuMoe));
  if (s.threads) args.push('--threads', String(s.threads));
  if (s.loadMode === 'mlock') args.push('--mlock');
  if (s.loadMode === 'ram') args.push('--no-mmap');
  if (s.vision && m.mmproj) args.push('--mmproj', `/data/models/${m.id}/${m.mmproj}`);
  if (s.speculative === 'mtp' && m.meta.mtp) args.push('--spec-mtp');
  if (s.reasoning !== 'auto') args.push('--reasoning', s.reasoning);
  if (s.reasoningBudget !== -1) args.push('--reasoning-budget', String(s.reasoningBudget));
  if (s.extraArgs) args.push(...s.extraArgs.split(/\s+/).filter(Boolean));
  return args;
}

/* ── The engine ───────────────────────────────────────────────────── */

const engine = {
  state: 'stopped', message: 'No model is loaded', progress: null, stage: '', since: now(), error: '',
  modelId: '', modelName: '', ctx: 0, slots: 1, restarts: 0, uptimeMs: 0, args: [],
  plan: null, readyAt: 0, run: 0,
};

function setEngine(fields) {
  Object.assign(engine, fields);
  if ('state' in fields) engine.since = now();
  pushStatus();
}

async function startEngine(m, { restart = false } = {}) {
  const run = ++engine.run;
  const alive = () => run === engine.run;
  const p = planFor(m, m.settings);
  if (restart) engine.restarts += 1;
  else engine.restarts = 0;
  setEngine({ state: 'planning', message: 'Planning where the weights go', progress: 0, stage: '', error: '', modelId: m.id, modelName: m.name,
    ctx: p.ctx, slots: effective(m).parallel, args: p.args, plan: p, readyAt: 0 });
  log(`socrates: planning ${m.name}`);
  for (let i = 1; i <= 5 && alive(); i += 1) {
    await sleep(220);
    if (alive()) setEngine({ progress: i / 5 });
  }
  if (!alive()) return;
  log(`socrates: ${p.summary}`);
  log(`socrates: starting llama-server ${p.args.join(' ')}`);
  setEngine({ state: 'starting', message: 'Starting llama-server', progress: null });
  await sleep(1200);
  if (!alive()) return;
  for (const line of [
    'build: 6912 (a1c3e0f2) with cc (Ubuntu 13.3.0) for x86_64-linux-gnu',
    'system info: n_threads = 8, n_threads_batch = 16, total_threads = 16',
    'ggml_cuda_init: found 1 CUDA devices:',
    `  Device 0: ${SYS.gpu.name}, compute capability 8.6, VMM: yes`,
    `llama_model_load_from_file_impl: using device CUDA0 (${SYS.gpu.name}) - 23871 MiB free`,
    `llama_model_loader: loaded meta data with 44 key-value pairs and ${m.meta.layers * 12 + 3} tensors from /data/models/${m.id}/${m.files[0].path}`,
    `print_info: file type   = ${m.quant}`,
    `print_info: file size   = ${(m.files.reduce((a, f) => a + f.size, 0) / GiB).toFixed(2)} GiB`,
    `print_info: n_ctx_train = ${m.meta.ctxTrain}`,
    `print_info: n_layer     = ${m.meta.layers}`,
    m.meta.moe ? `print_info: n_expert    = ${m.meta.experts}, n_expert_used = ${m.meta.expertsUsed}` : `print_info: model params = ${(m.meta.params / 1e9).toFixed(2)} B`,
  ]) log(line);
  setEngine({ state: 'loading', message: 'Loading weights into memory', progress: 0, stage: 'text_model' });
  const steps = m.meta.moe ? 60 : 36;
  for (let i = 1; i <= steps && alive(); i += 1) {
    await sleep(m.meta.moe ? 420 : 300);
    if (!alive()) return;
    setEngine({ progress: (i / steps) * (m.mmproj ? 0.93 : 1) });
    if (i === 4) log(`load_tensors: offloading ${p.gpuLayers} repeating layers to GPU`);
    if (i === 5) log(`load_tensors: offloaded ${p.gpuLayers}/${m.meta.layers + 1} layers to GPU`);
    if (i === 6) log(`load_tensors:        CUDA0 model buffer size = ${(p.placement.gpu.weights / MiB).toFixed(2)} MiB`);
    if (i === 7) log(`load_tensors:   CPU_Mapped model buffer size = ${(p.placement.ram.weights / MiB).toFixed(2)} MiB`);
    if (i % 9 === 0) log('.'.repeat(i));
  }
  if (!alive()) return;
  if (m.mmproj && effective(m).vision) {
    setEngine({ message: 'Loading the vision projector', stage: 'mmproj_model' });
    log(`clip_model_loader: model name:   ${m.name}`);
    log(`clip_model_loader: loading mmproj from /data/models/${m.id}/${m.mmproj}`);
    await sleep(900);
    if (!alive()) return;
  }
  log(`llama_context: n_ctx         = ${p.ctx}`);
  log(`llama_context: n_ctx_per_seq = ${Math.floor(p.ctx / effective(m).parallel)}`);
  log(`llama_kv_cache:      CUDA0 KV buffer size = ${(p.placement.gpu.kv / MiB).toFixed(2)} MiB`);
  log(`llama_context:      CUDA0 compute buffer size = ${(p.placement.gpu.compute / MiB).toFixed(2)} MiB`);
  log('srv    load_model: initializing slots, n_slots = 1');
  log('main: model loaded');
  log('main: server is listening on http://127.0.0.1:18080 - starting the main loop');
  log('srv  update_slots: all slots are idle');
  engine.plan.placement = { ...engine.plan.placement, source: 'measured' };
  setEngine({ state: 'ready', message: 'Ready', progress: null, stage: '', readyAt: now() });
}

function stopEngine() {
  engine.run += 1;
  log('socrates: stopping llama-server');
  log('srv    operator(): operator(): cleaning up before exit...');
  setEngine({ state: 'stopped', message: 'No model is loaded', progress: null, stage: '', modelId: '', modelName: '', ctx: 0, args: [], plan: null, readyAt: 0 });
}

/* ── Live activity ────────────────────────────────────────────────── */

const activity = { requests: [], last: null };
let requestSeq = 40;
const requests = [];
const waiters = new Set();

function whenReady() {
  if (engine.state === 'ready') return Promise.resolve(true);
  if (engine.state === 'stopped' || engine.state === 'error' || engine.state === 'crashed') return Promise.resolve(false);
  return new Promise((done) => waiters.add(done));
}

let gate = Promise.resolve();

/*
 * One request through the single slot: waits for the engine and the slot,
 * then reads the prompt, thinks and writes, calling emit() for each event.
 */
async function serve({ source, key, path, promptTokens, cached, reasoning, answer, maxTokens, emit, signal }) {
  const id = `r${++requestSeq}`;
  const r = { id, source, key, phase: 'queued', promptTokens, promptDone: 0, cachedTokens: cached, generated: 0, tps: 0, startedAt: now() };
  activity.requests.push(r);
  pushStatus();
  let release;
  const previous = gate;
  gate = new Promise((done) => { release = done; });
  try {
    const ready = await whenReady();
    if (!ready) throw new Error(engine.state === 'stopped' ? 'No model is loaded. Activate one on the Models page.' : `The engine is not running: ${engine.error || engine.message}`);
    await previous;
    if (signal.aborted) throw new Error('aborted');
    const m = models.find((x) => x.id === engine.modelId);
    const speed = m.sim;
    log(`slot launch_slot_: id  0 | task ${requestSeq} | processing task`);
    log(`slot update_slots: id  0 | task ${requestSeq} | new prompt, n_ctx_slot = ${engine.ctx}, n_keep = 0, n_prompt_tokens = ${promptTokens}`);
    if (cached) log(`slot update_slots: id  0 | task ${requestSeq} | kv cache rm [${cached}, end)`);
    r.phase = 'prompt';
    r.promptDone = cached;
    const startedPrompt = now();
    const batch = 512;
    while (r.promptDone < promptTokens) {
      const stepTokens = Math.min(batch, promptTokens - r.promptDone);
      // Short prompts read slowly (about 160 tok/s: fixed costs per batch), long ones near the full speed.
      await sleep((stepTokens / Math.min(speed.promptTps, 160 + promptTokens * 0.3)) * 1000 * rand(0.9, 1.1));
      if (signal.aborted) throw new Error('aborted');
      r.promptDone += stepTokens;
      r.tps = Math.round(((r.promptDone - cached) / Math.max(1, now() - startedPrompt)) * 1000 * 10) / 10;
      emit({ prompt_progress: { total: promptTokens, cache: cached, processed: r.promptDone, time_ms: now() - startedPrompt } });
      pushStatus();
    }
    const promptMs = Math.max(1, now() - startedPrompt);
    log(`slot update_slots: id  0 | task ${requestSeq} | prompt done, n_past = ${promptTokens}, n_tokens = ${promptTokens - cached}`);
    const startedGen = now();
    let generated = 0;
    const timings = () => ({
      cache_n: cached, prompt_n: promptTokens - cached, prompt_ms: promptMs,
      prompt_per_token_ms: promptMs / Math.max(1, promptTokens - cached),
      prompt_per_second: ((promptTokens - cached) / promptMs) * 1000,
      predicted_n: generated, predicted_ms: now() - startedGen,
      predicted_per_token_ms: (now() - startedGen) / Math.max(1, generated),
      predicted_per_second: generated ? (generated / Math.max(1, now() - startedGen)) * 1000 : 0,
    });
    const streamTokens = async (text, field, phase) => {
      r.phase = phase;
      for (const token of tokenize(text)) {
        if (maxTokens && generated >= maxTokens) return false;
        await sleep((1000 / speed.genTps) * rand(0.75, 1.25));
        if (signal.aborted) throw new Error('aborted');
        generated += 1;
        r.generated = generated;
        r.tps = Math.round(timings().predicted_per_second * 10) / 10;
        emit({ delta: { [field]: token }, timings: timings() });
      }
      return true;
    };
    let finished = true;
    if (reasoning) finished = await streamTokens(reasoning, 'reasoning_content', 'thinking');
    if (finished) finished = await streamTokens(answer, 'content', 'generating');
    const t = timings();
    activity.last = { promptTokens, cachedTokens: cached, completionTokens: generated, promptTps: Math.round(t.prompt_per_second * 10) / 10,
      genTps: Math.round(t.predicted_per_second * 10) / 10, ms: now() - r.startedAt, at: now() };
    log(`prompt eval time = ${t.prompt_ms.toFixed(2)} ms / ${t.prompt_n} tokens (${t.prompt_per_token_ms.toFixed(2)} ms per token, ${t.prompt_per_second.toFixed(2)} tokens per second)`);
    log(`       eval time = ${t.predicted_ms.toFixed(2)} ms / ${t.predicted_n} tokens (${t.predicted_per_token_ms.toFixed(2)} ms per token, ${t.predicted_per_second.toFixed(2)} tokens per second)`);
    log(`      total time = ${(t.prompt_ms + t.predicted_ms).toFixed(2)} ms / ${t.prompt_n + t.predicted_n} tokens`);
    log(`slot      release: id  0 | task ${requestSeq} | stop processing: n_past = ${promptTokens + generated}, truncated = 0`);
    log('srv  update_slots: all slots are idle');
    addRequest({ source, keyName: key, path, status: 200, promptTokens, cachedTokens: cached, completionTokens: generated,
      promptTps: activity.last.promptTps, genTps: activity.last.genTps, ms: activity.last.ms, error: '' });
    emit({ done: true, finish: finished ? 'stop' : 'length', timings: t, usage: { prompt_tokens: promptTokens, completion_tokens: generated, total_tokens: promptTokens + generated, prompt_tokens_details: { cached_tokens: cached } } });
    return { promptTokens, generated };
  } catch (error) {
    if (error.message === 'aborted') {
      log(`srv  cancel_tasks: cancel task, id_task = ${requestSeq}`);
      addRequest({ source, keyName: key, path, status: 499, promptTokens, cachedTokens: cached, completionTokens: r.generated, promptTps: 0, genTps: r.tps, ms: now() - r.startedAt, error: 'The client closed the connection.' });
    } else {
      addRequest({ source, keyName: key, path, status: 503, promptTokens: 0, cachedTokens: 0, completionTokens: 0, promptTps: 0, genTps: 0, ms: now() - r.startedAt, error: error.message });
    }
    throw error;
  } finally {
    activity.requests = activity.requests.filter((x) => x !== r);
    release();
    pushStatus();
  }
}

function tokenize(text) {
  return text.match(/\s*[^\s]{1,5}|\s+/g) || [];
}

function addRequest(fields) {
  const m = models.find((x) => x.id === engine.modelId);
  requests.unshift({ id: `q${requestSeq}`, at: now(), model: m ? m.id : '', ...fields });
  requests.length = Math.min(requests.length, 100);
  const k = keys.find((x) => x.name === fields.keyName);
  if (k) {
    k.lastUsedAt = now();
    k.requests += 1;
    k.tokensIn += fields.promptTokens;
    k.tokensOut += fields.completionTokens;
    change('keys');
  }
  change('requests');
}

/* Requests that came in before the mock started, for the list. */
(function seedRequests() {
  const paths = ['/v1/chat/completions', '/v1/chat/completions', '/v1/chat/completions', '/v1/messages', '/v1/models', '/v1/chat/completions'];
  let at = now() - 3 * 3600e3;
  for (let i = 0; i < 28; i += 1) {
    at += rand(2, 9) * 60e3;
    const path = pick(paths);
    const chat = i % 5 === 3;
    const isList = path === '/v1/models';
    const p = isList ? 0 : Math.round(rand(400, 14000));
    const c = isList ? 0 : Math.round(p * rand(0.2, 0.9));
    const g = isList ? 0 : Math.round(rand(60, 900));
    const failed = i === 17;
    requests.unshift({ id: `q${i}`, at, source: chat ? 'chat' : 'api', keyName: chat ? '' : pick(['opencode', 'opencode', 'home-assistant']), path: chat ? '/api/chat' : path, model: 'huihui-qwen3-8-27b-ud-q4-k-xl',
      status: failed ? 401 : 200, promptTokens: failed ? 0 : p, cachedTokens: failed ? 0 : c, completionTokens: failed ? 0 : g,
      promptTps: isList || failed ? 0 : Math.round(rand(1300, 1500) * 10) / 10, genTps: isList || failed ? 0 : Math.round(rand(64, 71) * 10) / 10,
      ms: isList ? 3 : failed ? 1 : Math.round(((p - c) / 1450 + g / 68) * 1000), error: failed ? 'The API key is not valid.' : '' });
  }
}());

/* ── Keys and settings ────────────────────────────────────────────── */

const keys = [
  { id: 'k1', name: 'opencode', prefix: 'sk-soc-7f3a', createdAt: now() - 8 * 86400e3, lastUsedAt: now() - 14 * 60e3, requests: 1284, tokensIn: 18420311, tokensOut: 1033871 },
  { id: 'k2', name: 'home-assistant', prefix: 'sk-soc-c91e', createdAt: now() - 6 * 86400e3, lastUsedAt: now() - 3 * 3600e3, requests: 212, tokensIn: 401233, tokensOut: 38122 },
  { id: 'k3', name: 'laptop scripts', prefix: 'sk-soc-02bd', createdAt: now() - 2 * 86400e3, lastUsedAt: 0, requests: 0, tokensIn: 0, tokensOut: 0 },
];
const settings = { hfToken: '', requireKey: true, modelsDir: '/data/models' };

/* ── Hugging Face ─────────────────────────────────────────────────── */

const hf = [
  { id: 'huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF', downloads: 48210, likes: 312, gguf: true, pipeline: 'image-text-to-text', params: 27.8e9, license: 'apache-2.0', baseModel: 'Qwen/Qwen3.8-27B',
    quants: [['Q2_K', 10.9], ['Q3_K_M', 13.4], ['Q4_K', 16.81], ['UD-Q4_K_XL', 17.36], ['Q5_K_M', 19.6], ['Q6_K', 22.8], ['Q8_0', 29.5], ['F16', 55.6]], file: 'Huihui-Qwen3.8-27B-abliterated.{q}.gguf',
    mmproj: [['mmproj-model-bf16.gguf', 0.93e9]] },
  { id: 'unsloth/Qwen3.8-Flash-Next-GGUF', downloads: 211904, likes: 1204, gguf: true, pipeline: 'text-generation', params: 235e9, license: 'apache-2.0', baseModel: 'Qwen/Qwen3.8-Flash-Next', moe: true,
    quants: [['UD-IQ1_S', 52.0], ['UD-IQ2_XXS', 64.3], ['UD-IQ3_XXS', 83.6], ['UD-Q4_K_XL', 134.1], ['Q8_0', 249.8]], file: '{q}/Qwen3.8-Flash-Next-{q}-0000{i}-of-0000{n}.gguf', split: 45 },
  { id: 'unsloth/Qwen3.8-27B-GGUF', downloads: 389120, likes: 2210, gguf: true, pipeline: 'image-text-to-text', params: 27.8e9, license: 'apache-2.0', baseModel: 'Qwen/Qwen3.8-27B',
    quants: [['UD-Q2_K_XL', 11.2], ['Q3_K_M', 13.4], ['Q4_K_M', 16.9], ['UD-Q4_K_XL', 17.3], ['Q5_K_M', 19.6], ['Q6_K', 22.8], ['Q8_0', 29.5], ['BF16', 55.6]], file: 'Qwen3.8-27B-{q}.gguf',
    mmproj: [['mmproj-F16.gguf', 0.93e9], ['mmproj-BF16.gguf', 0.93e9]] },
  { id: 'bartowski/google_gemma-4-12b-it-GGUF', downloads: 96433, likes: 401, gguf: true, pipeline: 'image-text-to-text', params: 12.2e9, license: 'gemma', baseModel: 'google/gemma-4-12b-it',
    quants: [['Q3_K_M', 6.0], ['Q4_K_M', 7.3], ['Q5_K_M', 8.44], ['Q6_K', 9.7], ['Q8_0', 12.5]], file: 'google_gemma-4-12b-it-{q}.gguf', mmproj: [['mmproj-google_gemma-4-12b-it-f16.gguf', 0.854e9]] },
  { id: 'unsloth/Qwen3.8-Coder-32B-Instruct-GGUF', downloads: 154002, likes: 870, gguf: true, pipeline: 'text-generation', params: 32.8e9, license: 'apache-2.0', baseModel: 'Qwen/Qwen3.8-Coder-32B-Instruct',
    quants: [['Q3_K_M', 15.9], ['Q4_K_M', 19.9], ['Q5_K_M', 23.3], ['Q6_K', 26.9], ['Q8_0', 34.8]], file: 'Qwen3.8-Coder-32B-Instruct-{q}.gguf' },
  { id: 'nomic-ai/nomic-embed-text-v2-GGUF', downloads: 51200, likes: 140, gguf: true, pipeline: 'feature-extraction', params: 0.475e9, license: 'apache-2.0', baseModel: 'nomic-ai/nomic-embed-text-v2',
    quants: [['Q8_0', 0.51], ['F16', 0.95]], file: 'nomic-embed-text-v2.{q}.gguf' },
  { id: 'Qwen/Qwen3.8-27B', downloads: 1520331, likes: 3120, gguf: false, convertible: true, pipeline: 'image-text-to-text', params: 27.8e9, license: 'apache-2.0', baseModel: '',
    quantized: [['unsloth/Qwen3.8-27B-GGUF', 389120], ['bartowski/Qwen_Qwen3.8-27B-GGUF', 120334], ['mradermacher/Qwen3.8-27B-GGUF', 49375]] },
  { id: 'mistralai/Magistral-Small-2611', downloads: 88123, likes: 610, gguf: false, convertible: true, pipeline: 'text-generation', params: 24e9, license: 'apache-2.0', baseModel: '',
    quantized: [['bartowski/mistralai_Magistral-Small-2611-GGUF', 33102], ['unsloth/Magistral-Small-2611-GGUF', 28870]] },
  { id: 'meta-llama/Llama-5-8B-Instruct', downloads: 702334, likes: 1980, gguf: false, convertible: true, gated: true, pipeline: 'text-generation', params: 8.0e9, license: 'llama5', baseModel: '',
    quantized: [['bartowski/Meta-Llama-5-8B-Instruct-GGUF', 210443]] },
  { id: 'research-lab/sparse-mixer-v0', downloads: 120, likes: 3, gguf: false, convertible: false, pipeline: 'text-generation', params: 3e9, license: 'mit', baseModel: '', quantized: [] },
];

function fitOf(size, moe) {
  const gpuRoom = SYS.gpu.total - SYS.gpu.other - 1 * GiB - 2.6 * GiB;
  const ramRoom = SYS.ram.total - SYS.ram.other - 4 * GiB;
  if (size <= gpuRoom) return ['gpu', `Fits on the GPU with 32k context, ${((gpuRoom - size) / GiB).toFixed(1)} GB to spare`];
  if (size <= gpuRoom + ramRoom) {
    const ram = size - gpuRoom * (moe ? 0.85 : 0.95);
    return ['hybrid', `${(ram / 1e9).toFixed(0)} GB of it in RAM${moe ? ': MoE experts run on the CPU, still usable' : ': noticeably slower'}`];
  }
  const freeDisk = SYS.disk.total - SYS.disk.used;
  if (size <= freeDisk) return ['disk', `About ${((size - gpuRoom - ramRoom) / 1e9).toFixed(0)} GB are read from the NVMe while it answers: slow`];
  return ['no', 'Larger than the GPU, RAM and the free NVMe together'];
}

function repoDetails(entry) {
  const base = { id: entry.id, gated: !!entry.gated, license: entry.license, pipeline: entry.pipeline, params: entry.params, baseModel: entry.baseModel, gguf: entry.gguf };
  if (!entry.gguf) {
    return { ...base, variants: [], mmproj: [], quantized: entry.quantized.map(([id, downloads]) => ({ id, downloads })), convertible: entry.convertible };
  }
  const variants = entry.quants.map(([q, gb]) => {
    const size = Math.round(gb * 1e9);
    const parts = entry.split && gb > entry.split ? Math.ceil(gb / entry.split) : 1;
    const files = Array.from({ length: parts }, (_, i) => entry.file.replaceAll('{q}', q).replace('{i}', String(i + 1)).replace('{n}', String(parts)))
      .map((f) => (parts === 1 ? f.replace(/-00001-of-00001/, '') : f));
    const [fit, note] = fitOf(size, entry.moe);
    return { quant: q, files, size, fit, note, recommended: false };
  });
  const order = ['gpu', 'hybrid', 'disk'];
  for (const fit of order) {
    const fitting = variants.filter((v) => v.fit === fit && !/^(F16|BF16)$/.test(v.quant));
    if (fitting.length) {
      fitting[fitting.length - 1].recommended = true;
      break;
    }
  }
  return { ...base, variants, mmproj: (entry.mmproj || []).map(([file, size]) => ({ file, size })), quantized: [], convertible: false };
}

function findRepo(id) {
  const lower = id.toLowerCase();
  const known = hf.find((e) => e.id.toLowerCase() === lower);
  if (known) return known;
  if (!/^[\w.-]+\/[\w.-]+$/.test(id)) return null;
  if (/gguf$/i.test(id)) {
    return { id, downloads: 812, likes: 4, gguf: true, pipeline: 'text-generation', params: 7.6e9, license: 'unknown', baseModel: '',
      quants: [['Q4_K_M', 4.68], ['Q5_K_M', 5.44], ['Q6_K', 6.25], ['Q8_0', 8.1]], file: `${id.split('/')[1].replace(/-GGUF$/i, '')}-{q}.gguf` };
  }
  return null;
}

/* ── Downloads and conversions ────────────────────────────────────── */

const transfers = new Map(); // modelId -> { file, done, total, bytesPerSec, state, phase }

function startTransfer(m, { convert = false } = {}) {
  const total = convert ? m.convertBytes : m.size;
  transfers.set(m.id, { file: convert ? 'model-00001-of-00012.safetensors' : m.files[0].path, done: Math.round(total * (m.progress || 0)), total, bytesPerSec: 0, state: convert ? 'converting' : 'downloading', phase: 'download' });
  m.state = convert ? 'converting' : 'downloading';
  m.error = '';
}

setInterval(() => {
  for (const [id, t] of transfers) {
    const m = models.find((x) => x.id === id);
    if (!m) {
      transfers.delete(id);
      continue;
    }
    if (m.failAfter && t.done / t.total > m.failAfter) {
      m.state = 'failed';
      m.error = 'Hugging Face refused the download (403): this model is gated. Accept its licence on huggingface.co and add a Hugging Face token on the API page.';
      m.failAfter = 0;
      transfers.delete(id);
      change('models');
      continue;
    }
    if (t.phase === 'download') {
      t.bytesPerSec = Math.round(rand(104, 118) * 1e6);
      t.done = Math.min(t.total, t.done + t.bytesPerSec / 2);
      if (t.state === 'converting') {
        const share = t.done / t.total;
        t.file = `model-${String(1 + Math.floor(share * 11.99)).padStart(5, '0')}-of-00012.safetensors`;
        m.progress = share * 0.7;
      } else {
        m.progress = t.done / t.total;
        if (m.files.length > 1) t.file = m.files[Math.min(m.files.length - 1, Math.floor(m.progress * m.files.length))].path;
      }
      if (t.done >= t.total) {
        if (t.state === 'converting') {
          t.phase = 'convert';
          t.bytesPerSec = 0;
          t.file = `${m.id}.f16.gguf`;
          log(`convert: ${m.repo} -> GGUF (f16)`);
        } else {
          m.state = 'ready';
          m.progress = 1;
          transfers.delete(id);
          change('models');
        }
      }
    } else {
      m.progress = Math.min(1, m.progress + 0.012);
      t.file = m.progress < 0.85 ? `${m.id}.f16.gguf` : m.files[0].path;
      if (m.progress >= 1) {
        m.state = 'ready';
        transfers.delete(id);
        change('models');
      }
    }
  }
  if (transfers.size) pushStatus();
}, 500);

/* ── Background API traffic, so the lists move ────────────────────── */

setInterval(() => {
  if (engine.state !== 'ready' || activity.requests.length || Math.random() < 0.55) return;
  const ctrl = new AbortController();
  const promptTokens = Math.round(rand(2400, 9000));
  serve({ source: 'api', key: pick(['opencode', 'opencode', 'home-assistant']), path: '/v1/chat/completions', promptTokens, cached: Math.round(promptTokens * rand(0.4, 0.9)),
    reasoning: '', answer: 'The function returns early when the list is empty, so the caller never sees the error. Moving the check after the loop fixes it.',
    maxTokens: 0, emit: () => {}, signal: ctrl.signal }).catch(() => {});
}, 20000);

/* ── Chat answers ─────────────────────────────────────────────────── */

const ANSWERS = [
  {
    reasoning: 'The user asks how the model is split between GPU, RAM and disk. I should explain the three tiers briefly: VRAM holds the layers that fit, plus the KV cache and compute buffers. The rest of the weights stay in RAM via mmap, and if RAM runs out, the page cache cannot hold them all, so the kernel reads them from the NVMe on demand. Give a short example and a tip about MoE experts.',
    answer: 'Socrates places the model in **three tiers**, fastest first:\n\n1. **GPU (VRAM)**: as many layers as fit, plus the KV cache and the compute buffers.\n2. **RAM**: the remaining weights, memory-mapped from the file, so they live in the page cache.\n3. **NVMe**: whatever does not fit into RAM is read from disk *as needed*, which is slow but works.\n\nFor a MoE model it keeps attention on the GPU and moves only the *experts* of some layers to RAM, e.g.:\n\n```sh\nllama-server -m model.gguf --n-gpu-layers 99 --n-cpu-moe 29\n```\n\nTip: lower `context` or use `kvType: q4_0` to fit more layers on the GPU.',
  },
  {
    reasoning: 'A short creative request. Keep it to three lines with 5-7-5 syllables. Theme: a GPU that is warm and busy.',
    answer: 'Fans hum through the night,\nweights glide from disk into light,\ntokens fall like rain.',
  },
  {
    reasoning: 'They want a quick Python example for streaming from an OpenAI-compatible server. Show the client with base_url and stream=True, iterate over chunks and print delta content. Mention that the API key comes from the API page.',
    answer: 'Here is a minimal streaming client with the official `openai` package:\n\n```python\nfrom openai import OpenAI\n\nclient = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-soc-...")\nstream = client.chat.completions.create(\n    model="huihui-qwen3-8-27b-ud-q4-k-xl",\n    messages=[{"role": "user", "content": "Hello!"}],\n    stream=True,\n)\nfor chunk in stream:\n    print(chunk.choices[0].delta.content or "", end="", flush=True)\n```\n\nCreate the key on the **API** page; it is shown only once.',
  },
];
let answerIndex = 0;

/* ── Logs and live events ─────────────────────────────────────────── */

const logLines = [];
let pendingLog = [];
function log(line) {
  logLines.push(line);
  if (logLines.length > 1000) logLines.splice(0, logLines.length - 1000);
  pendingLog.push(line);
}

const clients = new Set();
function send(event, data) {
  const payload = `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
  for (const res of clients) res.write(payload);
}

let statusQueued = false;
function pushStatus() {
  if (statusQueued) return;
  statusQueued = true;
  setTimeout(() => {
    statusQueued = false;
    send('status', status());
  }, 250);
}
function change(...topics) {
  send('change', { topics });
}

setInterval(() => {
  send('status', status());
  if (pendingLog.length) {
    send('log', { lines: pendingLog });
    pendingLog = [];
  }
  for (const done of waiters) {
    if (engine.state === 'ready') done(true);
    else if (['stopped', 'error', 'crashed'].includes(engine.state)) done(false);
    else continue;
    waiters.delete(done);
  }
}, 1000);
setInterval(() => {
  if (pendingLog.length) {
    send('log', { lines: pendingLog });
    pendingLog = [];
  }
}, 300);

function status() {
  const busy = activity.requests.some((r) => r.phase !== 'queued');
  const p = engine.plan ? engine.plan.placement : null;
  let gpuUsed = SYS.gpu.other;
  let rss = 0;
  let resident = 0;
  if (p && engine.state !== 'stopped') {
    const share = engine.state === 'ready' ? 1 : engine.state === 'loading' ? engine.progress : 0.02;
    gpuUsed += (p.gpu.weights + p.gpu.kv + p.gpu.compute) * share;
    resident = p.ram.weightsResident * share;
    rss = resident + p.ram.kv + p.ram.compute + 0.4 * GiB;
  }
  const ramUsed = SYS.ram.other + (rss - resident);
  const cached = SYS.ram.cache + resident;
  return {
    engine: {
      state: engine.state, message: engine.message, progress: engine.progress, stage: engine.stage, since: engine.since, error: engine.error,
      modelId: engine.modelId, modelName: engine.modelName, ctx: engine.ctx, slots: engine.slots, restarts: engine.restarts,
      uptimeMs: engine.readyAt ? now() - engine.readyAt : 0, args: engine.args, version: 'b6912',
      fits: engine.plan ? engine.plan.fits : '', summary: engine.plan ? engine.plan.summary : '',
    },
    activity: {
      running: activity.requests.filter((r) => r.phase !== 'queued').length,
      queued: activity.requests.filter((r) => r.phase === 'queued').length,
      requests: activity.requests.map((r) => ({ ...r })),
      last: activity.last,
    },
    placement: p ? JSON.parse(JSON.stringify(p)) : null,
    system: {
      gpus: [{ name: SYS.gpu.name, total: SYS.gpu.total, used: Math.round(gpuUsed), util: busy ? Math.round(rand(86, 97)) : Math.round(rand(0, 3)),
        tempC: busy ? Math.round(rand(63, 67)) : Math.round(rand(44, 47)), powerW: busy ? Math.round(rand(270, 320)) : Math.round(rand(24, 31)) }],
      ram: { total: SYS.ram.total, used: Math.round(ramUsed), available: Math.round(SYS.ram.total - ramUsed), cached: Math.round(cached), engineRss: Math.round(rss) },
      disk: { path: SYS.disk.path, total: SYS.disk.total, free: Math.round(SYS.disk.total - SYS.disk.used - [...transfers.values()].reduce((a, t) => a + t.done, 0)) },
      cpu: { name: SYS.cpu.name, threads: SYS.cpu.threads, load: busy ? rand(0.12, 0.2) : rand(0.02, 0.05) },
    },
    downloads: [...transfers].filter(([, t]) => t.phase === 'download').map(([modelId, t]) => ({ modelId, file: t.file, done: Math.round(t.done), total: t.total, bytesPerSec: t.bytesPerSec, state: t.state })),
    converts: [...transfers].filter(([, t]) => t.state === 'converting').map(([modelId, t]) => {
      const m = models.find((x) => x.id === modelId);
      const p = m ? m.progress : 0;
      const step = t.phase === 'download' ? 1 : p < 0.85 ? 2 : 3;
      const titles = ['Downloading the full-precision weights', 'Converting to GGUF (f16)', `Quantising to ${m ? m.quant : ''}`];
      const local = step === 1 ? t.done / t.total : step === 2 ? (p - 0.7) / 0.15 : (p - 0.85) / 0.15;
      return { modelId, step, steps: 3, title: titles[step - 1], progress: Math.max(0, Math.min(1, local)),
        message: step === 1 ? `${t.file} · ${(t.done / 1e9).toFixed(1)} of ${(t.total / 1e9).toFixed(1)} GB` : step === 2 ? `Writing ${t.file}` : `Writing ${m ? m.files[0].path : ''}` };
    }),
  };
}

/* ── HTTP ─────────────────────────────────────────────────────────── */

const sessions = new Set();
let password = process.env.MOCK_PASSWORD || '';

const json = (res, code, data, headers = {}) => {
  res.writeHead(code, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store', ...headers });
  res.end(JSON.stringify(data));
};
const fail = (res, code, error, fields) => json(res, code, fields ? { error, fields } : { error });

function sessionOf(req) {
  const match = /(?:^|;\s*)socrates_session=([\w-]+)/.exec(req.headers.cookie || '');
  return match && sessions.has(match[1]) ? match[1] : '';
}

function newSession(res) {
  const token = randomBytes(18).toString('base64url');
  sessions.add(token);
  return { 'Set-Cookie': `socrates_session=${token}; Path=/; HttpOnly; SameSite=Strict` };
}

async function body(req) {
  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  if (!chunks.length) return {};
  try {
    return JSON.parse(Buffer.concat(chunks).toString('utf8'));
  } catch {
    return {};
  }
}

const sleep = (ms) => new Promise((done) => setTimeout(done, ms));
const slug = (text) => text.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');

const TYPES = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.svg': 'image/svg+xml',
  '.json': 'application/json', '.woff2': 'font/woff2', '.map': 'application/json', '.png': 'image/png' };
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'";

async function serveFile(req, res) {
  const url = new URL(req.url, 'http://x');
  let path = decodeURIComponent(url.pathname);
  if (path === '/') path = '/index.html';
  const file = normalize(join(ROOT, path));
  if (!file.startsWith(ROOT)) return fail(res, 404, 'Not found.');
  try {
    const info = await stat(file);
    if (!info.isFile()) throw new Error('dir');
    const data = await readFile(file);
    res.writeHead(200, { 'Content-Type': TYPES[extname(file)] || 'application/octet-stream', 'Cache-Control': 'no-cache', 'Content-Security-Policy': CSP, 'X-Content-Type-Options': 'nosniff' });
    res.end(data);
  } catch {
    res.writeHead(404, { 'Content-Type': 'text/plain' });
    res.end('Not found');
  }
}

const routes = [];
const route = (method, pattern, handler, { open = false } = {}) => routes.push({ method, pattern: new RegExp(`^${pattern}$`), handler, open });

route('GET', '/api/session', (req, res) => json(res, 200, { signedIn: !!sessionOf(req), needsPassword: !password, version: '0.1.0' }), { open: true });
route('POST', '/api/setup', async (req, res) => {
  if (password) return fail(res, 409, 'The admin password is already set. Sign in instead.');
  const b = await body(req);
  if (!b.password || b.password.length < 10) return fail(res, 400, 'Use at least 10 characters.', { password: 'Use at least 10 characters.' });
  password = b.password;
  json(res, 200, { signedIn: true }, newSession(res));
}, { open: true });
route('POST', '/api/login', async (req, res) => {
  const b = await body(req);
  await sleep(300);
  if (!password) return fail(res, 409, 'Set the admin password first.');
  if (b.password !== password) return fail(res, 401, 'That password is not right.');
  json(res, 200, { signedIn: true }, newSession(res));
}, { open: true });
route('POST', '/api/logout', (req, res) => {
  sessions.delete(sessionOf(req));
  json(res, 200, {}, { 'Set-Cookie': 'socrates_session=; Path=/; Max-Age=0' });
});
route('POST', '/api/password', async (req, res) => {
  const b = await body(req);
  if (b.current !== password) return fail(res, 400, 'The current password is not right.', { current: 'The current password is not right.' });
  if (!b.password || b.password.length < 10) return fail(res, 400, 'Use at least 10 characters.', { password: 'Use at least 10 characters.' });
  password = b.password;
  json(res, 200, { message: 'The password was changed. Other browsers are signed out.' });
});

route('GET', '/api/events', (req, res) => {
  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store', Connection: 'keep-alive' });
  res.write(`event: status\ndata: ${JSON.stringify(status())}\n\n`);
  clients.add(res);
  req.on('close', () => clients.delete(res));
});
route('GET', '/api/status', (req, res) => json(res, 200, status()));
route('GET', '/api/logs', (req, res) => json(res, 200, { lines: logLines.slice(-1000) }));

const publicModel = (m) => {
  const { sim, mmprojSize, failAfter, convertBytes, ...rest } = m;
  return { ...rest, active: m.id === engine.modelId };
};
route('GET', '/api/models', (req, res) => json(res, 200, { models: models.map(publicModel), activeId: engine.modelId, dataDir: settings.modelsDir,
  freeBytes: status().system.disk.free }));
route('POST', '/api/models', async (req, res) => {
  const b = await body(req);
  const entry = findRepo(b.repo || '');
  if (!entry || !entry.gguf) return fail(res, 404, 'That repository has no such GGUF file.');
  const details = repoDetails(entry);
  const variant = details.variants.find((v) => v.files[0] === (b.files || [])[0]);
  if (!variant) return fail(res, 400, 'Choose a quantisation.');
  const id = slug(`${entry.id.split('/')[1].replace(/-GGUF$/i, '')}-${variant.quant}`);
  if (models.some((m) => m.id === id)) return fail(res, 409, 'This model is already installed.');
  const mm = (details.mmproj || []).find((x) => x.file === b.mmproj);
  const m = {
    id, name: (b.name || '').trim() || `${entry.id.split('/')[1].replace(/-GGUF$/i, '').replace(/[-_]/g, ' ')} · ${variant.quant}`,
    repo: entry.id, quant: variant.quant,
    files: variant.files.map((path, i, all) => ({ path, size: Math.round(variant.size / all.length) })),
    mmproj: mm ? mm.file : '', mmprojSize: mm ? mm.size : 0, size: variant.size + (mm ? mm.size : 0),
    state: 'queued', error: '', progress: 0, active: false,
    meta: { arch: 'qwen3', params: entry.params, ctxTrain: 131072, layers: Math.round(clamp(entry.params / 0.45e9, 24, 94)), experts: entry.moe ? 128 : 0,
      expertsUsed: entry.moe ? 8 : 0, moe: !!entry.moe, vision: !!mm, mtp: false, chatTemplate: true },
    settings: {}, createdAt: now(),
    sim: { kvPerToken: 40960, promptTps: 1800, genTps: 45, sharedShare: 0.08 },
    failAfter: entry.gated && !settings.hfToken ? 0.001 : 0,
  };
  models.push(m);
  setTimeout(() => { startTransfer(m); change('models'); }, 800);
  change('models');
  json(res, 200, publicModel(m));
});
route('POST', '/api/models/convert', async (req, res) => {
  const b = await body(req);
  const entry = findRepo(b.repo || '');
  if (!entry || entry.gguf || !entry.convertible) return fail(res, 400, 'This repository cannot be converted.');
  const quant = b.quant || 'Q4_K_M';
  const bits = { Q4_K_M: 4.85, Q5_K_M: 5.7, Q6_K: 6.6, Q8_0: 8.5 }[quant] || 4.85;
  const size = Math.round((entry.params * bits) / 8);
  const id = slug(`${entry.id.split('/')[1]}-${quant}`);
  if (models.some((m) => m.id === id)) return fail(res, 409, 'This model is already installed.');
  const m = {
    id, name: (b.name || '').trim() || `${entry.id.split('/')[1].replace(/[-_]/g, ' ')} · ${quant}`, repo: entry.id, quant,
    files: [{ path: `${id}.gguf`, size }], mmproj: '', mmprojSize: 0, size, state: 'converting', error: '', progress: 0, active: false,
    meta: { arch: 'llama', params: entry.params, ctxTrain: 131072, layers: 32, experts: 0, expertsUsed: 0, moe: false, vision: false, mtp: false, chatTemplate: true },
    settings: {}, createdAt: now(), convertBytes: Math.round(entry.params * 2),
    sim: { kvPerToken: 32768, promptTps: 3200, genTps: 92 },
    failAfter: entry.gated && !settings.hfToken ? 0.001 : 0,
  };
  models.push(m);
  startTransfer(m, { convert: true });
  change('models');
  json(res, 200, publicModel(m));
});
route('PATCH', '/api/models/([\\w-]+)', async (req, res, id) => {
  const m = models.find((x) => x.id === id);
  if (!m) return fail(res, 404, 'There is no such model.');
  const b = await body(req);
  if (typeof b.name === 'string') {
    if (!b.name.trim()) return fail(res, 400, 'Give the model a name.', { name: 'Give the model a name.' });
    m.name = b.name.trim();
  }
  if (b.settings) {
    const fields = {};
    const s = b.settings;
    if (s.context !== undefined && (s.context < 0 || s.context > m.meta.ctxTrain)) fields.context = `Between 0 and ${m.meta.ctxTrain}.`;
    if (s.parallel !== undefined && (s.parallel < 1 || s.parallel > 64)) fields.parallel = 'Between 1 and 64.';
    if (Object.keys(fields).length) return fail(res, 400, 'Some settings are not valid.', fields);
    m.settings = { ...b.settings };
  }
  change('models');
  if (m.id === engine.modelId) startEngine(m, { restart: true });
  json(res, 200, publicModel(m));
});
route('DELETE', '/api/models/([\\w-]+)', (req, res, id) => {
  const i = models.findIndex((x) => x.id === id);
  if (i < 0) return fail(res, 404, 'There is no such model.');
  if (models[i].id === engine.modelId) stopEngine();
  transfers.delete(id);
  models.splice(i, 1);
  change('models');
  json(res, 200, {});
});
route('POST', '/api/models/([\\w-]+)/activate', (req, res, id) => {
  const m = models.find((x) => x.id === id);
  if (!m) return fail(res, 404, 'There is no such model.');
  if (m.state !== 'ready') return fail(res, 409, 'This model is not downloaded yet.');
  startEngine(m);
  change('models');
  json(res, 200, {});
});
route('POST', '/api/models/([\\w-]+)/plan', async (req, res, id) => {
  const m = models.find((x) => x.id === id);
  if (!m) return fail(res, 404, 'There is no such model.');
  const b = await body(req);
  await sleep(rand(900, 1800));
  json(res, 200, planFor(m, b.settings || {}));
});
route('POST', '/api/models/([\\w-]+)/retry', (req, res, id) => {
  const m = models.find((x) => x.id === id);
  if (!m) return fail(res, 404, 'There is no such model.');
  if (settings.hfToken) m.failAfter = 0;
  else m.failAfter = 0.001;
  startTransfer(m, { convert: !!m.convertBytes });
  change('models');
  json(res, 200, {});
});
route('POST', '/api/engine/stop', (req, res) => {
  stopEngine();
  change('models');
  json(res, 200, {});
});
route('POST', '/api/engine/restart', (req, res) => {
  const m = models.find((x) => x.id === engine.modelId);
  if (!m) return fail(res, 409, 'No model is active.');
  startEngine(m, { restart: true });
  json(res, 200, {});
});

route('GET', '/api/hf/search', async (req, res) => {
  const q = (new URL(req.url, 'http://x').searchParams.get('q') || '').trim().toLowerCase();
  await sleep(rand(150, 450));
  const found = hf.filter((e) => !q || e.id.toLowerCase().includes(q) || q.split(/\s+/).every((w) => e.id.toLowerCase().includes(w)))
    .sort((a, b) => b.downloads - a.downloads)
    .map((e) => ({ id: e.id, downloads: e.downloads, likes: e.likes, gguf: e.gguf, updatedAt: now() - Math.round(rand(2, 60)) * 86400e3, pipeline: e.pipeline }));
  json(res, 200, { models: found });
});
route('GET', '/api/hf/repo', async (req, res) => {
  const id = (new URL(req.url, 'http://x').searchParams.get('id') || '').trim();
  await sleep(rand(300, 700));
  const entry = findRepo(id);
  if (!entry) return fail(res, 404, `Hugging Face has no public model called ${id}.`);
  json(res, 200, repoDetails(entry));
});

route('POST', '/api/chat', async (req, res) => {
  const b = await body(req);
  const messages = Array.isArray(b.messages) ? b.messages : [];
  if (!messages.length) return fail(res, 400, 'Send at least one message.');
  if (engine.state === 'stopped') return fail(res, 409, 'No model is loaded. Activate one on the Models page.');
  const chars = messages.reduce((n, m) => n + String(m.content || '').length, 0);
  const promptTokens = 148 + Math.round(chars / 3.6);
  const cached = messages.length > 2 ? Math.max(0, promptTokens - Math.round(String(messages[messages.length - 1].content || '').length / 3.6) - 12) : 0;
  const text = String(messages[messages.length - 1].content || '').toLowerCase();
  let canned = ANSWERS[answerIndex++ % ANSWERS.length];
  if (/haiku|poem/.test(text)) canned = ANSWERS[1];
  else if (/python|code|stream/.test(text)) canned = ANSWERS[2];
  else if (/gpu|ram|nvme|offload|mmap|memory/.test(text)) canned = ANSWERS[0];
  const thinking = !(b.chat_template_kwargs && b.chat_template_kwargs.enable_thinking === false);
  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store', Connection: 'keep-alive' });
  const ctrl = new AbortController();
  res.on('close', () => ctrl.abort());
  const created = Math.floor(now() / 1000);
  const id = `chatcmpl-${randomBytes(6).toString('hex')}`;
  const chunk = (choice, extra = {}) => res.write(`data: ${JSON.stringify({ id, object: 'chat.completion.chunk', created, model: engine.modelId, system_fingerprint: 'b6912-a1c3e0f2', choices: [choice], ...extra })}\n\n`);
  const keepAlive = setInterval(() => res.write(': waiting\n\n'), 5000);
  let first = true;
  try {
    await serve({
      source: 'chat', key: '', path: '/api/chat', promptTokens, cached, reasoning: thinking ? canned.reasoning : '', answer: canned.answer,
      maxTokens: Number(b.max_tokens) || 0, signal: ctrl.signal,
      emit(e) {
        if (e.prompt_progress) {
          chunk({ index: 0, delta: first ? { role: 'assistant', content: null } : {}, finish_reason: null }, { prompt_progress: e.prompt_progress });
          first = false;
        } else if (e.delta) {
          chunk({ index: 0, delta: e.delta, finish_reason: null }, { timings: e.timings });
        } else if (e.done) {
          chunk({ index: 0, delta: {}, finish_reason: e.finish }, { usage: e.usage, timings: e.timings });
          res.write('data: [DONE]\n\n');
        }
      },
    });
  } catch (error) {
    if (error.message !== 'aborted') res.write(`data: ${JSON.stringify({ error: { code: 503, message: error.message, type: 'unavailable_error' } })}\n\n`);
  } finally {
    clearInterval(keepAlive);
    res.end();
  }
});

route('GET', '/api/keys', (req, res) => {
  const host = (req.headers.host || 'localhost:8090').replace(/:\d+$/, '');
  json(res, 200, { keys, baseUrl: `http://${host}:8080/v1` });
});
route('POST', '/api/keys', async (req, res) => {
  const b = await body(req);
  const name = String(b.name || '').trim();
  if (!name) return fail(res, 400, 'Give the key a name.', { name: 'Give the key a name.' });
  if (keys.some((k) => k.name === name)) return fail(res, 409, 'A key with this name exists already.', { name: 'A key with this name exists already.' });
  const secret = `sk-soc-${randomBytes(24).toString('base64url')}`;
  const key = { id: `k${randomBytes(3).toString('hex')}`, name, prefix: secret.slice(0, 11), createdAt: now(), lastUsedAt: 0, requests: 0, tokensIn: 0, tokensOut: 0 };
  keys.push(key);
  change('keys');
  json(res, 200, { key, secret });
});
route('DELETE', '/api/keys/([\\w-]+)', (req, res, id) => {
  const i = keys.findIndex((k) => k.id === id);
  if (i < 0) return fail(res, 404, 'There is no such key.');
  keys.splice(i, 1);
  change('keys');
  json(res, 200, {});
});
route('GET', '/api/requests', (req, res) => json(res, 200, { requests }));
route('GET', '/api/settings', (req, res) => json(res, 200, { hfToken: settings.hfToken ? 'set' : '', requireKey: settings.requireKey, modelsDir: settings.modelsDir }));
route('PUT', '/api/settings', async (req, res) => {
  const b = await body(req);
  if (typeof b.hfToken === 'string') {
    if (b.hfToken && !/^hf_\w{8,}$/.test(b.hfToken)) return fail(res, 400, 'A Hugging Face token starts with hf_.', { hfToken: 'A Hugging Face token starts with hf_.' });
    settings.hfToken = b.hfToken;
  }
  if (typeof b.requireKey === 'boolean') settings.requireKey = b.requireKey;
  change('settings');
  json(res, 200, { hfToken: settings.hfToken ? 'set' : '', requireKey: settings.requireKey, modelsDir: settings.modelsDir });
});

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://x');
  if (!url.pathname.startsWith('/api/')) return serveFile(req, res);
  for (const r of routes) {
    if (r.method !== req.method) continue;
    const match = r.pattern.exec(url.pathname);
    if (!match) continue;
    if (!r.open && !sessionOf(req)) return fail(res, 401, 'Sign in first.');
    if (req.method !== 'GET' && req.headers['x-socrates'] !== '1') return fail(res, 403, 'The request lacks the X-Socrates header.');
    try {
      return await r.handler(req, res, ...match.slice(1));
    } catch (error) {
      console.error(error);
      if (!res.headersSent) return fail(res, 500, 'The mock failed. See its console.');
      return res.end();
    }
  }
  fail(res, 404, 'This API address does not exist.');
});

server.listen(PORT, () => {
  console.log(`Socrates API mock: http://localhost:${PORT}${password ? '' : ' (first start: set a password)'}`);
});

// The engine starts with the active model, as the server does after a restart.
log('socrates: Socrates API 0.1.0 starting');
log(`socrates: ${SYS.gpu.name} (${(SYS.gpu.total / GiB).toFixed(0)} GB), ${(SYS.ram.total / GiB).toFixed(0)} GB RAM, ${SYS.cpu.name}`);
startTransfer(models[2]);
startEngine(models[0]);
