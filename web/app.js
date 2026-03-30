const state = {
  config: null,
  runtime: null,
  meta: null,
  flashes: [],
  connectionLive: false,
  recorder: {
    stream: null,
    audioContext: null,
    source: null,
    processor: null,
    muteGain: null,
    chunks: [],
    sampleRate: 44100,
    active: false,
    samples: null,
    trimStart: 0,
    trimEnd: 1,
    previewAudioContext: null,
    previewSource: null,
  },
};

const els = {
  title: document.getElementById("app-title"),
  comboTier: document.getElementById("combo-tier"),
  comboValue: document.getElementById("combo-value"),
  energyLabel: document.getElementById("energy-label"),
  loopsLabel: document.getElementById("loops-label"),
  savePath: document.getElementById("save-path"),
  saveButton: document.getElementById("save-button"),
  saveStatus: document.getElementById("save-status"),
  samplesGain: document.getElementById("samples-gain"),
  samplesGainValue: document.getElementById("samples-gain-value"),
  masterGain: document.getElementById("master-gain"),
  masterGainValue: document.getElementById("master-gain-value"),
  synthGain: document.getElementById("synth-gain"),
  synthGainValue: document.getElementById("synth-gain-value"),
  hitThreshold: document.getElementById("hit-threshold"),
  hitThresholdValue: document.getElementById("hit-threshold-value"),
  samplesEnabled: document.getElementById("samples-enabled"),
  synthEnabledMaster: document.getElementById("synth-enabled-master"),
  recordingName: document.getElementById("recording-name"),
  recordStartButton: document.getElementById("record-start-button"),
  recordStopButton: document.getElementById("record-stop-button"),
  recordingStatus: document.getElementById("recording-status"),
  recordingEditor: document.getElementById("recording-editor"),
  recordingWaveform: document.getElementById("recording-waveform"),
  trimStart: document.getElementById("trim-start"),
  trimStartValue: document.getElementById("trim-start-value"),
  trimEnd: document.getElementById("trim-end"),
  trimEndValue: document.getElementById("trim-end-value"),
  recordPreviewButton: document.getElementById("record-preview-button"),
  recordSaveButton: document.getElementById("record-save-button"),
  recordDiscardButton: document.getElementById("record-discard-button"),
  layerList: document.getElementById("layer-list"),
  synthControls: document.getElementById("synth-controls"),
  fxControls: document.getElementById("fx-controls"),
  visualControls: document.getElementById("visual-controls"),
  impactSummary: document.getElementById("impact-summary"),
  toggle: document.getElementById("settings-toggle"),
  helpToggle: document.getElementById("help-toggle"),
  close: document.getElementById("settings-close"),
  drawer: document.getElementById("settings-drawer"),
  helpModal: document.getElementById("help-modal"),
  helpClose: document.getElementById("help-close"),
  canvas: document.getElementById("fx-canvas"),
};

const ctx = els.canvas.getContext("2d");
const patchDebouncers = new Map();
let eventSource;
let lastFrame = performance.now();

async function boot() {
  bindDrawer();
  bindRecordingControls();
  const response = await fetch("/api/state");
  applySnapshot(await response.json());
  renderControls();
  connectEvents();
  resizeCanvas();
  requestAnimationFrame(tick);
}

function bindDrawer() {
  els.toggle.addEventListener("click", () => {
    els.drawer.classList.toggle("is-open");
    els.drawer.setAttribute("aria-hidden", String(!els.drawer.classList.contains("is-open")));
  });
  els.close.addEventListener("click", () => {
    els.drawer.classList.remove("is-open");
    els.drawer.setAttribute("aria-hidden", "true");
  });
  els.helpToggle.addEventListener("click", () => {
    els.helpModal.classList.add("is-open");
    els.helpModal.setAttribute("aria-hidden", "false");
  });
  els.helpClose.addEventListener("click", closeHelp);
  els.helpModal.addEventListener("click", (event) => {
    if (event.target === els.helpModal) {
      closeHelp();
    }
  });
}

function closeHelp() {
  els.helpModal.classList.remove("is-open");
  els.helpModal.setAttribute("aria-hidden", "true");
}

function bindRecordingControls() {
  els.recordStartButton.addEventListener("click", startRecording);
  els.recordStopButton.addEventListener("click", stopRecordingAndPrepare);
  els.trimStart.addEventListener("input", () => handleTrimChange("start"));
  els.trimEnd.addEventListener("input", () => handleTrimChange("end"));
  els.recordPreviewButton.addEventListener("click", playTrimmedPreview);
  els.recordSaveButton.addEventListener("click", saveTrimmedRecording);
  els.recordDiscardButton.addEventListener("click", discardRecordingEdit);
}

function connectEvents() {
  eventSource = new EventSource("/api/events");
  eventSource.addEventListener("snapshot", (event) => {
    applySnapshot(JSON.parse(event.data));
    renderControls();
  });
  eventSource.addEventListener("impact", (event) => {
    handleImpact(JSON.parse(event.data));
  });
}

function applySnapshot(snapshot) {
  state.config = snapshot.config;
  state.runtime = snapshot.runtime;
  state.meta = snapshot.meta;
  els.title.textContent = state.config.web?.title || state.config.title || "spank instrument";
  if (document.activeElement !== els.savePath) {
    els.savePath.value = state.meta?.save_path || "";
  }
  updateHUD();
  updateSaveStatus();
}

function updateHUD() {
  if (!state.runtime) return;
  els.comboTier.textContent = state.runtime.tier || "idle";
  els.comboValue.textContent = String(state.runtime.combo || 0);
  els.energyLabel.textContent = `energy ${Number(state.runtime.energy || 0).toFixed(2)}`;
  els.loopsLabel.textContent = `loops ${state.runtime.active_loops || 0}`;
}

function updateSaveStatus(message) {
  if (message) {
    els.saveStatus.textContent = message;
    return;
  }
  if (!state.meta) {
    els.saveStatus.textContent = "ready";
    return;
  }
  if (state.meta.last_save_error) {
    els.saveStatus.textContent = `error: ${state.meta.last_save_error}`;
    return;
  }
  if (state.meta.dirty) {
    els.saveStatus.textContent = "unsaved changes";
    return;
  }
  if (state.meta.last_saved_unix) {
    els.saveStatus.textContent = `saved ${new Date(state.meta.last_saved_unix).toLocaleTimeString()}`;
    return;
  }
  els.saveStatus.textContent = "ready";
}

function renderControls() {
  if (!state.config) return;

  const audio = state.config.audio;
  const input = state.config.input || {};
  els.samplesEnabled.checked = !!audio.samples_enabled;
  els.synthEnabledMaster.checked = !!audio.synth.enabled;
  els.samplesGain.value = Number(audio.samples_gain ?? 1).toFixed(2);
  els.samplesGainValue.textContent = Number(audio.samples_gain ?? 1).toFixed(2);
  els.masterGain.value = Number(audio.master_gain).toFixed(2);
  els.masterGainValue.textContent = Number(audio.master_gain).toFixed(2);
  els.synthGain.value = Number(audio.synth_gain ?? 1).toFixed(2);
  els.synthGainValue.textContent = Number(audio.synth_gain ?? 1).toFixed(2);
  els.hitThreshold.value = Number(input.min_amplitude ?? 0.05).toFixed(2);
  els.hitThresholdValue.textContent = Number(input.min_amplitude ?? 0.05).toFixed(2);

  els.samplesEnabled.onchange = () => postJSON("/api/mixer", { samples_enabled: els.samplesEnabled.checked });
  els.synthEnabledMaster.onchange = () => postJSON("/api/synth", { enabled: els.synthEnabledMaster.checked });
  els.samplesGain.oninput = () => {
    const value = Number(els.samplesGain.value);
    els.samplesGainValue.textContent = value.toFixed(2);
    queueJSONPatch("mixer-samples-gain", "/api/mixer", { samples_gain: value });
  };
  els.masterGain.oninput = () => {
    const value = Number(els.masterGain.value);
    els.masterGainValue.textContent = value.toFixed(2);
    queueJSONPatch("mixer", "/api/mixer", { master_gain: value });
  };
  els.synthGain.oninput = () => {
    const value = Number(els.synthGain.value);
    els.synthGainValue.textContent = value.toFixed(2);
    queueJSONPatch("mixer-synth-gain", "/api/mixer", { synth_gain: value });
  };
  els.hitThreshold.oninput = () => {
    const value = Number(els.hitThreshold.value);
    els.hitThresholdValue.textContent = value.toFixed(2);
    queueJSONPatch("input-threshold", "/api/input", { min_amplitude: value });
  };
  els.saveButton.onclick = handleSave;

  renderLayers();
  renderSynthControls();
  renderFXControls();
  renderVisualControls();
}

function renderLayers() {
  els.layerList.innerHTML = "";
  state.config.audio.layers.forEach((layer) => {
    const isManagedRecording = !!layer.managed_recording;
    const wrapper = document.createElement("div");
    wrapper.className = "layer";
    wrapper.innerHTML = `
      <div class="layer-top">
        <div>
          <div class="layer-name">${escapeHtml(layer.label)}</div>
          <div class="layer-tag">${escapeHtml(layer.family)} / ${escapeHtml(layer.trigger)}${isManagedRecording ? " / recording" : ""}</div>
        </div>
        <div class="button-row">
          ${isManagedRecording ? '<button class="secondary delete-layer-button" type="button">delete</button>' : ""}
          <label class="toggle-row"><input type="checkbox" ${layer.enabled ? "checked" : ""}>on</label>
        </div>
      </div>
      <label>gain</label>
      <div class="slider-row">
        <input type="range" min="0" max="1.5" step="0.01" value="${Number(layer.gain).toFixed(2)}">
        <span>${Number(layer.gain).toFixed(2)}</span>
      </div>
      <label>delay send</label>
      <div class="slider-row layer-delay-row">
        <input type="range" min="0" max="1" step="0.01" value="${Number(layer.delay_send || 0).toFixed(2)}">
        <span>${Number(layer.delay_send || 0).toFixed(2)}</span>
      </div>
      <label>reverb send</label>
      <div class="slider-row layer-reverb-row">
        <input type="range" min="0" max="1" step="0.01" value="${Number(layer.reverb_send || 0).toFixed(2)}">
        <span>${Number(layer.reverb_send || 0).toFixed(2)}</span>
      </div>
    `;

    const checkbox = wrapper.querySelector('input[type="checkbox"]');
    const [gainSlider, delaySlider, reverbSlider] = wrapper.querySelectorAll('input[type="range"]');
    const [gainValue, delayValue, reverbValue] = wrapper.querySelectorAll(".slider-row span");
    const deleteButton = wrapper.querySelector(".delete-layer-button");

    checkbox.addEventListener("change", () => {
      postJSON(`/api/layers/${encodeURIComponent(layer.id)}`, { enabled: checkbox.checked });
    });

    gainSlider.addEventListener("input", () => {
      const gain = Number(gainSlider.value);
      gainValue.textContent = gain.toFixed(2);
      queueJSONPatch(`layer:${layer.id}`, `/api/layers/${encodeURIComponent(layer.id)}`, { gain });
    });

    delaySlider.addEventListener("input", () => {
      const send = Number(delaySlider.value);
      delayValue.textContent = send.toFixed(2);
      queueJSONPatch(`layer-delay:${layer.id}`, `/api/layers/${encodeURIComponent(layer.id)}`, { delay_send: send });
    });

    reverbSlider.addEventListener("input", () => {
      const send = Number(reverbSlider.value);
      reverbValue.textContent = send.toFixed(2);
      queueJSONPatch(`layer-reverb:${layer.id}`, `/api/layers/${encodeURIComponent(layer.id)}`, { reverb_send: send });
    });

    if (deleteButton) {
      deleteButton.addEventListener("click", async () => {
        deleteButton.disabled = true;
        try {
          await fetchJSON(`/api/layers/${encodeURIComponent(layer.id)}`, { method: "DELETE" });
        } catch (error) {
          console.error(error);
          deleteButton.disabled = false;
        }
      });
    }

    els.layerList.appendChild(wrapper);
  });
}

function renderSynthControls() {
  const synth = state.config.audio.synth;
  els.synthControls.innerHTML = `
    <div>
      <label for="synth-wave">wave</label>
      <select id="synth-wave">
        ${["sine", "triangle", "square", "saw", "reverse-saw"].map((wave) => `<option value="${wave}" ${synth.wave === wave ? "selected" : ""}>${wave}</option>`).join("")}
      </select>
    </div>
    ${rangeControl("synth-frequency", "frequency", 40, 880, 1, synth.frequency, "hz")}
    ${rangeControl("synth-pitch-follow", "pitch follow", 0, 2, 0.01, synth.pitch_follow ?? 0.9, "")}
    ${rangeControl("synth-gain", "gain", 0, 1.2, 0.01, synth.gain, "")}
    ${rangeControl("synth-duration", "duration", 40, 1000, 1, synth.duration_ms, "ms")}
    ${rangeControl("synth-attack", "attack", 0, 200, 1, synth.attack_ms, "ms")}
    ${rangeControl("synth-release", "release", 0, 800, 1, synth.release_ms, "ms")}
    <div class="button-row">
      <button id="synth-test-button" class="secondary" type="button">test synth</button>
      <span class="hint">plays on every hit when enabled</span>
    </div>
  `;

  document.getElementById("synth-wave").addEventListener("change", (event) => {
    postJSON("/api/synth", { wave: event.target.value });
  });
  document.getElementById("synth-test-button").addEventListener("click", () => {
    postJSON("/api/synth/test", {});
  });
  bindRange("synth-frequency", "synth-frequency-value", (value) => queueJSONPatch("synth-frequency", "/api/synth", { frequency: value }), "hz");
  bindRange("synth-pitch-follow", "synth-pitch-follow-value", (value) => queueJSONPatch("synth-pitch-follow", "/api/synth", { pitch_follow: value }));
  bindRange("synth-gain", "synth-gain-value", (value) => queueJSONPatch("synth-gain", "/api/synth", { gain: value }));
  bindRange("synth-duration", "synth-duration-value", (value) => queueJSONPatch("synth-duration", "/api/synth", { duration_ms: value }), "ms");
  bindRange("synth-attack", "synth-attack-value", (value) => queueJSONPatch("synth-attack", "/api/synth", { attack_ms: value }), "ms");
  bindRange("synth-release", "synth-release-value", (value) => queueJSONPatch("synth-release", "/api/synth", { release_ms: value }), "ms");
}

function renderFXControls() {
  const { delay, reverb } = state.config.audio.fx;
  els.fxControls.innerHTML = `
    <div class="hint">global fx for wavs and synth</div>
    <label class="toggle-row"><input id="delay-enabled" type="checkbox" ${delay.enabled ? "checked" : ""}> delay enabled</label>
    ${rangeControl("delay-time", "delay time", 40, 800, 1, delay.time_ms, "ms")}
    ${rangeControl("delay-feedback", "delay feedback", 0, 0.999, 0.001, delay.feedback, "")}
    ${rangeControl("delay-mix", "delay mix", 0, 1, 0.01, delay.mix, "")}
    <label class="toggle-row"><input id="reverb-enabled" type="checkbox" ${reverb.enabled ? "checked" : ""}> reverb enabled</label>
    ${rangeControl("reverb-pre", "reverb pre-delay", 0, 180, 1, reverb.pre_delay_ms, "ms")}
    ${rangeControl("reverb-decay", "reverb decay", 0, 0.999, 0.001, reverb.decay, "")}
    ${rangeControl("reverb-mix", "reverb mix", 0, 1, 0.01, reverb.mix, "")}
  `;

  document.getElementById("delay-enabled").addEventListener("change", (event) => {
    postJSON("/api/fx", { delay_enabled: event.target.checked });
  });
  document.getElementById("reverb-enabled").addEventListener("change", (event) => {
    postJSON("/api/fx", { reverb_enabled: event.target.checked });
  });
  bindRange("delay-time", "delay-time-value", (value) => queueJSONPatch("fx-delay-time", "/api/fx", { delay_time_ms: value }), "ms");
  bindRange("delay-feedback", "delay-feedback-value", (value) => queueJSONPatch("fx-delay-feedback", "/api/fx", { delay_feedback: value }));
  bindRange("delay-mix", "delay-mix-value", (value) => queueJSONPatch("fx-delay-mix", "/api/fx", { delay_mix: value }));
  bindRange("reverb-pre", "reverb-pre-value", (value) => queueJSONPatch("fx-reverb-pre", "/api/fx", { reverb_pre_delay_ms: value }), "ms");
  bindRange("reverb-decay", "reverb-decay-value", (value) => queueJSONPatch("fx-reverb-decay", "/api/fx", { reverb_decay: value }));
  bindRange("reverb-mix", "reverb-mix-value", (value) => queueJSONPatch("fx-reverb-mix", "/api/fx", { reverb_mix: value }));
}

function renderVisualControls() {
  const impact = state.config.visual.impact;
  const rgb = impact.color_rgb || [255, 255, 255];
  const preview = `rgb(${rgb[0]}, ${rgb[1]}, ${rgb[2]})`;
  els.visualControls.innerHTML = `
    <div class="color-preview-block">
      <div class="color-preview" style="background:${preview}"></div>
      <div class="hint">${preview}</div>
    </div>
    ${colorRangeControl("flash-r", "red", 0, 255, 1, rgb[0], "255,0,0")}
    ${colorRangeControl("flash-g", "green", 0, 255, 1, rgb[1], "0,255,0")}
    ${colorRangeControl("flash-b", "blue", 0, 255, 1, rgb[2], "0,128,255")}
    ${rangeControl("flash-fade", "fade out", 60, 3000, 1, impact.fade_ms, "ms")}
  `;

  const queueColorPatch = () => {
    const rgbValue = [
      Number(document.getElementById("flash-r").value),
      Number(document.getElementById("flash-g").value),
      Number(document.getElementById("flash-b").value),
    ];
    queueJSONPatch("visual-color", "/api/visual", { color_rgb: rgbValue });
  };

  bindRange("flash-r", "flash-r-value", queueColorPatch);
  bindRange("flash-g", "flash-g-value", queueColorPatch);
  bindRange("flash-b", "flash-b-value", queueColorPatch);
  bindRange("flash-fade", "flash-fade-value", (value) => queueJSONPatch("visual-fade", "/api/visual", { fade_ms: value }), "ms");
}

function colorRangeControl(id, label, min, max, step, value, rgb) {
  const numeric = Number(value);
  return `
    <div>
      <label for="${id}">${label}</label>
      <div class="channel-preview">
        <div id="${id}-bar" class="channel-bar" style="width:${(numeric / 255) * 100}%; background:rgb(${rgb})"></div>
      </div>
      <div class="slider-row">
        <input id="${id}" type="range" min="${min}" max="${max}" step="${step}" value="${numeric}">
        <span id="${id}-value">${numeric.toFixed(0)}</span>
      </div>
    </div>
  `;
}

function rangeControl(id, label, min, max, step, value, suffix) {
  const numeric = Number(value);
  const text = suffix ? `${numeric.toFixed(step >= 1 ? 0 : 2)}${suffix}` : numeric.toFixed(step >= 1 ? 0 : 2);
  return `
    <div>
      <label for="${id}">${label}</label>
      <div class="slider-row">
        <input id="${id}" type="range" min="${min}" max="${max}" step="${step}" value="${numeric}">
        <span id="${id}-value">${text}</span>
      </div>
    </div>
  `;
}

function bindRange(id, valueId, onChange, suffix = "") {
  const input = document.getElementById(id);
  const label = document.getElementById(valueId);
  const bar = document.getElementById(`${id}-bar`);
  input.addEventListener("input", () => {
    const numeric = Number(input.value);
    label.textContent = suffix ? `${numeric.toFixed(input.step >= 1 ? 0 : 2)}${suffix}` : numeric.toFixed(input.step >= 1 ? 0 : 2);
    if (bar) {
      bar.style.width = `${(numeric / Number(input.max || 255)) * 100}%`;
    }
    onChange(numeric);
  });
}

function handleImpact(event) {
  state.runtime = {
    combo: event.combo,
    energy: event.energy,
    tier: event.tier,
    active_loops: event.active_loops || 0,
  };
  updateHUD();

  const impactConfig = state.config.visual.impact;
  state.flashes.push({
    amplitude: normalizeAmplitude(event.amplitude),
    age: 0,
    flashMs: impactConfig.flash_ms || 120,
    fadeMs: impactConfig.fade_ms || 900,
    color: impactConfig.color_rgb || [255, 255, 255],
  });
  state.flashes = state.flashes.slice(-8);
}

function normalizeAmplitude(amplitude) {
  const fullScale = Math.max(0.01, Number(state.config?.visual?.impact?.full_scale_amplitude || 0.35));
  return Math.max(0, Math.min(1, amplitude / fullScale));
}

function tick(now) {
  const dt = Math.min(0.05, (now - lastFrame) / 1000);
  lastFrame = now;
  resizeCanvas();

  const width = window.innerWidth;
  const height = window.innerHeight;
  ctx.clearRect(0, 0, width, height);

  state.flashes = state.flashes.filter((flash) => flash.age * 1000 < flash.fadeMs);
  state.flashes.forEach((flash) => {
    flash.age += dt;
    const elapsedMs = flash.age * 1000;
    const whiteAlpha = elapsedMs < flash.flashMs ? flash.amplitude * (1 - elapsedMs / flash.flashMs) : 0;
    const colorAlpha = flash.amplitude * Math.max(0, 1 - elapsedMs / flash.fadeMs);
    const scaled = flash.color.map((channel) => Math.round(channel * flash.amplitude));

    if (whiteAlpha > 0.001) {
      ctx.fillStyle = `rgba(255,255,255,${whiteAlpha})`;
      ctx.fillRect(0, 0, width, height);
    }
    if (colorAlpha > 0.001) {
      ctx.fillStyle = `rgba(${scaled[0]},${scaled[1]},${scaled[2]},${colorAlpha})`;
      ctx.fillRect(0, 0, width, height);
    }
  });

  requestAnimationFrame(tick);
}

async function handleSave() {
  els.saveButton.disabled = true;
  updateSaveStatus("saving...");
  try {
    await postJSON("/api/save", { path: els.savePath.value.trim() });
  } catch (error) {
    console.error(error);
    updateSaveStatus("save failed");
  } finally {
    els.saveButton.disabled = false;
  }
}

function queueJSONPatch(key, url, payload, delay = 60) {
  clearTimeout(patchDebouncers.get(key));
  const timeout = setTimeout(() => {
    postJSON(url, payload);
  }, delay);
  patchDebouncers.set(key, timeout);
}

async function fetchJSON(url, options = {}) {
  const response = await fetch(url, options);
  const raw = await response.text();
  let snapshot = null;
  try {
    snapshot = raw ? JSON.parse(raw) : null;
  } catch (error) {
    snapshot = null;
  }
  if (snapshot) {
    applySnapshot(snapshot);
    renderControls();
  }
  if (!response.ok) {
    throw new Error((snapshot && snapshot.error) || raw || `request failed: ${response.status}`);
  }
  return snapshot;
}

async function postJSON(url, payload) {
  return fetchJSON(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
}

function resizeCanvas() {
  const ratio = window.devicePixelRatio || 1;
  const width = window.innerWidth;
  const height = window.innerHeight;
  if (els.canvas.width !== width * ratio || els.canvas.height !== height * ratio) {
    els.canvas.width = width * ratio;
    els.canvas.height = height * ratio;
    els.canvas.style.width = `${width}px`;
    els.canvas.style.height = `${height}px`;
    ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
  }
}

async function startRecording() {
  if (state.recorder.active) return;
  try {
    discardRecordingEdit(true);
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    const audioContext = new AudioContext();
    const source = audioContext.createMediaStreamSource(stream);
    const processor = audioContext.createScriptProcessor(4096, 1, 1);
    const muteGain = audioContext.createGain();
    muteGain.gain.value = 0;

    state.recorder.stream = stream;
    state.recorder.audioContext = audioContext;
    state.recorder.source = source;
    state.recorder.processor = processor;
    state.recorder.muteGain = muteGain;
    state.recorder.chunks = [];
    state.recorder.sampleRate = audioContext.sampleRate;
    state.recorder.active = true;

    processor.onaudioprocess = (event) => {
      if (!state.recorder.active) return;
      state.recorder.chunks.push(new Float32Array(event.inputBuffer.getChannelData(0)));
    };

    source.connect(processor);
    processor.connect(muteGain);
    muteGain.connect(audioContext.destination);

    els.recordStartButton.disabled = true;
    els.recordStopButton.disabled = false;
    els.recordingStatus.textContent = "recording...";
  } catch (error) {
    console.error(error);
    els.recordingStatus.textContent = "mic denied";
  }
}

async function stopRecordingAndPrepare() {
  if (!state.recorder.active) return;
  els.recordStopButton.disabled = true;
  els.recordingStatus.textContent = "encoding wav...";
  try {
    const result = await finishRecording();
    state.recorder.samples = result.samples;
    state.recorder.sampleRate = result.sampleRate;
    state.recorder.trimStart = 0;
    state.recorder.trimEnd = 1;
    openRecordingEditor();
    els.recordingStatus.textContent = "trim, preview or save";
  } catch (error) {
    console.error(error);
    els.recordingStatus.textContent = "record failed";
  } finally {
    els.recordStartButton.disabled = false;
    els.recordStopButton.disabled = true;
  }
}

async function finishRecording() {
  state.recorder.active = false;
  state.recorder.processor?.disconnect();
  state.recorder.source?.disconnect();
  state.recorder.muteGain?.disconnect();
  state.recorder.stream?.getTracks().forEach((track) => track.stop());
  if (state.recorder.audioContext) {
    await state.recorder.audioContext.close();
  }
  const merged = mergeChunks(state.recorder.chunks);
  const sampleRate = state.recorder.sampleRate;
  state.recorder.stream = null;
  state.recorder.audioContext = null;
  state.recorder.source = null;
  state.recorder.processor = null;
  state.recorder.muteGain = null;
  state.recorder.chunks = [];
  return { samples: merged, sampleRate };
}

function mergeChunks(chunks) {
  let total = 0;
  chunks.forEach((chunk) => {
    total += chunk.length;
  });
  const merged = new Float32Array(total);
  let offset = 0;
  chunks.forEach((chunk) => {
    merged.set(chunk, offset);
    offset += chunk.length;
  });
  return merged;
}

function encodeWav(samples, sampleRate) {
  const buffer = new ArrayBuffer(44 + samples.length * 2);
  const view = new DataView(buffer);
  writeAscii(view, 0, "RIFF");
  view.setUint32(4, 36 + samples.length * 2, true);
  writeAscii(view, 8, "WAVE");
  writeAscii(view, 12, "fmt ");
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true);
  view.setUint16(22, 1, true);
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  writeAscii(view, 36, "data");
  view.setUint32(40, samples.length * 2, true);
  let offset = 44;
  samples.forEach((sample) => {
    const clamped = Math.max(-1, Math.min(1, sample));
    view.setInt16(offset, clamped < 0 ? clamped * 0x8000 : clamped * 0x7fff, true);
    offset += 2;
  });
  return new Blob([buffer], { type: "audio/wav" });
}

function openRecordingEditor() {
  els.recordingEditor.classList.remove("is-hidden");
  els.recordPreviewButton.disabled = false;
  els.recordSaveButton.disabled = false;
  els.recordDiscardButton.disabled = false;
  els.trimStart.value = "0";
  els.trimEnd.value = "1";
  updateTrimLabels();
  drawRecordingWaveform();
}

function discardRecordingEdit(silent = false) {
  stopPreviewPlayback();
  state.recorder.samples = null;
  state.recorder.trimStart = 0;
  state.recorder.trimEnd = 1;
  els.recordingEditor.classList.add("is-hidden");
  els.recordPreviewButton.disabled = true;
  els.recordSaveButton.disabled = true;
  els.recordDiscardButton.disabled = true;
  els.trimStart.value = "0";
  els.trimEnd.value = "1";
  if (!silent) {
    els.recordingStatus.textContent = "discarded";
  }
}

function handleTrimChange(source) {
  let start = Number(els.trimStart.value);
  let end = Number(els.trimEnd.value);
  if (start > end - 0.005) {
    if (source === "start") {
      start = Math.max(0, end - 0.005);
      els.trimStart.value = String(start);
    } else {
      end = Math.min(1, start + 0.005);
      els.trimEnd.value = String(end);
    }
  }
  state.recorder.trimStart = Number(els.trimStart.value);
  state.recorder.trimEnd = Number(els.trimEnd.value);
  updateTrimLabels();
  drawRecordingWaveform();
}

function updateTrimLabels() {
  const duration = getRecordingDuration();
  els.trimStartValue.textContent = `${(duration * Number(els.trimStart.value)).toFixed(2)}s`;
  els.trimEndValue.textContent = `${(duration * Number(els.trimEnd.value)).toFixed(2)}s`;
}

function getRecordingDuration() {
  if (!state.recorder.samples || !state.recorder.sampleRate) return 0;
  return state.recorder.samples.length / state.recorder.sampleRate;
}

function getTrimmedSamples() {
  if (!state.recorder.samples) {
    return new Float32Array(0);
  }
  const startIndex = Math.max(0, Math.floor(state.recorder.samples.length * Number(els.trimStart.value)));
  const endIndex = Math.min(state.recorder.samples.length, Math.ceil(state.recorder.samples.length * Number(els.trimEnd.value)));
  return state.recorder.samples.slice(startIndex, Math.max(startIndex+1, endIndex));
}

function drawRecordingWaveform() {
  const canvas = els.recordingWaveform;
  const context = canvas.getContext("2d");
  const width = canvas.clientWidth || 360;
  const height = canvas.clientHeight || 96;
  const ratio = window.devicePixelRatio || 1;
  if (canvas.width !== width * ratio || canvas.height !== height * ratio) {
    canvas.width = width * ratio;
    canvas.height = height * ratio;
    context.setTransform(ratio, 0, 0, ratio, 0, 0);
  }
  context.clearRect(0, 0, width, height);
  context.fillStyle = "#050505";
  context.fillRect(0, 0, width, height);
  if (!state.recorder.samples || state.recorder.samples.length === 0) {
    return;
  }

  const startX = Math.floor(Number(els.trimStart.value) * width);
  const endX = Math.ceil(Number(els.trimEnd.value) * width);
  context.fillStyle = "rgba(255,255,255,0.08)";
  context.fillRect(startX, 0, Math.max(1, endX - startX), height);

  context.strokeStyle = "#ffffff";
  context.lineWidth = 1;
  context.beginPath();
  const samples = state.recorder.samples;
  const blockSize = Math.max(1, Math.floor(samples.length / width));
  const midY = height / 2;
  for (let x = 0; x < width; x++) {
    const start = x * blockSize;
    const end = Math.min(samples.length, start + blockSize);
    let peak = 0;
    for (let i = start; i < end; i++) {
      peak = Math.max(peak, Math.abs(samples[i]));
    }
    const y = peak * (height * 0.42);
    context.moveTo(x + 0.5, midY - y);
    context.lineTo(x + 0.5, midY + y);
  }
  context.stroke();
}

async function playTrimmedPreview() {
  const samples = getTrimmedSamples();
  if (!samples.length) {
    return;
  }
  stopPreviewPlayback();
  const audioContext = new AudioContext();
  const buffer = audioContext.createBuffer(1, samples.length, state.recorder.sampleRate);
  buffer.copyToChannel(samples, 0);
  const source = audioContext.createBufferSource();
  source.buffer = buffer;
  source.connect(audioContext.destination);
  source.onended = async () => {
    if (state.recorder.previewSource === source) {
      state.recorder.previewSource = null;
      state.recorder.previewAudioContext = null;
    }
    await audioContext.close();
  };
  state.recorder.previewAudioContext = audioContext;
  state.recorder.previewSource = source;
  source.start();
}

function stopPreviewPlayback() {
  if (state.recorder.previewSource) {
    try {
      state.recorder.previewSource.stop();
    } catch (error) {
      // ignore double-stop
    }
    state.recorder.previewSource = null;
  }
  if (state.recorder.previewAudioContext) {
    state.recorder.previewAudioContext.close().catch(() => {});
    state.recorder.previewAudioContext = null;
  }
}

async function saveTrimmedRecording() {
  const samples = getTrimmedSamples();
  if (!samples.length) {
    return;
  }
  els.recordSaveButton.disabled = true;
  els.recordingStatus.textContent = "saving...";
  try {
    const wavBlob = encodeWav(samples, state.recorder.sampleRate);
    const form = new FormData();
    const name = (els.recordingName.value || "").trim();
    form.append("name", name);
    form.append("file", wavBlob, `${name || "recording"}.wav`);

    const response = await fetch("/api/recordings", {
      method: "POST",
      body: form,
    });
    const raw = await response.text();
    let payload = {};
    try {
      payload = raw ? JSON.parse(raw) : {};
    } catch (error) {
      payload = {};
    }
    if (!response.ok) {
      throw new Error(payload?.error || raw || `upload failed: ${response.status}`);
    }
    applySnapshot(payload.snapshot);
    renderControls();
    els.recordingName.value = "";
    discardRecordingEdit(true);
    els.recordingStatus.textContent = "saved";
  } catch (error) {
    console.error(error);
    els.recordingStatus.textContent = "save failed";
  } finally {
    els.recordSaveButton.disabled = false;
  }
}

function writeAscii(view, offset, value) {
  for (let i = 0; i < value.length; i++) {
    view.setUint8(offset + i, value.charCodeAt(i));
  }
}

function escapeHtml(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

window.addEventListener("resize", () => {
  resizeCanvas();
  drawRecordingWaveform();
});
boot().catch((error) => {
  console.error(error);
});
